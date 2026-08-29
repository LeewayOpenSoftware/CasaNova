// Package applock provides per-app mutual exclusion for lifecycle operations.
// Prevents concurrent install/update/start/stop/uninstall on the same app,
// which can cause Docker state corruption and orphaned containers.
//
// Inspired by Runtipi's AsyncMutex pattern, but implemented with Go primitives
// instead of an external message queue dependency.
package applock

import (
	"fmt"
	"sync"
)

// Manager manages per-app locks to prevent concurrent lifecycle operations.
//
// Ownership is a boolean in a map guarded by one manager-wide mutex, NOT a
// per-app sync.Mutex. Two properties of the callers depend on that shape:
//
//   - IsLocked has to be a pure observation. Implemented as TryLock+Unlock on a
//     per-app mutex, merely *asking* whether an app was busy acquired its lock
//     for a few nanoseconds, so the advisory checks in ComposeApp.Update and
//     ComposeApp.Apply could make a concurrent Acquire fail with a bogus "busy"
//     while nothing whatsoever was running — which the nightly auto-update
//     sweep records as "skipped: app busy", deferring that app's updates 24h.
//   - Entries have to be evictable. A per-app mutex cannot be removed from the
//     map safely: a caller that already read the pointer would go on to lock
//     the orphan while a later caller creates a fresh mutex for the same name,
//     giving two simultaneous "holders" of one app. So the old map grew for
//     every app name ever installed and was never pruned, on a box expected to
//     run for months. A boolean deleted under the manager mutex has no such
//     window, because the map lookup and the ownership test are one critical
//     section.
type Manager struct {
	mu   sync.Mutex
	held map[string]bool
}

// NewManager creates a new per-app lock manager.
func NewManager() *Manager {
	return &Manager{
		held: make(map[string]bool),
	}
}

// Acquire obtains an exclusive lock for the given app ID.
// Returns a release function that MUST be called when the operation is complete.
// Returns an error if the app is already locked (non-blocking).
func (m *Manager) Acquire(appID string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.held[appID] {
		return nil, fmt.Errorf("app %s is busy with another operation", appID)
	}
	m.held[appID] = true

	// Releasing is idempotent. Callers hand this function to a goroutine and
	// also guard synchronous error paths with it; a double call must not free
	// the lock that the NEXT operation on the same app has meanwhile taken —
	// that would put two writers on one app's compose file, which is the exact
	// failure this package exists to prevent.
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.held, appID)
			m.mu.Unlock()
		})
	}, nil
}

// IsLocked reports whether an app currently has a lock held. It only observes:
// it never takes the app's lock, so racing an Acquire against it cannot make
// that Acquire fail.
func (m *Manager) IsLocked(appID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.held[appID]
}
