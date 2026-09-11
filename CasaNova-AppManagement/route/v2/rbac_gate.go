package v2

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// requirePerm is the entry-point guard used by every v2 handler that
// performs a state change. The v2 handlers are wired through oapi-codegen
// so per-route Echo middleware isn't available — each sensitive handler
// calls this helper at its first line instead.
//
// It deliberately does NOT trust a loopback RemoteAddr: the CasaNova gateway
// reverse-proxies LAN traffic over 127.0.0.1, so a loopback peer is NOT
// evidence of a trusted internal caller — treating it as such is the exact
// bypass that would let an unauthenticated LAN client reach privileged app
// operations. Authorization comes solely from the role the JWT middleware
// set on the context; a tokenless caller fails closed to guest. Returns an
// already-serialized JSON 403 when denied; callers return it directly.
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
		return echo.NewHTTPError(http.StatusForbidden, map[string]string{
			"message": "permission denied",
		})
	}
	// The role holds the permission. Second question: is there a principal
	// behind that role, or did an unauthenticated context borrow guest's?
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, map[string]string{
			"message": "authentication required",
		})
	}
	return nil
}

// requirePermInternal is intentionally NOT the default AppManagement gate.
// It exists only for the three explicitly named read-only diagnostics whose
// root-local CLI caller holds the per-boot, root-only peer secret:
// GetGlobalSettings, MyComposeApp, and ComposeAppLogs. They reveal sensitive
// configuration (the first and third may expose credentials), so mutations and
// every other route keep their ordinary user-JWT permission check. The gateway
// strips this header at its public proxy boundary; a valid header therefore
// proves a local peer, never a loopback/LAN caller.
func requirePermInternal(ctx echo.Context, p rbac.Permission) error {
	if internalauth.Valid(ctx.Request().Header.Get(internalauth.HeaderName)) {
		return nil
	}
	return requirePerm(ctx, p)
}
