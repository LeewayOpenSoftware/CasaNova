package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// ctxWith builds an echo.Context for the given transport peer, optional
// Authorization header, optional ?token= query param and optional internal
// secret header.
func ctxWith(remoteAddr, authHeader, tokenQuery, internalHdr string) echo.Context {
	e := echo.New()
	url := "/v1/users/current"
	if tokenQuery != "" {
		url += "?token=" + tokenQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = remoteAddr
	if authHeader != "" {
		req.Header.Set(echo.HeaderAuthorization, authHeader)
	}
	if internalHdr != "" {
		req.Header.Set(internalauth.HeaderName, internalHdr)
	}
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath("/v1/users/current")
	return c
}

// THE DECISION THIS PINS, and how it used to be wrong.
//
// The old skipper let ANY tokenless request through when the socket peer was
// 127.0.0.1. Every request reaching this service has that peer, because the
// gateway reverse-proxies the LAN and the Cloudflare tunnel over loopback — so
// the rule meant "anybody who omits the Authorization header", and /v1/users/*
// is account data.
//
// The previous version of this test asserted the old behaviour as correct
// ("tokenless local/CLI calls keep their bypass"). It passed for years. A test
// can pin a bug just as firmly as a feature.
func TestOnlyAProvenPeerSkipsAuth(t *testing.T) {
	internalauth.SetPathsForTest(t.TempDir())
	secret, err := internalauth.Secret()
	if err != nil {
		t.Fatalf("could not provision the internal secret: %v", err)
	}

	cases := []struct {
		name                                    string
		remoteAddr, authHeader, token, internal string
		wantSkip                                bool
	}{
		{"tokenless loopback — i.e. the internet through the gateway",
			"127.0.0.1:5000", "", "", "", false},
		{"tokenless loopback IPv6", "[::1]:5000", "", "", "", false},
		{"tokenless from a LAN peer", "192.168.1.50:5000", "", "", "", false},
		{"a presented token is validated, never skipped",
			"127.0.0.1:5000", "Bearer x.y.z", "", "", false},
		{"a query token is validated too", "127.0.0.1:5000", "", "x.y.z", "", false},
		{"a peer holding the internal secret skips", "127.0.0.1:5000", "", "", secret, true},
		{"a peer with the WRONG secret does not", "127.0.0.1:5000", "", "", secret + "x", false},
		{"the secret works from any peer address, because it is a credential",
			"192.168.1.50:5000", "", "", secret, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jwtLoopbackSkipper(ctxWith(tc.remoteAddr, tc.authHeader, tc.token, tc.internal))
			if got != tc.wantSkip {
				t.Fatalf("skip=%v, want %v — remote=%q auth=%q token=%q internal=%v",
					got, tc.wantSkip, tc.remoteAddr, tc.authHeader, tc.token, tc.internal != "")
			}
		})
	}
}
