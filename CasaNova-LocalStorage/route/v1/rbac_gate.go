// Wave 26: shared RBAC + path-confinement helpers for the LocalStorage
// v1 routes.
//
// Before this file landed, every LocalStorage endpoint relied on a
// generic JWT check — *any* logged-in user (including a guest) could
// format a disk, mount an arbitrary block device anywhere on the host,
// or umount /. Storage operations are blast-radius-grade-A: a wrong
// path corrupts user data, and a hostile path makes container escape
// trivial. They have to be admin-gated *and* path-restricted at the
// same time.
package v1

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/mount"
	"github.com/labstack/echo/v4"
)

// requirePerm rejects the request unless the caller carries the supplied
// RBAC bit. It deliberately does NOT trust a loopback RemoteAddr: the
// CasaNova gateway reverse-proxies LAN traffic over 127.0.0.1, so a loopback
// peer is NOT evidence of a trusted internal caller — treating it as such is
// the exact bypass that would let an unauthenticated LAN client format a disk
// or mount an arbitrary device. Authorization comes solely from the role the
// JWT middleware set on the context; a tokenless caller fails closed to guest.
// The error response is the only one returned — handlers must
// `return requirePerm(...)` and not continue on a non-nil result.
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
	return echo.NewHTTPError(http.StatusForbidden, model.Result{
		Success: common_err.CLIENT_ERROR,
		Message: "permission denied",
	})
}

// validateMountPoint canonicalizes mp and rejects it unless it lies
// strictly *inside* one of pkg/mount.AllowedRoots — never the root itself
// (mounting over /mnt would hide every other mount under it) and never
// outside the allowed roots.
//
// OLA 3 — this used to keep its own copy of the allow-list and the
// containment logic, byte-identical to route/v2's copy purely by luck; now
// both call the single shared CasaNova-LocalStorage/pkg/mount.ValidatePoint.
func validateMountPoint(mp string) error {
	return mount.ValidatePoint(mp)
}
