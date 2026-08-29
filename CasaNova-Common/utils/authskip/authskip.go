// Package authskip decides who may reach a handler WITHOUT a user token.
//
// It exists because the same wrong boolean was copy-pasted into nine routers:
//
//	loopback.IsLoopback(c) && c.Request().Header.Get(echo.HeaderAuthorization) == ""
//
// The gateway reverse-proxies every LAN request AND every Cloudflare-tunnel
// request to the backends over 127.0.0.1, so IsLoopback() is true for the whole
// internet; "sends no Authorization header" is something any caller achieves by
// not sending one. The condition read as "internal traffic only" and meant
// "anybody who omits a header". Measured on the production box with no
// credential of any kind, from another machine on the LAN:
// GET :85/v2/app_management/compose returned 47 host volume paths and 46
// environment variables; GET :85/v2/job_management/tasks returned 200 on a
// service whose write routes run /bin/sh -c as root.
//
// AN IP ADDRESS IS NOT A CREDENTIAL. There are exactly two grounds to answer
// without a user token, and each is a positive fact about the request rather
// than a fact about where it appears to come from:
//
//  1. it carries the per-boot internal secret — a 0600 file in a 0700
//     root-owned directory, so a caller who can read it is already root here;
//  2. its matched ROUTE PATTERN is on a short, reviewed list of paths that must
//     answer before any session can exist.
//
// Everything else authenticates. The list is closed: growing it is a visible
// edit to a reviewed variable, not an emergent property of a boolean.
package authskip

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// For builds the only JWT skipper a CasaNova router should use.
//
// allow is keyed on the route PATTERN as the router matched it — c.Path(), e.g.
// "/v1/recover/:type" — never the raw URL, so no percent-encoding, traversal or
// query trickery in the request line can be bent into a listed entry, and an
// unmatched route yields "" which is never in the map. Pass nil where nothing
// must answer pre-login.
//
// The bare func(echo.Context) bool return type is assignable to both
// echo_middleware.JWTConfig.Skipper and echojwt.Config.Skipper, so this package
// imports neither.
func For(allow map[string]bool) func(echo.Context) bool {
	return func(c echo.Context) bool {
		// A genuine peer service, proven by an unforgeable per-boot secret.
		//
		// This MUST be checked here and not only in the handler: the JWT
		// middleware runs first, so a peer presenting the internal header and
		// no bearer token would otherwise be refused 401 before any
		// requirePermInternal-style gate could see it.
		if internalauth.Valid(c.Request().Header.Get(internalauth.HeaderName)) {
			return true
		}
		return allow[c.Path()]
	}
}
