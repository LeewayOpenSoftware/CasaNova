package route

// ZimaOS-client LAN connect gate.
//
// The ZimaOS mobile client (connect-by-IP) probes a HARD-CODED TCP port
// 9527: it does `GET http://<ip>:9527/` and deserializes the raw response
// body straight into its flat DeviceInfo model. If that parse throws it
// shows "IP address cannot connect" and never proceeds. Our gateway serves
// the UI HTML at "/" on the web port (e.g. :85), so we cannot answer the
// probe from there — we bind :9527 ourselves and return the flat device
// document the client accepts.
//
// Naming: this is casanova-generic code. The only verbatim on-the-wire
// facts are the client's hard-coded port (9527), the flat JSON field names,
// and the deliberate OMISSION of `ip4` — the client types ip4 as a list of
// objects, so returning a list of strings throws in its JSON parser. See the
// memory note `zima-client-connect-contract` for the full teardown.
//
// The listener is unauthenticated by design (a bare native client can't
// present a JWT before it knows the box exists). To keep that surface tight
// it discloses ONLY the primary LAN address (never docker/bridge subnets),
// sends no wildcard-CORS header (the client isn't a browser), and caches the
// LAN address + gateway port so a request flood can't amplify into blocking
// management-API calls or an interface scan per request.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"go.uber.org/zap"
)

// zimaGatePort is the client's hard-coded probe port. ponytail: only change
// if a newer client generation probes a different port.
const zimaGatePort = "9527"

// Cached network facts. lanIPv4()/gatewayPublicPort() do a UDP-route lookup +
// a blocking loopback HTTP call; we compute them once and refresh on a ticker
// so an unauthenticated :9527 flood can't amplify per-request work.
var (
	deviceNetMu   sync.RWMutex
	cachedLANIPv4 []string
	cachedGWPort  int
)

func deviceNet() ([]string, int) {
	deviceNetMu.RLock()
	ips, port := cachedLANIPv4, cachedGWPort
	deviceNetMu.RUnlock()
	if len(ips) > 0 && port > 0 {
		return ips, port
	}
	return refreshDeviceNet()
}

func refreshDeviceNet() ([]string, int) {
	ips := lanIPv4()
	port := gatewayPublicPort()
	deviceNetMu.Lock()
	if len(ips) > 0 {
		cachedLANIPv4 = ips
	}
	if port > 0 {
		cachedGWPort = port
	}
	ips, port = cachedLANIPv4, cachedGWPort
	deviceNetMu.Unlock()
	return ips, port
}

// zimaDeviceInfoFlat builds the flat device document the ZimaOS client parses
// into its DeviceInfo model. Shared by the :9527 gate and the /v2/zimaos/device/info
// HTTP handler so both agree.
//
// Contract-critical:
//   - FLAT (no {"data":{...}} envelope) — the client parses the raw body.
//   - `initialized` must be a real JSON bool true (routes the client to login).
//   - `lan_ipv4` (list of strings) + `port` build the client's post-connect
//     base URL — point them at this box + the gateway's public port.
//   - `ip4` is intentionally ABSENT: the client types it as a list of objects.
func zimaDeviceInfoFlat() map[string]any {
	host, _ := os.Hostname()
	if host == "" {
		host = "casanova"
	}
	ips, port := deviceNet()
	return map[string]any{
		"device_name":        host,
		"hostname":           host,
		"device_model":       "CasaNova",
		"model":              "CasaNova",
		"device_code":        "casanova",
		"serial":             host,
		"hash":               host + "-casanova",
		"os_version":         compatOSVersion,
		"version":            compatOSVersion,
		"firmware":           compatOSVersion,
		"arch":               runtime.GOARCH,
		"architectures":      []string{runtime.GOARCH},
		"role":               "admin",
		"initialized":        true,
		"shutdown_mode":      "shutdown",
		"device_image":       "",
		"device_image_path":  "",
		"driver":             "",
		"driverversion":      "",
		"min_client_version": "0.1.0",
		"lan_ipv4":           ips,
		"port":               port,
	}
}

// lanIPv4 returns ONLY the box's primary LAN IPv4 address (as a single-element
// list — the client uses lan_ipv4[0]). We resolve the source IP the kernel
// would use for an off-box destination (no packet is sent — UDP "connect" just
// picks the default-route source), which is the real physical LAN address, not
// a docker/bridge subnet. Disclosing the whole interface list on an unauth
// listener would leak internal topology, so we deliberately return just one.
func lanIPv4() []string {
	if c, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		la, ok := c.LocalAddr().(*net.UDPAddr)
		_ = c.Close()
		if ok {
			if ip4 := la.IP.To4(); ip4 != nil {
				return []string{ip4.String()}
			}
		}
	}
	// Fallback: the first non-loopback IPv4 (single — never the full list).
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return []string{ip4.String()}
				}
			}
		}
	}
	return []string{}
}

// gatewayPublicPort asks the gateway for its live public port (where the UI +
// API are served). The management API answers /v1/gateway/port with a
// {"success":200,"message":"ok","data":"85"} envelope, so we parse `data`.
// Falls back to 80 (the ZimaOS/CasaOS default) if the gateway is unreachable.
func gatewayPublicPort() int {
	if service.MyService != nil && service.MyService.Gateway() != nil {
		if err, body := service.MyService.Gateway().GetPort(); err == nil {
			var env struct {
				Data string `json:"data"`
			}
			if json.Unmarshal([]byte(body), &env) == nil {
				if n, e := strconv.Atoi(strings.TrimSpace(env.Data)); e == nil && n > 0 {
					return n
				}
			}
			// Also accept a bare numeric body, just in case.
			if n, e := strconv.Atoi(strings.TrimSpace(body)); e == nil && n > 0 {
				return n
			}
		}
	}
	return 80
}

// StartZimaClientGate binds :9527 and answers the client's device probe on
// every path. Runs until ctx is cancelled. A bind conflict is logged, not
// fatal — the main dashboard must never go down because :9527 was taken.
func StartZimaClientGate(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("zima client gate panicked", zap.Any("recover", r))
		}
	}()

	// Warm the cache once, then refresh periodically so a request never
	// triggers the blocking GetPort / interface scan inline.
	refreshDeviceNet()
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshDeviceNet()
			}
		}
	}()

	handler := func(w http.ResponseWriter, r *http.Request) {
		// No Access-Control-Allow-Origin: the ZimaOS client is a native app,
		// not a browser — a wildcard here would let any web page the LAN user
		// visits read the box's identity + port cross-origin (drive-by recon).
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(zimaDeviceInfoFlat()); err != nil {
			logger.Error("zima gate encode error", zap.Error(err))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler) // catch-all: any probe on :9527 gets device info

	srv := &http.Server{
		Addr:              ":" + zimaGatePort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("zima client gate listening", zap.String("port", zimaGatePort))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("zima client gate stopped (port likely in use)", zap.Error(err))
	}
}

// ---------------------------------------------------------------------------
// Discovery admission control (CN-DOS-07).
//
// The UDP :9527 responder used to answer EVERY datagram it received, which is
// two bugs in one packet:
//
//	(a) reflection/amplification — UDP source addresses are trivially forged,
//	    so an attacker sent a ~20-byte probe with the victim's IP as source and
//	    we mailed the victim a ~700-byte DeviceInfo. No rate limit, so the same
//	    forged datagram replayed in a loop turned this box into an amplifier;
//	(b) information disclosure — hostname, model, OS version and the box's LAN
//	    address went to anyone who asked. This box is reachable from the
//	    internet through a Cloudflare tunnel, so "anyone" is not hypothetical.
//
// Invariant enforced below: we only ever answer a source address that lives in
// one of this box's directly-connected IPv4 subnets (a real layer-2 neighbour),
// at most a small bounded rate per source IP, and never more than a bounded
// TOTAL rate box-wide. The last clause is not redundant: a per-IP table alone
// hands every unseen key a full fresh burst, so an attacker rotating forged
// source addresses was never rate limited by it — only the aggregate ceiling
// bounds that.
//
// Why this does not break the product: the ZimaOS Windows/Android clients find
// the box by broadcasting to <subnet-broadcast>:9527, which by definition only
// reaches boxes on the same L2 segment — a legitimate probe's source address is
// always inside one of our own subnets. And a forged source outside those
// subnets could never have been a real client anyway: the reply was being sent
// somewhere the packet did not come from. The payload is untouched; only the
// "who" and "how often" changed.
// ---------------------------------------------------------------------------

const (
	// Connected subnets are re-read at most this often. Interface enumeration
	// is a syscall; doing it per datagram would hand an attacker a cheap CPU
	// amplifier, which is the class of bug we are closing.
	zimaNeighborTTL = 60 * time.Second

	// Token bucket per source IP. Burst absorbs a normal client's retry salvo
	// (clients broadcast once per interface and re-probe while their scan
	// screen is open); refill keeps a steady scan working forever at 1/s while
	// capping sustained amplification at one small datagram per second per IP.
	zimaProbeBurst  = 5.0
	zimaProbeRefill = 1.0 // tokens per second

	// Bound on the limiter's memory. See evictLocked: reaching the cap evicts,
	// it never denies — a limiter that starts refusing unknown IPs under
	// pressure would break discovery exactly when the LAN is busy.
	zimaProbeMaxIPs = 4096

	// Box-wide ceiling on discovery replies, regardless of source address.
	//
	// This is the ONLY thing that actually stops an attacker who rotates source
	// addresses: a per-source bucket hands every unseen key a full fresh burst,
	// so before this a forged-source flood was never rate limited at all — it
	// just paid for a table eviction per packet and got its reply anyway.
	//
	// Sizing vs. a LEGITIMATE flow: a real LAN discovery burst is a handful of
	// clients broadcasting once per interface and re-probing ~1/s while their
	// scan screen is open. Even 20 simultaneous clients sit an order of
	// magnitude under 50 replies/s, so this ceiling is invisible to real
	// discovery while capping outbound amplification at ~50 small datagrams/s.
	zimaProbeTotalBurst  = 100.0
	zimaProbeTotalRefill = 50.0 // replies per second, whole box

	// Admission budget for PREVIOUSLY-UNSEEN sources, deliberately kept below
	// the total ceiling. A rotating/forged-source flood only ever spends this
	// sub-budget, so the remainder of the total stays available to neighbours
	// already in the table — i.e. the client that is mid-scan keeps getting
	// answered while the flood is in progress.
	zimaProbeNewBurst  = 40.0
	zimaProbeNewRefill = 20.0 // new sources admitted per second

	// Eviction is done in BOUNDED batches: reaching the cap frees this many
	// entries in one pass, so the next zimaProbeEvictBatch insertions are free
	// (O(1) amortised). zimaProbeEvictScan caps the work spent hunting for idle
	// entries before we fall back to dropping an arbitrary sample.
	zimaProbeEvictBatch = 64
	zimaProbeEvictScan  = 128
)

var (
	neighborMu      sync.Mutex
	neighborNets    []*net.IPNet
	neighborFetched time.Time

	// netInterfaces is indirected only so the failure paths of
	// connectedIPv4Nets (netlink error / nothing enumerated) can be driven in a
	// test — those are the paths that used to re-scan per datagram.
	netInterfaces = net.Interfaces
)

// connectedIPv4Nets returns the IPv4 subnets directly attached to this box's
// up interfaces (loopback included: a probe from 127.0.0.1 can only originate
// on-box). Cached for zimaNeighborTTL.
//
// The cache is keyed on neighborFetched ALONE, not on "we have a non-empty
// result". Both failure paths below (netlink error, nothing enumerated) stamp
// neighborFetched too. Previously they returned early WITHOUT stamping, so
// while the box was in that state — a boot race with the LAN not up yet, or a
// transient netlink failure — every single datagram re-enumerated every
// interface and every address. That is exactly the per-datagram syscall
// amplifier zimaNeighborTTL exists to prevent, armed precisely when the box is
// already in trouble. Caching the failure keeps isNeighborIPv4's documented
// fallback policy (private/link-local/loopback only) in effect for the TTL
// instead of paying for it per packet.
func connectedIPv4Nets() []*net.IPNet {
	neighborMu.Lock()
	defer neighborMu.Unlock()
	if !neighborFetched.IsZero() && time.Since(neighborFetched) < zimaNeighborTTL {
		return neighborNets
	}

	ifaces, err := netInterfaces()
	if err != nil {
		// Keep whatever we had; a transient enumeration failure must not
		// silently flip the responder's policy. Stamp the clock so we retry in
		// zimaNeighborTTL rather than on the next datagram.
		neighborFetched = time.Now()
		return neighborNets
	}
	nets := make([]*net.IPNet, 0, 8)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aErr := iface.Addrs()
		if aErr != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			mask := ipnet.Mask
			if len(mask) == net.IPv6len {
				mask = mask[12:] // IPv4-mapped mask -> 4-byte form
			}
			if len(mask) != net.IPv4len {
				continue
			}
			nets = append(nets, &net.IPNet{IP: ip4.Mask(mask), Mask: mask})
		}
	}
	if len(nets) == 0 {
		// Same reasoning as the error path: keep the last good answer, but do
		// NOT re-scan every interface on the next datagram.
		neighborFetched = time.Now()
		return neighborNets
	}
	neighborNets = nets
	neighborFetched = time.Now()
	return neighborNets
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	for _, n := range nets {
		if n != nil && n.Contains(ip4) {
			return true
		}
	}
	return false
}

// isNeighborIPv4 reports whether ip is on one of our directly-connected
// subnets. If interface enumeration produced nothing at all (should be
// impossible on a running box) we fall back to "private/link-local/loopback
// only" instead of denying everything: failing closed there would silently
// kill LAN discovery, while the fallback still refuses to mail DeviceInfo to a
// forged public address, which is the amplification vector.
func isNeighborIPv4(ip net.IP) bool {
	nets := connectedIPv4Nets()
	if len(nets) == 0 {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return ipInNets(ip, nets)
}

type probeBucket struct {
	tokens float64
	last   time.Time
}

// take refills by elapsed time (clamped to burst) and spends one token.
// A zero-value bucket starts full, so a first-ever caller is always served.
func (b *probeBucket) take(now time.Time, burst, refill float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * refill
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// probeLimiter combines a per-source-IP token bucket with two box-wide
// ceilings. The per-IP bucket stops one host from hammering us; the box-wide
// ceilings are what stop an attacker who ROTATES source addresses, which the
// per-IP table structurally cannot (every unseen key gets a fresh burst).
type probeLimiter struct {
	mu     sync.Mutex
	seen   map[string]*probeBucket
	burst  float64
	refill float64
	maxIPs int

	evictBatch int
	evictScan  int

	// examined counts map entries touched by eviction, for the regression
	// test. Wall-clock is the wrong assertion for this property: the defect
	// being guarded is a complexity regression (O(table) per insertion), and a
	// time budget tight enough to catch it is also tight enough to trip on a
	// loaded CI box under -race. Entries-examined is the invariant itself and
	// is machine-independent. Written only under mu.
	examined int64

	// total bounds every reply we emit; newSrc bounds only the admission of
	// previously-unseen sources (the expensive, attacker-controlled path).
	total       probeBucket
	totalBurst  float64
	totalRefill float64
	newSrc      probeBucket
	newBurst    float64
	newRefill   float64

	now func() time.Time // overridable in tests
}

func newProbeLimiter() *probeLimiter {
	return &probeLimiter{
		seen:        make(map[string]*probeBucket),
		burst:       zimaProbeBurst,
		refill:      zimaProbeRefill,
		maxIPs:      zimaProbeMaxIPs,
		evictBatch:  zimaProbeEvictBatch,
		evictScan:   zimaProbeEvictScan,
		totalBurst:  zimaProbeTotalBurst,
		totalRefill: zimaProbeTotalRefill,
		newBurst:    zimaProbeNewBurst,
		newRefill:   zimaProbeNewRefill,
		now:         time.Now,
	}
}

func (l *probeLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, known := l.seen[key]
	if !known {
		// An unseen source is the ONLY path that costs a map insert and, once
		// the table is full, an eviction pass — so it is gated first on the
		// box-wide admission budget. Under a flood of rotating/forged source
		// addresses that budget is empty and each datagram costs one map
		// lookup plus a little float math: no allocation, no table churn, and
		// crucially no reply. Ordering matters: gating admission BEFORE the
		// table work is what keeps the attacker's per-packet cost flat.
		if !l.newSrc.take(now, l.newBurst, l.newRefill) {
			return false
		}
		if !l.total.take(now, l.totalBurst, l.totalRefill) {
			return false
		}
		if len(l.seen) >= l.maxIPs {
			l.evictLocked(now)
		}
		nb := &probeBucket{}
		nb.take(now, l.burst, l.refill) // spend this datagram's own token
		l.seen[key] = nb
		return true
	}

	// Known source: its own bucket decides FIRST, so a single flooder is cut
	// off by its per-IP limit and can never burn the shared budget that the
	// rest of the LAN needs.
	if !b.take(now, l.burst, l.refill) {
		return false
	}
	return l.total.take(now, l.totalBurst, l.totalRefill)
}

// evictLocked frees a BOUNDED batch of entries doing BOUNDED work. It makes
// room without ever denying a caller: an evicted source simply gets a fresh
// bucket on its next datagram, which is strictly more permissive than keeping
// it. This table is a heuristic, not accounting — dropping more entries than
// strictly necessary is harmless.
//
// Why bounded: the previous version did TWO full scans of the 4096-entry table
// on EVERY insertion once full — an idle sweep that frees nothing under a
// flood of fresh sources (they are all younger than the idle window), followed
// by a linear search for the oldest entry. Measured at 232us per packet versus
// 41ns with no eviction, and versus 14.3us for the ENTIRE pre-fix reply path
// (build + marshal DeviceInfo). The limiter was therefore a CPU amplifier ~17x
// worse than the reflector it was meant to bound, and it held the global mutex
// that also serialises legitimate discovery. Freeing a batch instead means the
// next `batch` insertions do no eviction work at all: O(1) amortised.
func (l *probeLimiter) evictLocked(now time.Time) {
	batch := l.evictBatch
	if batch < 1 {
		batch = 1
	}
	// Never wipe more than half the table in one go, so a small table (tests,
	// tiny deployments) keeps some history.
	if half := l.maxIPs / 2; half >= 1 && batch > half {
		batch = half
	}

	idle := time.Duration(l.burst/l.refill*float64(time.Second)) + time.Second
	deleted, scanned := 0, 0

	// Pass 1, bounded by evictScan: prefer entries whose bucket has fully
	// refilled — they carry no state worth keeping.
	for k, b := range l.seen {
		l.examined++
		if now.Sub(b.last) >= idle {
			delete(l.seen, k)
			deleted++
			if deleted >= batch {
				return
			}
		}
		scanned++
		if scanned >= l.evictScan {
			break
		}
	}

	// Pass 2, bounded by batch: still short of headroom, so drop an arbitrary
	// sample. Go randomises map iteration order, so this is a random sample
	// rather than a systematic bias against any particular neighbour.
	for k := range l.seen {
		l.examined++
		delete(l.seen, k)
		deleted++
		if deleted >= batch {
			return
		}
	}
}

var discoveryLimiter = newProbeLimiter()

// admitDiscoveryProbe is the single decision point for "do we answer this
// datagram". Both conditions must hold: real L2 neighbour, and within rate
// (per-source AND box-wide). The neighbour test runs first and off a cached
// subnet list, so a non-neighbour flood cannot even reach — let alone drain —
// the shared token budget.
func admitDiscoveryProbe(src *net.UDPAddr) bool {
	if src == nil {
		return false
	}
	ip4 := src.IP.To4()
	if ip4 == nil {
		return false
	}
	if !isNeighborIPv4(ip4) {
		return false
	}
	return discoveryLimiter.allow(ip4.String())
}

// StartZimaDiscoveryResponder binds UDP :9527 and answers the ZimaOS client's
// LAN discovery broadcast. The Windows (Electron/dgram) and Android clients
// broadcast a tiny probe datagram to <subnet-broadcast>:9527 per interface;
// the box replies with a unicast flat DeviceInfo JSON straight back to the
// sender, which is how a CasaNova box AUTO-APPEARS in the client's LAN device
// list. After this reply the client does the HTTP GET :9527/ follow-up that
// StartZimaClientGate (TCP, same port) already answers.
//
// UDP :9527 and TCP :9527 are distinct sockets — no conflict with the gate.
// We do not gate on datagram CONTENT: the Windows probe payload is a runtime
// Buffer with no recoverable magic, and Android sends "9527"/"DISCOVER", so
// content-gating would only risk dropping a valid client. We gate on the
// SENDER instead — see admitDiscoveryProbe (CN-DOS-07). The one field added
// over the HTTP document is `request_ip` — the device MUST echo the caller's
// IP or the Android client's accept filter drops the result. Non-fatal on bind
// conflict, same policy as the TCP gate.
func StartZimaDiscoveryResponder(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("zima discovery responder panicked", zap.Any("recover", r))
		}
	}()

	port, _ := strconv.Atoi(zimaGatePort)
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		logger.Error("zima discovery responder bind failed (udp port likely in use)", zap.Error(err))
		return
	}

	// Closing the socket unblocks the blocking ReadFromUDP so the loop exits.
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	logger.Info("zima discovery responder listening", zap.String("port", zimaGatePort), zap.String("proto", "udp"))

	serveZimaDiscovery(ctx, pc)
}

// serveZimaDiscovery is the read/answer loop, split out of the bind so the
// admission rules can be exercised over a real socket in tests.
func serveZimaDiscovery(ctx context.Context, pc *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		_, src, err := pc.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return // socket closed on shutdown — clean exit
			default:
				logger.Error("zima discovery read error", zap.Error(err))
				continue
			}
		}
		// CN-DOS-07: drop non-neighbour / over-rate probes BEFORE building or
		// writing anything. Dropped silently and without a log line on purpose
		// — logging here would just convert the packet flood into a disk/CPU
		// flood, i.e. the same DoS through another door.
		if !admitDiscoveryProbe(src) {
			continue
		}
		info := zimaDeviceInfoFlat()
		info["request_ip"] = src.IP.String()
		b, mErr := json.Marshal(info)
		if mErr != nil {
			logger.Error("zima discovery marshal error", zap.Error(mErr))
			continue
		}
		if _, wErr := pc.WriteToUDP(b, src); wErr != nil {
			logger.Error("zima discovery reply error", zap.Error(wErr))
		}
	}
}
