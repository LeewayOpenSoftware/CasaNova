// Wave 25: shared RBAC entry-point gate for v1 handlers.
//
// The v2 routes already use a similar pattern but it lives in route/v2.
// Duplicating it here keeps v1 handlers from importing the v2 package
// (which would create a cycle), and keeps the policy in one place inside
// each route version.
package v1

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// requirePerm returns nil when the caller may proceed and a 403 response
// when the caller lacks the requested permission.
//
// It deliberately does NOT trust a loopback RemoteAddr: the CasaNova gateway
// reverse-proxies LAN traffic over 127.0.0.1, so a loopback peer is NOT
// evidence of a trusted internal caller. Authorization comes solely from the
// role the JWT middleware set on the context; a tokenless caller fails closed
// to guest. Guests still hold read permissions, so internal tokenless reads
// keep working.
//
// SECURITY (2026-08-11): this returns an *echo.HTTPError and MUST NOT write the
// response itself. ctx.JSON reports the SERIALIZER's error, which is nil once
// the bytes are on the wire, so denying with `return ctx.JSON(403, …)` handed
// the caller a nil error. Every call site is `if err := requirePerm(…); err !=
// nil { return err }`, so the deny path did nothing: the client got a 403 and
// the handler ran to completion anyway. Proven in the main service against the
// live box (an unauthenticated DELETE answered 403 and still deleted), and this
// copy had the identical defect. The response body is unchanged: echo's default
// error handler serialises a non-string Message as-is.
// The permission test runs FIRST and the principal test SECOND, and that order
// is the safety argument: every request already refused is still refused at the
// same branch with the same 403 and the same body, so this can only ever turn an
// ALLOW into a DENY — never a DENY into an ALLOW, never a different denial.
//
// The second test exists because "does the role hold the permission" does not
// reject a request that arrived with NO principal: RoleFromContext fails closed
// to RoleGuest, and guest holds PermAppView and PermSystemRead, so a gate on
// either of those bits returned nil for a context nobody authenticated. See
// CasaNova/route/v1/rbac_gate.go for the full account.
func requirePerm(ctx echo.Context, p rbac.Permission) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return echo.NewHTTPError(http.StatusForbidden, map[string]string{"message": "permission denied"})
	}
	// The role holds the permission. Second question: is there a principal
	// behind that role, or did an unauthenticated context borrow guest's?
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, map[string]string{"message": "authentication required"})
	}
	return nil
}
