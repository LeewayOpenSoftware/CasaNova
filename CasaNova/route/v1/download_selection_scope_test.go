package v1

import (
	"archive/zip"
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A multi-item download archives each selected folder, but a symlink inside a
// selected folder must not pull in an unselected sibling: the containment root
// used to be the selection's common prefix, which also holds the siblings.
func TestDownloadArchiveDoesNotFollowSymlinkToUnselectedSibling(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	other := filepath.Join(dir, "other")
	for _, d := range []string{a, b, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		filepath.Join(a, "a.txt"):           "A",
		filepath.Join(b, "b.txt"):           "B",
		filepath.Join(other, "private.txt"): "PRIVATE",
		filepath.Join(a, "inner.txt"):       "INNER",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A link to an unselected sibling, and one that stays inside the selection.
	if err := os.Symlink(other, filepath.Join(a, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a, "inner.txt"), filepath.Join(a, "inner-link.txt")); err != nil {
		t.Fatal(err)
	}

	ctx, rec := adminCtx(t, http.MethodGet, "/v1/file/download?format=zip&files="+a+","+b, "")
	if err := GetDownloadFile(ctx); err != nil {
		t.Fatalf("GetDownloadFile: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("response is not a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	joined := strings.Join(names, "\n")
	if strings.Contains(joined, "private.txt") {
		t.Fatalf("archive carried a file from an unselected sibling through a symlink:\n%s", joined)
	}
	for _, want := range []string{"a/a.txt", "b/b.txt", "a/inner-link.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("archive is missing %s:\n%s", want, joined)
		}
	}
}
