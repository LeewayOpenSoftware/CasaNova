package route

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"github.com/labstack/echo/v4"
)

// WHO MAY SKIP AUTHENTICATION, AND WHY THE OLD ANSWER WAS WRONG.
//
// Both routers used to skip the JWT for:
//
//	loopback.IsLoopback(c) && c.Request().Header.Get(echo.HeaderAuthorization) == ""
//
// The comment above it warned that the gateway proxies every LAN request over
// 127.0.0.1 — and then shipped exactly the check it warned against. The gateway
// also proxies the Cloudflare tunnel, so IsLoopback() was true for the whole
// internet, and "presents no Authorization header" is something any caller
// achieves by not sending one. The condition read as "internal traffic only"
// and meant "anyone who omits a header".
//
// Measured on the production box before this change, with no credential of any
// kind: GET /v2/app_management/compose returned 200 with the compose definition
// of all 23 installed apps — 47 host volume paths and 46 environment variables,
// six of them named like credentials. GET /v1/sys/smb/browse ran avahi-browse
// and returned the LAN's SMB hosts.
//
// AN IP ADDRESS IS NOT A CREDENTIAL. There are exactly two reasons to answer
// without a user token, and both are positive facts about the request rather
// than facts about where it appears to come from:
//
//  1. It carries the per-boot internal secret, which lives in a 0600 file
//     inside a 0700 directory both owned by root. A caller who can read it is
//     already root on this machine. This is what peer services present.
//
//  2. It is one of a short, explicit, reviewed list of paths that MUST answer
//     before any session can exist — a browser arriving from an OAuth redirect,
//     or a device client that has to identify the box before pairing.
//
// Anything else authenticates. The list is closed: adding to it is a visible
// edit to a reviewed variable, not an emergent property of a boolean.
//
// THIS USED TO BE A SECOND, INDEPENDENT IMPLEMENTATION of exactly this
// decision — the body below was a near word-for-word copy of
// CasaNova-Common/utils/authskip.For, written by someone who didn't know that
// package existed (the same failure mode documented on requireSystemPrincipal
// in system.go, and the reason authskip was extracted in the first place: it
// was copy-pasted, wrong, into nine routers before that). Delegating instead
// of reimplementing means a future hardening of the shared decision reaches
// this router automatically instead of depending on someone remembering this
// file exists. The doc comment above stays — CasaNova's own incident history
// with this bug — but the logic itself now has one source.
func skipAuthFor(allow map[string]bool) func(echo.Context) bool {
	return authskip.For(allow)
}
