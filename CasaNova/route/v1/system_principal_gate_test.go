package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

func ctxWith(role rbac.Role, uid int) echo.Context {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	if role != "" {
		c.Set(commonmw.ContextKeyRole, role)
	}
	if uid != 0 {
		c.Set(commonmw.ContextKeyUserID, uid)
	}
	return c
}

// THE PROPERTY: a permission the guest role holds must not be borrowable by a
// caller who never authenticated.
//
// This is the trap requireSystemPrincipal exists for. guestPerms holds
// PermSystemRead, and a tokenless request resolves to RoleGuest with user id 0
// — so requirePerm(ctx, PermSystemRead) returns nil for the internet. A gate
// that reads correctly and allows everyone is worse than no gate, because it
// closes the question.
func TestGuestHeldPermissionIsNotBorrowableWithoutIdentity(t *testing.T) {
	// Precondition, asserted rather than assumed: if guest ever loses
	// PermSystemRead this test would pass for the wrong reason.
	if !rbac.Allows(rbac.RoleGuest, rbac.PermSystemRead) {
		t.Skip("guest no longer holds PermSystemRead; this trap no longer exists")
	}

	tokenless := ctxWith("", 0) // exactly what the JWT skipper leaves behind

	if err := requirePerm(tokenless, rbac.PermSystemRead); err != nil {
		t.Fatal("setup is wrong: requirePerm was expected to ALLOW this, which is " +
			"the whole reason the stricter helper exists")
	}
	if err := requireSystemPrincipal(tokenless, rbac.PermSystemRead); err == nil {
		t.Fatal("requireSystemPrincipal allowed a caller with no identity to use a " +
			"permission the guest role holds — the gate does not gate")
	} else if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusUnauthorized {
		t.Fatalf("denial must be a 401 HTTPError so the handler ABORTS; got %#v", err)
	}
}

// The complement: a real logged-in user with the permission still passes, or the
// gate is just an outage.
func TestAnIdentifiedUserWithThePermissionIsAllowed(t *testing.T) {
	if err := requireSystemPrincipal(ctxWith(rbac.RoleAdmin, 1), rbac.PermSystemRead); err != nil {
		t.Fatalf("an authenticated admin was refused: %v", err)
	}
	if err := requireSystemPrincipal(ctxWith(rbac.RoleUser, 7), rbac.PermSystemRead); err != nil {
		t.Fatalf("an authenticated ordinary user was refused a permission they "+
			"hold, which breaks their pages: %v", err)
	}
}

// An identified caller WITHOUT the permission is still refused — identity is an
// additional requirement, not a replacement for authorization.
func TestIdentityDoesNotSubstituteForPermission(t *testing.T) {
	if err := requireSystemPrincipal(ctxWith(rbac.RoleGuest, 42), rbac.PermUserManage); err == nil {
		t.Fatal("an authenticated guest was granted an admin permission")
	}
}
