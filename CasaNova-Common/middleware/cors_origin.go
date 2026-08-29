package middleware

import (
	"net"
	"net/url"
	"strings"
)

// IsAllowedCorsOrigin returns true if origin is one of:
//   - loopback: http(s)://localhost, http(s)://127.0.0.1[:port], http(s)://[::1][:port]
//   - private-range LAN (RFC1918): 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16
//   - link-local: 169.254.0.0/16
//
// Returned error is always nil — Echo's AllowOriginFunc signature requires
// it. A false return means the request is blocked with a 403.
//
// This is intentionally permissive for home-server LAN use but blocks every
// public origin so a malicious website the user visits can't read
// authenticated CasaNova responses.
func IsAllowedCorsOrigin(origin string) (bool, error) {
	if origin == "" {
		// Empty Origin can come from same-origin requests, but also from
		// non-browser clients or privacy-stripping proxies. Don't auto-allow;
		// let it fall through to the normal validation which will reject it
		// because an empty string has no parseable host.
		return false, nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false, nil
	}
	host := u.Hostname()
	if host == "" {
		return false, nil
	}

	// Explicit hostname allow-list for localhost aliases.
	switch strings.ToLower(host) {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true, nil
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Non-IP host (hostname) that isn't localhost — block.
		// Users on .local mDNS names should access via IP or add
		// an explicit whitelist entry.
		return false, nil
	}
	if ip.IsLoopback() {
		return true, nil
	}
	if ip.IsPrivate() {
		return true, nil
	}
	if ip.IsLinkLocalUnicast() {
		return true, nil
	}
	return false, nil
}
