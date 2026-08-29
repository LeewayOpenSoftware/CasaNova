package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restore writes to the path recorded in the .trashinfo file, which is
// untrusted: this product runs arbitrary containers, and any container with a
// bind mount of a filesystem that has a trash dir can plant an info file whose
// Path= points anywhere. Without a destination validator a single restore lands
// a root-owned file at that path — container→host RCE past the blocklist that
// guards the JWT key and the user DB. These tests pin the two guarantees:
// Restore refuses when no validator is set, and honours the validator's verdict.

func plantTrashedEntry(t *testing.T, trashRoot, name, originalPath, payload string) *Entry {
	t.Helper()
	filesDir := filepath.Join(trashRoot, "files")
	infoDir := filepath.Join(trashRoot, "info")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, name), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	info := "[Trash Info]\nPath=" + originalPath + "\nDeletionDate=2026-01-01T00:00:00\n"
	if err := os.WriteFile(filepath.Join(infoDir, name+".trashinfo"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Entry{
		ID:           name,
		TrashedName:  name,
		OriginalPath: originalPath,
		trashRoot:    trashRoot,
	}
}

// A Manager with no DestValidator must fail closed: an unconfigured trash
// manager is not a write-anywhere primitive.
func TestRestoreRefusesWithoutValidator(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "escaped", "pwn")
	entry := plantTrashedEntry(t, filepath.Join(dir, ".Trash-casanova"), "pwn", target, "payload")

	m := &Manager{} // DestValidator nil
	if err := m.Restore(entry); err == nil {
		t.Fatal("Restore ran without a destination validator")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the payload was written to %s despite no validator", target)
	}
}

// The validator's refusal must stop the restore. Here it rejects anything
// outside the sandbox, standing in for the real blocklist that rejects /etc.
func TestRestoreHonoursValidatorRejection(t *testing.T) {
	dir := t.TempDir()
	sandbox := filepath.Join(dir, "data")
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(dir, "etc", "cron.d", "pwn") // the "system dir"
	entry := plantTrashedEntry(t, filepath.Join(sandbox, ".Trash-casanova"), "pwn", escape, "payload")

	m := &Manager{
		DestValidator: func(p string) (string, error) {
			if !strings.HasPrefix(p, sandbox+string(filepath.Separator)) {
				return "", os.ErrPermission
			}
			return p, nil
		},
	}
	if err := m.Restore(entry); err == nil {
		t.Fatal("Restore wrote to a path the validator rejected")
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("the payload escaped to %s", escape)
	}
}

// A legitimate restore inside the sandbox still works, and lands at the exact
// path the validator returned.
func TestRestoreAllowsValidatedDestination(t *testing.T) {
	dir := t.TempDir()
	sandbox := filepath.Join(dir, "data")
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(sandbox, "sub", "recovered.txt")
	entry := plantTrashedEntry(t, filepath.Join(sandbox, ".Trash-casanova"), "recovered.txt", dst, "hello")

	m := &Manager{
		DestValidator: func(p string) (string, error) { return p, nil },
	}
	if err := m.Restore(entry); err != nil {
		t.Fatalf("a valid restore failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("restored content = %q", string(got))
	}
}
