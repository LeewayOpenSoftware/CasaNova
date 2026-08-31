package file

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory must be rejected with an error, not a panic, and the fd opened
// for the check must not leak (the leak is only observable indirectly, but the
// error path itself is what regressed: Stat's error was ignored and a nil
// FileInfo was dereferenced).
func TestNewReadLineFromEndRejectsDirectory(t *testing.T) {
	rd, err := NewReadLineFromEnd(t.TempDir())
	if err == nil {
		rd.Close()
		t.Fatal("expected an error for a directory, got nil")
	}
}

func TestNewReadLineFromEndMissingFile(t *testing.T) {
	_, err := NewReadLineFromEnd(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for a missing file, got nil")
	}
}

func TestNewReadLineFromEndReadsLastLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(p, []byte("first\nsecond\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := NewReadLineFromEnd(p)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	line, err := rd.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(line); got != "last\n" {
		t.Fatalf("expected %q, got %q", "last\n", got)
	}
}
