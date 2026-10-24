package file

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A public share of a folder archives it with its parent as commonPath (so
// the zip keeps the folder name). Symlink containment must be the shared
// folder itself: a link inside the share pointing to a SIBLING of it is
// outside what was shared and must not be followed.
func TestAddFileWithinDoesNotFollowSymlinkToSibling(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	private := filepath.Join(root, "private")
	for _, d := range []string{filepath.Join(public, "sub"), private} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(private, "payroll.csv"), "SECRET")
	mustWrite(filepath.Join(public, "readme.txt"), "hello")
	mustWrite(filepath.Join(public, "sub", "inner.txt"), "inner")
	if err := os.Symlink(private, filepath.Join(public, "innocent")); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the share is still followed.
	if err := os.Symlink(filepath.Join(public, "sub"), filepath.Join(public, "alias")); err != nil {
		t.Fatal(err)
	}

	_, ar, err := GetCompressionAlgorithm("zip")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ar.Create(&buf); err != nil {
		t.Fatal(err)
	}
	if err := AddFileWithin(ar, public, filepath.Dir(public), public); err != nil {
		t.Fatal(err)
	}
	ar.Close()

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range zr.File {
		got[filepath.ToSlash(f.Name)] = true
	}
	for _, n := range []string{"innocent/payroll.csv", "public/innocent/payroll.csv"} {
		if got[n] {
			t.Errorf("archive contains %s from outside the shared folder: %v", n, got)
		}
	}
	for _, n := range []string{"public/readme.txt", "public/sub/inner.txt", "public/alias/inner.txt"} {
		if !got[n] {
			t.Errorf("archive is missing %s (folder name / in-share content must be kept): %v", n, got)
		}
	}
}
