package v2

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// requirePerm enforces RBAC on a handler. It deliberately does NOT trust a
// loopback RemoteAddr: the CasaNova gateway reverse-proxies LAN traffic over
// 127.0.0.1, so a loopback peer is NOT evidence of a trusted internal caller —
// treating it as such is the exact bypass that would let an unauthenticated
// LAN client load kernel modules or switch GPU drivers. Authorization comes
// solely from the role the JWT middleware set; a tokenless caller fails closed
// to guest. Handlers must `return requirePerm(...)` on a non-nil result.
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
	// Requests stamped by the internal unix-socket listener come from a
	// root-owned sibling service (0600 socket ⇒ only root can connect) — a
	// stronger credential than any JWT role. This is NOT loopback trust: the
	// stamp is set by ConnContext on the socket server only, so no TCP/LAN
	// caller (gateway-proxied or otherwise) can ever carry it.
	if isFromInternalSocket(ctx) {
		return nil
	}
	if !rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return echo.NewHTTPError(http.StatusForbidden, model.Result{
			Success: http.StatusForbidden,
			Message: "permission denied",
		})
	}
	// The role holds the permission. Second question: is there a principal
	// behind that role, or did an unauthenticated context borrow guest's?
	// RoleFromContext fails closed to RoleGuest and guest holds PermSystemRead,
	// so requireRead below returned nil for a context nobody authenticated.
	//
	// This runs AFTER the socket check on purpose: a sibling root service on the
	// 0600 unix socket has no JWT and therefore no user id, and it is exempt by
	// the NAMED check above rather than by this default being lax. That is the
	// whole shape of the change — the exemption is visible, the default is not.
	//
	// It also runs AFTER the permission test, so every request already refused is
	// still refused at the same branch with the same 403 and body: the addition
	// can only turn an ALLOW into a DENY.
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, model.Result{
			Success: http.StatusUnauthorized,
			Message: "authentication required",
		})
	}
	return nil
}

// requireConfig gates privileged mutations (kernel-module load/unload, GPU
// driver selection) behind admin-grade PermSystemConfig. Loading an arbitrary
// kernel module or switching the GPU driver is root-equivalent.
func requireConfig(ctx echo.Context) error { return requirePerm(ctx, rbac.PermSystemConfig) }

// requireRead gates read-only enumeration behind PermSystemRead (held by any
// authenticated role).
func requireRead(ctx echo.Context) error { return requirePerm(ctx, rbac.PermSystemRead) }
