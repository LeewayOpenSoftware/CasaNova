package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/assert"
)

// waitFor polls until cond is true or the deadline expires. Supervision
// is inherently asynchronous (backoff + goroutine scheduling), so tests
// poll instead of sleeping a fixed amount and hoping.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func newTestGoroutineState(name string) *goroutineState {
	g := newGoroutineState(name)
	// Keep the restart cadence fast enough for a unit test while still
	// exercising the real backoff code path.
	g.initialBackoff = time.Millisecond
	g.maxBackoff = 5 * time.Millisecond
	g.stableRun = time.Hour

	return g
}

// A panicking goroutine must come back. The pre-B11 code recovered the
// panic, logged "OFFLINE until restart" and let the goroutine die for
// the lifetime of the daemon.
func TestSuperviseRestartsAfterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int64
	done := make(chan struct{})

	state := newTestGoroutineState("panicker")

	go func() {
		defer close(done)
		state.supervise(ctx, func() {
			if runs.Add(1) <= 3 {
				panic("dispatcher exploded")
			}
			<-ctx.Done()
		}, nil)
	}()

	waitFor(t, 5*time.Second, "supervised goroutine to be restarted after panics", func() bool {
		return runs.Load() >= 4 && state.running.Load()
	})

	snapshot := state.snapshot()
	assert.Equal(t, snapshot.Restarts >= 3, true, "expected at least 3 restarts, got %d", snapshot.Restarts)
	assert.Equal(t, snapshot.LastFailure, "panic: dispatcher exploded")
	assert.Assert(t, snapshot.LastFailureAt > 0)

	cancel()
	<-done
	assert.Equal(t, state.running.Load(), false)
}

// A goroutine that simply RETURNS is just as fatal as one that panics:
// the dispatcher's defer clears the inbound channel either way, so
// Publish() silently becomes a no-op. It must be restarted too.
func TestSuperviseRestartsAfterUnexpectedReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int64
	done := make(chan struct{})

	state := newTestGoroutineState("quitter")

	go func() {
		defer close(done)
		state.supervise(ctx, func() {
			if runs.Add(1) <= 2 {
				return
			}
			<-ctx.Done()
		}, nil)
	}()

	waitFor(t, 5*time.Second, "supervised goroutine to be restarted after silent returns", func() bool {
		return runs.Load() >= 3
	})

	snapshot := state.snapshot()
	assert.Equal(t, snapshot.Restarts >= 2, true, "expected at least 2 restarts, got %d", snapshot.Restarts)
	assert.Equal(t, snapshot.LastFailure, "returned unexpectedly while the bus was still running")

	cancel()
	<-done
}

// Shutdown is not a failure: no restart, no failure record, no leaked
// supervisor goroutine.
func TestSuperviseStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var runs atomic.Int64
	done := make(chan struct{})

	state := newTestGoroutineState("clean")

	go func() {
		defer close(done)
		state.supervise(ctx, func() {
			runs.Add(1)
			<-ctx.Done()
		}, nil)
	}()

	waitFor(t, 5*time.Second, "supervised goroutine to start", func() bool {
		return state.running.Load()
	})

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after context cancellation")
	}

	assert.Equal(t, runs.Load(), int64(1))
	assert.Equal(t, state.snapshot().Restarts, uint64(0))
	assert.Equal(t, state.snapshot().LastFailure, "")
}

// Every abnormal exit must reach the operator-visible sink (systemd
// STATUS= in production), and a broken sink must not take down the
// supervisor that exists to survive breakage.
func TestSuperviseNotifiesFailuresAndSurvivesBrokenNotifier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var notified atomic.Int64
	var runs atomic.Int64
	done := make(chan struct{})

	state := newTestGoroutineState("notifier")

	go func() {
		defer close(done)
		state.supervise(ctx, func() {
			if runs.Add(1) <= 2 {
				panic("boom")
			}
			<-ctx.Done()
		}, func(name string, reason string, restarts uint64) {
			notified.Add(1)
			panic("notifier is broken too")
		})
	}()

	waitFor(t, 5*time.Second, "failures to be reported and the goroutine to recover", func() bool {
		return notified.Load() >= 2 && runs.Load() >= 3 && state.running.Load()
	})

	cancel()
	<-done
}
