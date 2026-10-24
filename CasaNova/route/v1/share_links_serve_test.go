package v1

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
)

// serveShare is reached by anonymous internet users. A shared folder's zip
// must contain only what is inside that folder (a symlink to a sibling is
// not part of the share), and a shared file that has since been swapped for
// a symlink is no longer what was shared.
func serveShareForTest(t *testing.T, link *ShareLink) *httptest.ResponseRecorder {
	t.Helper()
	old := shareLinksFile
	shareLinksFile = filepath.Join(t.TempDir(), "share-links.json")
	t.Cleanup(func() { shareLinksFile = old })

	rec := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/public/share/x", nil), rec)
	if err := serveShare(ctx, link); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestServeShareFolderDoesNotFollowSymlinkOutsideShare(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	private := filepath.Join(root, "private")
	for _, d := range []string{public, private} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(private, "payroll.csv"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(public, "innocent")); err != nil {
		t.Fatal(err)
	}

	rec := serveShareForTest(t, &ShareLink{ID: "x", Path: public, IsDir: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	sawReadme := false
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "payroll.csv" {
			t.Errorf("public share of %s served %s from outside the shared folder", public, f.Name)
		}
		if filepath.ToSlash(f.Name) == "public/readme.txt" {
			sawReadme = true
		}
	}
	if !sawReadme {
		t.Error("zip lost the shared folder's own content / top-level folder name")
	}
}

func TestServeShareRefusesFileSwappedForSymlink(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	shared := filepath.Join(root, "shared.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, shared); err != nil {
		t.Fatal(err)
	}

	rec := serveShareForTest(t, &ShareLink{ID: "x", Path: shared})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a shared path that is now a symlink", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("SECRET")) {
		t.Error("symlink target content was served")
	}
}
