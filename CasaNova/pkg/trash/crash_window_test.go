package trash

// Crash-consistency of the two-step trash transaction.
//
// The two steps (metadata, rename) cannot be atomic together, so the design
// choice is WHICH side of the gap a crash lands on. These tests stand inside
// the gap via trashCrashWindowHook and pin the whole contract:
//
//   1. inside the window, the metadata exists and the file has NOT moved —
//      the order itself, the thing a refactor would silently flip back;
//   2. the residue a crash leaves (info without file) is detected by List,
//      skipped, and cleaned — never shown as a restorable phantom;
//   3. a rename failure compensates by removing the info it just wrote.
//
// Under the OLD order (rename first) the crash residue was a file in
// files/ with no .trashinfo: invisible to List forever, unrestorable, and
// silent. Test 1 fails loudly if anyone restores that order.

import (
	"os"
	"path/filepath"
	"testing"
)

func trashManagerForTest(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	prev := findMountpointFn
	findMountpointFn = func(string) (string, error) { return root, nil }
	t.Cleanup(func() { findMountpointFn = prev })
	m := NewManager()
	return m, root
}

func TestCrashWindowLandsOnTheRecoverableSide(t *testing.T) {
	m, root := trashManagerForTest(t)
	victim := filepath.Join(root, "documento.txt")
	if err := os.WriteFile(victim, []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawInfo, fileStillHome bool
	trashCrashWindowHook = func() {
		// This is the crash instant. What state is on disk?
		matches, _ := filepath.Glob(filepath.Join(root, "*", "info", "*.trashinfo"))
		sawInfo = len(matches) > 0
		_, err := os.Lstat(victim)
		fileStillHome = err == nil
	}
	defer func() { trashCrashWindowHook = nil }()

	if _, err := m.Trash(victim); err != nil {
		t.Fatalf("move: %v", err)
	}
	if !sawInfo {
		t.Fatal("inside the crash window the .trashinfo did not exist yet: the rename is happening FIRST, " +
			"and a crash there strands the file in files/ with no metadata — invisible and unrestorable")
	}
	if !fileStillHome {
		t.Fatal("inside the crash window the file had already moved: a crash there loses it")
	}
}

func TestListCleansTheCrashResidue(t *testing.T) {
	m, root := trashManagerForTest(t)
	victim := filepath.Join(root, "nota.md")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Manufacture the exact crash residue: run the transaction but "crash"
	// (skip the rename) by making the rename target unreachable mid-flight.
	// Simpler and more honest: move to trash normally, then recreate the
	// residue by deleting the trashed FILE while keeping its info.
	entry, err := m.Trash(victim)
	if err != nil {
		t.Fatal(err)
	}
	trashedFile := filepath.Join(entry.trashRoot, "files", entry.TrashedName)
	if err := os.Remove(trashedFile); err != nil {
		t.Fatal(err)
	}
	infoPath := filepath.Join(entry.trashRoot, "info", entry.TrashedName+".trashinfo")
	if _, err := os.Stat(infoPath); err != nil {
		t.Fatalf("setup: info missing: %v", err)
	}

	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.TrashedName == entry.TrashedName {
			t.Fatal("List offered a phantom: an entry whose file does not exist, whose Restore must fail")
		}
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatal("the stale .trashinfo was not cleaned up; it will reappear as a phantom on every listing")
	}
}

func TestRenameFailureRemovesTheInfoItWrote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only files/ dir does not refuse root")
	}
	m, root := trashManagerForTest(t)
	victim := filepath.Join(root, "config.json")
	if err := os.WriteFile(victim, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force the rename to fail: files/ exists but cannot receive entries.
	trashCrashWindowHook = func() {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "files"))
		for _, d := range matches {
			os.Chmod(d, 0o500)
		}
	}
	defer func() {
		trashCrashWindowHook = nil
		matches, _ := filepath.Glob(filepath.Join(root, "*", "files"))
		for _, d := range matches {
			os.Chmod(d, 0o755)
		}
	}()

	if _, err := m.Trash(victim); err == nil {
		t.Fatal("the impossible rename did not error")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("the original file is gone after a failed trash: %v", err)
	}
	leftover, _ := filepath.Glob(filepath.Join(root, "*", "info", "*.trashinfo"))
	if len(leftover) != 0 {
		t.Fatalf("stale .trashinfo left behind after compensation: %v", leftover)
	}
}
