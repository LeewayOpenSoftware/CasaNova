package route

// Authorization for the bus's WRITE surface.
//
// Authentication was fixed earlier (the loopback JWT skip is gone; TCP
// callers must present a valid token). But a valid token is not authority:
// the guest role passes the JWT middleware too, and PublishEvent /
// TriggerAction / the type-registration endpoints then executed with no
// role check at all. Events on this bus become UI notifications and audit
// history, and actions become behaviour — a guest able to forge either,
// with an arbitrary sourceID, is a sev-1.
//
// The rule: internal services publishing over the unix socket pass (their
// identity is the ConnContext stamp, provable and unforgeable from TCP);
// everyone else needs a role holding PermSystemConfig. Nothing legitimate
// is narrower — the UI only subscribes, and every in-house publisher uses
// the socket.

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// requireBusMutator gates a mutating bus endpoint. The error it returns is
// an echo.HTTPError, so the handler aborts by returning it — the return
// value is the abort, per the gate-return convention this codebase has been
// burned by three times.
func requireBusMutator(c echo.Context) error {
	if isFromUnixSocket(c) {
		return nil
	}
	role := commonmw.RoleFromContext(c)
	if !rbac.PermissionsFor(role).Has(rbac.PermSystemConfig) {
		return echo.NewHTTPError(http.StatusForbidden, "publishing to the message bus requires system-config authority")
	}
	return nil
}
