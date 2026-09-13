package storagemaint

// THE CAPABILITY CACHE'S OWN SEAM, pinned.
//
// capCache/capLoaded are package-level globals shared by every test that
// reaches CurrentCapabilities/DiscoverCapabilities in the same test binary —
// which includes every test in this package, plus, through the exported
// SetLookPath/ResetCapabilityCache pair, every test in CasaNova/route and
// CasaNova/route/v1 that exercises the storage-maintenance HTTP handlers.
//
// Found via a read-only seam census (c-storagemaint-seam-census): running
// the REAL, unmodified TestLabCapabilityDiscoveryMatchesReality (a Tier-1
// test that deliberately probes the real machine, in looplab_test.go) with
// no cleanup, followed by a cold read, showed every tool cached
// "Available=true" with real /usr/sbin paths, inherited with no indication
// of staleness. Given this package's real file order (format < looplab <
// sanitize), the practical direction is a FALSE GREEN: a future test
// reaching capabilities through anything other than a priming site that
// resets afterward would silently inherit an optimistic result, not a
// pessimistic one — the kind of bug that never gets found by symptom,
// because it does not hurt until the one time it does.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// capCacheLeakMarker cannot occur by accident: no real lookPath will ever
// resolve a tool to this path, so seeing it prove inheritance rather than
// coincidence.
const capCacheLeakMarker = "/nonexistent-blockdev-census-experiment/"

// TestSetLookPathInvalidatesTheCapabilityCache pins a side effect that five
// separate call sites — this package's own fixtures (fxNewLab,
// sanitizeFixture.installTools, TestLabCapabilityDiscoveryMatchesReality)
// plus CasaNova/route and CasaNova/route/v1's HTTP fixtures — depend on:
// SetLookPath setting capLoaded=false. If that side effect were ever
// "optimized away" for looking redundant next to a lookPath change, every
// one of those sites would lose its protection at once, with no test
// failing to say so. This turns the dependency into a contract.
func TestSetLookPathInvalidatesTheCapabilityCache(t *testing.T) {
	SetLookPath(func(name string) (string, error) { return "/whichever/" + name, nil })
	t.Cleanup(func() { SetLookPath(exec.LookPath); ResetCapabilityCache() })
	DiscoverCapabilities(context.Background())

	capMu.RLock()
	loaded := capLoaded
	capMu.RUnlock()
	if !loaded {
		t.Fatal("precondition: DiscoverCapabilities must have set capLoaded=true, or this test proves nothing")
	}

	SetLookPath(func(name string) (string, error) { return "/different/" + name, nil })

	capMu.RLock()
	loadedAfter := capLoaded
	capMu.RUnlock()
	if loadedAfter {
		t.Fatal("SetLookPath did not invalidate capLoaded; a cold read after changing lookPath would keep " +
			"serving the PREVIOUS lookPath's cached result instead of probing again")
	}
}

// TestResetCapabilityCacheClearsBothFields is the positive control this fix
// needs: proof that ResetCapabilityCache actually clears state, not just
// that it runs without panicking. Checked directly against the private
// fields, not through CurrentCapabilities, which would mask a broken reset
// by silently re-discovering.
func TestResetCapabilityCacheClearsBothFields(t *testing.T) {
	SetLookPath(func(name string) (string, error) { return "/whatever/" + name, nil })
	t.Cleanup(func() { SetLookPath(exec.LookPath); ResetCapabilityCache() })
	DiscoverCapabilities(context.Background())

	capMu.RLock()
	loadedBefore, cacheBefore := capLoaded, capCache
	capMu.RUnlock()
	if !loadedBefore || len(cacheBefore.Tools) == 0 {
		t.Fatal("precondition: DiscoverCapabilities must have populated the cache, or this test proves nothing about the reset")
	}

	ResetCapabilityCache()

	capMu.RLock()
	loadedAfter, cacheAfter := capLoaded, capCache
	capMu.RUnlock()
	if loadedAfter {
		t.Fatal("ResetCapabilityCache left capLoaded=true; a later cold read would keep serving the stale " +
			"cache instead of re-probing")
	}
	if len(cacheAfter.Tools) != 0 || !cacheAfter.DiscoveredAt.IsZero() {
		t.Fatalf("ResetCapabilityCache left capCache non-empty: %+v", cacheAfter)
	}
}

// TestCapabilityCachePhase1PrimesThenCleansUp primes the cache the way every
// real entry point now does: SetLookPath, then ResetCapabilityCache in
// t.Cleanup. Go runs the tests in this file in source order, so this always
// runs immediately before phase 2 — a language guarantee, not luck.
func TestCapabilityCachePhase1PrimesThenCleansUp(t *testing.T) {
	SetLookPath(func(name string) (string, error) {
		return capCacheLeakMarker + name, nil
	})
	t.Cleanup(func() { SetLookPath(exec.LookPath); ResetCapabilityCache() })
	DiscoverCapabilities(context.Background())
}

// TestCapabilityCachePhase2NeverSeesAPreviousTestsCache pins that phase 1's
// cleanup actually ran. If a future priming site forgets to call
// ResetCapabilityCache in its own t.Cleanup, this goes red — the same way
// TestFixtureMountTableDoesNotLeakTheHostsDisks does for the mount-table
// seam: the guard is a test that fails when the leak comes back, not a
// comment asking nicely.
func TestCapabilityCachePhase2NeverSeesAPreviousTestsCache(t *testing.T) {
	caps := CurrentCapabilities(context.Background())
	for _, ts := range caps.Tools {
		if strings.HasPrefix(ts.Path, capCacheLeakMarker) {
			t.Fatalf("%s still carries phase 1's cache marker (%s); ResetCapabilityCache did not run, "+
				"or a priming site forgot to call it in t.Cleanup", ts.Tool, ts.Path)
		}
	}
}
