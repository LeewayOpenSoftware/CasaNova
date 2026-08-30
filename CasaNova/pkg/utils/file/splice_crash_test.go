package file

// The chunks of a chunked upload are its resume state: as long as they
// exist, an interrupted assembly can be retried without re-uploading a
// byte. These tests pin the property that made the bug a bug — no chunk is
// deleted until the assembled file is safely in place — plus the happy path
// that keeps the fix honest.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeChunks(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), []byte{byte('a' + i - 1)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpliceKeepsChunksWhenPublishFails(t *testing.T) {
	chunkDir := t.TempDir()
	writeChunks(t, chunkDir, 3)

	// The failure must land at the LAST step — the publishing rename — with
	// every earlier step succeeding, because that is the window where the
	// old code had already destroyed the chunks. A directory planted at the
	// exact destination path stages it: the .tmp writes fine beside it, the
	// loop consumes all chunks, Sync succeeds, and the final rename fails
	// (cannot rename a file over an existing directory).
	destDir := t.TempDir()
	dest := filepath.Join(destDir, "ensamblado.bin")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	err := SpliceFiles(chunkDir, dest, 3, 1)
	if err == nil {
		t.Fatal("splice onto a directory did not error")
	}
	for i := 1; i <= 3; i++ {
		if _, statErr := os.Stat(filepath.Join(chunkDir, strconv.Itoa(i))); statErr != nil {
			t.Fatalf("chunk %d is gone after a FAILED splice: the upload's resume state was destroyed "+
				"along with the result — the user must now re-upload from byte zero", i)
		}
	}
}

func TestSpliceHappyPathAssemblesAndCleans(t *testing.T) {
	chunkDir := t.TempDir()
	writeChunks(t, chunkDir, 3)
	dest := filepath.Join(t.TempDir(), "ensamblado.bin")

	if err := SpliceFiles(chunkDir, dest, 3, 1); err != nil {
		t.Fatalf("splice: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "abc" {
		t.Fatalf("assembled = %q, err %v; want abc", got, err)
	}
	// Success MUST clean the chunks — the fix moved the deletion later, not
	// out of existence; leaving them would double the disk cost of every
	// completed upload forever.
	for i := 1; i <= 3; i++ {
		if _, statErr := os.Stat(filepath.Join(chunkDir, strconv.Itoa(i))); !os.IsNotExist(statErr) {
			t.Fatalf("chunk %d survived a successful splice", i)
		}
	}
}
