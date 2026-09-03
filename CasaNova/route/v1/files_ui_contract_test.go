package v1

// The contract between the Files UI and these two handlers.
//
// Both live bugs were contract breaks, not logic errors: the UI asked for
// something the server did not offer. These tests pin the exact request the
// shipped UI sends (CasaNova-UI-v2/src/api/files.ts) so a rename or an
// un-registered route cannot silently break it again.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/labstack/echo/v4"
)

// adminCtx builds a request context an administrator would produce.
func adminCtx(t *testing.T, method, target string, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, "admin", 1)
	return ctx, rec
}

// BUG 2, as the user hit it: "compress to zip" answered "format must be zip
// or targz". This is the exact body files.ts::compressFiles posts.
func TestCompressAcceptsTheFormatTheUISends(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "note.zip")

	body, _ := json.Marshal(map[string]any{
		"paths":       []string{src},
		"destination": dest,
		"format":      "zip",
	})
	ctx, rec := adminCtx(t, http.MethodPost, "/v1/file/compress", string(body))

	if err := PostFileCompress(ctx); err != nil {
		t.Fatalf("PostFileCompress returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "format must be") {
		t.Fatalf("the UI's own format was rejected: %s", rec.Body.String())
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the archive was not created: %v", err)
	}
}

// The formats the UI is allowed to ask for, per files.ts's own union type.
func TestCompressAcceptsEveryFormatTheUICanSend(t *testing.T) {
	for _, format := range []string{"zip", "targz"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "note.txt")
			if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{
				"paths":       []string{src},
				"destination": filepath.Join(dir, "out"),
				"format":      format,
			})
			ctx, rec := adminCtx(t, http.MethodPost, "/v1/file/compress", string(body))
			if err := PostFileCompress(ctx); err != nil {
				t.Fatalf("PostFileCompress: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("format %q rejected: %d %s", format, rec.Code, rec.Body.String())
			}
		})
	}
}

// BUG 1, as the user hit it: selecting several files and downloading gave 404.
// GetDownloadFile is the handler behind GET /v1/file/download; this asserts it
// actually serves a multi-file archive rather than refusing.
func TestDownloadServesAnArchiveForSeveralFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, rec := adminCtx(t, http.MethodGet,
		"/v1/file/download?format=zip&files="+a+","+b, "")
	if err := GetDownloadFile(ctx); err != nil {
		t.Fatalf("GetDownloadFile: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the archive body was empty")
	}
	// A zip's local file header. Proves an archive was streamed, not a JSON error.
	if got := rec.Body.String()[:2]; got != "PK" {
		t.Fatalf("body does not start with a zip signature: %q", got)
	}
}

// The download must name its attachment. This was set on the REQUEST headers,
// so the browser never saw it and saved the archive as "download".
func TestDownloadSetsContentDispositionOnTheResponse(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, rec := adminCtx(t, http.MethodGet,
		"/v1/file/download?format=zip&files="+a+","+b, "")
	if err := GetDownloadFile(ctx); err != nil {
		t.Fatalf("GetDownloadFile: %v", err)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition on the response = %q; the browser cannot name the file", cd)
	}
}
