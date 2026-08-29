package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ENV-001 — SECURITY DECISIONS MUST NEVER TRUST X-Forwarded-For.
//
// The inherited gateway trusted the header whenever the peer was loopback, on
// the reasoning that a loopback peer must be a reverse proxy of ours that
// overwrites it. Behind the Cloudflare tunnel every request has a loopback
// peer and the intermediary APPENDS, so the exemption covered the internet.
//
// A subsystem was then written to resolve a "client identity" from the header
// and feed it to the allow-lists and to a ban table. Two independent certifiers
// rejected it: any LAN device — or any of the box's own containers, since the
// Docker bridge sat inside the exempt range — could name an arbitrary remote
// victim and have it banned. The lesson is not that the resolver was written
// badly. It is that a value the client controls cannot be an authorization
// input at all, however carefully it is parsed.
//
// These tests fix the invariant so neither shape can come back. Every one of
// them fails if securityPeerIP is changed to consult a header.

// hostile builds a request whose peer is the tunnel's loopback address — the
// shape every internet request has on this deployment — carrying whatever the
// client chose to send.
func hostile(peer string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/file", nil)
	r.RemoteAddr = peer
	for _, v := range forwarded {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

//  1. A client-supplied header cannot change the identity a security control
//     sees. This is the whole finding in one assertion.
//
// Mutation that must fail this test: have securityPeerIP prefer a forwarded
// header when the peer is loopback (the inherited behaviour).
func TestForwardedHeaderCannotChangeTheSecurityIdentity(t *testing.T) {
	const peer = "127.0.0.1:44444"

	base := securityPeerIP(hostile(peer))
	if base != "127.0.0.1" {
		t.Fatalf("peer identity = %q, want the socket peer", base)
	}

	for _, forged := range []string{
		"203.0.113.9",
		"192.168.1.5",
		"203.0.113.9, 127.0.0.1",
		"127.0.0.1, 203.0.113.9",
		"10.0.0.1, 10.0.0.2, 10.0.0.3",
	} {
		if got := securityPeerIP(hostile(peer, forged)); got != base {
			t.Fatalf("X-Forwarded-For %q changed the security identity to %q; "+
				"a client-controlled header is deciding authorization", forged, got)
		}
	}
}

//  2. A LAN peer cannot select an arbitrary remote victim. This is the exact
//     primitive the rejected subsystem handed out: reach the gateway, name
//     someone else, and have the box act on that name.
func TestLanPeerCannotNameAnotherPrincipal(t *testing.T) {
	victim := "203.0.113.77"
	r := hostile("192.168.1.50:51000", victim)

	if got := securityPeerIP(r); got == victim {
		t.Fatal("a LAN peer selected a remote victim's identity through X-Forwarded-For")
	} else if got != "192.168.1.50" {
		t.Fatalf("identity = %q, want the observed peer 192.168.1.50", got)
	}
}

//  3. A container on the Docker bridge is not privileged either. Every app the
//     user installs from the store runs there, so treating that range as
//     trusted means every installed app inherits the gateway's trust.
func TestDockerBridgePeerCannotManufactureAnIdentity(t *testing.T) {
	r := hostile("172.17.0.4:33000", "8.8.8.8")
	if got := securityPeerIP(r); got != "172.17.0.4" {
		t.Fatalf("identity = %q, want the container's own address", got)
	}
}

//  4. Malformed, empty and repeated headers must not perturb the decision. A
//     parser that behaves differently on odd input is a parser an attacker
//     tunes against.
func TestMalformedForwardedHeadersDoNotAffectTheDecision(t *testing.T) {
	const peer = "127.0.0.1:9999"
	want := securityPeerIP(hostile(peer))

	for name, hdrs := range map[string][]string{
		"empty":              {""},
		"only commas":        {",,,"},
		"spaces":             {"   "},
		"not an address":     {"not-an-ip"},
		"null byte":          {"203.0.113.9\x00"},
		"repeated header":    {"203.0.113.9", "198.51.100.4"},
		"empty then address": {"", "203.0.113.9"},
		"very long chain":    {"1.1.1.1, 2.2.2.2, 3.3.3.3, 4.4.4.4, 5.5.5.5, 6.6.6.6"},
	} {
		if got := securityPeerIP(hostile(peer, hdrs...)); got != want {
			t.Fatalf("%s: identity = %q, want %q", name, got, want)
		}
	}
}

//  5. RemoteAddr remains authoritative, including the shapes Go can hand us:
//     IPv6 with a zone, and a value with no port at all.
func TestRemoteAddrIsTheAuthoritativeIdentity(t *testing.T) {
	for addr, want := range map[string]string{
		"192.168.1.7:1234":  "192.168.1.7",
		"[2001:db8::1]:443": "2001:db8::1",
		"[fe80::1%eth0]:80": "fe80::1%eth0",
		"10.0.0.9":          "10.0.0.9", // no port: used as-is rather than dropped
		"203.0.113.4:0":     "203.0.113.4",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr
		if got := securityPeerIP(r); got != want {
			t.Fatalf("RemoteAddr %q -> %q, want %q", addr, got, want)
		}
	}
}

//  6. Legitimate proxying keeps working. The header is still handled for
//     telemetry — the fix removes its authority, not its existence — and a
//     request that carries none must not acquire one.
func TestLegitimateProxyingStillFunctions(t *testing.T) {
	r := hostile("127.0.0.1:5000", "203.0.113.9")
	rewriteRequestSourceIP(r)
	// Whatever the telemetry path decides, it must not have become the
	// security identity.
	if securityPeerIP(r) != "127.0.0.1" {
		t.Fatal("rewriteRequestSourceIP changed the security identity")
	}

	clean := httptest.NewRequest(http.MethodGet, "/", nil)
	clean.RemoteAddr = "192.168.1.9:6000"
	rewriteRequestSourceIP(clean)
	if v := clean.Header.Get("X-Forwarded-For"); v != "" {
		t.Fatalf("a request that carried no forwarding header acquired one: %q", v)
	}
	if securityPeerIP(clean) != "192.168.1.9" {
		t.Fatal("a direct request lost its own identity")
	}
}

//  7. The header must not be readable as an identity anywhere in this file's
//     decision path. This is the unwiring guard: it fails if a future edit
//     reintroduces header-derived identity, naming the function.
func TestSecurityIdentityNeverConsultsAHeader(t *testing.T) {
	// Two requests differing ONLY in headers must be indistinguishable to
	// every security-relevant accessor.
	a := httptest.NewRequest(http.MethodGet, "/v1/file", nil)
	a.RemoteAddr = "127.0.0.1:1111"

	b := httptest.NewRequest(http.MethodGet, "/v1/file", nil)
	b.RemoteAddr = "127.0.0.1:1111"
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded",
		"Cf-Connecting-IP", "True-Client-IP", "X-Client-IP", "X-Forwarded"} {
		b.Header.Set(h, "203.0.113.200")
	}

	if securityPeerIP(a) != securityPeerIP(b) {
		t.Fatalf("securityPeerIP is header-sensitive: %q vs %q — some forwarding "+
			"header has become an authorization input again",
			securityPeerIP(a), securityPeerIP(b))
	}
}
