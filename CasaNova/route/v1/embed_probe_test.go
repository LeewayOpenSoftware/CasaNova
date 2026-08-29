package v1

// The embed probe exists to prevent one specific bad outcome: the Apps
// section rendering a blank iframe for an app that refuses framing. Every
// test here is a row of that truth table, served by a real HTTP server so
// the header plumbing is exercised, not simulated.
//
// The redirect test pins the security property: a probed app must not be
// able to bounce CasaNova into fetching an arbitrary LAN URL. The probe
// follows redirects only while they stay on loopback; anything else is
// answered with the app's own (last loopback) response.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("test server port: %v", err)
	}
	return p
}

func serveHeaders(h map[string]string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range h {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
	}
}

func TestEmbedProbeTruthTable(t *testing.T) {
	cases := []struct {
		name       string
		headers    map[string]string
		embeddable bool
	}{
		{"sin cabeceras (immich, jellyfin)", nil, true},
		{"x-frame-options deny (grafana)", map[string]string{"X-Frame-Options": "deny"}, false},
		{"x-frame-options DENY mayusculas", map[string]string{"X-Frame-Options": "DENY"}, false},
		// SAMEORIGIN reads like consent but the dashboard is another origin.
		{"sameorigin (nextcloud, syncthing)", map[string]string{"X-Frame-Options": "SAMEORIGIN"}, false},
		{"frame-ancestors none (portainer)", map[string]string{"Content-Security-Policy": "frame-ancestors 'none'"}, false},
		{"frame-ancestors con lista", map[string]string{"Content-Security-Policy": "default-src 'self'; frame-ancestors https://example.com"}, false},
		{"frame-ancestors *", map[string]string{"Content-Security-Policy": "frame-ancestors *"}, true},
		// CSP without the directive says nothing about framing.
		{"csp sin frame-ancestors", map[string]string{"Content-Security-Policy": "default-src 'self'"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(serveHeaders(tc.headers, http.StatusOK))
			defer srv.Close()

			got := probeOne("http", portOf(t, srv))
			if got.Embeddable != tc.embeddable {
				t.Fatalf("embeddable = %v (reason %q), want %v — a wrong yes renders a blank panel, a wrong no hides a working app",
					got.Embeddable, got.Reason, tc.embeddable)
			}
			if !got.Embeddable && got.Reason == "" {
				t.Fatal("a refusal with no reason gives the operator nothing to act on")
			}
		})
	}
}

func TestEmbedProbeUnreachablePortIsAVerdictNotAnError(t *testing.T) {
	// Grab a port that is genuinely free by binding and releasing it.
	srv := httptest.NewServer(http.NotFoundHandler())
	port := portOf(t, srv)
	srv.Close()

	got := probeOne("http", port)
	if got.Embeddable {
		t.Fatal("an unreachable app reported as embeddable would render a blank panel")
	}
	if got.Reason != "unreachable" {
		t.Fatalf("reason = %q, want %q", got.Reason, "unreachable")
	}
	if got.Status != 0 {
		t.Fatalf("status = %d for a dead port, want 0", got.Status)
	}
}

func TestEmbedProbeFollowsLoopbackRedirects(t *testing.T) {
	// jellyfin's shape: / answers 302 to /web/, and the final page's headers
	// are the ones that govern framing.
	final := serveHeaders(map[string]string{"X-Frame-Options": "DENY"}, http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/" {
			final(w, r)
			return
		}
		http.Redirect(w, r, "/web/", http.StatusFound)
	}))
	defer srv.Close()

	got := probeOne("http", portOf(t, srv))
	if got.Embeddable {
		t.Fatal("the DENY on the redirect target was not seen: the probe judged the 302 instead of the page")
	}
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the followed redirect", got.Status)
	}
}

func TestEmbedProbeDoesNotFollowRedirectsOffLoopback(t *testing.T) {
	// The SSRF pin. A malicious or misconfigured app redirects the probe at
	// the router's admin page; CasaNova must not fetch it. 192.0.2.1 is
	// TEST-NET-1 — guaranteed unroutable, so if the client DID follow, the
	// probe would hang toward timeout and report unreachable/err rather than
	// the app's own 302.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.0.2.1/admin", http.StatusFound)
	}))
	defer srv.Close()

	got := probeOne("http", portOf(t, srv))
	if got.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302 (the app's own answer) — anything else means the probe left loopback", got.Status)
	}
}

func TestEmbedVerdictPrecedence(t *testing.T) {
	// DENY beats a permissive frame-ancestors: the stricter header wins in
	// browsers that honour X-Frame-Options, so the verdict must not promise
	// what half the browsers will refuse.
	ok, reason := embedVerdict(200, "DENY", "frame-ancestors *")
	if ok {
		t.Fatalf("DENY + frame-ancestors * judged embeddable (reason %q)", reason)
	}
}

func TestEmbedProbeSurvivesRedirectIntoBrokenScheme(t *testing.T) {
	// nextcloud's shape on this box: / answers 302 to
	// https://127.0.0.1:<same-port>/login — https on a port that only speaks
	// http, so the second hop can never succeed. The probe must answer with
	// the 302 it DID receive, not bury it under the failed hop and call a
	// responding app unreachable.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		http.Redirect(w, r, "https://"+srv.Listener.Addr().String()+"/login", http.StatusFound)
	}))
	defer srv.Close()

	got := probeOne("http", portOf(t, srv))
	if got.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302 — the app answered and that answer was thrown away", got.Status)
	}
	if got.Embeddable {
		t.Fatal("the 302's SAMEORIGIN was lost: a refusing app was judged embeddable")
	}
	if got.Reason == "unreachable" {
		t.Fatal("a responding app reported as unreachable — the failed second hop erased the first")
	}
}
