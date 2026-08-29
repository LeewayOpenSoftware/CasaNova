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
	"path/filepath"
	"strings"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
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

// allowedMountRoots are the only directories under which a CasaNova
// mount may be created via the API. Anything outside is rejected.
//
// /mnt and /media are the standard Linux conventions for removable
// storage; /DATA is CasaNova's user-data root. Mounting *over* /etc,
// /usr, /var, /proc, /sys, /dev, /boot, or any other system path would
// shadow real files and is precisely the privilege-escalation primitive
// the API must not expose.
var allowedMountRoots = []string{
	"/mnt",
	"/media",
	"/DATA",
}

// validateMountPoint canonicalizes mp and rejects it unless it lies
// strictly *inside* one of allowedMountRoots — never the root itself
// (mounting over /mnt would hide every other mount under it) and never
// outside the allowed roots.
func validateMountPoint(mp string) error {
	if mp == "" {
		return errInvalidMountPoint("mount point is required")
	}
	clean := filepath.Clean(mp)
	if !filepath.IsAbs(clean) {
		return errInvalidMountPoint("mount point must be absolute")
	}
	if strings.Contains(clean, "..") {
		return errInvalidMountPoint("mount point must not contain ..")
	}
	for _, root := range allowedMountRoots {
		if clean == root {
			return errInvalidMountPoint("mount point cannot be the root itself; use a subdirectory")
		}
		if strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return nil
		}
	}
	return errInvalidMountPoint("mount point must live under /mnt, /media, or /DATA")
}

// errInvalidMountPoint is a tiny helper so the validator can return a
// distinct sentinel without pulling errors.New into every call site.
type mountPointError string

func (e mountPointError) Error() string { return string(e) }

func errInvalidMountPoint(msg string) error { return mountPointError(msg) }
