package service

import (
	"strconv"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/portalloc"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// Package-level port allocator. Shared across all install/uninstall flows
// so a concurrent install of two apps can't both believe the same port
// is free. Before the allocator was wired, `GetPortsInUse` checked the
// live host state at validation time — but two installs running within
// the same window could both pass validation, race to Docker run, and
// one would fail with a cryptic port-in-use error after the other grabbed
// it first.
var (
	portAllocator     *portalloc.Allocator
	portAllocatorOnce sync.Once
)

// PortAllocator returns the process-wide allocator, lazily initializing
// it with the default 8000-8999 user-app range on first use.
func PortAllocator() *portalloc.Allocator {
	portAllocatorOnce.Do(func() {
		portAllocator = portalloc.New()
		logger.Info("port allocator initialized", zap.Int("range_start", 8000), zap.Int("range_end", 8999))
	})
	return portAllocator
}

// ReserveComposePorts walks every service in a compose app and reserves
// each published port with the allocator. On the first conflict the
// already-reserved ports from this app are rolled back so there's no
// partial reservation state on failure.
//
// Returns a human-readable error naming the conflicting port so the UI
// can tell the user exactly which port to free or reassign.
func ReserveComposePorts(appName string, services []composeServicePorts) error {
	alloc := PortAllocator()
	var reserved []int
	for _, svc := range services {
		for _, published := range svc.Published {
			port, err := strconv.Atoi(published)
			if err != nil || port <= 0 {
				continue // skip non-numeric (e.g. templated) ports
			}
			if err := alloc.Reserve(port, appName); err != nil {
				// Roll back every port we've taken for this install so far.
				for _, r := range reserved {
					alloc.Release(r, appName)
				}
				return err
			}
			reserved = append(reserved, port)
		}
	}
	return nil
}

// ReleaseComposePorts frees every port held by appName.
// Idempotent — safe to call even if the app had no reservations.
func ReleaseComposePorts(appName string) {
	PortAllocator().ReleaseApp(appName)
}

// composeServicePorts is a minimal port projection of a compose service,
// decoupled from compose-go types so pkg/portalloc stays import-light.
type composeServicePorts struct {
	Published []string
}
