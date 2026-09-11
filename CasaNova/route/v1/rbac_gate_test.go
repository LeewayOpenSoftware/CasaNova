package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// The gate helpers must RETURN their refusal, never write it.
//
// ctx.JSON reports the serializer's error, which is nil once the bytes are on
// the wire. A helper that denied with `return ctx.JSON(403, …)` therefore
// handed its caller a nil error, and every call site is written as
// `if err := requirePerm(…); err != nil { return err }` — so the deny path did
// nothing but commit a 403 while the handler ran to completion. Confirmed on
// the live box before the fix: an unauthenticated DELETE /v1/batch answered
// 403 and moved the file to the recycle bin anyway.
func TestRequirePermAbortsTheHandler(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	// No role in the context resolves to guest, which lacks PermFileDelete.
	ctx := e.NewContext(httptest.NewRequest(http.MethodDelete, "/v1/batch", nil), rec)

	sideEffect := false
	handler := func(c echo.Context) error {
		if err := requirePerm(c, rbac.PermFileDelete); err != nil {
			return err
		}
		sideEffect = true
		return c.NoContent(http.StatusOK)
	}

	err := handler(ctx)
	if err == nil {
		t.Fatal("requirePerm returned nil on the deny path; the handler would continue")
	}
	if sideEffect {
		t.Fatal("the handler ran its side effect despite being denied")
	}

	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("want *echo.HTTPError, got %T", err)
	}
	if he.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", he.Code)
	}
	// Nothing may be committed by the helper: the error handler owns the body.
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("helper wrote the response itself (code=%d, body=%q)", rec.Code, rec.Body.String())
	}
}

// A permission the role does hold must let the handler through untouched.
//
// The context carries a real principal (a signed-in guest ACCOUNT, uid 7). It
// used to be a bare context with nothing set, which passed only because an
// absent principal fell back to RoleGuest and borrowed its permissions — so
// the test was asserting the fail-open default rather than the allow path. A
// guest account is still allowed; only the ABSENT principal is refused.
func TestRequirePermAllowsWhenPermitted(t *testing.T) {
	e := echo.New()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/app/list", nil), httptest.NewRecorder())
	ctx.Set(commonmw.ContextKeyRole, rbac.RoleGuest)
	ctx.Set(commonmw.ContextKeyUserID, 7)
	// Guest holds PermAppView.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		t.Fatalf("an identified guest account was denied PermAppView: %v", err)
	}
}

// The wire format must not change: clients already parse the model.Result
// envelope on 403. echo's default error handler serialises a non-string
// Message as-is, which is why the envelope is carried as the Message.
func TestRequirePermKeepsTheResponseEnvelope(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodDelete, "/v1/batch", nil), rec)

	err := requirePerm(ctx, rbac.PermFileDelete)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	e.HTTPErrorHandler(err, ctx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if _, ok := body["success"]; !ok {
		t.Fatalf("body lost the result envelope: %q", rec.Body.String())
	}
	if body["message"] != "permission denied" {
		t.Fatalf("message = %v, want \"permission denied\"", body["message"])
	}
}

// validatePath and its siblings are the same shape of helper and had the same
// defect: the caller checks the returned error and would otherwise carry on
// with an empty path after a 400 was already committed.
func TestValidatePathAbortsOnRejection(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/file", nil), rec)

	clean, err := validatePath(ctx, "")
	if err == nil {
		t.Fatal("validatePath returned nil for an empty path; the caller would continue")
	}
	if clean != "" {
		t.Fatalf("clean = %q, want empty", clean)
	}
	if _, ok := err.(*echo.HTTPError); !ok {
		t.Fatalf("want *echo.HTTPError, got %T", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("helper wrote the response itself: %q", rec.Body.String())
	}
}

func TestValidateUploadNamesAbortsOnBackslash(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/file/upload", nil), rec)

	if err := validateUploadNames(ctx, `AC\DC.mp3`, ""); err == nil {
		t.Fatal("a backslash in the file name was accepted")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("helper wrote the response itself: %q", rec.Body.String())
	}
	if err := validateUploadNames(ctx, "normal.mp3", "sub/dir"); err != nil {
		t.Fatalf("a legitimate name was rejected: %v", err)
	}
}

// requirePermInternal must NOT trust loopback. Its predecessor let any tokenless
// request through — and the gateway makes every internet request look tokenless
// over loopback — so DELETE /v1/samba/shares and POST /v1/notify were reachable
// unauthenticated from the internet. Now a caller without the internal secret
// header falls through to plain RBAC, so a guest (the role a tokenless request
// resolves to) is denied. The attack is precisely "omit the header", which this
// pins closed without touching /run.
func TestRequirePermInternalDeniesWithoutSecretHeader(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	// A request with no internal header and no user token → guest role, which
	// lacks PermStorageWrite. This is the samba-teardown attack shape.
	ctx := e.NewContext(httptest.NewRequest(http.MethodDelete, "/v1/samba/shares/1", nil), rec)

	err := requirePermInternal(ctx, rbac.PermStorageWrite)
	if err == nil {
		t.Fatal("requirePermInternal allowed a request with no internal secret and no token")
	}
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusForbidden {
		t.Fatalf("want 403 HTTPError, got %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("the gate wrote a body itself: %q", rec.Body.String())
	}
}

// A wrong (non-empty) secret must also be refused — it must not be treated as a
// peer just because a header is present.
func TestRequirePermInternalDeniesWrongSecret(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/v1/samba/shares/1", nil)
	req.Header.Set("X-CasaNova-Internal", "definitely-not-the-secret")
	ctx := e.NewContext(req, httptest.NewRecorder())

	if err := requirePermInternal(ctx, rbac.PermStorageWrite); err == nil {
		t.Fatal("a forged internal secret was accepted")
	}
}
