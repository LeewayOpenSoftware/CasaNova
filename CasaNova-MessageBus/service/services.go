package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
)

type Services struct {
	EventTypeService *EventTypeService
	EventServiceWS   *EventServiceWS

	ActionTypeService *ActionTypeService
	ActionServiceWS   *ActionServiceWS

	SocketIOService *SocketIOService

	YSKService *YSKService

	// B11: supervision state for the three long-lived goroutines. Kept
	// as pointers so Services stays copyable by value the way
	// NewServices already returns it.
	eventState  *goroutineState
	actionState *goroutineState
	socketState *goroutineState

	// statusNotifier, when set, receives a one-line degradation summary
	// on every abnormal dispatcher exit. main() wires it to systemd's
	// STATUS= so `systemctl status casanova-message-bus` shows the
	// damage without anyone having to grep journald. Must be set before
	// Start(); the goroutines Start() launches read it, and goroutine
	// creation gives the required happens-before.
	statusNotifier func(string)

	wg sync.WaitGroup
}

var (
	ErrInboundChannelNotFound     = errors.New("inbound channel not found")
	ErrSubscriberChannelsNotFound = errors.New("subscriber channels not found")
	ErrAlreadySubscribed          = errors.New("already subscribed")

	// B11: Publish()/Trigger() used to swallow every one of these and
	// let the HTTP layer answer 200 OK. They are returned to the caller
	// now so a producer can tell "delivered to the dispatcher" from
	// "went nowhere".
	ErrBusNotStarted = errors.New("message bus dispatcher has not started")
	ErrBusStopping   = errors.New("message bus dispatcher is stopping")
	ErrEventDropped  = errors.New("event dropped: bus inbound channel saturated")
	ErrActionDropped = errors.New("action dropped: bus inbound channel saturated")
)

// dispatcherLoopStaleAfter is how long a running dispatcher may go
// without touching its loop timestamp before liveness calls it stale.
// Both dispatchers wake on a 10s ticker regardless of traffic, so three
// missed ticks means the select loop is genuinely wedged (not idle).
const dispatcherLoopStaleAfter = 35 * time.Second

// healthLockWait bounds how long Health() will wait for a dispatcher
// mutex before reporting the subscriber count as unknown. Long enough
// to ride out a normal snapshot under the lock, short enough that the
// endpoint always answers.
const healthLockWait = 50 * time.Millisecond

// SetStatusNotifier installs the degradation sink. Call before Start().
func (s *Services) SetStatusNotifier(notify func(string)) {
	s.statusNotifier = notify
}

func (s *Services) onGoroutineFailure(name string, reason string, restarts uint64) {
	if s.statusNotifier == nil {
		return
	}

	s.statusNotifier(fmt.Sprintf("DEGRADED: %s died and was restarted (%d restarts so far) — %s",
		name, restarts, reason))
}

func (s *Services) Start(ctx *context.Context) {
	// B11: when the event dispatcher is restarted its whole subscriber
	// map is rebuilt from scratch, so every in-process consumer has to
	// re-subscribe or it stays silently detached. YSKService subscribes
	// exactly once at boot, so without this hook a dispatcher restart
	// would "recover" into a bus that no longer feeds YSK cards.
	s.EventServiceWS.SetRestartHook(func() {
		logger.Info("event dispatcher restarted — re-subscribing internal consumers")
		s.YSKService.Start(false)
	})

	s.wg.Add(3)

	go func() {
		defer s.wg.Done()
		s.eventState.supervise(*ctx, func() { s.EventServiceWS.Start(ctx) }, s.onGoroutineFailure)
	}()

	go func() {
		defer s.wg.Done()
		s.actionState.supervise(*ctx, func() { s.ActionServiceWS.Start(ctx) }, s.onGoroutineFailure)
	}()

	go func() {
		defer s.wg.Done()
		s.socketState.supervise(*ctx, func() { s.SocketIOService.Start(ctx) }, s.onGoroutineFailure)
	}()
}

// Wait blocks until all service goroutines launched by Start have returned.
func (s *Services) Wait() {
	s.wg.Wait()
}

// DispatcherHealth is the observable state of one dispatcher loop.
type DispatcherHealth struct {
	// Accepting reports whether the inbound channel is installed, i.e.
	// whether a publish can be handed to the dispatcher at all.
	Accepting bool `json:"accepting"`
	// LoopLastSeenAt is the unix time of the last dispatcher loop
	// iteration. It is a real liveness signal: the loop stamps it on
	// every wake-up, including the idle 10s heartbeat tick.
	LoopLastSeenAt int64 `json:"loop_last_seen_at"`
	LoopStale      bool  `json:"loop_stale"`
	// Subscribers is -1 when the count could not be read without
	// blocking on the dispatcher mutex. Reporting "unknown" is the
	// honest answer; waiting for a lock the dispatcher may be stuck
	// holding would hang the health check itself.
	Subscribers int `json:"subscribers"`
	// InboundDroppedTotal counts producer-side drops (bus saturated),
	// SubscriberDroppedTotal counts consumer-side drops (slow client).
	InboundDroppedTotal    uint64 `json:"inbound_dropped_total"`
	SubscriberDroppedTotal uint64 `json:"subscriber_dropped_total"`
}

// BusHealth is the payload of the liveness endpoint.
type BusHealth struct {
	Healthy       bool              `json:"healthy"`
	CheckedAt     int64             `json:"checked_at"`
	RestartsTotal uint64            `json:"restarts_total"`
	Problems      []string          `json:"problems"`
	Goroutines    []GoroutineHealth `json:"goroutines"`
	Events        DispatcherHealth  `json:"events"`
	Actions       DispatcherHealth  `json:"actions"`
}

// Health snapshots everything an operator needs to answer "is the bus
// actually delivering?" without reading logs. It takes no lock that a
// wedged dispatcher could be holding, so it still answers when the
// subsystem it describes is stuck.
func (s *Services) Health() BusHealth {
	now := time.Now()

	health := BusHealth{
		CheckedAt: now.Unix(),
		Problems:  []string{},
		Goroutines: []GoroutineHealth{
			s.eventState.snapshot(),
			s.actionState.snapshot(),
			s.socketState.snapshot(),
		},
		Events: DispatcherHealth{
			Accepting:              s.EventServiceWS.Accepting(),
			LoopLastSeenAt:         unixOrZero(s.EventServiceWS.LastLoopAt()),
			Subscribers:            countOrUnknown(s.EventServiceWS.TrySubscriberCount(healthLockWait)),
			InboundDroppedTotal:    s.EventServiceWS.PublishDroppedTotal(),
			SubscriberDroppedTotal: s.EventServiceWS.DispatchDroppedTotal(),
		},
		Actions: DispatcherHealth{
			Accepting:              s.ActionServiceWS.Accepting(),
			LoopLastSeenAt:         unixOrZero(s.ActionServiceWS.LastLoopAt()),
			Subscribers:            countOrUnknown(s.ActionServiceWS.TrySubscriberCount(healthLockWait)),
			InboundDroppedTotal:    s.ActionServiceWS.TriggerDroppedTotal(),
			SubscriberDroppedTotal: s.ActionServiceWS.DispatchDroppedTotal(),
		},
	}

	for _, g := range health.Goroutines {
		health.RestartsTotal += g.Restarts
		if !g.Running {
			health.Problems = append(health.Problems, g.Name+" is not running")
		}
	}

	health.Events.LoopStale = loopIsStale(s.eventState, s.EventServiceWS.LastLoopAt(), now)
	health.Actions.LoopStale = loopIsStale(s.actionState, s.ActionServiceWS.LastLoopAt(), now)

	if health.Events.LoopStale {
		health.Problems = append(health.Problems, "event dispatcher loop is stale (no iteration within the heartbeat window)")
	}
	if health.Actions.LoopStale {
		health.Problems = append(health.Problems, "action dispatcher loop is stale (no iteration within the heartbeat window)")
	}
	if s.eventState.running.Load() && !health.Events.Accepting {
		health.Problems = append(health.Problems, "event dispatcher is not accepting publishes")
	}
	if s.actionState.running.Load() && !health.Actions.Accepting {
		health.Problems = append(health.Problems, "action dispatcher is not accepting triggers")
	}

	health.Healthy = len(health.Problems) == 0

	return health
}

// loopIsStale reports whether a running dispatcher has stopped iterating.
// A goroutine that is not running is already reported as a problem on its
// own, and a loop timestamp is meaningless before the first run, so both
// cases deliberately return false instead of a second, redundant alarm.
func loopIsStale(state *goroutineState, lastLoop time.Time, now time.Time) bool {
	if !state.running.Load() || lastLoop.IsZero() {
		return false
	}

	return now.Sub(lastLoop) > dispatcherLoopStaleAfter
}

// countOrUnknown maps a failed non-blocking read to the -1 sentinel.
func countOrUnknown(count int, ok bool) int {
	if !ok {
		return -1
	}

	return count
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.Unix()
}

func NewServices(repository *repository.Repository) Services {
	eventTypeService := NewEventTypeService(repository)
	actionTypeService := NewActionTypeService(repository)

	eventServiceWS := NewEventServiceWS(eventTypeService)

	return Services{
		EventTypeService: eventTypeService,
		SocketIOService:  NewSocketIOService(),
		EventServiceWS:   eventServiceWS,

		ActionTypeService: actionTypeService,
		ActionServiceWS:   NewActionServiceWS(actionTypeService),
		YSKService:        NewYSKService(repository, eventServiceWS, eventTypeService),

		eventState:  newGoroutineState("event-dispatcher"),
		actionState: newGoroutineState("action-dispatcher"),
		socketState: newGoroutineState("socketio-server"),
	}
}
