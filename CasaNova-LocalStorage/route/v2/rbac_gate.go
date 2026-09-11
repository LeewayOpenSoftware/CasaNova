// Wave 26: shared RBAC + path-confinement helpers for the LocalStorage
// v2 routes. See route/v1/rbac_gate.go for the rationale — same problem,
// same solution, scoped to the v2 codegen handlers.
package v2

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/mount"
	"github.com/labstack/echo/v4"
)

// requirePerm rejects the request unless the caller holds the supplied RBAC
// bit. It deliberately does NOT trust a loopback RemoteAddr: the CasaNova
// gateway reverse-proxies LAN traffic over 127.0.0.1, so a loopback peer is
// NOT evidence of a trusted internal caller. Authorization comes solely from
// the role the JWT middleware set on the context; a tokenless caller fails
// closed to guest. Returns nil on success; on failure it has already written
// the response and the handler must return.
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
	msg := "permission denied"
	return echo.NewHTTPError(http.StatusForbidden, codegen.BaseResponse{Message: &msg})
}

// validateMountPoint canonicalizes mp and rejects it unless it lies
// strictly inside one of pkg/mount.AllowedRoots.
//
// OLA 3 — this used to keep its own copy of the allow-list and the
// containment logic, byte-identical to route/v1's copy purely by luck; now
// both call the single shared CasaNova-LocalStorage/pkg/mount.ValidatePoint.
func validateMountPoint(mp string) error {
	return mount.ValidatePoint(mp)
}
