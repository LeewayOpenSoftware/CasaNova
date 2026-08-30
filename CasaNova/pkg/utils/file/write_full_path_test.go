package file

// WriteToFullPath's contract after the CreateTemp switch:
//
//   1. the caller's mode survives (PutFileContent passes the existing file's
//      mode; CreateTemp's forced 0600 must not leak through), and the
//      &^0o022 masking still applies;
//   2. a symlink PRE-PLANTED at the predictable "<dest>.tmp" name no longer
//      captures the write — that was a root arbitrary-file-write — and a
//      symlink AT the destination is replaced, not written through;
//   3. concurrent writers to one destination never publish a torn file:
//      the destination always holds exactly one writer's complete payload.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteToFullPathPreservesCallerMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "script.sh")

	if err := WriteToFullPath([]byte("#!/bin/sh\necho v1\n"), dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode after create = %v, want 0755", st.Mode().Perm())
	}
	// The rewrite path is the one that matters: CreateTemp makes a 0600 file,
	// so a missing Chmod would silently strip +x on every edit.
	if err := WriteToFullPath([]byte("#!/bin/sh\necho v2\n"), dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode after rewrite = %v, want 0755 — the edit stripped the executable bit", st.Mode().Perm())
	}
}

func TestWriteToFullPathDropsGroupAndWorldWrite(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f")
	if err := WriteToFullPath([]byte("x"), dest, 0o666); err != nil {
		t.Fatal(err)
	}
	// The &^0o022 masking is part of the contract (0666 → 0644).
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644 (group/world write masked)", st.Mode().Perm())
	}
}

func TestWriteToFullPathIgnoresPlantedTmpSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode-based protections do not apply")
	}
	dir := t.TempDir()
	// The attacker plants a symlink at the OLD predictable temp name,
	// pointing at a file outside. With the fixed ".tmp" name and no O_EXCL,
	// the write followed it — an arbitrary-file-write as root. The random
	// CreateTemp name is unguessable, so the plant catches nothing.
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if err := os.Symlink(victim, dest+".tmp"); err != nil {
		t.Fatal(err)
	}

	if err := WriteToFullPath([]byte("payload"), dest, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "original" {
		t.Fatalf("the write hit the planted .tmp symlink's target: victim = %q", got)
	}
}

func TestWriteToFullPathReplacesSymlinkRatherThanWritingThrough(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if err := os.Symlink(victim, dest); err != nil {
		t.Fatal(err)
	}

	if err := WriteToFullPath([]byte("new"), dest, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "must survive" {
		t.Fatalf("the write followed the symlink to its target: victim = %q", got)
	}
	if st, _ := os.Lstat(dest); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("dest is still a symlink; the write did not replace it")
	}
}

func TestWriteToFullPathConcurrentWritersNeverPublishTornFile(t *testing.T) {
	// The fixed ".tmp" name let two concurrent writers share one temp file
	// and publish each other's half-written bytes. Random temp names isolate
	// them: the destination always holds exactly one writer's full payload.
	dir := t.TempDir()
	dest := filepath.Join(dir, "shared")
	a := []byte(strings.Repeat("A", 256*1024))
	b := []byte(strings.Repeat("B", 256*1024))

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = WriteToFullPath(a, dest, 0o644) }()
		go func() { defer wg.Done(); _ = WriteToFullPath(b, dest, 0o644) }()
	}
	wg.Wait()

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 256*1024 {
		t.Fatalf("torn file: %d bytes, want %d — a writer published a partial", len(got), 256*1024)
	}
	// Every byte identical → it is exactly one writer's payload, not a splice.
	if n := strings.Count(string(got), string(got[0])); n != len(got) {
		t.Fatalf("mixed content: %d of %d bytes match the first — two writers' bytes interleaved", n, len(got))
	}
	// No leftover temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "shared" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}
