package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

const RoutesFile = "routes.json"

// proxyTransport is the shared http.Transport used by every reverse
// proxy created via newConfiguredProxy. The default
// http.DefaultTransport has no ResponseHeaderTimeout, so a backend that
// accepts the connection but never writes a response header keeps the
// gateway goroutine pinned forever — exhausting fd's and blocking the
// client until the kernel TCP keepalive eventually fires (hours).
//
// Values are tuned for *local* backends only (every CasaNova service
// listens on 127.0.0.1):
//   - DialContext 5s: localhost dial should be sub-millisecond; 5s
//     leaves headroom for a service that's mid-restart.
//   - ResponseHeaderTimeout 60s: long enough for slow operations like
//     "list all docker images" but short enough that a hung daemon
//     surfaces as a 502 instead of an indefinite stall.
//   - IdleConnTimeout 90s and the rest mirror DefaultTransport so
//     connection reuse isn't degraded.
//
// We deliberately share one Transport across all proxies so the idle
// connection pool is shared too — otherwise each route would maintain
// its own pool and the gateway would hold N×M idle sockets.
var proxyTransport http.RoundTripper = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2: true,
	MaxIdleConns:      100,
	// v0.1: per-host caps. Without these, a misbehaving client (or
	// the polled stats endpoint under high install activity) could
	// open hundreds of concurrent connections to one local backend
	// before any of them returned, exhausting the file-descriptor
	// budget and dragging the whole gateway down. The number is
	// generous — even under sustained traffic we'd want at most a
	// few dozen concurrent calls per backend on a home server.
	MaxIdleConnsPerHost:   16,
	MaxConnsPerHost:       128,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
}

// newConfiguredProxy creates a reverse proxy for the given target URL with:
//   - ErrorHandler that logs failures and returns a useful 502 message
//   - Director that sets X-Forwarded-Host and X-Forwarded-Proto
//   - FlushInterval = -1 so streaming and WebSocket (101) responses work
//   - Shared Transport with explicit dial + response-header timeouts
func newConfiguredProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = proxyTransport

	// Wrap the default Director to add forwarding headers.
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		// Remove any client-supplied X-Forwarded-* headers to prevent
		// header injection attacks. Do not reflect the client's Host
		// header into X-Forwarded-Host.
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-For")
		// Strip identity headers — backends may set these from the JWT,
		// but a client must NEVER be able to spoof them. Multiple casing
		// variants are deleted because Go canonicalizes inconsistently
		// when keys contain underscores.
		req.Header.Del("user_id")
		req.Header.Del("User_id")
		req.Header.Del("User-Id")
		req.Header.Del("role")
		req.Header.Del("Role")
		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}

	// Log proxy errors instead of silently returning empty 502s.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("reverse proxy error",
			zap.String("path", r.URL.Path),
			zap.String("target", target.String()),
			zap.Error(err),
		)
		http.Error(w, fmt.Sprintf("Backend unavailable: %v", err), http.StatusBadGateway)
	}

	// Strip hop-by-hop headers from backend responses (defense-in-depth).
	// Exception: 101 Switching Protocols MUST keep Connection and Upgrade
	// headers — they are required for WebSocket handshake completion. The
	// browser will reject the upgrade if these are missing.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusSwitchingProtocols {
			return nil
		}
		hopByHop := []string{
			"Connection", "Keep-Alive", "Proxy-Authenticate",
			"Proxy-Authorization", "TE", "Trailer",
			"Transfer-Encoding", "Upgrade",
		}
		for _, h := range hopByHop {
			resp.Header.Del(h)
		}
		return nil
	}

	// COPY BUFFER: 256 KiB from a shared pool, not the 32 KiB ReverseProxy
	// allocates per request.
	//
	// httputil.copyBuffer does `if len(buf) == 0 { buf = make([]byte, 32*1024) }`,
	// so with no BufferPool every proxied response both allocates a fresh 32 KiB
	// buffer AND moves the body in 32 KiB steps. Measured on the production box,
	// downloading a 512 MiB file from CasaNova through this gateway:
	//
	//	direct to the backend   3.18 GB/s
	//	through the gateway     1.55 GB/s
	//
	// The gateway HALVED throughput. Measuring at 64, 128 and 512 MiB gave a
	// constant 10.7-11.7 us of overhead per 32 KiB chunk across an 8x range —
	// a per-chunk cost, not per-request setup. In CPU terms the gateway burned
	// 867 ms per GiB it merely forwarded.
	//
	// That per-chunk cost is mostly the FLUSH, not the buffer: see the
	// FlushInterval note below. The pool is here for the allocation, which is
	// 32 KiB of garbage per RESPONSE — including every 200-byte JSON poll,
	// because copyBuffer takes the buffer once per response regardless of body
	// size.
	//
	// 64 KiB, and the reason is measured rather than intuitive. A bigger buffer
	// does NOT proportionally reduce the write count, because copyBuffer does
	// src.Read(buf) and a socket returns what it has, not a full buffer — the
	// same config measured 577 and 1003 writes on consecutive runs of an
	// identical 32 MiB body. What the buffer size DOES control is garbage, and
	// there the curve turns around, because sync.Pool is drained at every GC
	// and a drained large pool re-allocates a large buffer:
	//
	//	              1 KiB response      32 MiB response
	//	no pool        46,559 B/op         81,019 B/op
	//	64 KiB         15,250 B/op         49,321 B/op   <- best on both
	//	256 KiB        21,756 B/op         62,180 B/op
	//
	// 64 KiB is the only size measured better than the status quo on BOTH
	// populations the gateway actually carries: the 1-3 req/s of small JSON
	// polls, and the occasional bulk transfer.
	proxy.BufferPool = proxyBuffers

	// FLUSH: let the ResponseWriter batch, EXCEPT where Go knows it must not.
	//
	// This was -1ms, which means "flush after every single write" — 16384 flush
	// calls for that same 512 MiB file. The comment said it existed to enable
	// streaming and WebSocket pass-through. Verified against the Go 1.24 source
	// rather than assumed, and neither actually depends on it:
	//
	//   - reverseproxy.go flushInterval() OVERRIDES this field, returning -1
	//     when Content-Type is text/event-stream (SSE) or when
	//     res.ContentLength == -1 (chunked / unknown length — every streaming
	//     response). Our install-progress SSE and log streams hit both.
	//   - A 101 Switching Protocols response is handed to handleUpgradeResponse
	//     and RETURNS before copyResponse is ever reached (reverseproxy.go:495
	//     vs :520), so WebSocket never consults this field at all.
	//
	// So 0 keeps streaming immediate where it matters and stops paying a flush
	// per 32 KiB on bulk transfers. It also avoids the maxLatencyWriter timer
	// goroutine that any non-zero value starts per response.
	proxy.FlushInterval = 0

	return proxy
}

// proxyBuffers is the shared copy-buffer pool for every reverse proxy.
//
// Shared rather than per-proxy for the same reason proxyTransport is: a pool
// per route would multiply the resident buffers by the number of routes while
// each one stayed mostly cold.
var proxyBuffers = &bufferPool{
	pool: sync.Pool{New: func() any {
		b := make([]byte, proxyCopyBufferSize)
		return &b
	}},
}

// proxyCopyBufferSize is the unit in which a proxied body moves. See the
// measurement at proxy.BufferPool above before changing it — the win is
// proportional to how many times per body the copy loop runs.
const proxyCopyBufferSize = 64 * 1024

// bufferPool adapts sync.Pool to httputil.BufferPool.
//
// It stores *[]byte rather than []byte on purpose: putting a slice into a
// sync.Pool allocates, because the slice header has to escape to the interface.
// The pointer does not.
type bufferPool struct{ pool sync.Pool }

func (b *bufferPool) Get() []byte    { return *(b.pool.Get().(*[]byte)) }
func (b *bufferPool) Put(buf []byte) { b.pool.Put(&buf) }

// whitelistFile is the persistent path the global IP allow-list lives
// at. Outside the gateway's tmpfs runtime dir so a reboot doesn't wipe
// the operator's configuration.
const whitelistFile = "/etc/casanova/whitelist.json"

type Management struct {
	mu                  sync.RWMutex
	pathTargetMap       map[string]string
	pathReverseProxyMap map[string]*httputil.ReverseProxy
	// Global IP allow-list. Empty list = no restriction. Separate from
	// the per-route policy because it applies to every request before route
	// dispatch and persists to a different file (survives reboots).
	globalWhitelist []*net.IPNet

	// routePolicy owns the per-route IP whitelists. It is a separate,
	// durable store rather than a field of the route registration on
	// purpose — see route_policy.go. `mu` above is never held for writing
	// while calling into it; lock order is always Management.mu → store.mu.
	routePolicy *routePolicyStore

	State *State
}

func NewManagementService(state *State) *Management {
	routesFilepath := filepath.Join(state.GetRuntimePath(), RoutesFile)

	// routes.json is the AVAILABILITY half of a route: path → target. It is
	// rebuilt automatically by every service's registration, and it lives in
	// a tmpfs runtime directory, so losing it is harmless.
	//
	// It also still carries per-route CIDR whitelists, because releases
	// between Wave 33 and this one persisted them there. Those are read once
	// here and MIGRATED into the durable policy store below; from then on the
	// store is authoritative and routes.json is only a mirror.
	pathTargetMap, legacyAllowedNetsMap, err := loadPathTargetMapFrom(routesFilepath)
	if err != nil {
		logger.Error("Failed to load routes", zap.Any("error", err), zap.Any("filepath", routesFilepath))
		pathTargetMap = make(map[string]string)
		legacyAllowedNetsMap = make(map[string][]*net.IPNet)
	}

	pathReverseProxyMap := make(map[string]*httputil.ReverseProxy)

	for path, target := range pathTargetMap {
		targetURL, err := url.Parse(target)
		if err != nil {
			logger.Error("Failed to parse target", zap.Any("error", err), zap.String("target", target))
			continue
		}
		pathReverseProxyMap[path] = newConfiguredProxy(targetURL)
	}

	routePolicy := newRoutePolicyStore(routePolicyFile)
	// Migration is idempotent: it only fills in routes the store has no
	// decision on record for, so a whitelist an admin explicitly removed is
	// never resurrected from a stale routes.json.
	if _, err := routePolicy.AdoptLegacy(legacyAllowedNetsMap); err != nil {
		logger.Error("failed to migrate per-route policy out of routes.json",
			zap.String("invariant", invRoutePolicyDurable),
			zap.Error(err))
	}
	if h := routePolicy.Health(); !h.OK {
		logger.Error("per-route authorization policy is DEGRADED",
			zap.String("invariant", h.FailedInvariant),
			zap.String("detail", h.Detail),
			zap.String("path", h.Path))
	}

	return &Management{
		pathTargetMap:       pathTargetMap,
		pathReverseProxyMap: pathReverseProxyMap,
		globalWhitelist:     loadGlobalWhitelist(),
		routePolicy:         routePolicy,
		State:               state,
	}
}

// loadGlobalWhitelist reads the persisted CIDR list. Returns nil when
// the file is missing — equivalent to "no restriction" so a fresh box is
// reachable from anywhere by default.
func loadGlobalWhitelist() []*net.IPNet {
	data, err := os.ReadFile(whitelistFile)
	if err != nil {
		return nil
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Error("Failed to parse whitelist", zap.Error(err))
		return nil
	}
	out := make([]*net.IPNet, 0, len(raw))
	for _, s := range raw {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func saveGlobalWhitelist(nets []*net.IPNet) error {
	raw := make([]string, 0, len(nets))
	for _, n := range nets {
		raw = append(raw, n.String())
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(whitelistFile), 0o755); err != nil {
		return err
	}
	tmp := whitelistFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, whitelistFile)
}

// GetWhitelist returns the current allow-list as []string for the API.
// Empty slice signals "no restriction" to the caller (and to the proxy
// middleware) — the file format and the in-memory representation agree.
func (g *Management) GetWhitelist() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.globalWhitelist))
	for _, n := range g.globalWhitelist {
		out = append(out, n.String())
	}
	return out
}

// AddWhitelist appends a CIDR (or single IP) to the allow-list. Single
// IPs get a /32 (or /128) so the persisted format is always CIDR.
// Duplicates are silent no-ops so the UI can be idempotent.
func (g *Management) AddWhitelist(cidr string) error {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return fmt.Errorf("cidr is empty")
	}
	// Promote bare IPs to a host-mask CIDR so we can parse uniformly.
	if !strings.Contains(cidr, "/") {
		if ip := net.ParseIP(cidr); ip != nil {
			if ip.To4() != nil {
				cidr += "/32"
			} else {
				cidr += "/128"
			}
		}
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, existing := range g.globalWhitelist {
		if existing.String() == n.String() {
			return nil
		}
	}
	g.globalWhitelist = append(g.globalWhitelist, n)
	return saveGlobalWhitelist(g.globalWhitelist)
}

// RemoveWhitelist drops a CIDR from the allow-list. Unknown entries are
// a no-op so a double-delete from the UI doesn't error.
func (g *Management) RemoveWhitelist(cidr string) error {
	cidr = strings.TrimSpace(cidr)
	g.mu.Lock()
	defer g.mu.Unlock()
	next := make([]*net.IPNet, 0, len(g.globalWhitelist))
	for _, n := range g.globalWhitelist {
		if n.String() != cidr {
			next = append(next, n)
		}
	}
	g.globalWhitelist = next
	return saveGlobalWhitelist(g.globalWhitelist)
}

// IsAllowedGlobal reports whether the supplied remote address satisfies
// the global allow-list. An empty allow-list means "everything allowed",
// preserving fresh-install reachability.
//
// Wave 42: (1) this is now actually CALLED — coreHandler enforces it before
// dispatch; it was a dead control before. (2) Loopback is ALWAYS allowed
// regardless of the list, so a whitelist scoped to the LAN (e.g.
// 192.168.1.0/24) can't lock the box's own daemons / health-checks (which
// reach the gateway over 127.0.0.1) out. (3) A nil ip (unparseable client)
// fails closed when a whitelist is active.
func (g *Management) IsAllowedGlobal(ip net.IP) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.globalWhitelist) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range g.globalWhitelist {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// policyIntent says whether a route payload is talking about authorization
// at all. This is the distinction the previous code did not have, and its
// absence is what let an availability re-registration delete a whitelist.
type policyIntent int

const (
	// policyIntentNone: no allowed_cidrs field in the payload (JSON absent or
	// null → nil slice). This is what every service registration and the
	// 30s availability watchdog send. Existing policy is PRESERVED.
	policyIntentNone policyIntent = iota
	// policyIntentClear: an explicit empty list (`"allowed_cidrs": []`).
	// Only an administrator writing the field on purpose can express this.
	// Note the Go client cannot: model.Route tags the field `omitempty`, so
	// a service registration is structurally incapable of clearing policy.
	policyIntentClear
	// policyIntentSet: a non-empty list → authoritative replacement.
	policyIntentSet
)

func policyIntentOf(route *model.Route) policyIntent {
	if route.AllowedCIDRs == nil {
		return policyIntentNone
	}
	if len(route.AllowedCIDRs) == 0 {
		return policyIntentClear
	}
	return policyIntentSet
}

// CreateRoute registers a route's AVAILABILITY (path → target) and, only when
// the payload explicitly says something about authorization, applies that too.
//
// The guarantee, stated exactly as implemented: a payload with no
// allowed_cidrs field leaves the route's authorization policy untouched. It is
// NOT a full no-op — the target and its proxy are refreshed, which is the
// point of re-registration.
func (g *Management) CreateRoute(route *model.Route) error {
	parsedURL, err := url.Parse(route.Target)
	if err != nil {
		return err
	}

	intent := policyIntentOf(route)

	// Validate policy BEFORE touching any state, so a bad CIDR changes
	// nothing at all rather than half-applying.
	if intent == policyIntentSet {
		for _, cidr := range route.AllowedCIDRs {
			if _, _, parseErr := net.ParseCIDR(cidr); parseErr != nil {
				return parseErr
			}
		}
	}

	g.mu.Lock()
	g.pathTargetMap[route.Path] = route.Target
	g.pathReverseProxyMap[route.Path] = newConfiguredProxy(parsedURL)
	// BUG 3 fix: snapshot the map under the lock so savePathTargetMapTo does
	// not read the live map after Unlock.
	targetSnapshot := make(map[string]string, len(g.pathTargetMap))
	for k, v := range g.pathTargetMap {
		targetSnapshot[k] = v
	}
	g.mu.Unlock()

	// Apply the administrative half only when it was asked for.
	var policyErr error
	if intent != policyIntentNone {
		policyErr = g.routePolicy.Set(route.Path, route.AllowedCIDRs)
	}

	// routes.json keeps mirroring the CIDRs. It is no longer the authority
	// (the policy store is), but the mirror keeps the file meaningful to an
	// operator reading it and lets a rollback to an older gateway still find
	// the whitelists. It is always written from the store, so it can never
	// resurrect a policy the store says was removed.
	routesFilePath := filepath.Join(g.State.GetRuntimePath(), RoutesFile)
	saveErr := savePathTargetMapTo(routesFilePath, targetSnapshot, g.routePolicy.Snapshot())

	return errors.Join(policyErr, saveErr)
}

// SetRoutePolicy is the explicit administrative operation: it replaces the
// authorization policy of a route outright, and a nil/empty list means "no
// restriction" and is recorded as a decision. Unlike CreateRoute it says
// nothing about the route's target, so it cannot be reached accidentally by
// an availability mechanism.
func (g *Management) SetRoutePolicy(routePath string, cidrs []string) error {
	if err := g.routePolicy.Set(routePath, cidrs); err != nil {
		return err
	}

	g.mu.RLock()
	targetSnapshot := make(map[string]string, len(g.pathTargetMap))
	for k, v := range g.pathTargetMap {
		targetSnapshot[k] = v
	}
	g.mu.RUnlock()

	routesFilePath := filepath.Join(g.State.GetRuntimePath(), RoutesFile)
	return savePathTargetMapTo(routesFilePath, targetSnapshot, g.routePolicy.Snapshot())
}

// GetRoutePolicy returns the CIDR list on record for a route and whether any
// decision was ever recorded for it.
func (g *Management) GetRoutePolicy(routePath string) ([]string, bool) {
	return g.routePolicy.CIDRsFor(routePath)
}

// RoutePolicyHealth exposes whether per-route authorization policy is in a
// state the gateway can vouch for, naming the failed invariant when not.
func (g *Management) RoutePolicyHealth() RoutePolicyHealth {
	return g.routePolicy.Health()
}

// pathHasPrefix returns true iff prefix is a *path-segment* prefix of path.
// Plain strings.HasPrefix lets "/v1" match "/v1foo", which would route a
// request meant for an unrelated endpoint into the wrong upstream — both
// a routing correctness bug and (when whitelists are involved) an access-
// control bypass: the wrong route's whitelist is applied. Allow either an
// exact match, a path that continues with "/", or a prefix that already
// terminates at "/" (handles the root "/" case cleanly).
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+"/")
}

// IsIPAllowedForPath returns true if the given client IP is permitted to
// access the route matching `path`. Returns true (allowed) if the route has
// no whitelist configured (default/backward-compatible).
func (g *Management) IsIPAllowedForPath(path, clientIP string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Find the matching route by longest-prefix, same as GetProxy.
	paths := getSortedKeys(g.pathReverseProxyMap)
	var matched string
	for _, p := range paths {
		if pathHasPrefix(path, p) {
			matched = p
			break
		}
	}
	if matched == "" {
		return true // no matching route — let the proxy 404 handle it
	}

	acl, declared := g.routePolicy.Lookup(matched)
	if !declared {
		return true // no decision on record = everyone allowed
	}
	// acl.allows treats an explicitly emptied list as unrestricted and an
	// unreconstructable one as deny-all; see routeACL.
	return acl.allows(net.ParseIP(clientIP))
}

func (g *Management) GetRoutes() []*model.Route {
	g.mu.RLock()
	defer g.mu.RUnlock()

	routes := make([]*model.Route, 0, len(g.pathTargetMap))
	for path, target := range g.pathTargetMap {
		r := &model.Route{
			Path:   path,
			Target: target,
		}
		// Reflect the whitelist back to the client so the UI can show which
		// routes are restricted. A route with no decision on record, and one
		// whose restriction an admin explicitly removed, both come back with
		// AllowedCIDRs nil — which is also the payload shape that means "do
		// not touch policy" on the way back in, so a read-modify-write client
		// cannot delete a whitelist by echoing what it read.
		if cidrs, ok := g.routePolicy.CIDRsFor(path); ok && len(cidrs) > 0 {
			r.AllowedCIDRs = cidrs
		}
		routes = append(routes, r)
	}
	return routes
}

func (g *Management) GetProxy(path string) *httputil.ReverseProxy {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// sort paths by length in descending order
	// (without this step, a path like "/abcd" can potentially be matched with "/ab")
	// Use pathHasPrefix so the match respects path-segment boundaries: /v1
	// must not match /v1foo. See pathHasPrefix above for why.
	paths := getSortedKeys(g.pathReverseProxyMap)
	for _, p := range paths {
		if pathHasPrefix(path, p) {
			return g.pathReverseProxyMap[p]
		}
	}
	return nil
}

func (g *Management) GetGatewayPort() string {
	return g.State.GetGatewayPort()
}

func (g *Management) SetGatewayPort(port string) error {
	if err := g.State.SetGatewayPort(port); err != nil {
		return err
	}

	return nil
}

func getSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for key := range m {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	return keys
}

// persistedRoute is the on-disk JSON shape for a single route. The CIDR
// list is intentionally a slice of strings rather than `*net.IPNet` so
// the file remains human-readable and editable, and so we can
// re-validate on load instead of trusting whatever blob is on disk.
type persistedRoute struct {
	Target       string   `json:"target"`
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"`
}

// loadPathTargetMapFrom reads routes.json and returns both the
// target map and the per-route CIDR whitelist map. It accepts two
// on-disk formats so an existing routes.json from before Wave 33
// (a flat `map[string]string`) still loads cleanly:
//
//  1. New: { "path": { "target": "...", "allowed_cidrs": ["..."] } }
//  2. Old: { "path": "target" }
//
// Format detection is by attempting the rich shape first; on failure
// we fall back to the legacy shape. Anything else is a hard error so
// a corrupt file does not silently boot the gateway with no routes.
func loadPathTargetMapFrom(routesFilepath string) (map[string]string, map[string][]*net.IPNet, error) {
	content, err := os.ReadFile(routesFilepath)
	if err != nil {
		return nil, nil, err
	}

	pathTargetMap := make(map[string]string)
	pathAllowedNetsMap := make(map[string][]*net.IPNet)

	// Try the rich format first.
	rich := make(map[string]persistedRoute)
	if err := json.Unmarshal(content, &rich); err == nil && isRichShape(content) {
		for path, entry := range rich {
			pathTargetMap[path] = entry.Target
			if len(entry.AllowedCIDRs) == 0 {
				continue
			}
			nets := make([]*net.IPNet, 0, len(entry.AllowedCIDRs))
			for _, cidr := range entry.AllowedCIDRs {
				_, ipnet, parseErr := net.ParseCIDR(cidr)
				if parseErr != nil {
					logger.Error("dropping invalid CIDR from persisted routes",
						zap.String("path", path),
						zap.String("cidr", cidr),
						zap.Error(parseErr))
					continue
				}
				nets = append(nets, ipnet)
			}
			if len(nets) > 0 {
				pathAllowedNetsMap[path] = nets
			}
		}
		return pathTargetMap, pathAllowedNetsMap, nil
	}

	// Fall back to the legacy flat shape.
	if err := json.Unmarshal(content, &pathTargetMap); err != nil {
		return nil, nil, err
	}
	return pathTargetMap, pathAllowedNetsMap, nil
}

// isRichShape returns true iff the JSON document at the top level is
// an object whose first value is itself an object — i.e. the rich
// `{path: {target, allowed_cidrs}}` format. We need this because
// `json.Unmarshal` into `map[string]persistedRoute` happily succeeds
// against the legacy `map[string]string` format with empty Targets,
// which would silently zero out every route's target on first boot
// after upgrade.
func isRichShape(content []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return false
	}
	for _, v := range raw {
		t := strings.TrimSpace(string(v))
		return strings.HasPrefix(t, "{")
	}
	// Empty object — treat as rich so we save in the new format next.
	return true
}

func savePathTargetMapTo(routesFilepath string, pathTargetMap map[string]string, pathAllowedCIDRsMap map[string][]string) error {
	rich := make(map[string]persistedRoute, len(pathTargetMap))
	for path, target := range pathTargetMap {
		rich[path] = persistedRoute{
			Target:       target,
			AllowedCIDRs: pathAllowedCIDRsMap[path],
		}
	}
	content, err := json.Marshal(rich)
	if err != nil {
		return err
	}

	// DO NOT REWRITE AN IDENTICAL FILE.
	//
	// Every service re-registers all of its route prefixes every 30 seconds
	// (CasaNova-Common/external/route_watchdog.go). Across the nine services
	// that is 51 CreateRoute calls per 30 s — about 146,880 a day — and the
	// content is byte-for-byte identical every time, because re-registration
	// is an availability mechanism, not a change.
	//
	// Measured: the atomic durable write below costs 2.63 ms (temp file, write,
	// fsync, rename). Marshalling and comparing costs 30-60 us. The box was
	// spending roughly 6.4 minutes a day fsyncing a file that had not changed,
	// and writing it to the SSD to say nothing.
	//
	// The guard checks CONTENT AND THE FILE ON DISK, not content alone: if
	// routes.json is deleted or truncated underneath us, the size check misses
	// and we rewrite it. A cache that cannot notice its own file disappearing
	// would turn a cheap optimisation into a missing routes.json after a
	// manual cleanup.
	lastWritten.mu.Lock()
	if lastWritten.content != nil && bytes.Equal(lastWritten.content, content) {
		if fi, statErr := os.Stat(routesFilepath); statErr == nil && fi.Size() == lastWritten.size {
			lastWritten.mu.Unlock()
			return nil
		}
	}
	lastWritten.mu.Unlock()

	// Atomic durable write: write to a temp file, fsync the data to disk,
	// then rename. os.WriteFile does not fsync, so on a crash the rename
	// can land in the directory entry while the file's contents are still
	// in the kernel page cache — leaving routes.json visible but empty
	// after a power loss. The fsync before rename closes that window.
	tmp := routesFilepath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, routesFilepath); err != nil {
		return err
	}

	// Recorded only after the rename succeeded. Recording it earlier would let
	// a failed write convince every later call that the file is already right.
	lastWritten.mu.Lock()
	lastWritten.content = content
	lastWritten.size = int64(len(content))
	lastWritten.mu.Unlock()
	return nil
}

// lastWritten remembers what routes.json was last persisted with, so an
// unchanged re-registration costs a comparison instead of a durable write.
var lastWritten struct {
	mu      sync.Mutex
	content []byte
	size    int64
}
