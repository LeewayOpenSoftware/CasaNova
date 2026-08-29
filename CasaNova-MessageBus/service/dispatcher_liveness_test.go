package service

import (
	"context"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"gotest.tools/assert"
)

const (
	testSourceID  = "b11-source"
	testEventName = "b11-source:event:test"
)

func newRunningServices(t *testing.T) (*Services, context.CancelFunc) {
	t.Helper()

	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)

	services := NewServices(&repo)

	ctx, cancel := context.WithCancel(context.Background())
	services.Start(&ctx)

	stop := func() {
		cancel()
		services.Wait()
		repo.Close()
	}

	waitFor(t, 5*time.Second, "dispatchers to start", func() bool {
		return services.Health().Events.Accepting
	})

	_, err = services.EventTypeService.RegisterEventType(model.EventType{
		SourceID: testSourceID,
		Name:     testEventName,
	})
	assert.NilError(t, err)

	return &services, stop
}

// killEventDispatcher reproduces the B11 failure mode from the outside:
// closing the inbound channel makes the dispatch loop return, and its
// own teardown then double-closes that channel and panics. That is the
// same end state as any other panic in the loop — goroutine gone,
// inboundChannelPtr nil, Publish() a no-op — which is exactly what used
// to be permanent.
func killEventDispatcher(t *testing.T, s *EventServiceWS) {
	t.Helper()

	chPtr := s.inboundChannelPtr.Load()
	assert.Assert(t, chPtr != nil, "dispatcher is not running, nothing to kill")

	close(*chPtr)
}

// The regression test for B11: after the dispatcher dies, the bus must
// come back on its own and deliver events again.
//
// Mutation that must fail this test: restore the pre-B11 Services.Start
// (bare goroutine + recover that only logs). The dispatcher then stays
// dead, Accepting() stays false forever and no event is ever delivered.
func TestEventDispatcherIsRestartedAfterItDies(t *testing.T) {
	services, stop := newRunningServices(t)
	defer stop()

	killEventDispatcher(t, services.EventServiceWS)

	waitFor(t, 10*time.Second, "event dispatcher to be restarted", func() bool {
		health := services.Health()
		return health.Goroutines[0].Restarts >= 1 && health.Goroutines[0].Running && health.Events.Accepting
	})

	// The restart is only worth anything if delivery actually resumes.
	subscriber, err := services.EventServiceWS.Subscribe(testSourceID, []string{testEventName})
	assert.NilError(t, err)

	assert.NilError(t, services.EventServiceWS.Publish(model.Event{
		SourceID: testSourceID,
		Name:     testEventName,
	}))

	select {
	case event := <-subscriber:
		assert.Equal(t, event.Name, testEventName)
	case <-time.After(5 * time.Second):
		t.Fatal("event was not delivered after the dispatcher restarted")
	}

	// The recovery must leave a trace an operator can find later: the
	// process stays `active` through all of this, so the counters and
	// the recorded failure are the only evidence that events were lost.
	health := services.Health()
	assert.Assert(t, health.RestartsTotal >= 1)
	assert.Assert(t, health.Goroutines[0].LastFailure != "")
	assert.Assert(t, health.Goroutines[0].LastFailureAt > 0)
}

// A dispatcher restart rebuilds the subscriber map from scratch, so
// in-process consumers have to be re-attached or the bus "recovers"
// into a state where nothing internal is listening.
//
// Mutation that must fail this test: drop the SetRestartHook call in
// Services.Start. The subscriber count then stays at 0 after the
// restart and YSK cards stop reaching the UI forever.
func TestInternalConsumersResubscribeAfterDispatcherRestart(t *testing.T) {
	services, stop := newRunningServices(t)
	defer stop()

	services.YSKService.Start(true)

	waitFor(t, 5*time.Second, "YSK service to subscribe", func() bool {
		return services.EventServiceWS.SubscriberCount() >= 1
	})

	killEventDispatcher(t, services.EventServiceWS)

	waitFor(t, 10*time.Second, "internal consumers to re-subscribe after restart", func() bool {
		return services.Health().Goroutines[0].Restarts >= 1 &&
			services.EventServiceWS.SubscriberCount() >= 1
	})
}

// Liveness must report a dispatcher that never started, instead of the
// green "process is alive" answer systemd gives.
func TestHealthReportsDispatcherThatIsNotRunning(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	health := services.Health()
	assert.Equal(t, health.Healthy, false)
	assert.Equal(t, health.Events.Accepting, false)
	assert.Assert(t, len(health.Problems) >= 3, "expected every stopped goroutine to be reported, got %v", health.Problems)
}

// The liveness stamp is only worth anything if the dispatcher loop
// actually maintains it. This is the wiring half of the wedged-loop
// check: the timestamp must exist from the first moment the dispatcher
// is up, and it must ADVANCE as the loop iterates.
//
// Mutation that must fail this test: remove the lastLoopAt.Store call
// from the dispatcher loop (whether or not the one in Start's setup
// stays). The timestamp then never moves, so a wedged loop would keep
// reporting whatever it reported at boot.
func TestDispatcherLoopMaintainsItsLivenessStamp(t *testing.T) {
	services, stop := newRunningServices(t)
	defer stop()

	first := services.EventServiceWS.LastLoopAt()
	assert.Assert(t, !first.IsZero(), "a running dispatcher must publish a liveness timestamp")

	assert.NilError(t, services.EventServiceWS.Publish(model.Event{
		SourceID: testSourceID,
		Name:     testEventName,
	}))

	waitFor(t, 5*time.Second, "the dispatcher loop to refresh its liveness stamp", func() bool {
		return services.EventServiceWS.LastLoopAt().After(first)
	})

	assert.Equal(t, services.Health().Events.LoopStale, false)
}

// A goroutine that exists but no longer iterates is the nastiest
// variant of B11: `running` is true and nothing ever logs. This covers
// the rule that turns a frozen timestamp into a reported problem.
func TestHealthReportsWedgedDispatcherLoop(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	// Simulate a goroutine that is alive and accepting but whose select
	// loop has not iterated in far longer than the heartbeat window.
	services.eventState.running.Store(true)
	inbound := make(chan model.Event, 1)
	services.EventServiceWS.inboundChannelPtr.Store(&inbound)
	services.EventServiceWS.lastLoopAt.Store(time.Now().Add(-5 * dispatcherLoopStaleAfter).UnixNano())

	health := services.Health()
	assert.Equal(t, health.Events.LoopStale, true)
	assert.Equal(t, health.Healthy, false)

	// And a dispatcher that iterated just now is NOT flagged — a
	// liveness signal that cries wolf on an idle bus is worse than none.
	services.EventServiceWS.lastLoopAt.Store(time.Now().UnixNano())
	assert.Equal(t, services.Health().Events.LoopStale, false)
}

// The liveness check must answer even when the dispatcher is stuck
// holding its own mutex — that is precisely the case an operator is
// trying to diagnose.
//
// Mutation that must fail this test: have Health() call the blocking
// SubscriberCount() instead of TrySubscriberCount(). The health
// endpoint then hangs forever on the wedged dispatcher's lock.
func TestHealthDoesNotBlockOnAWedgedDispatcherLock(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	services.EventServiceWS.mutex.Lock()
	defer services.EventServiceWS.mutex.Unlock()

	answered := make(chan BusHealth, 1)
	go func() {
		answered <- services.Health()
	}()

	select {
	case health := <-answered:
		assert.Equal(t, health.Events.Subscribers, -1, "subscriber count must be reported as unknown, not waited for")
	case <-time.After(5 * time.Second):
		t.Fatal("Health() blocked on the dispatcher mutex")
	}
}

// Publish must tell the caller when the event went nowhere. Before B11
// it returned void and the HTTP layer answered 200 OK regardless.
func TestPublishReportsThatTheDispatcherIsGone(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	err = services.EventServiceWS.Publish(model.Event{SourceID: testSourceID, Name: testEventName})
	assert.Equal(t, err, ErrInboundChannelNotFound)

	assert.Equal(t, services.ActionServiceWS.Trigger(model.Action{SourceID: testSourceID, Name: testEventName}), ErrInboundChannelNotFound)
}

// Saturation is a delivery failure too: the event is dropped, so the
// producer has to hear about it.
func TestPublishReportsDropWhenInboundIsSaturated(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	// A full inbound channel with no dispatcher draining it is exactly
	// what a saturated bus looks like to a producer.
	ctx := context.Context(context.Background())
	full := make(chan model.Event) // unbuffered, nobody receiving
	services.EventServiceWS.ctxPtr.Store(&ctx)
	services.EventServiceWS.inboundChannelPtr.Store(&full)

	err = services.EventServiceWS.Publish(model.Event{SourceID: testSourceID, Name: testEventName})
	assert.Equal(t, err, ErrEventDropped)
	assert.Equal(t, services.EventServiceWS.PublishDroppedTotal(), uint64(1))
}

// A restart must not silently detach a consumer that subscribed while the
// dispatcher was down. Subscribe() only needs the event-type table, so it
// succeeds in the window between the dying run's teardown and the
// supervisor's next Start() — and Start() then installs a fresh subscriber
// map. If the old map is overwritten instead of closed, that channel is
// orphaned: nothing will ever send on it and nothing will ever close it,
// so the websocket goroutine parked on `<-channel` leaks and the client
// holds an open connection that no event reaches again. Recovering the bus
// by permanently detaching a live consumer is the restart causing the very
// failure it is supposed to end.
//
// Mutation that must fail this test: drop the closeSubscribersLocked() call
// from EventServiceWS.Start / ActionServiceWS.Start setup. The receive
// below then blocks until the test's own deadline.
func TestDispatcherStartClosesSubscribersOrphanedByTheRestartWindow(t *testing.T) {
	repo, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	defer repo.Close()

	services := NewServices(&repo)

	_, err = services.EventTypeService.RegisterEventType(model.EventType{
		SourceID: testSourceID,
		Name:     testEventName,
	})
	assert.NilError(t, err)

	_, err = services.ActionTypeService.RegisterActionType(model.ActionType{
		SourceID: testSourceID,
		Name:     testEventName,
	})
	assert.NilError(t, err)

	// Subscribing with no dispatcher running is exactly the state the
	// restart window leaves behind: a rebuilt map holding a live channel.
	eventChannel, err := services.EventServiceWS.Subscribe(testSourceID, []string{testEventName})
	assert.NilError(t, err)

	actionChannel, err := services.ActionServiceWS.Subscribe(testSourceID, []string{testEventName})
	assert.NilError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	services.Start(&ctx)
	defer func() {
		cancel()
		services.Wait()
	}()

	// Every wait here is bounded: an orphaned channel is one that never
	// closes, so an uncapped receive would hang the whole suite.
	select {
	case _, ok := <-eventChannel:
		assert.Equal(t, ok, false, "orphaned event subscriber must be closed, not fed")
	case <-time.After(5 * time.Second):
		t.Fatal("event subscriber was orphaned by the dispatcher start: channel never closed, its websocket goroutine would block forever")
	}

	select {
	case _, ok := <-actionChannel:
		assert.Equal(t, ok, false, "orphaned action subscriber must be closed, not fed")
	case <-time.After(5 * time.Second):
		t.Fatal("action subscriber was orphaned by the dispatcher start: channel never closed, its websocket goroutine would block forever")
	}
}
