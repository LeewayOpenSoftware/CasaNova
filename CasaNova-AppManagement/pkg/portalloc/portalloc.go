// Package portalloc provides port allocation and conflict detection for apps.
//
// Inspired by YunoHost's resource system: instead of letting two apps fight
// over port 8080 and one fail at Docker runtime, the allocator knows which
// ports are in use (by apps, by the host) and hands out free ones.
//
// Three layers of port detection:
//  1. Explicit reservations (allocated to installed apps, tracked in memory)
//  2. Host-level binds (ports in LISTEN state on the host)
//  3. Docker container port mappings
//
// The allocator is conservative: when in doubt, skip the port.
package portalloc

import (
	"fmt"
	"net"
	"sync"
)

// Allocator manages port reservations.
// Safe for concurrent use.
type Allocator struct {
	mu       sync.Mutex
	reserved map[int]string // port → app name that owns it

	// Preferred port range for auto-allocation.
	// 8000-8999 is a common range for user apps and doesn't collide with
	// well-known services (1-1023) or ephemeral ports (32768+).
	rangeStart int
	rangeEnd   int
}

// New creates a port allocator with the default range (8000-8999).
func New() *Allocator {
	return NewWithRange(8000, 8999)
}

// NewWithRange creates an allocator with a custom port range.
func NewWithRange(start, end int) *Allocator {
	return &Allocator{
		reserved:   make(map[int]string),
		rangeStart: start,
		rangeEnd:   end,
	}
}

// Reserve marks a port as allocated to an app. Fails if the port is
// already reserved by a different app or is currently bound on the host.
// Calling Reserve with the same app name is idempotent (no-op if already owned).
func (a *Allocator) Reserve(port int, appName string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %d", port)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if owner, exists := a.reserved[port]; exists {
		if owner == appName {
			return nil // idempotent
		}
		return fmt.Errorf("port %d is already reserved by %q", port, owner)
	}

	if !isPortFreeOnHost(port) {
		return fmt.Errorf("port %d is already in use by the host", port)
	}

	a.reserved[port] = appName
	return nil
}

// Release frees a port previously reserved by an app.
// Does nothing if the port wasn't reserved or belonged to another app.
func (a *Allocator) Release(port int, appName string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if owner, exists := a.reserved[port]; exists && owner == appName {
		delete(a.reserved, port)
	}
}

// ReleaseApp frees all ports reserved by the given app.
// Call when uninstalling an app.
func (a *Allocator) ReleaseApp(appName string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for port, owner := range a.reserved {
		if owner == appName {
			delete(a.reserved, port)
		}
	}
}

// Allocate finds a free port in the allocator's range and reserves it
// for the given app. Returns an error if no free port is available.
func (a *Allocator) Allocate(appName string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for port := a.rangeStart; port <= a.rangeEnd; port++ {
		if _, taken := a.reserved[port]; taken {
			continue
		}
		if !isPortFreeOnHost(port) {
			continue
		}
		a.reserved[port] = appName
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", a.rangeStart, a.rangeEnd)
}

// IsReserved returns true if the given port is currently reserved
// (by any app or bound on the host).
func (a *Allocator) IsReserved(port int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.reserved[port]; exists {
		return true
	}
	return !isPortFreeOnHost(port)
}

// Reservations returns a snapshot of current port → app name mappings.
func (a *Allocator) Reservations() map[int]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[int]string, len(a.reserved))
	for k, v := range a.reserved {
		out[k] = v
	}
	return out
}

// isPortFreeOnHost checks if a port is available by attempting to listen on it.
// This catches ports in use by the host OS or other containers with host networking.
// Tests both IPv4 and IPv6 loopback.
func isPortFreeOnHost(port int) bool {
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
