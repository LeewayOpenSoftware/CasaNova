package service

import (
	"os"
	"path/filepath"
	"testing"
)

// The maintenance busy-check reads the upload registry on EVERY clean, through
// readUploadSessions. A read must never mutate: an earlier version deleted the
// file when it failed to parse, so a momentarily corrupt (or concurrently
// half-written) registry got destroyed by a read-only status check — and a
// live upload's .tmp, no longer recorded, would be offered for deletion 48h
// later. This pins that readUploadSessions leaves the file untouched.
func TestReadUploadSessionsNeverMutates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upload-sessions.json")
	const corrupt = `{"id": this is not json`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	old := uploadSessionsFile
	uploadSessionsFile = path
	t.Cleanup(func() { uploadSessionsFile = old })

	sessions := readUploadSessions()
	if len(sessions) != 0 {
		t.Fatalf("a corrupt registry should read as empty, got %d entries", len(sessions))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readUploadSessions deleted the registry on a read path: %v", err)
	}
	if string(raw) != corrupt {
		t.Fatalf("readUploadSessions rewrote the registry: %q", string(raw))
	}
}

// Corruption recovery is legitimate, but it belongs to service start
// (loadUploadSessions) and it preserves the evidence rather than destroying it:
// the bad file is moved to <name>.corrupt, not unlinked.
func TestLoadUploadSessionsMovesCorruptAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upload-sessions.json")
	if err := os.WriteFile(path, []byte(`{"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}

	old := uploadSessionsFile
	uploadSessionsFile = path
	t.Cleanup(func() { uploadSessionsFile = old })

	sessions := loadUploadSessions()
	if len(sessions) != 0 {
		t.Fatalf("load should start fresh from a corrupt file, got %d entries", len(sessions))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the corrupt registry should have been renamed away, still at %s", path)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("the corrupt registry was not preserved as .corrupt: %v", err)
	}
}
