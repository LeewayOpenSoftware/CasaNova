package v1

// Three routes carried no authorization of any kind. A protected request
// with no token never reaches a handler, so these were never anonymous-
// internet holes — but "no gate at all" admits every authenticated role,
// guest included, and for two of them it also admitted a context carrying no
// principal at all.
//
// Each probe is paired with a positive control, so a gate that simply
// refused everyone could not make this file pass.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// principalCtx builds a request context. An empty role leaves the context
// with no principal at all — RoleFromContext then answers RoleGuest and
// UserIDFromContext answers 0, which is the shape requireSystemPrincipal
// exists to refuse.
func principalCtx(t *testing.T, path, role string, userID int) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if role != "" {
		commonmw.SetRoleInContext(c, role, userID)
	}
	return c, rec
}

// httpStatus reads the status whether the handler answered with ctx.JSON or
// aborted by returning an *echo.HTTPError.
func httpStatus(err error, rec *httptest.ResponseRecorder) int {
	if he, ok := err.(*echo.HTTPError); ok {
		return he.Code
	}
	return rec.Code
}

// --- #13a: cloud backup status names every configured remote ---

func TestCloudBackupStatusRefusesAPrincipalLessCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		h    func(echo.Context) error
	}{
		{"status", "/v1/sys/cloud/status", GetCloudBackupStatus},
		{"sync status", "/v1/sys/cloud/sync/status", GetCloudBackupSyncStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rec := principalCtx(t, tc.path, "", 0)
			err := tc.h(ctx)
			if got := httpStatus(err, rec); got != http.StatusUnauthorized {
				t.Fatalf("a caller with no principal got %d, want 401 — this endpoint names every configured cloud remote", got)
			}
		})
	}
}

// TestCloudBackupStatusPositiveControl: the gate copied from
// GetCloudBackupProviders admits a real principal holding PermSystemRead.
func TestCloudBackupStatusPositiveControl(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		h    func(echo.Context) error
	}{
		{"status", "/v1/sys/cloud/status", GetCloudBackupStatus},
		{"sync status", "/v1/sys/cloud/sync/status", GetCloudBackupSyncStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rec := principalCtx(t, tc.path, string(rbac.RoleAdmin), 1)
			err := tc.h(ctx)
			if got := httpStatus(err, rec); got == http.StatusUnauthorized || got == http.StatusForbidden {
				t.Fatalf("an administrator was refused (%d) — the gate is not discriminating, it is just closed", got)
			}
		})
	}
}

// --- #17: 2FA status ---

// The 2FA config is server-GLOBAL (a single /var/lib/casanova/2fa.json), so
// this leaks no account's existence and is not an enumeration aid. It does
// report whether 2FA is on and how many backup codes remain, which is
// reconnaissance on the administrator's fallback credential.
func TestTOTPStatusRefusesCallersWhoCannotConfigureIt(t *testing.T) {
	for _, role := range []string{"", "guest", "user"} {
		t.Run("role="+role, func(t *testing.T) {
			ctx, rec := principalCtx(t, "/v1/sys/2fa/status", role, 1)
			err := GetTOTPStatus(ctx)
			if got := httpStatus(err, rec); got != http.StatusForbidden {
				t.Fatalf("role %q got %d, want 403 — 2FA posture belongs to whoever can configure 2FA", role, got)
			}
		})
	}
}

// TestTOTPStatusPositiveControl: an admin gets PAST the gate. It may still
// fail downstream if no 2FA config exists on this box, which is why the
// assertion is "not refused" rather than "200" — claiming a 200 here would
// be asserting something this fixture does not control.
func TestTOTPStatusPositiveControl(t *testing.T) {
	ctx, rec := principalCtx(t, "/v1/sys/2fa/status", string(rbac.RoleAdmin), 1)
	err := GetTOTPStatus(ctx)
	if got := httpStatus(err, rec); got == http.StatusForbidden || got == http.StatusUnauthorized {
		t.Fatalf("an administrator was refused (%d) — the gate is just closed, not discriminating", got)
	}
}
