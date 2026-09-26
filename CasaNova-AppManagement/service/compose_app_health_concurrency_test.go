package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainComposeHealthProbeSem empties the cap so a test starts from a known
// state, and returns a function that restores whatever it found. Tests in
// this package run in one process and share composeHealthProbeSem, so
// leaving it dirty would make the NEXT test lie rather than this one.
func drainComposeHealthProbeSem(t *testing.T) {
	t.Helper()
	for {
		select {
		case <-composeHealthProbeSem:
		default:
			return
		}
	}
}

// fillComposeHealthProbeSem takes every slot. It fails rather than blocks if
// the cap is already occupied, so a leak shows up as a failure here instead
// of a hang.
func fillComposeHealthProbeSem(t *testing.T) {
	t.Helper()
	for i := 0; i < composeHealthProbeConcurrency; i++ {
		select {
		case composeHealthProbeSem <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatalf("could not take slot %d of %d: cap was already occupied", i+1, composeHealthProbeConcurrency)
		}
	}
}

// TestAcquireComposeHealthProbeCapsConcurrency is the amplification
// measurement: many more callers than slots, and the peak number holding a
// slot at the same time is the ceiling, not the number of callers.
func TestAcquireComposeHealthProbeCapsConcurrency(t *testing.T) {
	drainComposeHealthProbeSem(t)
	defer drainComposeHealthProbeSem(t)

	const callers = 200

	restore := composeHealthProbeWait
	composeHealthProbeWait = 5 * time.Second // long enough that nobody is refused here
	defer func() { composeHealthProbeWait = restore }()

	var inFlight, peak int64
	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireComposeHealthProbe()
			if err != nil {
				t.Errorf("caller refused with a 5s wait budget: %v", err)
				return
			}
			defer release()

			now := atomic.AddInt64(&inFlight, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond) // hold the slot long enough to overlap
			atomic.AddInt64(&inFlight, -1)
		}()
	}
	wg.Wait()

	got := atomic.LoadInt64(&peak)
	if got > composeHealthProbeConcurrency {
		t.Fatalf("cap breached: peak %d concurrent probes, cap is %d", got, composeHealthProbeConcurrency)
	}
	// Anti-vacuity: without this, a build where every caller happened to run
	// one-at-a-time would pass while proving nothing. Demand that the cap was
	// actually reached, i.e. the test really did press against it.
	if got != composeHealthProbeConcurrency {
		t.Fatalf("cap never reached: peak %d with %d concurrent callers, expected exactly %d "+
			"(if this is flaky the hold is too short, not the cap wrong)",
			got, callers, composeHealthProbeConcurrency)
	}
	t.Logf("amplification: %d concurrent callers -> peak %d concurrent probes (cap %d)",
		callers, got, composeHealthProbeConcurrency)
}

// TestAcquireComposeHealthProbeRefusesWhenSaturated proves the overflow
// path: a caller that cannot get a slot is refused after the wait budget,
// and recovers as soon as a slot frees. Refusing is what keeps the queue
// from becoming the new lever.
func TestAcquireComposeHealthProbeRefusesWhenSaturated(t *testing.T) {
	drainComposeHealthProbeSem(t)
	defer drainComposeHealthProbeSem(t)

	restore := composeHealthProbeWait
	composeHealthProbeWait = 50 * time.Millisecond
	defer func() { composeHealthProbeWait = restore }()

	fillComposeHealthProbeSem(t)

	start := time.Now()
	release, err := acquireComposeHealthProbe()
	waited := time.Since(start)

	if err != errComposeHealthProbeBusy {
		if release != nil {
			release()
		}
		t.Fatalf("saturated cap handed out slot %d: err=%v", composeHealthProbeConcurrency+1, err)
	}
	if waited < composeHealthProbeWait {
		t.Fatalf("refused after %v, before the %v budget was spent: the caller never queued", waited, composeHealthProbeWait)
	}
	if waited > 10*composeHealthProbeWait {
		t.Fatalf("refused after %v, far past the %v budget: the wait is not bounded", waited, composeHealthProbeWait)
	}

	// Free one slot; the next caller must get it.
	<-composeHealthProbeSem
	release, err = acquireComposeHealthProbe()
	if err != nil {
		t.Fatalf("slot freed but caller still refused: %v", err)
	}
	release()
	t.Logf("saturated cap refused after %v (budget %v) and recovered on release", waited, composeHealthProbeWait)
}

// TestComposeAppHealthCheckGoesThroughTheCap is the load-bearing one: it
// proves the PRODUCTION path is wired to the cap, not just that a helper
// exists. With every slot taken, HealthCheck must refuse with
// errComposeHealthProbeBusy.
//
// It doubles as proof that the cap is taken BEFORE StoreInfo: this runs on
// a ComposeApp with no XCasaOS extension at all, so any error other than
// errComposeHealthProbeBusy means the function reached StoreInfo first.
func TestComposeAppHealthCheckGoesThroughTheCap(t *testing.T) {
	drainComposeHealthProbeSem(t)
	defer drainComposeHealthProbeSem(t)

	restore := composeHealthProbeWait
	composeHealthProbeWait = 50 * time.Millisecond
	defer func() { composeHealthProbeWait = restore }()

	fillComposeHealthProbeSem(t)

	a := &ComposeApp{Name: "no-such-app"}
	done := make(chan error, 1)
	go func() {
		_, err := a.HealthCheck()
		done <- err
	}()

	select {
	case err := <-done:
		if err != errComposeHealthProbeBusy {
			t.Fatalf("HealthCheck did not go through the cap: err=%v "+
				"(a StoreInfo error here means the cap is taken too late, or not at all)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HealthCheck blocked on a saturated cap instead of refusing: the wait is unbounded")
	}
}
