package v1

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCreateBackupTarballReportsUnreadableDirectory(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "unreadable")
	if err := os.Mkdir(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	oldSources := backupSources
	oldWalk := backupWalk
	backupSources = []string{blocked}
	backupWalk = func(path string, walkFn filepath.WalkFunc) error {
		info, err := os.Stat(path)
		if err != nil {
			return walkFn(path, nil, err)
		}
		if info.IsDir() && info.Mode().Perm()&0o400 == 0 {
			return walkFn(path, nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES})
		}
		return filepath.Walk(path, walkFn)
	}
	t.Cleanup(func() {
		backupSources = oldSources
		backupWalk = oldWalk
	})

	dest := filepath.Join(root, "backup.tar.gz")
	result, err := createBackupTarball(dest, backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("expected one skipped path, got %#v", result.Skipped)
	}
	if result.Skipped[0].Path != blocked || !strings.Contains(result.Skipped[0].Reason, "permission denied") {
		t.Fatalf("unexpected skipped entry: %#v", result.Skipped)
	}
}
