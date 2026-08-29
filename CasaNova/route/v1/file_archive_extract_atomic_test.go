package v1

// Pins the all-or-nothing property of member extraction: a failure mid-copy
// must leave a pre-existing destination file exactly as it was. The previous
// implementation opened the destination with O_TRUNC before reading a single
// byte, so a corrupt archive or a cancel destroyed the old copy and left a
// partial in its place.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("stream died mid-member")
	}
	f.n--
	p[0] = 'X'
	return 1, nil
}

func TestExtractFailureLeavesExistingFileIntact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yml")
	original := "the old copy the user still needs"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := writeArchiveEntry(context.Background(), target, &failingReader{n: 3}, 0o644, 1<<20, nil)
	if err == nil {
		t.Fatal("the failing stream did not fail")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the original file is gone: %v", err)
	}
	if string(got) != original {
		t.Fatalf("original content destroyed by a failed extract: %q", got)
	}
	// And no temp litter left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".extract-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestExtractSuccessReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := writeArchiveEntry(context.Background(), target, io.LimitReader(strings.NewReader("new content"), 11), 0o600, 1<<20, nil)
	if err != nil || n != 11 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new content" {
		t.Fatalf("content = %q", got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestExtractOntoPlantedSymlinkRefuses(t *testing.T) {
	// The contract the wider suite already pins (see
	// TestExtractArchiveDoesNotWriteThroughExistingSymlink): a symlink
	// planted at the destination is refused loudly — neither written
	// through nor silently replaced.
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "member.txt")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}

	if _, err := writeArchiveEntry(context.Background(), target, strings.NewReader("payload"), 0o644, 1<<20, nil); err == nil {
		t.Fatal("a planted symlink at the destination was accepted")
	}
	if got, _ := os.ReadFile(victim); string(got) != "must survive" {
		t.Fatalf("the write went THROUGH the planted symlink: victim = %q", got)
	}
	if st, err := os.Lstat(target); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the planted symlink itself was destroyed; refusal must leave the scene untouched")
	}
}
