package service

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixtureTasksJSON(t *testing.T, base, content string) string {
	t.Helper()
	path := filepath.Join(base, "jobs", "tasks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("failed to create fixture jobs dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write fixture tasks.json: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}

func TestMigrateLegacyTasksStore_DifferentBases_CopiesOldToNew(t *testing.T) {
	oldBase := t.TempDir()
	newBase := t.TempDir()
	writeFixtureTasksJSON(t, oldBase, `{"tasks":{"t-1":{}}}`)

	MigrateLegacyTasksStore(oldBase, newBase)

	newPath := filepath.Join(newBase, "jobs", "tasks.json")
	if got := readFile(t, newPath); got != `{"tasks":{"t-1":{}}}` {
		t.Fatalf("migrated content = %q, want the old store's content copied verbatim", got)
	}
}

func TestMigrateLegacyTasksStore_SameBase_SkipsEntirely(t *testing.T) {
	sharedBase := t.TempDir()
	sharedPath := writeFixtureTasksJSON(t, sharedBase, `{"tasks":{"already-here":{}}}`)

	// oldBase == newBase: e.g. an operator's RuntimePath override already
	// equals the new persistent default. Nothing should be touched.
	MigrateLegacyTasksStore(sharedBase, sharedBase)

	if got := readFile(t, sharedPath); got != `{"tasks":{"already-here":{}}}` {
		t.Fatalf("shared store content = %q, want it untouched", got)
	}
}

func TestMigrateLegacyTasksStore_NewAlreadyHasFile_NeverOverwritten(t *testing.T) {
	oldBase := t.TempDir()
	newBase := t.TempDir()
	writeFixtureTasksJSON(t, oldBase, `{"tasks":{"legacy":{}}}`)
	newPath := writeFixtureTasksJSON(t, newBase, `{"tasks":{"current":{}}}`)

	MigrateLegacyTasksStore(oldBase, newBase)

	if got := readFile(t, newPath); got != `{"tasks":{"current":{}}}` {
		t.Fatalf("new store content = %q, want it left exactly as it was (never overwritten)", got)
	}
}

func TestMigrateLegacyTasksStore_NoLegacyFile_SilentNoOp(t *testing.T) {
	oldBase := t.TempDir()
	newBase := t.TempDir()

	MigrateLegacyTasksStore(oldBase, newBase)

	newPath := filepath.Join(newBase, "jobs", "tasks.json")
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("expected no file to be created at %s when there is no legacy store, stat err = %v", newPath, err)
	}
}
