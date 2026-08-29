package storagemaint

import (
	"context"
	"os"
	"testing"
)

// A root whose sweep was cut short by the deadline must still be swept later.
//
// seenRoots is derived from RUNNING records, and recovery retires those records
// before it sweeps. So without a hand-off file the roots left over from an
// interrupted pass are unreachable for ever: the next boot cannot re-derive
// them, and their artifacts occupy the disk with nothing pointing at them.
// Bounding startup must not cost the operator silent disk loss.
func TestARecordedRootIsSweptEvenWhenNoLiveRecordNamesIt(t *testing.T) {
	f := newSanitizeFixture(t)

	// The situation a bounded recovery leaves behind: a previous pass retired
	// the records for this root and then ran out of time before sweeping it.
	// Nothing RUNNING points here any more, so seenRoots can never re-derive
	// it — the hand-off file is the only thing that can.
	if err := savePendingSweepRoots([]string{f.root}); err != nil {
		t.Fatal(err)
	}

	rep := RecoverSanitizeJobs(context.Background())

	found := false
	for _, r := range rep.Roots {
		if r == f.root {
			found = true
		}
	}
	if !found {
		t.Fatalf("the recorded root was not swept (%v), so artifacts left by an "+
			"interrupted pass stay on disk with nothing pointing at them", rep.Roots)
	}

	// And a completed sweep must clear it, or every later boot re-sweeps for ever.
	if _, err := os.Stat(pendingSweepPath()); !os.IsNotExist(err) {
		t.Fatalf("the hand-off survived a completed sweep (%v), so this root is "+
			"re-swept on every boot from now on", err)
	}
}

// The complementary half: a pass that stops before retiring anything owes
// nothing, and must NOT leave a hand-off behind. Those records are still
// RUNNING and are re-derived normally.
func TestAPassThatRetiredNothingRecordsNothing(t *testing.T) {
	f := newSanitizeFixture(t)
	seedInterruptedJob(t, f, "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = RecoverSanitizeJobs(ctx)

	if got := loadPendingSweepRoots(); len(got) != 0 {
		t.Fatalf("a pass that retired nothing recorded %v; every boot would now "+
			"re-sweep a root nothing is owed on", got)
	}
}
