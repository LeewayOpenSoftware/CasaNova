package trash

// The silent no-op: a destructive operation the user asked for that cannot
// succeed, reported as success.
//
// Empty and Purge both looped with `if err := m.Delete(&e); err == nil { count++ }`,
// so every per-entry failure was swallowed — not logged, not counted, not
// returned. The route handler then answered SUCCESS with the message
// "trash emptied" and deleted:0 while every file was still on disk, and the
// janitor's `if count > 0` notification made "nothing was old enough to
// remove" and "every single removal was refused" literally indistinguishable.
//
// These tests pin the distinction. Each failure case is paired with a
// positive control, because a test that reports zero removals proves nothing
// unless the same fixture is shown removing something when it can.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lockTrashFiles makes the files/ directory refuse removals, which is how a
// read-only mount or a permission fault presents to Delete. Returns a restore
// func so t.TempDir cleanup can still remove the tree.
func lockTrashFiles(t *testing.T, root string) func() {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(root, "*", "files"))
	if len(matches) == 0 {
		t.Fatal("no trash files/ dir to lock — fixture did not trash anything")
	}
	for _, d := range matches {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, d := range matches {
			os.Chmod(d, 0o755)
		}
	}
}

func seedTrashedFile(t *testing.T, m *Manager, root, name string) {
	t.Helper()
	victim := filepath.Join(root, name)
	if err := os.WriteFile(victim, []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Trash(victim); err != nil {
		t.Fatal(err)
	}
}

// TestEmptyReportsWhatItCouldNotDelete is the regression. Before the fix this
// returned (0, nil) and the handler said "trash emptied".
func TestEmptyReportsWhatItCouldNotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only files/ dir does not refuse root")
	}
	m, root := trashManagerForTest(t)
	seedTrashedFile(t, m, root, "documento.txt")

	restore := lockTrashFiles(t, root)
	defer restore()

	count, failed, err := m.Empty([]string{root})
	if err != nil {
		t.Fatalf("Empty returned a hard error; it should report per-entry failures instead: %v", err)
	}
	if count != 0 {
		t.Errorf("counted %d removals that did not happen", count)
	}
	if len(failed) != 1 {
		t.Fatalf("the refused entry was swallowed: failed=%v — this is the silent no-op", failed)
	}
	if failed[0].Reason == "" {
		t.Error("a failure with no reason tells the user nothing")
	}
	// The point of the whole exercise: the file is still there.
	leftover, _ := filepath.Glob(filepath.Join(root, "*", "files", "*"))
	if len(leftover) == 0 {
		t.Fatal("premise broken: the fixture did not actually prevent deletion")
	}
}

// TestEmptyPositiveControl proves the fixture CAN report a real removal, so
// the zero in the test above is evidence and not an inert test.
func TestEmptyPositiveControl(t *testing.T) {
	m, root := trashManagerForTest(t)
	seedTrashedFile(t, m, root, "documento.txt")

	count, failed, err := m.Empty([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(failed) != 0 {
		t.Fatalf("clean empty should be (1, none), got (%d, %v)", count, failed)
	}
}

// TestPurgeDistinguishesNothingDueFromEverythingRefused is the janitor bug:
// both outcomes used to be (0, nil) and both were silent.
func TestPurgeDistinguishesNothingDueFromEverythingRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only files/ dir does not refuse root")
	}

	// Case A — nothing is old enough. Quiet is CORRECT here.
	mA, rootA := trashManagerForTest(t)
	seedTrashedFile(t, mA, rootA, "reciente.txt")
	mA.Retention = time.Hour
	countA, failedA, errA := mA.Purge([]string{rootA})
	if errA != nil {
		t.Fatal(errA)
	}
	if countA != 0 || len(failedA) != 0 {
		t.Fatalf("nothing was due, so this must be (0, none), got (%d, %v)", countA, failedA)
	}

	// Case B — everything is due and every removal is refused. Quiet is a LIE.
	mB, rootB := trashManagerForTest(t)
	seedTrashedFile(t, mB, rootB, "viejo.txt")
	mB.Retention = time.Nanosecond
	time.Sleep(2 * time.Millisecond) // put the entry firmly behind the cutoff
	restore := lockTrashFiles(t, rootB)
	defer restore()

	countB, failedB, errB := mB.Purge([]string{rootB})
	if errB != nil {
		t.Fatalf("Purge returned a hard error; per-entry failures belong in the failed set: %v", errB)
	}
	if countB != 0 {
		t.Errorf("counted %d removals that did not happen", countB)
	}
	if len(failedB) == 0 {
		t.Fatal("every removal was refused and Purge reported nothing — indistinguishable from case A, which is the bug")
	}

	// The contract, stated as the janitor sees it.
	if (countA == countB) && (len(failedA) == len(failedB)) {
		t.Fatal("case A and case B are still indistinguishable to the caller")
	}
}

// TestDeletionDateRoundTripsInLocalTime pins the two halves of the trashinfo
// round trip to the same clock. Trash writes a zoneless LOCAL wall-clock time
// (what the freedesktop spec asks for); the reader used time.Parse, which
// reads a zoneless layout as UTC. West of Greenwich that made every entry read
// back as its own UTC offset OLDER than it was, and retention expired early.
func TestDeletionDateRoundTripsInLocalTime(t *testing.T) {
	m, root := trashManagerForTest(t)
	before := time.Now()
	seedTrashedFile(t, m, root, "fecha.txt")

	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	skew := entries[0].DeletionDate.Sub(before)
	if skew < 0 {
		skew = -skew
	}
	// Anything beyond a couple of seconds is a zone offset, not clock jitter.
	if skew > 2*time.Second {
		t.Fatalf("DeletionDate round-tripped with %v of skew — the writer and the reader are using different clocks", skew)
	}
}

// TestPurgeWillNotAgeOutAnEntryWithAnUnreadableDate: the parser leaves the
// zero time when DeletionDate cannot be read, and the zero time is Before
// every cutoff — so one truncated .trashinfo used to mean the file it
// described was destroyed on the next janitor tick rather than kept.
func TestPurgeWillNotAgeOutAnEntryWithAnUnreadableDate(t *testing.T) {
	m, root := trashManagerForTest(t)
	seedTrashedFile(t, m, root, "corrupto.txt")

	infos, _ := filepath.Glob(filepath.Join(root, "*", "info", "*.trashinfo"))
	if len(infos) != 1 {
		t.Fatalf("expected 1 trashinfo, got %d", len(infos))
	}
	// A partial write from power loss is the ordinary way to get here.
	if err := os.WriteFile(infos[0], []byte("[Trash Info]\nPath=/tmp/corrupto.txt\nDeletionDate=\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].DeletionDate.IsZero() {
		t.Fatalf("premise broken: expected one entry with an unparsed date, got %+v", entries)
	}

	m.Retention = time.Hour
	count, failed, err := m.Purge([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || len(failed) != 0 {
		t.Fatalf("an entry of unknown age was aged out anyway: removed=%d failed=%v", count, failed)
	}
	if remaining, _ := filepath.Glob(filepath.Join(root, "*", "files", "*")); len(remaining) != 1 {
		t.Fatal("the file was destroyed despite its age being unknown")
	}
}
