package v2

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/labstack/echo/v4"
)

// requirePerm enforces RBAC on a handler. It deliberately does NOT trust a
// loopback RemoteAddr: the CasaNova gateway reverse-proxies LAN traffic over
// 127.0.0.1, so a loopback peer is NOT evidence of a trusted internal caller —
// treating it as such is the exact bypass that would let an unauthenticated
// LAN client reach privileged VM operations. Authorization comes solely from
// the role the JWT middleware set on the context; a tokenless caller fails
// closed to guest. Handlers must `return requirePerm(...)` on a non-nil result.
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
func requirePerm(c echo.Context, p rbac.Permission) error {
	if rbac.Allows(commonmw.RoleFromContext(c), p) {
		return nil
	}
	return echo.NewHTTPError(http.StatusForbidden, model.BaseResponse{Message: "permission denied"})
}

// requireConfig gates VM-mutating and console operations behind admin-grade
// PermSystemConfig — VM lifecycle, define/undefine, autostart, and console
// access are all full control of the guest and must be admin-only.
func requireConfig(c echo.Context) error { return requirePerm(c, rbac.PermSystemConfig) }

// requireRead gates read-only enumeration behind PermSystemRead (held by any
// authenticated role).
func requireRead(c echo.Context) error { return requirePerm(c, rbac.PermSystemRead) }
