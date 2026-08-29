package service

import (
	"testing"
	"time"
)

// Three properties, one per finding closed in this pass.

// P1: the window between the claim and the publish must not consume the probe
// slot. If it does, a worker that really wedges is never examined again, which
// is a LOST DETECTION — strictly worse than the skipped tick it replaces.
func TestWedgeProbeIsRetriedWhenPathsAreNotPublishedYet(t *testing.T) {
	const id = "op-unpublished"
	ctl := newOpControl()
	ctl.claimed.Store(true) // claimed...
	// ...but paths deliberately NOT stored: this is the real window in beginOp
	// between the claim CAS and ctl.paths.Store.
	opControls.Store(id, ctl)
	t.Cleanup(func() { opControls.Delete(id); wedgedOps.Delete(id) })

	considerWedge(id)

	if ctl.probing.Load() {
		t.Fatal("the probe slot stayed latched, so this operation would never be " +
			"examined again even after its paths became visible")
	}
	if _, wedged := OpWedged(id); wedged {
		t.Fatal("a wedge was recorded for a worker that has not named its tree")
	}

	// And the retry must actually work once the tree is published.
	paths := []string{"/DATA/gone"}
	ctl.paths.Store(&paths)
	realProbe := probePathAlive
	probePathAlive = func(string, time.Duration) bool { return false }
	t.Cleanup(func() { probePathAlive = realProbe })

	considerWedge(id)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, wedged := OpWedged(id); wedged {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the retry after publication never recorded the wedge")
}

// P2: an operator can release a tree held by a worker that is never coming
// back. Before this, the only exit was restarting the service.
func TestDiscardReleasesTheReservationAndUnblocksTheTree(t *testing.T) {
	const id = "op-dead-mount"
	ctl := newOpControl()
	ctl.claimed.Store(true)
	paths := []string{"/DATA/decommissioned"}
	ctl.paths.Store(&paths)
	opControls.Store(id, ctl)
	t.Cleanup(func() { opControls.Delete(id); wedgedOps.Delete(id) })

	if !markWedged(id, ctl, WedgeReasonUnresponsivePath, "/DATA/decommissioned") {
		t.Fatal("setup: the wedge was not recorded")
	}

	opTrackMu.Lock()
	_, blocked := wedgeConflictLocked([]string{"/DATA/decommissioned/file"})
	opTrackMu.Unlock()
	if !blocked {
		t.Fatal("setup: the tree was not actually reserved")
	}

	info, ok := DiscardWedgeReservation(id)
	if !ok {
		t.Fatal("the reservation could not be discarded")
	}
	if info.ID != id {
		t.Fatalf("discarded the wrong record: %q", info.ID)
	}

	opTrackMu.Lock()
	conflict, stillBlocked := wedgeConflictLocked([]string{"/DATA/decommissioned/file"})
	opTrackMu.Unlock()
	if stillBlocked {
		t.Fatalf("the tree is still reserved by %q after an explicit discard", conflict)
	}

	// The safety half: releasing the tree without cancelling first would let a
	// new operation run beside a worker that still believes it owns the path.
	if !ctl.cancelRequested.Load() {
		t.Error("the operation was not marked cancelled before its tree was freed")
	}
	select {
	case <-ctl.ctx.Done():
	default:
		t.Error("the operation context was not cancelled, so a worker that wakes " +
			"up would resume copying into a tree somebody else may now own")
	}
}

// P3: discarding leaves the control intact, so the worker's real exit path
// still runs correctly if the mount ever does come back.
func TestDiscardLeavesTheControlSoFinishOpStillWorks(t *testing.T) {
	const id = "op-returns-later"
	ctl := newOpControl()
	ctl.claimed.Store(true)
	paths := []string{"/DATA/slow"}
	ctl.paths.Store(&paths)
	opControls.Store(id, ctl)
	t.Cleanup(func() { opControls.Delete(id); wedgedOps.Delete(id) })

	markWedged(id, ctl, WedgeReasonUnresponsivePath, "/DATA/slow")
	DiscardWedgeReservation(id)

	if _, live := loadOpControl(id); !live {
		t.Fatal("the control was destroyed, so the worker's exit path has nothing to unwind")
	}
	finishOp(id, ctl) // must not panic, and must be a no-op on the reservation
	if _, still := OpWedged(id); still {
		t.Fatal("finishOp re-created a reservation that was explicitly discarded")
	}
}
