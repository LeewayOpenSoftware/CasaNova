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
// everyone else needs a role holding PermSystemConfig. Nothing legitimate is
// narrower: every in-house publisher uses the socket (external.PublishEventInSocket
// goes through SocketHTTPClient), and no first-party client publishes over TCP.
//
// WHAT THIS GATE DOES NOT COVER. The previous wording here said "the UI only
// subscribes", and that is false: THE v2 UI DOES NOT SUBSCRIBE TO THIS BUS AT
// ALL. It polls the YSK REST endpoints instead, and its own source says so
// (CasaNova-UI-v2/src/stores/ysk.ts, "rather than a full bus WebSocket
// subscription"; src/api/ysk.ts). There is no WS or Socket.IO subscription
// anywhere in the v2 UI.
//
// The read surface is SubscribeEventWS, and it has authentication but NO
// authorization: any authenticated principal, the guest role included, may
// subscribe to any source, and omitting `names` subscribes to every name that
// source has registered. That is the KNOWN state of this bus, recorded here so
// the next reader can tell a deliberate absence from an oversight.
//
// The old sentence was a trap for one specific decision. Anyone weighing a gate
// on the subscribe side would read "the UI only subscribes" as "gating subscribe
// breaks the UI", and drop it. The truth is the reverse: no first-party caller
// subscribes, so the first-party blast radius of such a gate is zero. The real
// constraint is out-of-tree ZimaOS clients, which cannot be measured from this
// repository — and "no first-party caller" is not "no caller".

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
