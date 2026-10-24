package trash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidTrashedName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "/"} {
		if validTrashedName(bad) {
			t.Errorf("%q accepted as a trash entry name", bad)
		}
	}
	for _, ok := range []string{"a", "...", ".hidden", "file.txt", "a..b"} {
		if !validTrashedName(ok) {
			t.Errorf("%q refused as a trash entry name", ok)
		}
	}
}

// Every info/ dirent that trims to "", "." or ".." must be skipped by List,
// and Restore/Delete must refuse such an entry even if one is built by hand:
// joined onto files/, those names are the trash itself.
func TestTrashRefusesEntryNamesThatAreNotOneComponent(t *testing.T) {
	root := t.TempDir()
	trashRoot := filepath.Join(root, TrashDirName)
	filesDir := filepath.Join(trashRoot, "files")
	infoDir := filepath.Join(trashRoot, "info")
	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(filesDir, "someone-elses-file")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "someone-elses-file.trashinfo"),
		[]byte("[Trash Info]\nPath=/DATA/x\nDeletionDate=2000-01-01T00:00:00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{".trashinfo", "..trashinfo", "...trashinfo"} {
		if err := os.WriteFile(filepath.Join(infoDir, n),
			[]byte("[Trash Info]\nPath=/DATA/y\nDeletionDate=2000-01-01T00:00:00\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := NewManager()
	m.DestValidator = func(p string) (string, error) { return filepath.Join(root, "restored"), nil }
	entries, err := m.List([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !validTrashedName(e.TrashedName) {
			t.Errorf("List returned an entry named %q", e.TrashedName)
		}
	}
	if _, _, err := m.Purge([]string{root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filesDir); err != nil {
		t.Fatalf("purge removed the trash's files/ directory: %v", err)
	}

	for _, n := range []string{"", ".", ".."} {
		e := &Entry{trashRoot: trashRoot, TrashedName: n, OriginalPath: "/DATA/z"}
		if err := m.Delete(e); err == nil {
			t.Errorf("Delete accepted entry name %q", n)
		}
		if err := m.Restore(e); err == nil {
			t.Errorf("Restore accepted entry name %q", n)
		}
	}
	if _, err := os.Stat(trashRoot); err != nil {
		t.Fatalf("the trash root is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "restored")); err == nil {
		t.Fatal("Restore moved the trash itself to the destination")
	}
}
