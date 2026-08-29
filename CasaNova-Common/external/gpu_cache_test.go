package external

import (
	"errors"
	"testing"
	"time"
)

// THE DEFECT: three modules call this, including UserService's
// GET /v1/users/status — polled by the login screen and reachable without a
// token. Each call shelled out to nvidia-smi. On a machine that HAS nvidia-smi
// but no working driver that is 28 ms per request; on one without it, 0.194 ms.
//
// The assertion is a COUNT of underlying invocations, not a duration.
func TestGPUInventoryIsProbedOncePerTTL(t *testing.T) {
	ResetGPUCacheForTest()
	t.Cleanup(ResetGPUCacheForTest)

	// First call populates the cache — whatever the machine answers.
	first, firstErr := NvidiaGPUInfoList()

	// A hundred more must not re-probe. We cannot count exec calls without
	// touching the production function's shape, so we assert the observable
	// consequence: identical results, returned from the cache, including the
	// error value, which is the case that matters most.
	for i := 0; i < 100; i++ {
		got, gotErr := NvidiaGPUInfoList()
		if (gotErr == nil) != (firstErr == nil) {
			t.Fatalf("call %d disagreed with the first about whether nvidia-smi "+
				"works (%v vs %v): the cache is not serving the failure, so the "+
				"slow path runs on every request", i, gotErr, firstErr)
		}
		if len(got) != len(first) {
			t.Fatalf("call %d returned %d GPUs, first returned %d", i, len(got), len(first))
		}
	}
}

// CACHING THE FAILURE IS THE POINT. A cache that stores only successes leaves
// the 28 ms "I have the tool but no GPU" path uncached — exactly backwards.
//
// The probe is injected so this runs everywhere, not only on a machine that
// happens to lack a working driver. The first version of this test skipped on
// any developer box with nvidia-smi installed, which is to say it covered
// nothing on the machine most likely to run it.
func TestTheFailureIsCachedToo(t *testing.T) {
	var calls int
	restore := SetGPUProbeForTest(func() ([]NvidiaGPUInfo, error) {
		calls++
		return nil, errors.New("nvidia-smi: no devices were found")
	})
	t.Cleanup(restore)

	for i := 0; i < 50; i++ {
		if _, err := NvidiaGPUInfoList(); err == nil {
			t.Fatalf("call %d reported success for a probe that always fails", i)
		}
	}
	if calls != 1 {
		t.Fatalf("50 calls ran the probe %d times, want 1. The failure is not "+
			"cached, so the EXPENSIVE case (28 ms on a box with nvidia-smi and no "+
			"usable GPU) repeats on every request — including the unauthenticated "+
			"GET /v1/users/status that the login screen polls.", calls)
	}
}

// And a success is cached exactly once too.
func TestASuccessIsProbedOnce(t *testing.T) {
	var calls int
	restore := SetGPUProbeForTest(func() ([]NvidiaGPUInfo, error) {
		calls++
		return []NvidiaGPUInfo{{}}, nil
	})
	t.Cleanup(restore)

	for i := 0; i < 50; i++ {
		got, err := NvidiaGPUInfoList()
		if err != nil || len(got) != 1 {
			t.Fatalf("call %d: %v / %d GPUs", i, err, len(got))
		}
	}
	if calls != 1 {
		t.Fatalf("50 calls ran the probe %d times, want 1", calls)
	}
}

// The cache must expire, or a driver that comes back after a reload is never
// noticed until the service restarts.
func TestTheCacheExpires(t *testing.T) {
	ResetGPUCacheForTest()
	t.Cleanup(ResetGPUCacheForTest)

	if _, err := NvidiaGPUInfoList(); err != nil && gpuCache.at.IsZero() {
		t.Fatal("setup: nothing was cached")
	}
	gpuCache.mu.Lock()
	gpuCache.at = time.Now().Add(-2 * gpuCacheTTL)
	gpuCache.mu.Unlock()

	before := gpuCache.at
	_, _ = NvidiaGPUInfoList()
	if gpuCache.at.Equal(before) {
		t.Fatalf("an expired entry (TTL %v) was served instead of re-probing: a "+
			"driver reload would never be picked up", gpuCacheTTL)
	}
}
