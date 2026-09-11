package v1

// PutUPSConfig used to persist via a plain os.WriteFile straight onto
// upsConfigFile: open-with-O_TRUNC + write, no temp file, no rename. Same
// ATOMICITY bug class already centralized away in commonfile.WriteFileAtomic
// (see its doc comment) and already adopted by smb.go, certs.go, totp.go —
// host_ups.go was the one call site still reinventing a plain write. Two
// concrete, testable consequences of that:
//
//  1. os.WriteFile truncates the DESTINATION file in place before writing,
//     so any failure mid-write (or a crash before the page cache flushes)
//     can leave upsConfigFile corrupt or empty — and loadUPSConfig silently
//     swallows the resulting json.Unmarshal error, so UPS monitoring would
//     silently stop with nothing surfaced anywhere.
//  2. os.WriteFile with O_TRUNC needs WRITE PERMISSION ON THE EXISTING FILE
//     itself. WriteFileAtomic instead writes a temp file and renames over
//     the destination, which only needs write permission on the DIRECTORY —
//     so persistence no longer depends on the destination's own mode bits.
//     That is a genuine, observable behavior difference the fix introduces.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

func upsConfigTestContext(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/sys/host/ups/config", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)
	return ctx, rec
}

func pointUPSConfigAt(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ups-config.json")
	prev := upsConfigFile
	upsConfigFile = path
	t.Cleanup(func() { upsConfigFile = prev })
	return path
}

func TestPutUPSConfigSurvivesAReadOnlyDestinationFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permission bits do not restrict root")
	}
	dir := t.TempDir()
	path := pointUPSConfigAt(t, dir)
	if err := os.WriteFile(path, []byte(`{"host":"old","upsName":""}`), 0o400); err != nil {
		t.Fatal(err)
	}

	ctx, rec := upsConfigTestContext(t, `{"host":"192.168.3.214","upsName":"ups"}`)
	if err := PutUPSConfig(ctx); err != nil {
		t.Fatalf("PutUPSConfig returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	var got UPSConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v (content: %q)", err, raw)
	}
	if got.Host != "192.168.3.214" || got.UPSName != "ups" {
		t.Fatalf("saved config = %+v, want host=192.168.3.214 upsName=ups", got)
	}
}

func TestPutUPSConfigLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	pointUPSConfigAt(t, dir)

	ctx, rec := upsConfigTestContext(t, `{"host":"","upsName":""}`)
	if err := PutUPSConfig(ctx); err != nil {
		t.Fatalf("PutUPSConfig returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.casanova-tmp"))
	if len(matches) != 0 {
		t.Fatalf("leftover temp file(s): %v", matches)
	}
}
