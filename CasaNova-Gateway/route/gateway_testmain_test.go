package route

import (
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
)

// TestMain keeps every test in this package off the real /etc/casanova path.
// Tests here build service.Management the normal production way —
// service.NewManagementService(service.NewState()) — because that is exactly
// what mgmtHandler, setup, and mgmtSkipper need to drive the real route tree.
// Without this, that construction reads (and can be DEGRADED by) whatever
// /etc/casanova happens to be on the machine running the suite: root-owned on
// a real box, absent in CI, writable in a dev container. None of that may
// change what these tests assert.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "casanova-gateway-route-testmain")
	if err != nil {
		panic(err)
	}
	service.SetConfigPathsForTest(dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
