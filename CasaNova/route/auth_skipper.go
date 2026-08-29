package route

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
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
func skipAuthFor(allow map[string]bool) func(echo.Context) bool {
	return func(c echo.Context) bool {
		// A genuine peer service, proven by an unforgeable secret.
		//
		// This has to happen HERE and not only in the handler: the JWT
		// middleware runs first, so without this a peer presenting the internal
		// header and no bearer token is refused with 401 before
		// requirePermInternal ever sees it.
		if internalauth.Valid(c.Request().Header.Get(internalauth.HeaderName)) {
			return true
		}

		// Match on the ROUTE PATTERN (c.Path()), not the raw URL. c.Path()
		// is what the router matched — "/v1/recover/:type" — so no amount of
		// encoding, traversal or query trickery in the request line can be
		// bent into a listed entry, and a parameterised route is expressed
		// once instead of as a prefix that would swallow its children.
		return allow[c.Path()]
	}
}
