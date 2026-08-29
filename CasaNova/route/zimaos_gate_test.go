package route

// CN-DOS-07 regression tests: the UDP :9527 discovery responder must answer
// ONLY real layer-2 neighbours, at a bounded rate, without changing the wire
// payload the ZimaOS clients parse.
//
// Every test here drives a REAL UDP socket through the production loop
// (serveZimaDiscovery) and the production admission function
// (admitDiscoveryProbe) — nothing is stubbed on the path under test — and
// every read carries a deadline so a regression fails fast instead of hanging
// the suite.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// pinNeighborNets forces the connected-subnet cache to a known value for the
// duration of a test and restores the real one afterwards.
func pinNeighborNets(t *testing.T, cidrs ...string) {
	t.Helper()
	neighborMu.Lock()
	prevNets, prevAt := neighborNets, neighborFetched
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			neighborMu.Unlock()
			t.Fatalf("bad test CIDR %q: %v", c, err)
		}
		nets = append(nets, n)
	}
	neighborNets, neighborFetched = nets, time.Now()
	neighborMu.Unlock()

	t.Cleanup(func() {
		neighborMu.Lock()
		neighborNets, neighborFetched = prevNets, prevAt
		neighborMu.Unlock()
	})
}

// resetDiscoveryLimiter gives each test a fresh token budget.
func resetDiscoveryLimiter(t *testing.T) {
	t.Helper()
	prev := discoveryLimiter
	discoveryLimiter = newProbeLimiter()
	t.Cleanup(func() { discoveryLimiter = prev })
}

// startResponder runs the production loop on an ephemeral loopback UDP port.
func startResponder(t *testing.T) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveZimaDiscovery(ctx, pc)
	}()
	t.Cleanup(func() {
		cancel()
		_ = pc.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("responder loop did not exit")
		}
	})
	return pc.LocalAddr().(*net.UDPAddr)
}

func probeOnce(t *testing.T, client *net.UDPConn, server *net.UDPAddr) {
	t.Helper()
	if _, err := client.WriteToUDP([]byte("9527"), server); err != nil {
		t.Fatalf("probe write: %v", err)
	}
}

// readReply returns the reply body, or nil if none arrived before the deadline.
func readReply(t *testing.T, client *net.UDPConn, wait time.Duration) []byte {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil
		}
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

// INVARIANT: a probe whose source address is NOT on a directly-connected
// subnet gets no reply at all — no reflection target, no DeviceInfo leak.
//
// The loopback client is made a non-neighbour by pinning the connected-subnet
// cache to an unrelated /24. Reverting the fix (dropping the
// admitDiscoveryProbe call from serveZimaDiscovery, i.e. the original "reply
// to anyone" behaviour) makes this test fail: a reply arrives.
func TestZimaDiscovery_IgnoresNonNeighborSource(t *testing.T) {
	pinNeighborNets(t, "10.99.0.0/24")
	resetDiscoveryLimiter(t)

	server := startResponder(t)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	probeOnce(t, client, server)
	if body := readReply(t, client, 400*time.Millisecond); body != nil {
		t.Fatalf("responder answered a non-neighbour source: %s", body)
	}
}

// CONTRACT: a real neighbour still gets the flat DeviceInfo, unchanged. The
// loopback client is a neighbour here because 127.0.0.0/8 is pinned in.
func TestZimaDiscovery_AnswersNeighborWithFlatPayload(t *testing.T) {
	pinNeighborNets(t, "127.0.0.0/8")
	resetDiscoveryLimiter(t)

	server := startResponder(t)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	probeOnce(t, client, server)
	body := readReply(t, client, 2*time.Second)
	if body == nil {
		t.Fatal("neighbour probe got no reply — discovery is broken")
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("reply is not JSON: %v (%s)", err, body)
	}
	if _, envelope := doc["data"]; envelope {
		t.Fatal("reply is enveloped; the client parses the raw body flat")
	}
	// The fields the clients gate on, plus the discovery-only request_ip echo.
	for _, k := range []string{"device_name", "os_version", "min_client_version", "initialized", "lan_ipv4", "port", "request_ip"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("reply missing contract field %q", k)
		}
	}
	// `ip4` must stay absent: the Android client types it as a list of objects.
	if _, bad := doc["ip4"]; bad {
		t.Error("reply contains ip4; that breaks the Android client's parser")
	}
	if got := doc["request_ip"]; got != "127.0.0.1" {
		t.Errorf("request_ip = %v, want the caller's address 127.0.0.1", got)
	}
}

// INVARIANT: a flood from a single source is capped. 40 probes must not
// produce 40 replies — that ratio is the amplification the finding describes.
// Removing the limiter (making admitDiscoveryProbe return on the neighbour
// check alone) makes this fail with ~40 replies.
func TestZimaDiscovery_RateLimitsFloodFromOneSource(t *testing.T) {
	pinNeighborNets(t, "127.0.0.0/8")
	resetDiscoveryLimiter(t)

	server := startResponder(t)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	const probes = 40
	for i := 0; i < probes; i++ {
		probeOnce(t, client, server)
	}

	replies := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if body := readReply(t, client, 250*time.Millisecond); body == nil {
			break
		}
		replies++
	}

	// At least one reply: a legitimate client that retries must still be found.
	if replies == 0 {
		t.Fatal("flood produced zero replies — the limiter starves real clients")
	}
	// Upper bound: burst (5) plus refill over the few ms the burst takes.
	// Anything near `probes` means the limiter is not wired in.
	if replies > 10 {
		t.Fatalf("responder answered %d/%d probes from one source; amplification is still open", replies, probes)
	}
}

// Unit-level check of the subnet predicate, independent of sockets.
func TestIPInNets(t *testing.T) {
	_, own, err := net.ParseCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	nets := []*net.IPNet{own}

	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.1.188", true}, // the box's own segment: a real client
		{"192.168.1.1", true},   // the LAN gateway
		{"192.168.2.5", false},  // routed neighbour, not directly connected
		{"203.0.113.7", false},  // public victim of a forged-source probe
		{"8.8.8.8", false},      // ditto
		{"127.0.0.1", false},    // not in the pinned list
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := ipInNets(ip, nets); got != c.want {
			t.Errorf("ipInNets(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// The token bucket must refill, so a steady 1/s scan is served indefinitely
// while a burst is capped. Uses a fake clock: no sleeping, no flakiness.
func TestProbeLimiter_BurstThenRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }

	allowed := 0
	for i := 0; i < 20; i++ {
		if l.allow("192.168.1.50") {
			allowed++
		}
	}
	if allowed != int(zimaProbeBurst) {
		t.Fatalf("burst allowed %d, want %v", allowed, zimaProbeBurst)
	}

	// A different source is unaffected by the first one's flood.
	if !l.allow("192.168.1.51") {
		t.Error("second source denied; one flooder must not deny other clients")
	}

	// One second later the first source is served again.
	now = now.Add(time.Second)
	if !l.allow("192.168.1.50") {
		t.Error("bucket did not refill; a steady 1/s client would be starved")
	}
	if l.allow("192.168.1.50") {
		t.Error("bucket refilled faster than the configured rate")
	}
}

// liftAggregateCeiling removes the box-wide ceilings from l so a test can
// isolate the per-source-IP table behaviour. The ceilings have their own tests
// (TestProbeLimiter_AggregateCeilingCapsRotatingSources); mixing them in here
// would make "was this source denied?" ambiguous.
func liftAggregateCeiling(l *probeLimiter) {
	l.totalBurst, l.totalRefill = 1e12, 1e12
	l.newBurst, l.newRefill = 1e12, 1e12
}

// INVARIANT (M3): table pressure must EVICT, never deny. A limiter that starts
// refusing unknown IPs when the table is full breaks discovery on a busy LAN —
// exactly when it is most needed.
func TestProbeLimiter_EvictsInsteadOfDenying(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }
	l.maxIPs = 8
	liftAggregateCeiling(l)

	for i := 0; i < 200; i++ {
		key := net.IPv4(10, 0, byte(i/256), byte(i%256)).String()
		if !l.allow(key) {
			t.Fatalf("first-ever probe from %s denied at i=%d", key, i)
		}
		now = now.Add(10 * time.Millisecond)
	}
	if len(l.seen) > l.maxIPs {
		t.Fatalf("limiter table grew to %d entries, cap is %d", len(l.seen), l.maxIPs)
	}
}

// INVARIANT: the cost of admitting a NEVER-BEFORE-SEEN source, with the table
// already at its production cap, must not scale with the table size.
//
// This is the regression that made the previous "fix" worse than the bug it
// replaced: evictLocked did two full scans of the 4096-entry map on every
// insertion once full, so a flood of rotating source addresses paid ~232us of
// CPU per packet (vs 41ns with no eviction, vs 14.3us for the whole pre-fix
// build+marshal reply path) while holding the mutex that also serialises real
// discovery. That is a ~17x CPU amplifier built by the mitigation itself.
//
// The defect this guards is a COMPLEXITY regression: the first version of the
// limiter scanned the whole 4096-entry table twice on every insertion once
// full, which measured 232us/op against 14.3us for the entire pre-fix reply
// path — the mitigation cost 16x more than the work it prevented.
//
// The assertion is on entries examined, not elapsed time. A wall-clock budget
// tight enough to catch the regression (the healthy and broken values differ by
// ~400x, but under -race the healthy value is only ~2x under an 8us budget) also
// trips on a loaded CI box: measured 2 failures in 8 runs of the real command,
// because `go test ./service/ ./route/...` runs packages in parallel by
// construction. A count invariant separates the two states by the same ~400x
// with no dependence on machine speed or contention.
//
// Bound: eviction runs only when the table is at its cap, examines at most
// evictScan (128) in pass 1 plus evictBatch (64) in pass 2, and frees 64
// entries — so the next 64 insertions evict nothing. That is (128+64)/64 = 3
// entries per insertion amortised. The O(table) version examines ~8192.
func TestProbeLimiter_EvictionCostIsAmortizedConstant(t *testing.T) {
	const (
		ops = 20000
		// 20 per op is ~6.6x above the theoretical 3 and ~400x below the
		// broken 8192, so it tolerates any reasonable retuning of the eviction
		// constants while still catching a return to whole-table scanning.
		maxExaminedPerOp = 20
	)

	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }
	liftAggregateCeiling(l)
	if l.maxIPs != zimaProbeMaxIPs {
		t.Fatalf("expected production table cap, got %d", l.maxIPs)
	}

	key := func(i int) string {
		return net.IPv4(byte(10+i>>24), byte(i>>16), byte(i>>8), byte(i)).String()
	}

	// Fill the table to its cap first: the pathological cost only appears once
	// every insertion has to make room.
	for i := 0; i < l.maxIPs; i++ {
		l.allow(key(i))
	}
	if len(l.seen) != l.maxIPs {
		t.Fatalf("table did not fill: %d entries, want %d", len(l.seen), l.maxIPs)
	}

	keys := make([]string, ops)
	for i := 0; i < ops; i++ {
		keys[i] = key(l.maxIPs + i)
	}

	l.mu.Lock()
	l.examined = 0
	l.mu.Unlock()

	start := time.Now()
	for i := 0; i < ops; i++ {
		// Advance the fake clock a little so entries genuinely age; this also
		// keeps the idle-preference pass honest.
		now = now.Add(time.Millisecond)
		l.allow(keys[i])
	}
	elapsed := time.Since(start)

	l.mu.Lock()
	examined := l.examined
	l.mu.Unlock()
	perOp := float64(examined) / float64(ops)

	// Logged for context only — nothing asserts on it.
	t.Logf("distinct-source flood against a full %d-entry table: %d entries examined over %d ops "+
		"(%.2f/op), %v wall (table now %d entries)",
		l.maxIPs, examined, ops, perOp, elapsed, len(l.seen))

	if len(l.seen) > l.maxIPs {
		t.Fatalf("table exceeded cap under flood: %d entries", len(l.seen))
	}
	if perOp > maxExaminedPerOp {
		t.Fatalf("eviction examined %.1f entries per insertion (budget %d): it has gone back to "+
			"scanning the table and the limiter is again a CPU amplifier", perOp, maxExaminedPerOp)
	}
}

func TestProbeLimiter_AggregateCeilingCapsRotatingSources(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }

	// One simulated second of flood, 100k distinct forged sources.
	const sources = 100000
	allowed := 0
	for i := 0; i < sources; i++ {
		now = now.Add(10 * time.Microsecond)
		if l.allow(net.IPv4(byte(10+i>>24), byte(i>>16), byte(i>>8), byte(i)).String()) {
			allowed++
		}
	}

	// Ceiling over ~1s = burst + one second of refill, with slack.
	max := int(zimaProbeNewBurst + zimaProbeNewRefill + 5)
	t.Logf("rotating-source flood: %d probes -> %d replies in ~1s simulated", sources, allowed)
	if allowed > max {
		t.Fatalf("rotating-source flood got %d replies from %d probes (ceiling ~%d); "+
			"source rotation is unlimited again", allowed, sources, max)
	}
	// And the table must not have absorbed the flood.
	if len(l.seen) > max {
		t.Fatalf("table grew to %d entries from a flood that should have been refused admission", len(l.seen))
	}
}

// A legitimate neighbour already known to the limiter must keep being answered
// WHILE a rotating-source flood is in progress. This is the "the mitigation
// must not be worse than the bug" property for the aggregate ceiling: the
// new-source sub-budget is smaller than the total, so the flood cannot consume
// the whole box-wide allowance.
func TestProbeLimiter_KnownNeighborSurvivesRotatingFlood(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }

	const neighbor = "192.168.1.50"
	if !l.allow(neighbor) {
		t.Fatal("neighbour denied on its very first probe")
	}

	served := 0
	for sec := 0; sec < 10; sec++ {
		// 5000 forged sources this second.
		for i := 0; i < 5000; i++ {
			now = now.Add(200 * time.Microsecond)
			l.allow(net.IPv4(byte(10+sec), byte(i>>16), byte(i>>8), byte(i)).String())
		}
		// The real neighbour re-probes once, as a client mid-scan does.
		if l.allow(neighbor) {
			served++
		}
	}
	if served < 10 {
		t.Fatalf("known neighbour answered %d/10 times during a rotating flood; "+
			"the aggregate ceiling is starving real discovery", served)
	}
}

// Reported cost numbers for the record. Run with:
//
//	go test ./route -run XXX -bench ProbeLimiter -benchmem
func BenchmarkProbeLimiterFloodDistinctSources(b *testing.B) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }
	liftAggregateCeiling(l) // measure the TABLE path, not the cheap refusal

	key := func(i int) string {
		return net.IPv4(byte(10+i>>24), byte(i>>16), byte(i>>8), byte(i)).String()
	}
	for i := 0; i < l.maxIPs; i++ {
		l.allow(key(i))
	}
	keys := make([]string, b.N)
	for i := 0; i < b.N; i++ {
		keys[i] = key(l.maxIPs + i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(time.Millisecond)
		l.allow(keys[i])
	}
}

// The production path under a rotating flood: refused at the aggregate gate,
// which must never touch the table.
func BenchmarkProbeLimiterFloodRefused(b *testing.B) {
	now := time.Unix(1_700_000_000, 0)
	l := newProbeLimiter()
	l.now = func() time.Time { return now }

	keys := make([]string, b.N)
	for i := 0; i < b.N; i++ {
		keys[i] = net.IPv4(byte(10+i>>24), byte(i>>16), byte(i>>8), byte(i)).String()
	}
	// Drain the admission budget with DISTINCT sources (a repeated key would
	// only spend one new-source token) so every measured call is the refusal
	// path. The fake clock never advances, so nothing refills.
	for i := 0; i < int(zimaProbeNewBurst)+2; i++ {
		l.allow(net.IPv4(172, 16, byte(i>>8), byte(i)).String())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.allow(keys[i])
	}
}

// INVARIANT: connectedIPv4Nets must consult the OS at most once per TTL even
// when the lookup FAILS.
//
// Both failure paths used to return without stamping neighborFetched, so while
// the box was in that state — a boot race with the LAN not up yet, or a
// transient netlink failure — every single discovery datagram re-enumerated
// every interface and every address. That is the per-datagram syscall
// amplifier zimaNeighborTTL exists to prevent, armed exactly when the box is
// already unhealthy. Restoring either bare `return neighborNets` makes this
// test fail with calls == probes.
func TestConnectedIPv4Nets_CachesFailurePaths(t *testing.T) {
	cases := []struct {
		name string
		stub func() ([]net.Interface, error)
	}{
		{"netlink error", func() ([]net.Interface, error) { return nil, errors.New("netlink down") }},
		{"nothing enumerated", func() ([]net.Interface, error) { return []net.Interface{}, nil }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			restoreNetInterfaces(t, func() ([]net.Interface, error) {
				calls++
				return c.stub()
			})
			clearNeighborCache(t)

			const probes = 500
			for i := 0; i < probes; i++ {
				connectedIPv4Nets()
			}
			if calls != 1 {
				t.Fatalf("%d probes caused %d interface enumerations, want 1: "+
					"the failure path is re-scanning per datagram", probes, calls)
			}
		})
	}
}

// A failed lookup must not discard the last good subnet list, and must not
// flip the responder's policy for the sources it already knows about.
func TestConnectedIPv4Nets_FailureKeepsLastGoodNets(t *testing.T) {
	restoreNetInterfaces(t, func() ([]net.Interface, error) { return nil, errors.New("netlink down") })
	pinNeighborNets(t, "192.168.1.0/24")

	// Force the TTL to have expired so the failing lookup actually runs.
	neighborMu.Lock()
	neighborFetched = time.Now().Add(-2 * zimaNeighborTTL)
	neighborMu.Unlock()

	nets := connectedIPv4Nets()
	if len(nets) != 1 || !nets[0].Contains(net.ParseIP("192.168.1.188")) {
		t.Fatalf("failed lookup dropped the last good subnet list: %v", nets)
	}
	if !isNeighborIPv4(net.ParseIP("192.168.1.188")) {
		t.Error("known neighbour rejected after a transient enumeration failure")
	}
}

func restoreNetInterfaces(t *testing.T, stub func() ([]net.Interface, error)) {
	t.Helper()
	prev := netInterfaces
	netInterfaces = stub
	t.Cleanup(func() { netInterfaces = prev })
}

func clearNeighborCache(t *testing.T) {
	t.Helper()
	neighborMu.Lock()
	prevNets, prevAt := neighborNets, neighborFetched
	neighborNets, neighborFetched = nil, time.Time{}
	neighborMu.Unlock()
	t.Cleanup(func() {
		neighborMu.Lock()
		neighborNets, neighborFetched = prevNets, prevAt
		neighborMu.Unlock()
	})
}

// The real box must classify its own LAN address as a neighbour and a public
// address as a stranger, using the live interface list (no pinning).
func TestIsNeighborIPv4_RealInterfaces(t *testing.T) {
	nets := connectedIPv4Nets()
	if len(nets) == 0 {
		t.Skip("no IPv4 interfaces in this environment")
	}
	var own net.IP
	for _, a := range mustInterfaceAddrs(t) {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				own = ip4
				break
			}
		}
	}
	if own == nil {
		t.Skip("no IPv4 address on this host")
	}
	if !isNeighborIPv4(own) {
		t.Errorf("own address %s classified as non-neighbour; discovery would break", own)
	}
	// TEST-NET-3; never routed to a home LAN, and a classic reflection victim.
	if isNeighborIPv4(net.ParseIP("203.0.113.7")) {
		t.Error("public address classified as neighbour; reflection still possible")
	}
}

func mustInterfaceAddrs(t *testing.T) []net.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("InterfaceAddrs: %v", err)
	}
	return addrs
}
