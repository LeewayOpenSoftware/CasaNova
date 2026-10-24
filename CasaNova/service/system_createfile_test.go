package service

// Regression tests for CreateFile's honesty about failure.
//
// The middle branch used to be:
//
//	if os.IsNotExist(err) {
//	    file.CreateFile(path)      // error discarded
//	    return common_err.SUCCESS, nil
//	}
//
// so every reason a create can fail — a missing parent, a read-only mount,
// ENOSPC, a permission os.Stat could not predict — was reported to the caller
// as SUCCESS, for a file that was never written. The handler above it then
// answered HTTP 200. Nothing logged it, so the only symptom was some later
// operation failing on a file the client had been told existed.
//
// These assert the outcome the caller can observe, not that an error was
// wrapped a particular way: a create that did not happen must not be reported
// as one that did.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
)

func TestCreateFileReportsFailureWhenParentIsMissing(t *testing.T) {
	c := &systemService{}
	// No mkdir: the parent genuinely does not exist, which is the single most
	// likely way this fails in production (a client composing a path under a
	// folder it assumed was there).
	target := filepath.Join(t.TempDir(), "no-such-dir", "note.md")

	code, err := c.CreateFile(target)

	if code == common_err.SUCCESS {
		t.Fatalf("reported SUCCESS for a file that was never created (path %q)", target)
	}
	if code != common_err.SERVICE_ERROR {
		t.Fatalf("code = %d, want SERVICE_ERROR (%d)", code, common_err.SERVICE_ERROR)
	}
	if err == nil {
		t.Fatal("returned a nil error alongside SERVICE_ERROR: the caller has nothing to log or show")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("file exists after a failed create: %v", statErr)
	}
}

func TestCreateFileReportsFailureWhenParentIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		// root ignores the mode bits, so this particular failure cannot be
		// staged. Skipping is honest; asserting a pass here would not be.
		t.Skip("running as root: a 0500 directory does not deny root")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(dir, "note.md")

	code, err := c().CreateFile(target)

	if code == common_err.SUCCESS {
		t.Fatalf("reported SUCCESS for a create the filesystem refused (path %q)", target)
	}
	if err == nil {
		t.Fatal("no error returned for a refused create")
	}
}

func TestCreateFileStillSucceedsAndReportsCollision(t *testing.T) {
	// The two branches that must keep working, so the fix above cannot pass by
	// turning every create into a failure.
	dir := t.TempDir()
	target := filepath.Join(dir, "note.md")

	if code, err := c().CreateFile(target); code != common_err.SUCCESS || err != nil {
		t.Fatalf("first create: code = %d, err = %v; want SUCCESS, nil", code, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("file missing after a reported success: %v", err)
	}
	if code, err := c().CreateFile(target); code != common_err.FILE_OR_DIR_EXISTS || err != nil {
		t.Fatalf("second create: code = %d, err = %v; want FILE_OR_DIR_EXISTS, nil", code, err)
	}
}

func c() *systemService { return &systemService{} }

// os.Stat reports a dangling symlink as missing, and os.Create used to follow
// it: a link planted in storage made CreateFile write wherever it pointed,
// outside the ceiling the caller's path check had just approved.
func TestCreateFileDoesNotFollowDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "authorized_keys")
	link := filepath.Join(dir, "notes.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	code, err := c().CreateFile(link)

	if code == common_err.SUCCESS {
		t.Errorf("reported SUCCESS creating through a dangling symlink (err %v)", err)
	}
	if _, statErr := os.Lstat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("CreateFile created the symlink's target %s: %v", outside, statErr)
	}
}
