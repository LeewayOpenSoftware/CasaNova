package trash

// Found while reviewing the trash-symlink fix of 26.10.1:
// checkTrashDirs defends trashRoot/files/info against being SYMLINKS, but
// never validates that an individual entry's TrashedName — once joined onto
// "files" or "info" — stays a single, contained path component. A dirent in
// info/ literally named "...trashinfo" (three dots then the suffix; legal on
// ext4/btrfs: not "." and not ".." themselves) survives
// strings.TrimSuffix(name, ".trashinfo") as exactly "..". List() happily
// returns an Entry with TrashedName == "..", and both Delete() and the
// deleteAll() loop behind Empty()/Purge() then compute
// filepath.Join(trashRoot, "files", "..") == trashRoot itself and
// os.RemoveAll() it — wiping every OTHER user's trashed files (and the trash
// directory structure) on that mountpoint. No symlink is ever touched, so
// checkTrashDirs never triggers; this is a DIFFERENT path to the same sink
// the fix's own docstring describes ("daily janitor deletes outside the
// trash... refuse the whole root instead"). Reachable precisely under the
// fix's own stated threat model: a container or SMB client with write access
// to the trash root on user-writable storage (/DATA) plants the file; ANY
// subsequent Delete-by-ID of that entry, OR a normal "empty trash" click
// (Empty/Purge iterate every entry, including this one), triggers it.
//
// The symlink fix alone did not close this; this test is the red proof for
// the follow-up that requires a TrashedName to be exactly one path component.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteRefusesDotDotTrashedName(t *testing.T) {
	root := t.TempDir()
	trashRoot := filepath.Join(root, TrashDirName)
	filesDir := filepath.Join(trashRoot, "files")
	infoDir := filepath.Join(trashRoot, "info")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A legitimate, unrelated entry already in the trash (stands in for
	// "every other user's trashed files on this mountpoint").
	victimName := "someone-elses-photo.jpg"
	if err := os.WriteFile(filepath.Join(filesDir, victimName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, victimName+".trashinfo"),
		[]byte("[Trash Info]\nPath=/DATA/"+victimName+"\nDeletionDate=2026-01-01T00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The planted entry. No symlink anywhere — just a crafted dirent name.
	plantedInfoName := "...trashinfo"
	if err := os.WriteFile(filepath.Join(infoDir, plantedInfoName),
		[]byte("[Trash Info]\nPath=/DATA/whatever\nDeletionDate=2026-01-01T00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	var planted *Entry
	for i := range entries {
		if entries[i].TrashedName == ".." {
			planted = &entries[i]
		}
	}
	if planted == nil {
		// List no longer surfaces it (26.10.1 entry-name fix). Delete must
		// still refuse the same entry if one is ever built some other way, so
		// the rest of the test runs against a hand-built ".." entry instead of
		// skipping.
		planted = &Entry{trashRoot: trashRoot, TrashedName: "..", ID: ".."}
	}

	// EXPECTED (safe) behaviour: Delete refuses a traversal-shaped
	// TrashedName instead of acting on it.
	if err := m.Delete(planted); err == nil {
		t.Error("Delete() accepted a \"..\" TrashedName instead of refusing it")
	}

	// Whether or not Delete returned an error, the victim's trashed file and
	// the files/ directory itself must still be there afterwards.
	if _, err := os.Lstat(filepath.Join(filesDir, victimName)); err != nil {
		t.Errorf("victim's trashed file was destroyed: %v", err)
	}
	if _, err := os.Lstat(filesDir); err != nil {
		t.Errorf("files/ directory itself was destroyed: %v", err)
	}
}
