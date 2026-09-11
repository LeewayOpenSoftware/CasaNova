package v1

// The SMB toggle used to destroy every share definition it could not read.
//
// loadSmbConfig returns a DEFAULT config — Workgroup "WORKGROUP", Shares an
// EMPTY slice — ALONGSIDE its error. PostSmbToggle read it as `cfg, _ :=`,
// set cfg.Enabled, and saved. So a smb_shares.json that merely failed to
// parse (a partial write from power loss is an ordinary way for a NAS to get
// one) was overwritten with an empty share list, and the handler answered
// SUCCESS. Nothing in the response or the logs said anything had been lost.
//
// The three sibling handlers (PostSmbShare, PutSmbShare, DeleteSmbShare) all
// checked this error already; the toggle was the lone outlier.

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

func smbTestContext(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/sys/smb/toggle", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)
	return ctx, rec
}

// pointSmbConfigAt redirects the share store at a temp file for the test.
func pointSmbConfigAt(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "smb_shares.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := smbConfigPath
	smbConfigPath = path
	prevLook := smbdLookPath
	smbdLookPath = func() error { return nil } // pretend Samba is installed
	t.Cleanup(func() { smbConfigPath = prev; smbdLookPath = prevLook })
	return path
}

func TestSmbToggleRefusesToSaveOverAConfigItCannotRead(t *testing.T) {
	// A truncated write: valid prefix, no valid JSON.
	const corrupt = `{"enabled":true,"workgroup":"WORKGROUP","shares":[{"name":"fotos","pa`
	path := pointSmbConfigAt(t, corrupt)

	ctx, rec := smbTestContext(t, `{"enabled":false}`)
	if err := PostSmbToggle(ctx); err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("an unreadable config must be refused, got HTTP %d", rec.Code)
	}

	// The heart of it: the file is untouched. Before the fix this had been
	// replaced with an empty share list and the user was told SUCCESS.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != corrupt {
		t.Fatalf("the toggle wrote over a config it could not read.\n before: %s\n after:  %s", corrupt, string(after))
	}
}

// TestSmbTogglePositiveControl proves the same fixture DOES persist a toggle
// when the config is readable — otherwise the test above would pass just as
// well against a handler that refuses everything.
func TestSmbTogglePositiveControl(t *testing.T) {
	const valid = `{"enabled":true,"workgroup":"WORKGROUP","shares":[{"name":"fotos","path":"/DATA/fotos"}]}`
	path := pointSmbConfigAt(t, valid)

	// enabled:false takes the branch that does not rewrite /etc/samba.
	ctx, rec := smbTestContext(t, `{"enabled":false}`)
	if err := PostSmbToggle(ctx); err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("a readable config must toggle, got HTTP %d: %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got SmbConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if got.Enabled {
		t.Error("the toggle did not persist enabled=false")
	}
	if len(got.Shares) != 1 || got.Shares[0].Name != "fotos" {
		t.Fatalf("the share was lost on a NORMAL toggle: %+v", got.Shares)
	}
}
