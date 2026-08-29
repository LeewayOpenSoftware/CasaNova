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
func requirePerm(ctx echo.Context, p rbac.Permission) error {
	if rbac.Allows(commonmw.RoleFromContext(ctx), p) {
		return nil
	}
	return echo.NewHTTPError(http.StatusForbidden, model.Result{
		Success: common_err.CLIENT_ERROR,
		Message: "permission denied",
	})
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
