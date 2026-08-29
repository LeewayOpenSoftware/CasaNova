package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ---------------------------------------------------------------------------
// Outbound fetch guard
//
// Several handlers take a URL (or a URL fragment) from the client and make
// THIS process open a socket to it. Hardening those handlers used to happen
// per-handler and only on the DESTINATION half of the problem: /v1/sys/proxy
// grew a re-resolving dialer, a private-range block, no-redirects and a
// LimitReader, while the CALLER half (who may originate traffic at all) and
// the RESPONSE half (what the browser does with the bytes we hand back) were
// never addressed. The result was an anonymous open proxy that also reflected
// attacker-chosen Content-Type at CasaNova's own origin — same-origin XSS
// against any admin who followed the link.
//
// This file is the one place where all three halves are decided, so a new
// URL-taking handler inherits them by construction:
//
//	caller    requireOutboundFetchCaller — real login + an explicit permission
//	           (guest holds PermSystemRead, so a permission check ALONE does
//	           not stop an anonymous caller; the login check is what does)
//	budget    acquireOutboundSlot — bounded concurrent upstream connections
//	destination validateOutboundURL + outboundSafeClient — scheme allow-list,
//	           private-range block re-checked at dial time, no redirects
//	response  writeInertUpstreamResponse — upstream bytes are never labelled
//	           as an active document type on this origin (see its doc comment
//	           for what that does and does not cover)
//
// Scope note: this guards traffic THIS process originates on behalf of a
// client request. It says nothing about the box's other egress (docker pulls,
// apt, the update channel), which do not take a destination from the client.
// ---------------------------------------------------------------------------

const (
	// outboundFetchTimeout caps a single upstream request end to end.
	outboundFetchTimeout = 30 * time.Second

	// outboundMaxResponseBytes caps how much of an upstream body is relayed.
	outboundMaxResponseBytes int64 = 10 * 1024 * 1024

	// outboundMaxReadBytes caps bodies we parse in-process (registry JSON).
	outboundMaxReadBytes int64 = 4 * 1024 * 1024

	// outboundMaxInFlight is how many upstream connections this process will
	// hold open for client-driven fetches at once.
	outboundMaxInFlight = 8

	// outboundQueueWait is how long a request waits for a slot before it is
	// told to retry. Chosen so a legitimate burst (a grid of icons) is
	// SERIALISED rather than rejected: at 8 concurrent fetches of a normal
	// upstream, a queued request is served long before this expires. It only
	// fires when every slot is stuck on a slow upstream, which is exactly the
	// case where failing fast beats piling up.
	outboundQueueWait = 15 * time.Second
)

// errOutboundBusy is the internal signal that no slot became free in time.
var errOutboundBusy = errors.New("outbound fetch capacity exhausted")

// outboundLimiter bounds concurrent upstream fetches with a counting
// semaphore. Deliberately NOT a per-caller rate limiter: a per-IP map is
// itself attacker-controlled state (it grows with distinct sources), and a
// previous rate limiter in this codebase cost more than the work it avoided.
// A fixed-size semaphore costs one channel operation per request, allocates
// nothing on the uncontended path, and its worst case is bounded by its own
// capacity rather than by how many distinct clients show up.
type outboundLimiter struct {
	slots chan struct{}
	wait  time.Duration
}

func newOutboundLimiter(inFlight int, wait time.Duration) *outboundLimiter {
	if inFlight < 1 {
		inFlight = 1
	}
	return &outboundLimiter{slots: make(chan struct{}, inFlight), wait: wait}
}

// acquire takes a slot. It returns ctx.Err() if the caller went away first and
// errOutboundBusy if the wait expired. Every successful acquire MUST be paired
// with exactly one release.
func (l *outboundLimiter) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(l.wait)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errOutboundBusy
	}
}

// release returns a slot taken by a successful acquire.
func (l *outboundLimiter) release() { <-l.slots }

// outboundFetchLimiter is the process-wide budget for client-driven fetches.
var outboundFetchLimiter = newOutboundLimiter(outboundMaxInFlight, outboundQueueWait)

// requireOutboundFetchCaller is the caller half of the guard.
//
// A permission check on its own is not enough here. Guest is the role a
// TOKENLESS request resolves to, and guest deliberately holds PermSystemRead
// so internal read paths keep working — so any gate built only on a read
// permission still admits the anonymous internet caller that the gateway
// presents to this process over loopback. Requiring a real login first
// (UserIDFromContext is 0 unless the JWT middleware validated a token) is what
// actually removes the anonymous case; the permission then decides which
// logged-in roles may spend the box's egress. Same shape as GetDiagnosis.
//
// Returns an *echo.HTTPError which the handler must RETURN, never one it has
// already written — a helper that denies with ctx.JSON hands back the
// serializer's nil error and the handler runs anyway (see requirePerm).
func requireOutboundFetchCaller(ctx echo.Context, p rbac.Permission) error {
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "authentication required",
		})
	}
	return requirePerm(ctx, p)
}

// acquireOutboundSlot reserves capacity for one upstream fetch and returns the
// release func. On refusal it returns an *echo.HTTPError to return as-is.
func acquireOutboundSlot(ctx echo.Context) (func(), error) {
	if err := outboundFetchLimiter.acquire(ctx.Request().Context()); err != nil {
		if errors.Is(err, errOutboundBusy) {
			ctx.Response().Header().Set("Retry-After", "5")
			return nil, echo.NewHTTPError(http.StatusServiceUnavailable, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "too many upstream fetches in flight, retry shortly",
			})
		}
		// Caller hung up or its deadline passed while queued.
		return nil, echo.NewHTTPError(http.StatusRequestTimeout, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "request cancelled while waiting for upstream capacity",
		})
	}
	return outboundFetchLimiter.release, nil
}

// isPrivateIP checks whether the given IP belongs to a private or internal range.
func isPrivateIP(ip net.IP) bool {
	// Unspecified (0.0.0.0 / ::) routes to loopback on Linux, so it must be
	// treated as private — otherwise ?url=http://0.0.0.0:PORT reaches the
	// loopback-only gateway/APIs, re-opening the exact block this enforces.
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	privateRanges := []string{
		"127.0.0.0/8",
		"0.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// outboundSafeTransport refuses to dial any address that resolves to a
// private/internal IP. The check happens at *connect* time, after DNS
// resolution, which closes the DNS-rebinding TOCTOU window: a stale
// pre-flight `net.LookupIP` answer cannot fool the dialer because the
// dialer re-resolves and inspects the actual destination IP.
//
// Shared by every client-driven fetch so a new caller cannot accidentally
// reach for a bare http.Client and lose the dial-time check.
var outboundSafeTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// Resolve through the same resolver the dialer would use.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		var firstErr error
		for _, ipa := range ips {
			if isPrivateIP(ipa.IP) {
				// Pretend the address is unreachable rather than leak
				// "this is private" to the caller — the same way a real
				// firewall would behave.
				firstErr = fmt.Errorf("destination address %s is not allowed", ipa.IP.String())
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			firstErr = err
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("no usable address for %s", host)
		}
		return nil, firstErr
	},
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          10,
	IdleConnTimeout:       30 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

// outboundSafeClient is the only http.Client that may be used for a fetch whose
// destination came from a client request.
var outboundSafeClient = &http.Client{
	Transport: outboundSafeTransport,
	Timeout:   outboundFetchTimeout,
	// Don't follow redirects automatically — a 302 to http://10.0.0.1
	// would re-trigger the dialer (which would block it), but we'd
	// rather just say "no" up front and not give the attacker hints
	// about what's reachable.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// validateOutboundURL is the destination half of the guard: scheme allow-list
// plus a pre-flight private-range refusal.
//
// The pre-flight resolve is a fast "no", NOT the security boundary — the
// dialer in outboundSafeTransport re-resolves at connect time and is what
// actually survives DNS rebinding. Refusals are returned as *echo.HTTPError
// carrying the same model.Result body the previous ctx.JSON refusals wrote, so
// the wire format is unchanged for callers that were allowed through before.
func validateOutboundURL(rawURL string) (*url.URL, error) {
	badRequest := func(msg string) error {
		return echo.NewHTTPError(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}

	if rawURL == "" {
		return nil, badRequest("url parameter is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, badRequest("invalid url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, badRequest("only http and https schemes are allowed")
	}
	if parsed.Host == "" {
		return nil, badRequest("invalid url")
	}

	ips, err := net.LookupIP(parsed.Hostname())
	if err != nil {
		return nil, badRequest("unable to resolve hostname")
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, echo.NewHTTPError(http.StatusForbidden, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "requests to private/internal addresses are not allowed",
			})
		}
	}
	return parsed, nil
}

// inlineSafeUpstreamTypes are the only upstream media types echoed back with
// their own label. Everything else is relabelled application/octet-stream.
//
// The list is a set of PASSIVE types. image/svg+xml is deliberately absent:
// an SVG is an active document (it can carry <script> and same-origin
// navigation to it executes), so relaying one under its own type would keep
// the XSS this guard exists to remove. text/* is absent for the same reason.
var inlineSafeUpstreamTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/avif":               true,
	"image/bmp":                true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"application/json":         true,
}

// writeInertUpstreamResponse is the response half of the guard: it relays the
// upstream body under headers that keep a browser from ever treating those
// bytes as an active document on CasaNova's origin.
//
// What it guarantees:
//   - the served Content-Type is either application/octet-stream or one entry
//     from inlineSafeUpstreamTypes — never the raw upstream label
//   - nosniff, so the type we chose is the type the browser uses
//   - Content-Disposition: attachment, so a top-level navigation downloads
//     instead of rendering (it does not affect <img>, which ignores it)
//   - a sandbox CSP, so if some client renders it anyway it does so in an
//     opaque origin with no access to CasaNova's DOM, storage or token
//
// What it does NOT guarantee: nothing about the trustworthiness of the bytes
// themselves. A caller that hands this body to a parser owns that risk.
//
// Content-Length is relayed only when upstream declared a length that fits
// under the limit. Copying it unconditionally while truncating at the limit
// (the previous behaviour) promised the client more bytes than were sent, so
// an oversized upstream produced a broken/hung response instead of a short one.
func writeInertUpstreamResponse(ctx echo.Context, resp *http.Response, limit int64) error {
	mediaType := "application/octet-stream"
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if parsed, _, err := mime.ParseMediaType(ct); err == nil {
			if lower := strings.ToLower(strings.TrimSpace(parsed)); inlineSafeUpstreamTypes[lower] {
				mediaType = lower
			}
		}
	}
	served := mediaType
	if served == "application/json" {
		served += "; charset=utf-8"
	}

	h := ctx.Response().Header()
	h.Set("Content-Type", served)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "attachment")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n >= 0 && n <= limit {
		h.Set("Content-Length", strconv.FormatInt(n, 10))
	}

	ctx.Response().WriteHeader(resp.StatusCode)
	_, err := io.Copy(ctx.Response(), io.LimitReader(resp.Body, limit))
	return err
}

// readUpstreamBody reads at most limit bytes of an upstream body. Used by
// callers that parse the response in-process instead of relaying it.
func readUpstreamBody(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
