package service

import (
	"path/filepath"
	"testing"
)

// SetConfigPathsForTest redirects the gateway's durable, /etc/casanova-backed
// configuration (the global IP allow-list and the per-route authorization
// policy) to files under dir, for the remaining lifetime of the test binary.
//
// Exported — rather than a _test.go-only helper — so any package that builds
// a Management the normal way, via NewManagementService(NewState()), can
// isolate it too. route's own tests are the reason this exists: they
// construct Management directly and, before this, had no way to reach these
// unexported package vars, so every one of them read (and could be degraded
// by) whatever /etc/casanova happens to be on the machine running the suite.
//
// Panics outside a test binary, the same guard internalauth.SetPathsForTest
// uses: these are the files an administrator's decisions live in, and
// relocating them in a running gateway would silently drop that state.
func SetConfigPathsForTest(dir string) {
	if !testing.Testing() {
		panic("service.SetConfigPathsForTest called outside a test binary: this " +
			"would relocate the gateway's durable security config in a running system")
	}
	whitelistFile = filepath.Join(dir, "whitelist.json")
	routePolicyFile = filepath.Join(dir, "route-policy.json")
}
