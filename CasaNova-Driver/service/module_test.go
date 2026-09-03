package service

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
)

// GET /modules used to call moduleIsOutOfTree once per loaded module, one at a
// time, each a real modinfo subprocess spawn. /proc/modules routinely holds
// 150-300+ entries on a NAS box, so one request could cost seconds-to-minutes
// of pure serial spawn overhead. classifyOutOfTree now caches per module name
// and fans a cache miss out over a bounded worker pool.

// resetOutOfTreeState clears the package-level cache between tests so one
// test's writes cannot leak into another's assertions.
func resetOutOfTreeState(t *testing.T) {
	t.Helper()
	outOfTreeCacheMu.Lock()
	outOfTreeCache = map[string]outOfTreeCacheEntry{}
	outOfTreeCacheMu.Unlock()

	origRunner := runModinfoFilename
	origTTL := moduleOutOfTreeCacheTTL
	origConcurrency := moduleOutOfTreeMaxConcurrency
	t.Cleanup(func() {
		runModinfoFilename = origRunner
		moduleOutOfTreeCacheTTL = origTTL
		moduleOutOfTreeMaxConcurrency = origConcurrency
		outOfTreeCacheMu.Lock()
		outOfTreeCache = map[string]outOfTreeCacheEntry{}
		outOfTreeCacheMu.Unlock()
	})
}

func TestModuleIsOutOfTreeCachesAcrossCalls(t *testing.T) {
	resetOutOfTreeState(t)

	var calls atomic.Int32
	runModinfoFilename = func(name string) ([]byte, error) {
		calls.Add(1)
		return []byte("/lib/modules/6.1/updates/nvidia.ko"), nil
	}

	first := moduleIsOutOfTree("nvidia")
	second := moduleIsOutOfTree("nvidia")

	if !first || !second {
		t.Fatalf("expected both calls to report out-of-tree, got first=%v second=%v", first, second)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("a cached module must not re-invoke modinfo: want 1 call, got %d", got)
	}
}

func TestModuleIsOutOfTreeReclassifiesAfterTTLExpires(t *testing.T) {
	resetOutOfTreeState(t)
	moduleOutOfTreeCacheTTL = 10 * time.Millisecond

	var calls atomic.Int32
	runModinfoFilename = func(name string) ([]byte, error) {
		calls.Add(1)
		return []byte("/lib/modules/6.1/kernel/drivers/net/e1000e.ko"), nil
	}

	moduleIsOutOfTree("e1000e")
	time.Sleep(30 * time.Millisecond)
	moduleIsOutOfTree("e1000e")

	if got := calls.Load(); got != 2 {
		t.Fatalf("an expired cache entry must be re-checked: want 2 calls, got %d", got)
	}
}

// This is the RED-first case for the PERF finding: before classifyOutOfTree
// existed, List() called moduleIsOutOfTree once per module IN THE SAME LOOP
// THAT SCANS /proc/modules, so nothing about that shape could run more than
// one modinfo at a time. Asserting real concurrency here fails against that
// shape and passes against the bounded worker pool.
func TestClassifyOutOfTreeRunsModinfoConcurrentlyUpToTheBound(t *testing.T) {
	resetOutOfTreeState(t)
	moduleOutOfTreeMaxConcurrency = 4

	const moduleCount = 12
	release := make(chan struct{})
	var inFlight atomic.Int32
	var maxObserved atomic.Int32

	runModinfoFilename = func(name string) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			cur := maxObserved.Load()
			if n <= cur || maxObserved.CompareAndSwap(cur, n) {
				break
			}
		}
		<-release
		return []byte(""), nil
	}

	mods := make([]model.KernelModule, moduleCount)
	for i := range mods {
		mods[i] = model.KernelModule{Name: "mod" + string(rune('a'+i))}
	}

	done := make(chan struct{})
	go func() {
		classifyOutOfTree(mods)
		close(done)
	}()

	// Give every goroutine a chance to reach the fake modinfo and block on
	// release before checking how many made it in at once.
	deadline := time.After(2 * time.Second)
	for {
		if inFlight.Load() == int32(moduleOutOfTreeMaxConcurrency) {
			break
		}
		select {
		case <-deadline:
			close(release)
			<-done
			t.Fatalf("expected %d concurrent modinfo calls in flight, only reached %d", moduleOutOfTreeMaxConcurrency, inFlight.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// And it must never exceed the bound even once every worker has run.
	close(release)
	<-done

	if got := maxObserved.Load(); got != int32(moduleOutOfTreeMaxConcurrency) {
		t.Fatalf("expected peak concurrency to equal the bound %d, observed %d", moduleOutOfTreeMaxConcurrency, got)
	}
}

func TestClassifyOutOfTreeNeverExceedsTheConcurrencyBound(t *testing.T) {
	resetOutOfTreeState(t)
	moduleOutOfTreeMaxConcurrency = 3

	const moduleCount = 30
	var inFlight atomic.Int32
	var mu sync.Mutex
	var violated bool

	runModinfoFilename = func(name string) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		if n > int32(moduleOutOfTreeMaxConcurrency) {
			mu.Lock()
			violated = true
			mu.Unlock()
		}
		time.Sleep(time.Millisecond)
		return []byte(""), nil
	}

	mods := make([]model.KernelModule, moduleCount)
	for i := range mods {
		mods[i] = model.KernelModule{Name: "mod" + strconv.Itoa(i)}
	}

	classifyOutOfTree(mods)

	mu.Lock()
	defer mu.Unlock()
	if violated {
		t.Fatalf("observed more than %d concurrent modinfo calls", moduleOutOfTreeMaxConcurrency)
	}
}
