package middleware

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// This package deliberately exports no authorization middleware.
//
// It used to export RequirePermission / RequireRole / RequireAdmin, each of
// which began with `if loopback.IsLoopback(c) { return next(c) }`. The gateway
// reverse-proxies every LAN request — and, over the Cloudflare tunnel, every
// internet request — to the backends over 127.0.0.1, so that condition is true
// for any remote caller and the checks authorized everyone. The eight
// per-service route/*/rbac_gate.go gates were rewritten for exactly this
// reason; these shared copies kept the pre-rewrite shape with zero callers.
//
// They were removed rather than repaired: nothing in any of the ten modules
// referenced them, and the Go linker had already eliminated all three symbols
// from every shipped binary. A privileged helper that is dead but exported is
// a ready-made handler waiting to be picked up in a hurry.
//
// Authorization decisions belong in the per-service gates, which read the role
// via RoleFromContext below. Requests from peer services authenticate with
// utils/internalauth (a per-boot root-only shared secret), never with the peer
// address. loopback_trust_sweep_test.go enforces that no replacement helper
// reintroduces the loopback-as-trust shape.

// Role/permission context keys. Every RBAC-aware JWT middleware should
// stash the authenticated role into the request's echo.Context under
// these keys so downstream handlers and inner middlewares can read it
// without re-parsing the JWT.
const (
	ContextKeyRole   = "rbac_role"
	ContextKeyUserID = "rbac_user_id"
)

// SetRoleInContext is called by each service's JWT parse callback once
// it has verified the token. Passes the role string from Claims.Role
// through rbac.Normalize so downstream checks see a clean enum value
// (including the empty-string → admin backward-compat rule).
func SetRoleInContext(c echo.Context, role string, userID int) {
	c.Set(ContextKeyRole, rbac.Normalize(role))
	c.Set(ContextKeyUserID, userID)
}

// RoleFromContext reads back the role that SetRoleInContext stored.
// Returns RoleGuest (least privilege) when no role was set — fail closed.
func RoleFromContext(c echo.Context) rbac.Role {
	v := c.Get(ContextKeyRole)
	if v == nil {
		return rbac.RoleGuest
	}
	r, ok := v.(rbac.Role)
	if !ok {
		return rbac.RoleGuest
	}
	return r
}

// UserIDFromContext returns the authenticated user ID or 0 if unauthenticated.
func UserIDFromContext(c echo.Context) int {
	v := c.Get(ContextKeyUserID)
	if v == nil {
		return 0
	}
	id, _ := v.(int)
	return id
}
