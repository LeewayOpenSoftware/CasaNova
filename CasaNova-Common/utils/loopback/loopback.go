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
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
