package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/common"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"go.uber.org/zap"
)

// publishWaitTimeout is how long Publish will block trying to enqueue
// an event onto the inbound channel before giving up. Wave 28: the
// previous code used a non-blocking send with a `default:` drop and
// no logging at all, so under load events vanished without trace and
// producers had no way to react. A short blocking window applies very
// light back-pressure, gives the dispatcher a chance to drain in the
// common burst case, and produces audible log lines (rate-limited)
// when the bus is genuinely overloaded.
const publishWaitTimeout = 100 * time.Millisecond

type EventServiceWS struct {
	typeService *EventTypeService

	// Wave 30: ctx is now an atomic.Pointer so Publish() can read it
	// without taking the mutex (hot path) while Start() writes it
	// safely. Previously the bare *context.Context field was a textbook
	// data race — Publish checked it for nil and then dereferenced it
	// with no synchronization, so the read could observe a torn or
	// stale value depending on goroutine scheduling.
	ctxPtr atomic.Pointer[context.Context]
	stop   chan struct{}

	mutex sync.Mutex

	// Wave 32: inboundChannel is now read via atomic.Pointer so
	// Publish() never sees a torn channel header during Start()'s
	// teardown. The remaining close-then-send race is handled with
	// recover() at the send site — even with atomic loads, Start()
	// can close the channel after Publish() loaded the pointer but
	// before it reached the send. Without these two guards a clean
	// shutdown raced against an in-flight Publish would panic the
	// producer goroutine (which is often a long-lived service worker
	// — not a short-lived HTTP handler — and there is no HTTP-layer
	// recover above it).
	inboundChannelPtr  atomic.Pointer[chan model.Event]
	subscriberChannels map[string]map[string][]chan model.Event

	// Wave 28: drop counters made externally observable (atomic so
	// they can be read from metrics/health endpoints without taking
	// the dispatcher lock). publishDropped counts producer-side
	// drops; dispatchDropped counts consumer-side drops.
	publishDropped  atomic.Uint64
	dispatchDropped atomic.Uint64

	// publishDropLogAt rate-limits the per-drop warning to one line
	// per second so a runaway producer can't flood journald.
	publishDropLogMu sync.Mutex
	publishDropLogAt time.Time

	// B11: liveness. lastLoopAt (unix nanoseconds) is stamped on every
	// dispatcher loop iteration, including the idle 10s heartbeat tick,
	// so a health check can tell "idle" from "wedged". runs counts how
	// many times Start() has entered its loop; restartHook is invoked
	// on every run after the first so in-process consumers can
	// re-subscribe to the freshly rebuilt subscriber map.
	lastLoopAt  atomic.Int64
	runs        atomic.Uint64
	restartHook atomic.Pointer[func()]
}

// SetRestartHook installs the callback invoked after the dispatcher
// restarts (never after its first start). Used to re-attach in-process
// subscribers, whose channels the previous run closed on its way out.
func (s *EventServiceWS) SetRestartHook(hook func()) {
	s.restartHook.Store(&hook)
}

// Accepting reports whether the dispatcher currently has an inbound
// channel installed — i.e. whether a Publish can be handed over at all.
func (s *EventServiceWS) Accepting() bool {
	return s.inboundChannelPtr.Load() != nil
}

// LastLoopAt returns the time of the last dispatcher loop iteration, or
// the zero time if the dispatcher never ran.
func (s *EventServiceWS) LastLoopAt() time.Time {
	ns := s.lastLoopAt.Load()
	if ns == 0 {
		return time.Time{}
	}

	return time.Unix(0, ns)
}

// SubscriberCount returns the number of distinct subscriber channels
// currently attached to the dispatcher. It blocks on the dispatcher
// mutex; health checks must use TrySubscriberCount instead.
func (s *EventServiceWS) SubscriberCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.countSubscribersLocked()
}

// TrySubscriberCount is the health-check variant: it gives up instead of
// blocking. A liveness endpoint that can wait on the very mutex a
// wedged dispatcher is holding would hang exactly when it is needed
// most, so the count is reported as unavailable rather than paid for
// with an unbounded wait. The short retry window absorbs the normal
// case, where the dispatcher holds the lock only for a subscriber
// snapshot.
func (s *EventServiceWS) TrySubscriberCount(wait time.Duration) (int, bool) {
	deadline := time.Now().Add(wait)
	for {
		if s.mutex.TryLock() {
			count := s.countSubscribersLocked()
			s.mutex.Unlock()

			return count, true
		}

		if time.Now().After(deadline) {
			return 0, false
		}

		time.Sleep(time.Millisecond)
	}
}

func (s *EventServiceWS) countSubscribersLocked() int {
	seen := make(map[chan model.Event]struct{})
	for _, source := range s.subscriberChannels {
		for _, subscribers := range source {
			for _, subscriber := range subscribers {
				seen[subscriber] = struct{}{}
			}
		}
	}

	return len(seen)
}

// closeSubscribersLocked closes every attached subscriber channel and
// drops the map. Callers must hold s.mutex.
//
// B11: this exists in ONE place on purpose. Both the dispatcher's
// teardown and its next start have to honour the same invariant — a
// subscriber channel is never discarded without being closed first —
// and two hand-written copies of that loop is exactly how the invariant
// would drift apart again.
func (s *EventServiceWS) closeSubscribersLocked() {
	if s.subscriberChannels == nil {
		return
	}

	// Track already-closed channels to avoid a double-close panic: one
	// channel can be subscribed to several event names.
	closed := make(map[chan model.Event]struct{})
	for _, source := range s.subscriberChannels {
		for _, subscribers := range source {
			for _, subscriber := range subscribers {
				if _, ok := closed[subscriber]; !ok {
					close(subscriber)
					closed[subscriber] = struct{}{}
				}
			}
		}
	}

	s.subscriberChannels = nil
}

// Publish hands an event to the dispatcher.
//
// B11: it now returns an error instead of logging and returning void.
// The HTTP publish endpoint answered 200 OK on every one of these
// paths, which is what let a dead dispatcher look perfectly healthy to
// every producer on the box.
func (s *EventServiceWS) Publish(event model.Event) (err error) {
	chPtr := s.inboundChannelPtr.Load()
	if chPtr == nil {
		logger.Error("error when publishing event via websocket", zap.Error(ErrInboundChannelNotFound))
		return ErrInboundChannelNotFound
	}
	inbound := *chPtr

	ctxPtr := s.ctxPtr.Load()
	if ctxPtr == nil {
		logger.Error("error when publishing event via websocket: context not initialized")
		return ErrBusNotStarted
	}
	ctx := *ctxPtr

	if event.Timestamp == 0 {
		event.Timestamp = time.Now().Unix()
	}

	// Wave 32: recover the panic that occurs when Start()'s defer
	// closes inboundChannel between the atomic load above and the
	// select send below. This is the only way to make Publish()
	// truly safe against a concurrent shutdown — neither a sync.Mutex
	// nor an additional close-flag check can close the gap, because
	// even after a flag check the dispatcher's defer can still run
	// before the send completes. Catching the panic here turns the
	// race into a clean drop instead of crashing the producer.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered panic publishing event — bus is shutting down",
				zap.Any("panic", r),
				zap.String("sourceID", event.SourceID),
				zap.String("name", event.Name))
			err = ErrBusStopping
		}
	}()

	// Wave 28: light back-pressure window. The previous non-blocking
	// send hid every overflow event from operators. Block briefly so
	// the dispatcher has a real chance to drain a burst, then drop
	// loudly with a rate-limited warning that includes the running
	// drop count so the size of the loss is visible in logs.
	timer := time.NewTimer(publishWaitTimeout)
	defer timer.Stop()
	select {
	case inbound <- event:
		return nil
	case <-ctx.Done():
		if ctxErr := ctx.Err(); ctxErr != nil {
			logger.Info(ctxErr.Error())
		}
		return ErrBusStopping
	case <-timer.C:
		dropped := s.publishDropped.Add(1)
		s.publishDropLogMu.Lock()
		now := time.Now()
		shouldLog := now.Sub(s.publishDropLogAt) >= time.Second
		if shouldLog {
			s.publishDropLogAt = now
		}
		s.publishDropLogMu.Unlock()
		if shouldLog {
			logger.Error("dropping inbound event — bus inbound channel saturated",
				zap.String("sourceID", event.SourceID),
				zap.String("name", event.Name),
				zap.Uint64("publish_dropped_total", dropped))
		}

		return ErrEventDropped
	}
}

// PublishDroppedTotal returns the running count of producer-side drops.
// Exposed so a future metrics endpoint can surface it without reaching
// into private state.
func (s *EventServiceWS) PublishDroppedTotal() uint64 {
	return s.publishDropped.Load()
}

// DispatchDroppedTotal returns the running count of consumer-side drops.
func (s *EventServiceWS) DispatchDroppedTotal() uint64 {
	return s.dispatchDropped.Load()
}

func (s *EventServiceWS) Subscribe(sourceID string, names []string) (chan model.Event, error) {
	if len(names) == 0 {
		eventTypes, err := s.typeService.GetEventTypesBySourceID(sourceID)
		if err != nil {
			return nil, err
		}

		for _, eventType := range eventTypes {
			names = append(names, eventType.Name)
		}
	}

	for _, name := range names {
		eventType, err := s.typeService.GetEventType(sourceID, name)
		if err != nil {
			return nil, err
		}

		if eventType == nil {
			return nil, ErrEventNameNotFound
		}
	}

	c := func() chan model.Event {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		if s.subscriberChannels == nil {
			s.subscriberChannels = make(map[string]map[string][]chan model.Event)
		}

		if s.subscriberChannels[sourceID] == nil {
			s.subscriberChannels[sourceID] = make(map[string][]chan model.Event)
		}

		c := make(chan model.Event, 32)

		for _, name := range names {
			if s.subscriberChannels[sourceID][name] == nil {
				s.subscriberChannels[sourceID][name] = make([]chan model.Event, 0)
			}
			s.subscriberChannels[sourceID][name] = append(s.subscriberChannels[sourceID][name], c)
		}
		return c
	}()

	return c, nil
}

func (s *EventServiceWS) Unsubscribe(sourceID string, name string, c chan model.Event) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.subscriberChannels == nil {
		return ErrSubscriberChannelsNotFound
	}

	if s.subscriberChannels[sourceID] == nil {
		return ErrEventSourceIDNotFound
	}

	if s.subscriberChannels[sourceID][name] == nil {
		return ErrEventNameNotFound
	}

	for i, subscriber := range s.subscriberChannels[sourceID][name] {
		if subscriber == c {
			logger.Info("unsubscribing from event type", zap.String("sourceID", sourceID), zap.String("name", name), zap.Int("subscriber", i))
			s.subscriberChannels[sourceID][name] = append(s.subscriberChannels[sourceID][name][:i], s.subscriberChannels[sourceID][name][i+1:]...)
			return nil
		}
	}

	return nil
}

func (s *EventServiceWS) Start(ctx *context.Context) {
	inbound := make(chan model.Event, 64)
	func() {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		s.ctxPtr.Store(ctx)
		s.inboundChannelPtr.Store(&inbound)

		// B11 restart window: the map is NOT always empty here. Between the
		// dying run's teardown (which sets it to nil) and this line, a
		// Subscribe() can land — it only needs the event-type table, not a
		// live dispatcher — and rebuild the map with a fresh subscriber.
		// Overwriting it would drop that channel on the floor WITHOUT
		// closing it: the websocket goroutine parked on `<-channel` never
		// wakes (its read pump only closes the conn, it cannot unblock the
		// writer), so the goroutine leaks and the client sits on an open
		// connection that no event will ever reach again. That is the same
		// silently-detached consumer this whole item exists to eliminate,
		// and the restart itself would be what caused it. Close first, so
		// any orphan sees EOF, unsubscribes and reconnects.
		s.closeSubscribersLocked()

		s.subscriberChannels = make(map[string]map[string][]chan model.Event)
		s.stop = make(chan struct{})
	}()

	// B11: stamp liveness before the first tick so a freshly started
	// dispatcher is never reported as stale.
	s.lastLoopAt.Store(time.Now().UnixNano())

	// B11: every run past the first is a restart by definition (the
	// supervisor is the only caller), and a restart wiped the subscriber
	// map above. Give in-process consumers a chance to re-attach before
	// the loop starts draining, otherwise the bus "recovers" into a
	// state where nothing internal is listening any more. Runs under its
	// own recover(): a broken hook must not take the dispatcher down
	// again and spin the supervisor.
	if s.runs.Add(1) > 1 {
		if hook := s.restartHook.Load(); hook != nil && *hook != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error("recovered panic in event dispatcher restart hook", zap.Any("panic", r))
					}
				}()
				(*hook)()
			}()
		}
	}

	defer func() {
		// Wave 32: clear the published channel pointer FIRST so any
		// concurrent Publish() that hasn't already loaded the pointer
		// observes nil and bails out cleanly. Producers that already
		// loaded the pointer are protected by the recover() in
		// Publish() against the close-then-send race.
		s.inboundChannelPtr.Store(nil)

		// Wave 34-X residual: every step below mutates s.subscriberChannels
		// and s.stop, but the original teardown took no lock. Subscribe(),
		// Unsubscribe() and the dispatch hot-path all hold s.mutex while
		// touching the same maps, so a shutdown that races a late
		// Subscribe() would walk a half-built map and panic on
		// concurrent-map-write — exactly what we saw in the daemon
		// shutdown logs after Wave 32 hardening fixed Publish() but left
		// the teardown unprotected. Take the lock for the whole tear-down
		// the same way Start()'s setup func does.
		s.mutex.Lock()
		defer s.mutex.Unlock()

		s.closeSubscribersLocked()

		close(inbound)

		close(s.stop)
		s.stop = nil
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Local copy of the context for the lifetime of Start(); ctx never
	// changes during Start, and the dispatch hot-paths don't need to
	// pay the atomic.Load cost on every iteration.
	loopCtx := *ctx

	for {
		// B11: liveness stamp. A dispatcher goroutine that exists but no
		// longer iterates is indistinguishable from a healthy idle one
		// unless the loop itself leaves a mark; the 10s ticker below
		// guarantees this is refreshed even with zero traffic.
		s.lastLoopAt.Store(time.Now().UnixNano())

		select {

		case <-loopCtx.Done():
			return

		case event, ok := <-inbound:
			if !ok {
				return
			}

			// Snapshot subscriber list under lock so dispatch doesn't race
			// with Subscribe/Unsubscribe.
			var targets []chan model.Event
			s.mutex.Lock()
			if s.subscriberChannels != nil &&
				s.subscriberChannels[event.SourceID] != nil &&
				s.subscriberChannels[event.SourceID][event.Name] != nil {
				src := s.subscriberChannels[event.SourceID][event.Name]
				targets = make([]chan model.Event, len(src))
				copy(targets, src)
			}
			s.mutex.Unlock()

			for _, c := range targets {
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Error("recovered panic sending event to subscriber", zap.Any("panic", r), zap.String("event", event.Name))
						}
					}()
					select {
					case c <- event:
					case <-loopCtx.Done():
					default: // drop event if subscriber buffer is full
						s.dispatchDropped.Add(1)
						logger.Info("dropping event for slow consumer", zap.String("sourceID", event.SourceID), zap.String("name", event.Name))
					}
				}()
			}

		case <-ticker.C:
			// Snapshot all subscribers for heartbeat under lock.
			var allSubs []chan model.Event
			s.mutex.Lock()
			if s.subscriberChannels != nil {
				for _, source := range s.subscriberChannels {
					for _, subscribers := range source {
						allSubs = append(allSubs, subscribers...)
					}
				}
			}
			s.mutex.Unlock()

			if len(allSubs) == 0 {
				continue
			}

			heartbeat := model.Event{
				SourceID:  common.MessageBusSourceID,
				Name:      common.MessageBusHeartbeatName,
				Timestamp: time.Now().Unix(),
			}

			for _, subscriber := range allSubs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Error("recovered panic sending heartbeat to subscriber", zap.Any("panic", r))
						}
					}()
					select {
					case subscriber <- heartbeat:
					case <-loopCtx.Done():
					default:
						s.dispatchDropped.Add(1)
						logger.Info("dropping heartbeat for slow consumer")
					}
				}()
			}
		}
	}
}

func NewEventServiceWS(eventTypeService *EventTypeService) *EventServiceWS {
	return &EventServiceWS{
		typeService: eventTypeService,
	}
}
