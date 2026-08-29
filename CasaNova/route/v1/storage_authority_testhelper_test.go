package v1

import (
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// TestMain provisions the storage authority for this package's tests.
//
// Wiring the handlers to the authority made seven pre-existing tests answer
// 503 POLICY_UNAVAILABLE. That is the authority behaving correctly — an
// unprovisioned authority refuses everything — but those tests measure walk
// bounds, cancellation and bulk limits, not authorization, and their fixtures
// live under the OS temporary directory.
//
// So the test binary declares the temporary tree as storage this appliance
// manages. This is a fixture, NOT a bypass: every rule still evaluates, and a
// path outside the declared root is still refused. The tests that assert a
// refusal (system locations, unauthorised principals, paths outside storage)
// keep asserting it, because none of them live under the temp root.
func TestMain(m *testing.M) {
	stateDir, err := os.MkdirTemp("", "casanova-storageauth-state-*")
	if err != nil {
		panic("test setup: " + err.Error())
	}
	storageauth.SetStateDir(stateDir)

	if err := storageauth.Ensure(storageauth.Deps{
		DataRoots: []string{os.TempDir()},
	}); err != nil {
		panic("test setup: provision storage authority: " + err.Error())
	}

	code := m.Run()
	os.RemoveAll(stateDir)
	os.Exit(code)
}

// authorizeStorageUnder narrows the authority to specific roots for a single
// test that needs to assert a refusal outside them.
func authorizeStorageUnder(t *testing.T, roots ...string) {
	t.Helper()
	if len(roots) == 0 {
		t.Fatal("authorizeStorageUnder needs at least one root")
	}
	storageauth.SetStateDir(t.TempDir())
	if err := storageauth.Ensure(storageauth.Deps{DataRoots: roots}); err != nil {
		t.Fatalf("provision storage authority for %v: %v", roots, err)
	}
	t.Cleanup(func() {
		storageauth.SetStateDir(t.TempDir())
		_ = storageauth.Ensure(storageauth.Deps{DataRoots: []string{os.TempDir()}})
	})
}
