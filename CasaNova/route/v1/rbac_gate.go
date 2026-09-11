package v1

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// requirePerm gates a handler entry point on an RBAC permission.
//
// It does NOT trust loopback. The gateway reverse-proxies every LAN
// request over 127.0.0.1, so a loopback short-circuit here would grant
// every unauthenticated LAN caller full privileges (the confirmed
// loopback auth-bypass). Real WebUI traffic carries a JWT that the JWT
// middleware validates and turns into a role via RoleFromContext; a
// tokenless request resolves to the guest role, which holds only READ
// permissions — so internal tokenless reads keep working while writes
// are denied. Returns nil when allowed, or a 403 the caller must return.
//
// It returns an *echo.HTTPError and does NOT write the response itself. That
// distinction is the whole security property: ctx.JSON returns the SERIALIZER's
// error, which is nil once the bytes are written, so a helper that denied with
// `return ctx.JSON(403, …)` handed its caller a nil error. Every call site is
// written as `if err := requirePerm(…); err != nil { return err }`, so the deny
// path was a no-op: the client got 403 and the handler ran anyway. Verified on
// the live box — an unauthenticated DELETE /v1/batch answered 403 and moved the
// file to the recycle bin. Carrying the same model.Result as the error Message
// keeps the wire format byte-identical: echo's default error handler serialises
// a non-string Message as-is.
// AUTHORIZATION IS TWO QUESTIONS, AND THIS HELPER USED TO ASK ONLY ONE.
//
// "Does the role hold the permission" was the whole check. A request that
// arrives with NO principal is not rejected by that question — RoleFromContext
// fails closed to RoleGuest, and guestPerms deliberately holds PermAppView and
// PermSystemRead, so `requirePerm(ctx, PermSystemRead)` RETURNED NIL for a
// context nobody ever authenticated. It reads like a gate, it survives review,
// and for those two bits it enforced nothing. Roughly forty routes were closed
// by hand, in several waves, before it was clear the patches were chasing a
// DEFAULT THAT FAILS OPEN rather than a run of individual oversights.
//
// So the second question is now asked HERE, once, instead of being remembered
// at each of the 230+ call sites: a permission that resolved only because an
// absent principal fell back to guest is not authorization, and is refused 401.
//
// THE ORDER OF THE TWO CHECKS IS THE ENTIRE SAFETY ARGUMENT — do not swap it.
// The permission test runs FIRST, so every request this helper already refused
// is still refused at the same branch, with the same 403 and the same body. The
// principal test can therefore only ever turn an ALLOW into a DENY; it cannot
// turn a DENY into an ALLOW, and it cannot change the shape of an existing
// denial. That makes the change MONOTONE, which is what lets it land on a
// feature-frozen line: no caller that works today can stop working unless it
// was relying on the borrowed-guest hole, which is the hole being closed.
//
// WHO IS EXEMPT, AND WHY THAT IS NOT SPELLED HERE. A genuine peer service has a
// real credential — the per-boot internal secret — and its exemption is the
// separately NAMED requirePermInternal below, which answers before this helper
// is consulted. An exemption you can see in the route's own gate is reviewable;
// a tolerance buried in the default is not. Nothing else is exempt: measured
// across the tree, the only tokenless peer callers are /v1/notify/:path,
// /v1/samba/shares/:id and /v1/gateway/{routes,port}, none of which is gated on
// a permission guest holds.
func requirePerm(ctx echo.Context, p rbac.Permission) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return echo.NewHTTPError(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "permission denied",
		})
	}
	// The role holds the permission. Second question: is there a principal
	// behind that role, or did an unauthenticated context borrow guest's?
	// UserIDFromContext is 0 exactly when the JWT middleware set nothing —
	// the same predicate requireSystemPrincipal already uses, kept identical
	// on purpose so the strict gate and the default cannot drift apart.
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, model.Result{
			Success: common_err.ERROR_AUTH_TOKEN,
			Message: "authentication required",
		})
	}
	return nil
}

// requirePermInternal is requirePerm PLUS an allowance for a genuine peer
// CasaNova service — proven by an UNFORGEABLE internal secret, never by the
// network.
//
// It exists ONLY for the handful of privileged endpoints a peer invokes without
// a user token (verified callers: CasaNova-LocalStorage → POST /v1/notify/:path
// and /v1/notify/system_status via external.NotifyService, and
// DELETE /v1/samba/shares/:id via external.ShareService).
//
// The previous version trusted `IsLoopback(ctx) && no-token`. But the gateway
// reverse-proxies EVERY request — LAN and internet-over-tunnel — to the backend
// over 127.0.0.1 with no auth of its own, so `IsLoopback` was true for the whole
// internet. An external attacker who simply omitted the Authorization header was
// indistinguishable from a peer: DELETE /v1/samba/shares/:id (SMB config
// teardown) and POST /v1/notify/* (spoofed "system" notifications → update
// phishing) were reachable UNAUTHENTICATED from anywhere. Loopback is not a
// trust signal; the peer now presents the per-boot secret from a root-only file
// under /run that an external caller cannot read. A request carrying a real user
// token still goes through normal RBAC, so a low-privilege user proxied through
// the gateway cannot ride this allowance either.
func requirePermInternal(ctx echo.Context, p rbac.Permission) error {
	if internalauth.Valid(ctx.Request().Header.Get(internalauth.HeaderName)) {
		return nil
	}
	return requirePerm(ctx, p)
}
