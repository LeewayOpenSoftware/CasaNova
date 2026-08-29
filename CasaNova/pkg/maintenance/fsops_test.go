package maintenance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// scanTarget builds the target a category's scan would have produced for path.
func scanTarget(t *testing.T, path string, cutoff time.Time) target {
	t.Helper()
	st, err := lstatNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	kind, ok := kindOf(&st)
	if !ok {
		t.Fatalf("%s is not a file, symlink or directory", path)
	}
	return targetFrom(path, kind, &st, cutoff, nil)
}

// fakeFD pins the answer to "is this inode open" so a test does not have to
// arrange for a real process to cooperate.
type fakeFD struct{ held map[devIno]string }

func (f fakeFD) Holder(dev, ino uint64) (string, bool) {
	h, ok := f.held[devIno{dev, ino}]
	return h, ok
}

// Rule 5, identity half: between the scan and the run the file was replaced by
// a different inode with the same name. Deleting it would remove whatever took
// its place.
func TestRemoveRefusesWhenTheInodeChanged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	victim := filepath.Join(dir, "file")
	writeFile(t, victim, "original")

	tgt := scanTarget(t, victim, time.Time{})

	// Someone replaces the file — the classic TOCTOU swap. Renaming a
	// pre-existing file over it guarantees a different inode (a plain
	// remove+create often gets the same one back on tmpfs), and the mtime is
	// restored afterwards so ONLY the identity check can catch this.
	other := filepath.Join(dir, "other")
	writeFile(t, other, "somebody else's data")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victim, tgt.MTime, tgt.MTime); err != nil {
		t.Fatal(err)
	}

	g := newTestGuard(t, root)
	err := g.Remove(tgt, nil)
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("Remove after an inode swap = %v, want ErrIdentity", err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("the replacement file was deleted: %v", err)
	}
}

// Rule 5, mtime half: the file was written to after the scan, so it is not the
// idle leftover we decided about.
func TestRemoveRefusesWhenTheFileWasModified(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "sub", "file")
	writeFile(t, victim, "original")
	tgt := scanTarget(t, victim, time.Time{})

	// Rewind then bump the mtime so the change is unambiguous on filesystems
	// with coarse timestamps.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(victim, future, future); err != nil {
		t.Fatal(err)
	}

	g := newTestGuard(t, root)
	if err := g.Remove(tgt, nil); !errors.Is(err, ErrChanged) {
		t.Fatalf("Remove after a write = %v, want ErrChanged", err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("a file modified after the scan was deleted: %v", err)
	}
}

// Rule 5, age half: an entry that is no longer older than its category's
// cutoff must not be removed even if nothing else changed.
func TestRemoveRefusesWhenYoungerThanTheCutoff(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "sub", "file")
	writeFile(t, victim, "fresh")

	// Cutoff in the past: the file's mtime (now) is not before it.
	tgt := scanTarget(t, victim, time.Now().Add(-time.Hour))

	g := newTestGuard(t, root)
	if err := g.Remove(tgt, nil); !errors.Is(err, ErrChanged) {
		t.Fatalf("Remove of a too-young entry = %v, want ErrChanged", err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("a file younger than the cutoff was deleted: %v", err)
	}
}

// Rule 6: an open descriptor beats everything. Unlinking here would free no
// space at all while the writer keeps appending to a file with no name.
func TestRemoveRefusesWhenAProcessHoldsItOpen(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "sub", "syslog.1")
	writeFile(t, victim, "log line")
	tgt := scanTarget(t, victim, time.Time{})

	fdx := fakeFD{held: map[devIno]string{{tgt.Dev, tgt.Ino}: "pid 1 (rsyslogd)"}}

	g := newTestGuard(t, root)
	if err := g.Remove(tgt, fdx); !errors.Is(err, ErrOpenFD) {
		t.Fatalf("Remove of an open file = %v, want ErrOpenFD", err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("a file with an open descriptor was deleted: %v", err)
	}
	// And it goes when nobody holds it.
	if err := g.Remove(tgt, fakeFD{held: map[devIno]string{}}); err != nil {
		t.Fatalf("Remove of an idle file failed: %v", err)
	}
}

// A directory is removed with rmdir, never with a recursive delete, so it can
// never take unexpected content with it.
func TestRemoveDirectoryOnlyWhenEmpty(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tree", "leafdir")
	child := filepath.Join(dir, "unexpected")
	writeFile(t, child, "appeared after the scan")

	dirTarget := scanTarget(t, dir, time.Time{})
	g := newTestGuard(t, root)

	if err := g.Remove(dirTarget, nil); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rmdir of a non-empty directory = %v, want ErrNotEmpty", err)
	}
	if _, err := os.Lstat(child); err != nil {
		t.Fatalf("the unexpected child was deleted: %v", err)
	}

	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(dirTarget, nil); err != nil {
		t.Fatalf("rmdir of an empty directory failed: %v", err)
	}
}

// Kind is part of the identity: a file that became a symlink is a swap.
func TestRemoveRefusesWhenTheKindChanged(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "sub", "file")
	writeFile(t, victim, "original")
	tgt := scanTarget(t, victim, time.Time{})

	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/shadow", victim); err != nil {
		t.Fatal(err)
	}

	g := newTestGuard(t, root)
	if err := g.Remove(tgt, nil); !errors.Is(err, ErrChanged) {
		t.Fatalf("Remove of a swapped-in symlink = %v, want ErrChanged", err)
	}
}

// expandTree lists children before their parent, so an interruption leaves a
// partial but coherent state (rule 12).
func TestExpandTreeIsLeavesFirst(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "upgrade-20260808-163700")
	writeFile(t, filepath.Join(tree, "bin", "casanova"), "binary")
	writeFile(t, filepath.Join(tree, "update-report.json"), "{}")

	g := newTestGuard(t, root)
	targets, err := g.expandTree(tree, time.Time{}, newSizer(), newPlanLimits())
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for i, tg := range targets {
		seen[tg.Path] = i
	}
	if seen[tree] != len(targets)-1 {
		t.Fatalf("the container directory is not last: %v", targets)
	}
	if seen[filepath.Join(tree, "bin", "casanova")] > seen[filepath.Join(tree, "bin")] {
		t.Fatal("a file was ordered after the directory holding it")
	}
}

// Rule 18: a remote or unwalkable mount is refused, not walked. A dead NFS
// share answers no syscall at all, and hanging the handler forever is worse
// than reclaiming nothing.
func TestExpandTreeRefusesRemoteMounts(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	share := filepath.Join(tree, "share")
	mkdirAll(t, share)

	g, err := NewGuard([]string{root}, map[string]bool{share: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.expandTree(tree, time.Time{}, newSizer(), newPlanLimits()); !errors.Is(err, ErrRemoteMount) {
		t.Fatalf("expandTree crossed into a remote mount: %v", err)
	}
}

// Anything that is not a file, a symlink or a directory means we do not
// understand the tree, so we decline the WHOLE tree rather than the entry.
func TestExpandTreeRefusesUnknownEntryTypes(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	mkdirAll(t, tree)
	writeFile(t, filepath.Join(tree, "regular"), "fine")
	if err := unix.Mkfifo(filepath.Join(tree, "fifo"), 0o600); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}

	g := newTestGuard(t, root)
	if _, err := g.expandTree(tree, time.Time{}, newSizer(), newPlanLimits()); err == nil {
		t.Fatal("a tree containing a fifo was accepted")
	}
	if _, err := os.Lstat(filepath.Join(tree, "regular")); err != nil {
		t.Fatalf("the tree was touched despite being refused: %v", err)
	}
}

// Rule 17: a plan that grows past the hard caps is a bug in a detector, so it
// aborts instead of being trimmed.
func TestExpandTreeRespectsPlanLimits(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(tree, "f"+string(rune('a'+i))), "x")
	}
	g := newTestGuard(t, root)
	lim := &planLimits{MaxEntries: 3}
	if _, err := g.expandTree(tree, time.Time{}, newSizer(), lim); !errors.Is(err, ErrPlanTooLarge) {
		t.Fatalf("expandTree ignored its entry cap: %v", err)
	}
}

// A symlink is removed as itself: unlinking a link never touches its target.
// That is what lets the user clean up a planted link from the UI at all.
func TestRemoveUnlinksSymlinkWithoutTouchingTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	writeFile(t, filepath.Join(outside, "real"), "keep me")
	link := filepath.Join(root, "sub", "link")
	mkdirAll(t, filepath.Dir(link))
	if err := os.Symlink(filepath.Join(outside, "real"), link); err != nil {
		t.Fatal(err)
	}

	tgt := scanTarget(t, link, time.Time{})
	g := newTestGuard(t, root)
	if err := g.Remove(tgt, nil); err != nil {
		t.Fatalf("Remove of a symlink failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "real")); err != nil {
		t.Fatalf("unlinking the symlink removed its target: %v", err)
	}
}
