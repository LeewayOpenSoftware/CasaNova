package v1

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateLogOldRemovedRecentSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateLog(path, 1024, 24*time.Hour, false)
	if err != nil || !rotated {
		t.Fatalf("rotate old log: rotated=%v err=%v", rotated, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old active log still exists: %v", err)
	}
	data, err := os.ReadFile(path + ".1")
	if err != nil || string(data) != "old" {
		t.Fatalf("rotated old content lost: %q %v", data, err)
	}

	if err := os.WriteFile(path, []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err = rotateLog(path, 1024, 24*time.Hour, false)
	if err != nil || rotated {
		t.Fatalf("recent log rotated: rotated=%v err=%v", rotated, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "recent" {
		t.Fatalf("recent content did not survive: %q %v", data, err)
	}
}

func TestRotateLogDoesNotTouchActiveOrUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("active"), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateLog(path, 1, time.Nanosecond, true)
	if err != nil || rotated {
		t.Fatalf("active log rotated: rotated=%v err=%v", rotated, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// A dangling link represents an indeterminate stat: do not treat the
	// missing target as an ancient log and remove/rename anything.
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	rotated, err = rotateLog(path, 1, time.Nanosecond, false)
	if err != nil || rotated {
		t.Fatalf("indeterminate log rotated: rotated=%v err=%v", rotated, err)
	}
}

func TestLogRotationConfig(t *testing.T) {
	t.Setenv("CASANOVA_TEST_MAX_BYTES", "123")
	t.Setenv("CASANOVA_TEST_RETENTION_DAYS", "7")
	bytes, age := logRotationConfig("CASANOVA_TEST")
	if bytes != 123 || age != 7*24*time.Hour {
		t.Fatalf("configured retention: %d %s", bytes, age)
	}
	t.Setenv("CASANOVA_TEST_MAX_BYTES", "bad")
	t.Setenv("CASANOVA_TEST_RETENTION_DAYS", "0")
	bytes, age = logRotationConfig("CASANOVA_TEST")
	if bytes != defaultLogMaxBytes || age != defaultLogMaxAge {
		t.Fatalf("invalid retention did not use defaults: %d %s", bytes, age)
	}
}
