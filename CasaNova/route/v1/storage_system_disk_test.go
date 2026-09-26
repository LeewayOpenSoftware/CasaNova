package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// The administrative surface for the system-disk switch. The specification is
// explicit that a common user neither sees nor touches it, so "who may call
// this" is the feature and not packaging around it.

func systemDiskCtx(t *testing.T, role rbac.Role, uid int, id, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/storage/system-disk-access/"+id, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if role != "" {
		c.Set(commonmw.ContextKeyRole, role)
	}
	if uid != 0 {
		c.Set(commonmw.ContextKeyUserID, uid)
	}
	return c, rec
}

// THE RULE: a common user cannot change the switch — not their own, not
// anybody's. If this ever returns 200, a user grants themselves the whole disk.
func TestCommonUserCannotChangeSystemDiskAccess(t *testing.T) {
	for _, role := range []rbac.Role{rbac.RoleUser, rbac.RoleGuest} {
		c, rec := systemDiskCtx(t, role, 7, "7", `{"enabled":true,"role":"user"}`)

		err := PutStorageSystemDiskAccess(c)

		status := rec.Code
		if he, ok := err.(*echo.HTTPError); ok {
			status = he.Code
		} else if err != nil {
			t.Fatalf("%s: unexpected error type %T: %v", role, err, err)
		}
		if status != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 — a non-administrator changed the system-disk switch", role, status)
		}
	}
}

// The list is administrator-only too: it says which accounts can read every
// colleague's files.
func TestCommonUserCannotListSystemDiskAccess(t *testing.T) {
	c := ctxWith(rbac.RoleUser, 7)

	err := GetStorageSystemDiskAccess(c)

	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("a common user was served the switch list (err = %v)", err)
	}
	if he.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", he.Code)
	}
}

// An unauthenticated caller resolves to the guest role with user id 0. The
// permission gate must refuse it on identity as well as on role, which is the
// trap requirePerm already guards for every other endpoint here.
func TestTokenlessCallerCannotChangeSystemDiskAccess(t *testing.T) {
	c, rec := systemDiskCtx(t, rbac.RoleGuest, 0, "1", `{"enabled":true,"role":"admin"}`)

	err := PutStorageSystemDiskAccess(c)

	status := rec.Code
	if he, ok := err.(*echo.HTTPError); ok {
		status = he.Code
	}
	if status == http.StatusOK {
		t.Fatal("a tokenless caller changed the system-disk switch")
	}
}

// A malformed account id must be refused rather than silently keyed on a
// string the authority will never match.
func TestSystemDiskAccessRejectsANonNumericAccountID(t *testing.T) {
	c, rec := systemDiskCtx(t, rbac.RoleAdmin, 1, "../../etc", `{"enabled":true,"role":"user"}`)

	if err := PutStorageSystemDiskAccess(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-numeric account id", rec.Code)
	}
}
