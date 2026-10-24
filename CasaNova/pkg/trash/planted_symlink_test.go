package trash

// The trash root lives on user-writable storage (/DATA, USB disks), so a
// container or SMB client can plant .Trash-casanova/files as a symlink to a
// host directory plus a matching old .trashinfo. List used to follow the
// link and Purge (the daily janitor) then os.RemoveAll'd through it, deleting
// outside the trash. Trash() likewise created its dirs through such a link.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func plantFilesSymlink(t *testing.T, root, target string) {
	t.Helper()
	trashRoot := filepath.Join(root, TrashDirName)
	if err := os.MkdirAll(filepath.Join(trashRoot, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(trashRoot, "files")); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeDoesNotFollowPlantedFilesSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "db")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	plantFilesSymlink(t, root, outside)
	old := time.Now().Add(-90 * 24 * time.Hour).Format("2006-01-02T15:04:05")
	info := "[Trash Info]\nPath=/DATA/whatever\nDeletionDate=" + old + "\n"
	if err := os.WriteFile(filepath.Join(root, TrashDirName, "info", "db.trashinfo"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("List returned %d entries from a trash root whose files/ is a symlink", len(entries))
	}
	if _, _, err := m.Purge([]string{root}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Empty([]string{root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("purge/empty deleted %s outside the trash through a planted symlink: %v", victim, err)
	}
}

func TestTrashRefusesPlantedFilesSymlink(t *testing.T) {
	m, root := trashManagerForTest(t)
	outside := t.TempDir()
	plantFilesSymlink(t, root, outside)

	src := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Trash(src); err == nil {
		t.Error("Trash moved a file into a trash whose files/ is a symlink")
	}
	if _, err := os.Lstat(filepath.Join(outside, "doc.txt")); err == nil {
		t.Error("file landed outside the trash through the planted symlink")
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("source should be untouched: %v", err)
	}
}
