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

// triggerWaitTimeout mirrors publishWaitTimeout in event_service_websocket.go.
// Wave 30: previously Trigger() did a non-blocking send that silently
// dropped on a full inbound channel — actions just vanished, no logs,
// no metrics. Light back-pressure plus rate-limited error logging gives
// the dispatcher a chance to drain a burst and makes genuine overload
// visible to operators.
const triggerWaitTimeout = 100 * time.Millisecond

type ActionServiceWS struct {
	typeService *ActionTypeService

	// Wave 30: ctx is now an atomic.Pointer so Trigger() can read it
	// without taking the mutex (hot path) while Start() writes it
	// safely. The previous bare *context.Context field was a textbook
	// data race — Trigger checked it for nil and then dereferenced it
	// with no synchronization, so the read could observe a torn or
	// stale value depending on goroutine scheduling.
	ctxPtr atomic.Pointer[context.Context]

	mutex sync.Mutex
	stop  chan struct{}

	// Wave 32: same close-then-send race fix as EventServiceWS.
	// inboundChannel is now read via atomic.Pointer + the send is
	// guarded by recover() so a concurrent Start() teardown turns the
	// race into a clean drop instead of a panic that crashes the
	// producer goroutine.
	inboundChannelPtr  atomic.Pointer[chan model.Action]
	subscriberChannels map[string]map[string][]chan model.Action

	// Wave 30: drop counters + rate-limited log timestamp, mirroring
	// the EventServiceWS pattern from Wave 28.
	triggerDropped   atomic.Uint64
	triggerDropLogMu sync.Mutex
	triggerDropLogAt time.Time

	// B11: consumer-side drop counter (the dispatch `default:` branches
	// discarded actions without counting them anywhere) plus the same
	// loop-liveness stamp the event dispatcher carries.
	dispatchDropped atomic.Uint64
	lastLoopAt      atomic.Int64
}

// Accepting reports whether the dispatcher has an inbound channel
// installed — i.e. whether a Trigger can be handed over at all.
func (s *ActionServiceWS) Accepting() bool {
	return s.inboundChannelPtr.Load() != nil
}

// LastLoopAt returns the time of the last dispatcher loop iteration, or
// the zero time if the dispatcher never ran.
func (s *ActionServiceWS) LastLoopAt() time.Time {
	ns := s.lastLoopAt.Load()
	if ns == 0 {
		return time.Time{}
	}

	return time.Unix(0, ns)
}

// SubscriberCount returns the number of distinct subscriber channels
// currently attached to the dispatcher. It blocks on the dispatcher
// mutex; health checks must use TrySubscriberCount instead.
func (s *ActionServiceWS) SubscriberCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.countSubscribersLocked()
}

// TrySubscriberCount is the non-blocking health-check variant — see
// EventServiceWS.TrySubscriberCount for why the liveness path must
// never wait on the dispatcher's own mutex.
func (s *ActionServiceWS) TrySubscriberCount(wait time.Duration) (int, bool) {
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

func (s *ActionServiceWS) countSubscribersLocked() int {
	seen := make(map[chan model.Action]struct{})
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
// drops the map. Callers must hold s.mutex. Mirrors the event
// dispatcher: a subscriber channel is never discarded without being
// closed first, or the websocket goroutine waiting on it never wakes.
func (s *ActionServiceWS) closeSubscribersLocked() {
	if s.subscriberChannels == nil {
		return
	}

	// One channel can be subscribed to several action names — dedupe or
	// the second close panics.
	closed := make(map[chan model.Action]struct{})
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

// DispatchDroppedTotal exposes consumer-side drops for metrics surfaces.
func (s *ActionServiceWS) DispatchDroppedTotal() uint64 {
	return s.dispatchDropped.Load()
}

// Trigger hands an action to the dispatcher.
//
// B11: returns an error instead of logging and returning void, so the
// HTTP layer can stop answering 200 OK for actions that went nowhere.
func (s *ActionServiceWS) Trigger(action model.Action) (err error) {
	chPtr := s.inboundChannelPtr.Load()
	if chPtr == nil {
		logger.Error("error when triggering action via websocket", zap.Error(ErrInboundChannelNotFound))
		return ErrInboundChannelNotFound
	}
	inbound := *chPtr

	ctxPtr := s.ctxPtr.Load()
	if ctxPtr == nil {
		logger.Error("error when triggering action via websocket: context not initialized")
		return ErrBusNotStarted
	}
	ctx := *ctxPtr

	if action.Timestamp == 0 {
		action.Timestamp = time.Now().Unix()
	}

	// TODO - ensure properties are valid for action type

	// Wave 32: recover the panic that occurs when Start()'s defer
	// closes inbound between the atomic load above and the select
	// send below. Mirrors EventServiceWS.Publish — neither a mutex
	// nor a close-flag check can fully close the gap; recover() at
	// the send site turns the race into a clean drop instead of
	// crashing the producer.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered panic triggering action — bus is shutting down",
				zap.Any("panic", r),
				zap.String("sourceID", action.SourceID),
				zap.String("name", action.Name))
			err = ErrBusStopping
		}
	}()

	timer := time.NewTimer(triggerWaitTimeout)
	defer timer.Stop()
	select {
	case inbound <- action:
		return nil
	case <-ctx.Done():
		if ctxErr := ctx.Err(); ctxErr != nil {
			logger.Info(ctxErr.Error())
		}
		return ErrBusStopping
	case <-timer.C:
		dropped := s.triggerDropped.Add(1)
		s.triggerDropLogMu.Lock()
		now := time.Now()
		shouldLog := now.Sub(s.triggerDropLogAt) >= time.Second
		if shouldLog {
			s.triggerDropLogAt = now
		}
		s.triggerDropLogMu.Unlock()
		if shouldLog {
			logger.Error("dropping inbound action — bus inbound channel saturated",
				zap.String("sourceID", action.SourceID),
				zap.String("name", action.Name),
				zap.Uint64("trigger_dropped_total", dropped))
		}

		return ErrActionDropped
	}
}

// TriggerDroppedTotal exposes the running drop count for metrics surfaces.
func (s *ActionServiceWS) TriggerDroppedTotal() uint64 {
	return s.triggerDropped.Load()
}

func (s *ActionServiceWS) Subscribe(sourceID string, names []string) (chan model.Action, error) {
	if len(names) == 0 {
		actionTypes, err := s.typeService.GetActionTypesBySourceID(sourceID)
		if err != nil {
			return nil, err
		}

		for _, actionType := range actionTypes {
			names = append(names, actionType.Name)
		}
	}

	for _, name := range names {
		actionType, err := s.typeService.GetActionType(sourceID, name)
		if err != nil {
			return nil, err
		}

		if actionType == nil {
			return nil, ErrActionNameNotFound
		}
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.subscriberChannels == nil {
		s.subscriberChannels = make(map[string]map[string][]chan model.Action)
	}

	if s.subscriberChannels[sourceID] == nil {
		s.subscriberChannels[sourceID] = make(map[string][]chan model.Action)
	}

	c := make(chan model.Action, 32)

	for _, name := range names {
		if s.subscriberChannels[sourceID][name] == nil {
			s.subscriberChannels[sourceID][name] = make([]chan model.Action, 0)
		}
		s.subscriberChannels[sourceID][name] = append(s.subscriberChannels[sourceID][name], c)
	}

	return c, nil
}

func (s *ActionServiceWS) Unsubscribe(sourceID string, name string, c chan model.Action) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.subscriberChannels == nil {
		return ErrSubscriberChannelsNotFound
	}

	if s.subscriberChannels[sourceID] == nil {
		return ErrActionSourceIDNotFound
	}

	if s.subscriberChannels[sourceID][name] == nil {
		return ErrActionNameNotFound
	}

	// Wave 30: removed an unreachable bounds check from this loop. The
	// previous code did `if i >= len(slice)` *inside* `range slice`,
	// which can never be true because the slice header is captured at
	// the start of range and the loop holds the mutex throughout.
	for i, subscriber := range s.subscriberChannels[sourceID][name] {
		if subscriber == c {
			s.subscriberChannels[sourceID][name] = append(s.subscriberChannels[sourceID][name][:i], s.subscriberChannels[sourceID][name][i+1:]...)
			return nil
		}
	}

	return nil
}

func (s *ActionServiceWS) Start(ctx *context.Context) {
	inbound := make(chan model.Action, 64)
	// Initialize under lock to prevent race with concurrent Subscribe/Trigger.
	s.mutex.Lock()
	s.ctxPtr.Store(ctx)
	s.inboundChannelPtr.Store(&inbound)

	// B11 restart window: a Subscribe() that landed after the previous
	// run's teardown rebuilt this map, and overwriting it would discard
	// that subscriber's channel without closing it — its websocket
	// goroutine would block on `<-channel` forever while the client kept a
	// connection that no action would ever reach again. Same invariant and
	// same reasoning as EventServiceWS.Start.
	s.closeSubscribersLocked()

	s.subscriberChannels = make(map[string]map[string][]chan model.Action)
	s.stop = make(chan struct{})
	s.mutex.Unlock()

	defer func() {
		// Wave 32: clear the published channel pointer FIRST so any
		// concurrent Trigger() that hasn't already loaded the pointer
		// observes nil and bails out cleanly. Producers that already
		// loaded the pointer are protected by the recover() in
		// Trigger() against the close-then-send race.
		s.inboundChannelPtr.Store(nil)

		// Close all subscriber channels so blocked WS goroutines exit cleanly
		// instead of leaking indefinitely. Mirrors EventServiceWS pattern.
		s.mutex.Lock()
		s.closeSubscribersLocked()
		s.mutex.Unlock()

		close(inbound)

		close(s.stop)
		s.stop = nil
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// The dispatcher loop owns its own copy of the context dereference
	// for the lifetime of Start(). ctx never changes during Start.
	loopCtx := *ctx

	for {
		// B11: liveness stamp — see EventServiceWS.Start. The 10s ticker
		// refreshes this even with zero traffic, so a stale timestamp
		// means the loop is wedged, not idle.
		s.lastLoopAt.Store(time.Now().UnixNano())

		select {

		case <-loopCtx.Done():
			return

		case action, ok := <-inbound:
			if !ok {
				return
			}

			s.mutex.Lock()
			var subscribers []chan model.Action
			if s.subscriberChannels != nil && s.subscriberChannels[action.SourceID] != nil && s.subscriberChannels[action.SourceID][action.Name] != nil {
				subscribers = make([]chan model.Action, len(s.subscriberChannels[action.SourceID][action.Name]))
				copy(subscribers, s.subscriberChannels[action.SourceID][action.Name])
			}
			s.mutex.Unlock()

			for _, c := range subscribers {
				// Per-subscriber recover — a concurrent Unsubscribe +
				// goroutine exit could close this channel before we
				// send, and sending on a closed channel panics.
				// Mirrors the guard in EventServiceWS.Start.
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Error("recovered panic sending action to subscriber",
								zap.Any("panic", r), zap.String("action", action.Name))
						}
					}()
					select {
					case c <- action:
					case <-loopCtx.Done():
					default: // drop action if the subscriber buffer is full
						s.dispatchDropped.Add(1)
						logger.Info("dropping action for slow consumer",
							zap.String("sourceID", action.SourceID),
							zap.String("name", action.Name))
					}
				}()
			}

		case <-ticker.C:
			s.mutex.Lock()
			var allSubscribers []chan model.Action
			if s.subscriberChannels != nil {
				for _, source := range s.subscriberChannels {
					for _, subs := range source {
						allSubscribers = append(allSubscribers, subs...)
					}
				}
			}
			s.mutex.Unlock()

			if len(allSubscribers) == 0 {
				continue
			}

			heartbeat := model.Action{
				SourceID:  common.MessageBusSourceID,
				Name:      common.MessageBusHeartbeatName,
				Timestamp: time.Now().Unix(),
			}

			for _, subscriber := range allSubscribers {
				select {
				case subscriber <- heartbeat:
				case <-loopCtx.Done():
					return
				default: // drop heartbeat if the subscriber buffer is full
					s.dispatchDropped.Add(1)
					logger.Info("dropping action heartbeat for slow consumer")
				}
			}
		}
	}
}

func NewActionServiceWS(actionTypeService *ActionTypeService) *ActionServiceWS {
	return &ActionServiceWS{
		typeService: actionTypeService,
	}
}
