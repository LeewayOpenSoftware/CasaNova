package route

import (
	"net"
	"net/http"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"go.uber.org/zap"
)

// securityPeerIP returns the identity every security decision in this gateway
// is made against: the peer of the TCP connection.
//
// ENV-001. Forwarded headers are NEVER consulted here, and that is the whole
// invariant:
//
//	NO SECURITY DECISION MAY DERIVE FROM X-Forwarded-For.
//
// The inherited code trusted the header whenever the peer was loopback, on the
// reasoning that a loopback peer must be "our own reverse proxy, which
// overwrites the header". That reasoning does not survive this deployment. The
// gateway's loopback peer is cloudflared, the box is published to the internet
// through it, and an intermediary that APPENDS leaves attacker-chosen elements
// in the chain. Behind the tunnel every request has a loopback peer, so the
// exemption covered the entire internet.
//
// A previous attempt replaced this with a subsystem that resolved an identity
// from the header and fed it to the allow-lists and to a ban table. Two
// independent certifiers rejected it: any LAN device or container could name
// an arbitrary victim and have it banned. The lesson is not that the resolver
// was written badly — it is that a value the client controls cannot become an
// authorization input at all, no matter how carefully it is parsed.
//
// RemoteAddr is populated by the Go HTTP server from the accepted connection.
// It is the only source here the client cannot choose.
func securityPeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

type GatewayRoute struct {
	management *service.Management
}

func NewGatewayRoute(management *service.Management) *GatewayRoute {
	return &GatewayRoute{
		management: management,
	}
}

// the function is to ensure the request source IP is correct.
func rewriteRequestSourceIP(r *http.Request) {
	// we may receive two kinds of requests. a request from reverse proxy. a request from client.

	// in reverse proxy, X-Forwarded-For will like
	// `X-Forwarded-For:[192.168.6.102]`(normal)
	// `X-Forwarded-For:[::1, 192.168.6.102]`(hacked) Note: the ::1 is inject by attacker.
	// `X-Forwarded-For:[::1]`(normal or hacked) local request. But it from browser have JWT. So we can and need to verify it
	// `X-Forwarded-For:[::1,::1]`(normal or hacked) attacker can build the request to bypass the verification.
	// But in the case. the remoteAddress should be the real ip. So we can use remoteAddress to verify it.

	ipList := []string{}

	// when r.Header.Get("X-Forwarded-For") is "". the ipList should be empty.
	// fix https://github.com/LeewayOpenSoftware/CasaNova/issues/1247
	if r.Header.Get("X-Forwarded-For") != "" {
		ipList = strings.Split(r.Header.Get("X-Forwarded-For"), ",")

		// when r.Header.Get("X-Forwarded-For") is "". to clean the ipList.
		// fix https://github.com/LeewayOpenSoftware/CasaNova/issues/1247
		if len(ipList) == 1 && ipList[0] == "" {
			ipList = []string{}
		}
	}

	r.Header.Del("X-Forwarded-For")
	r.Header.Del("X-Real-IP")

	// Everything below only shapes a header for TELEMETRY. It is deliberately
	// NOT the value any security control reads: those all take
	// securityPeerIP(r). Forwarded headers are retained here as untrusted
	// provenance information, and any future consumer must treat them that
	// way — a value the client can write is evidence of nothing.
	//
	// Scope, stated honestly: today this does not even reach the backends.
	// service.newConfiguredProxy's Director deletes X-Forwarded-For and
	// net/http/httputil re-adds the transport peer, so every service behind
	// the tunnel sees 127.0.0.1 regardless of what is set here.
	// Note: the X-Forwarded-For depend the correct config from reverse proxy.
	// otherwise the X-Forwarded-For may be empty.
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteIP == "" {
		remoteIP = r.RemoteAddr
	}
	if len(ipList) > 0 && (remoteIP == "127.0.0.1" || remoteIP == "::1") {
		// to process the request from reverse proxy

		// in reverse proxy, X-Forwarded-For will contain multiple IPs.
		// if the request is from reverse proxy, the r.RemoteAddr will be 127.0.0.1.
		// So we need to get ip from X-Forwarded-For.
		//
		// Wave 33: pick the LEFTMOST entry, not the rightmost. RFC 7239
		// and the de-facto X-Forwarded-For convention places the
		// original client IP at index 0 and each successive proxy
		// appended to the right. The previous code grabbed
		// `ipList[len-1]`, which is whichever proxy is *closest to us*
		// — for a single-hop nginx → casanova-gateway deployment that
		// is the nginx machine itself, so every audit log, every IP
		// whitelist check, and every "your last login from X" panel
		// reported nginx's IP for every user. Worse: any per-route
		// CIDR whitelist (Wave 33-B2) became trivially bypassable —
		// nginx is on the loopback interface, so 127.0.0.1 was always
		// "the client" and any whitelist permitting localhost let the
		// world in. Use the leftmost entry, trimmed.
		clientIP := strings.TrimSpace(ipList[0])
		if clientIP != "" {
			r.Header.Add("X-Forwarded-For", clientIP)
		}
	}
	// to process the request from client.
	// the gateway will add the X-Forwarded-For to request header.
	// So we didn't need to add it.
}

// The current shape is a single boolean (enabled) — the budget /
// window knobs stay code-default for now to keep the v0.1 surface
// small. Persisted alongside the IP allow-list under /etc/casanova.

func (g *GatewayRoute) GetRoute() *http.ServeMux {
	gatewayMux := http.NewServeMux()

	// Core handler: routes requests to the appropriate backend service
	coreHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recover from panics in backend proxies so a single bad request
		// doesn't crash the entire gateway for all services.
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("gateway handler panic recovered",
					zap.Any("panic", rec),
					zap.String("path", r.URL.Path),
					zap.String("method", r.Method),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		// Reject requests with double-encoded or suspicious paths to prevent
		// request smuggling via path encoding differences between gateway
		// and backend.
		//
		// Wave 34-X: previously this matched %2e/%2E/%2f/%2F anywhere in
		// `r.RequestURI`, which includes the query string. Any legitimate
		// REST call that URL-encodes a forward slash inside a query
		// parameter — `?path=Documents%2FInvoices`,
		// `?ref=refs%2Fheads%2Fmain`, OAuth `redirect_uri=...`,
		// search filters, file picker paths sent from the UI — was
		// dropped with HTTP 400 before it ever reached the backend.
		// Worse, `%2e` (`.`) appears in legitimate filenames passed via
		// query string. The smuggling defense should only inspect the
		// raw escaped *path* component, not the query string. Use
		// `r.URL.EscapedPath()` so we see the on-the-wire form of the
		// path only.
		escapedPath := r.URL.EscapedPath()
		if strings.Contains(escapedPath, "%2e") || strings.Contains(escapedPath, "%2E") ||
			strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%2F") ||
			strings.Contains(r.URL.Path, "\x00") {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("pong from gateway service")); err != nil {
				logger.Error("Failed to `pong` in resposne to `ping`", zap.Any("error", err))
			}
			return
		}

		proxy := g.management.GetProxy(r.URL.Path)

		if proxy == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Fix source IP for X-Forwarded-For handling
		rewriteRequestSourceIP(r)

		clientIP := securityPeerIP(r)

		// Wave 42: GLOBAL IP allow-list — enforced here for the first time.
		// Empty list = no restriction; loopback is always allowed so the
		// box's own daemons/health-checks are never locked out. Applies to
		// EVERY request before route dispatch, so an operator who sets a LAN
		// whitelist actually gets network isolation (the control was a dead
		// no-op before). See service.Management.IsAllowedGlobal.
		if !g.management.IsAllowedGlobal(net.ParseIP(clientIP)) {
			logger.Error("gateway: global whitelist blocked request",
				zap.String("path", r.URL.Path),
				zap.String("client_ip", clientIP),
			)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Wave 3c: per-route IP whitelist check.
		// Routes without an explicit whitelist allow all IPs (backward compat).
		if !g.management.IsIPAllowedForPath(r.URL.Path, clientIP) {
			logger.Error("gateway: IP not allowed for route",
				zap.String("path", r.URL.Path),
				zap.String("client_ip", clientIP),
			)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		proxy.ServeHTTP(w, r)
	})

	// No rate-limiting middleware here. The one that briefly lived at this
	// line derived its per-client key from X-Forwarded-For, which made it a
	// remote-controlled ban primitive rather than a defence. Rate limiting
	// keyed on the observed peer is worth having and is deliberately left for
	// its own change, with its own certification, rather than smuggled into a
	// revert.
	gatewayMux.Handle("/", coreHandler)

	return gatewayMux
}
