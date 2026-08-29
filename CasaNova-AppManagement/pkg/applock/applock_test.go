package applock

import (
	"sync"
	"testing"
)

// TestAcquireIsExclusive is the base contract: one holder at a time, and the
// app becomes available again once that holder releases.
func TestAcquireIsExclusive(t *testing.T) {
	m := NewManager()

	release, err := m.Acquire("navidrome")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	if _, err := m.Acquire("navidrome"); err == nil {
		t.Fatal("two operations hold the same app at once")
	}

	// A different app must not be affected.
	otherRelease, err := m.Acquire("syncthing")
	if err != nil {
		t.Fatalf("locking one app blocked another: %v", err)
	}
	otherRelease()

	release()

	again, err := m.Acquire("navidrome")
	if err != nil {
		t.Fatalf("the app stayed locked after its holder released: %v", err)
	}
	again()
}

// TestIsLockedDoesNotAcquire: IsLocked used to be TryLock+Unlock, so merely
// ASKING whether an app was busy took its lock for a few nanoseconds. The
// advisory checks in ComposeApp.Update/Apply call it on every request, so a
// concurrent Acquire could be told "busy" while nothing at all was running —
// which the nightly auto-update sweep records as a skip, deferring that app's
// updates for a day. This hammers both sides; a probing IsLocked loses.
func TestIsLockedDoesNotAcquire(t *testing.T) {
	m := NewManager()

	stop := make(chan struct{})
	var probes sync.WaitGroup
	for i := 0; i < 4; i++ {
		probes.Add(1)
		go func() {
			defer probes.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.IsLocked("navidrome")
			}
		}()
	}

	for i := 0; i < 20000; i++ {
		release, err := m.Acquire("navidrome")
		if err != nil {
			close(stop)
			probes.Wait()
			t.Fatalf("Acquire #%d was refused although the app was free - IsLocked is taking the lock it claims to only observe: %v", i, err)
		}
		release()
	}

	close(stop)
	probes.Wait()
}

// TestIsLockedReportsTheRealState pins the other half: an observer must still
// see a held lock. A "return false always" IsLocked would pass the test above.
func TestIsLockedReportsTheRealState(t *testing.T) {
	m := NewManager()

	if m.IsLocked("navidrome") {
		t.Fatal("a never-locked app reports as locked")
	}

	release, err := m.Acquire("navidrome")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !m.IsLocked("navidrome") {
		t.Fatal("a held app reports as free - every advisory busy-check is blind")
	}
	if m.IsLocked("syncthing") {
		t.Fatal("locking one app makes another look locked")
	}

	release()
	if m.IsLocked("navidrome") {
		t.Fatal("a released app still reports as locked")
	}
}

// TestReleaseIsIdempotent: callers hand the release function to a goroutine and
// also guard synchronous error paths with it (ComposeService.Install does
// exactly that). A second call must be a no-op, NOT a release of whatever
// operation holds the app by then — that would put two writers on one app's
// compose file, which is what this package exists to prevent. The pre-existing
// implementation panicked on the second call ("unlock of unlocked mutex"),
// taking the daemon down.
func TestReleaseIsIdempotent(t *testing.T) {
	m := NewManager()

	first, err := m.Acquire("navidrome")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first()
	first() // must not panic and must not affect anything

	second, err := m.Acquire("navidrome")
	if err != nil {
		t.Fatalf("re-Acquire after release: %v", err)
	}

	// The stale release must not free the lock the second operation holds.
	first()

	if !m.IsLocked("navidrome") {
		t.Fatal("a stale release freed a lock held by a later operation - two writers now own the app")
	}
	if _, err := m.Acquire("navidrome"); err == nil {
		t.Fatal("a stale release let a third operation in while the second was still running")
	}

	second()
}

// TestReleaseEvictsTheEntry: the manager lives for the whole uptime of the
// daemon — months — and used to keep a mutex per app name forever, including
// for apps installed and uninstalled long ago. Nothing swept it.
func TestReleaseEvictsTheEntry(t *testing.T) {
	m := NewManager()

	for i := 0; i < 1000; i++ {
		release, err := m.Acquire(string(rune('a'+i%26)) + string(rune('a'+i/26)))
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		release()
	}

	m.mu.Lock()
	remaining := len(m.held)
	m.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d lock entries survived their operations - the map grows for every app name ever seen", remaining)
	}
}

// TestConcurrentAcquireElectsExactlyOneWinner: the double-click case, and the
// sweep-meets-admin case. Exactly one of N racing operations may proceed.
func TestConcurrentAcquireElectsExactlyOneWinner(t *testing.T) {
	m := NewManager()

	const contenders = 64
	start := make(chan struct{})
	results := make(chan func(), contenders)

	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := m.Acquire("navidrome")
			if err != nil {
				return
			}
			results <- release
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for release := range results {
		winners++
		release()
	}

	if winners != 1 {
		t.Fatalf("%d operations were let into the same app simultaneously, want exactly 1", winners)
	}
}
