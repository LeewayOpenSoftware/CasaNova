package v2

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
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
func requirePerm(ctx echo.Context, p rbac.Permission) error {
	if rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return nil
	}
	return echo.NewHTTPError(http.StatusForbidden, map[string]string{
		"message": "permission denied",
	})
}
