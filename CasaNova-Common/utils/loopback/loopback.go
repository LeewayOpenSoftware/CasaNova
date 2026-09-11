// Package loopback provides a tamper-resistant check for whether an incoming
// HTTP request originated from the loopback interface.
//
// Why this exists: Echo's c.RealIP() respects X-Forwarded-For, X-Real-IP, and
// Forwarded by default. JWT auth middlewares across CasaNova used
//
//	c.RealIP() == "127.0.0.1" || c.RealIP() == "::1"
//
// as a skipper, meaning any remote attacker could send
//
//	X-Forwarded-For: 127.0.0.1
//
// and bypass authentication entirely. This package reads c.Request().RemoteAddr
// directly — that field is populated by the Go HTTP server from the TCP
// connection and cannot be forged from the HTTP payload.
package loopback

import (
	"net"
	"strings"

	"github.com/labstack/echo/v4"
)

// IsLoopback reports whether the TCP peer for this request is on the loopback
// interface. Use this for every "allow localhost without auth" skipper.
//
// Safe to call on requests arriving from Unix sockets (RemoteAddr is
// empty or "@") — those are treated as loopback since only local processes
// can connect to a Unix socket.
func IsLoopback(c echo.Context) bool {
	if c == nil || c.Request() == nil {
		return false
	}
	return isLoopbackAddr(c.Request().RemoteAddr)
}

// isLoopbackAddr parses a RemoteAddr string ("ip:port" or "[ipv6]:port") and
// returns true if the IP is a loopback address. Unix sockets (empty or "@")
// are also loopback.
func isLoopbackAddr(remoteAddr string) bool {
	if remoteAddr == "" || strings.HasPrefix(remoteAddr, "@") {
		return true // Unix socket
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Fallback for RemoteAddr without a port (rare but possible).
		host = remoteAddr
	}
	return IsLoopbackHost(host)
}

// IsLoopbackHost reports whether host is a LITERAL loopback IP address.
//
// Unlike IsLoopback, this takes a bare host string rather than a request — it
// is for validating a DIAL TARGET (a redirect Location header, a proxy route
// target) rather than an inbound caller's address. Hostnames are refused on
// purpose, never resolved: two independent call sites in this codebase
// (CasaNova-Gateway's route-target validator, CasaNova's embed-probe redirect
// walker) used to each decide this for themselves, and one of the two
// additionally accepted the literal string "localhost" — a hostname whose
// meaning depends on DNS/hosts resolution the caller who names it does not
// control, but which a poisoned resolver or a doctored /etc/hosts entry can
// repoint after this check passes. A literal IP is the exact bytes that will
// be dialed; nothing about a name is checked twice and guaranteed to still be
// true when the dial happens.
func IsLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
