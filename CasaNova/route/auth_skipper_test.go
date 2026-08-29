package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/labstack/echo/v4"
)

// ctxFor builds a context whose matched ROUTE PATTERN is p — the thing the
// skipper actually consults — with an optional raw URL and headers.
func ctxFor(pattern, rawURL string, hdr map[string]string) echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "127.0.0.1:54321" // the address that used to be enough
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath(pattern)
	return c
}

// THE PROPERTY THE OLD SKIPPER FAILED.
//
// Every request from the LAN and from the Cloudflare tunnel arrives at this
// process over 127.0.0.1, because the gateway proxies it. So a loopback source
// address carries no information about who is calling, and omitting the
// Authorization header is something any caller can do. Measured on the
// production box under the old skipper, with no credential: GET
// /v2/app_management/compose returned 200 with 23 apps' compose definitions.
func TestLoopbackAndNoTokenIsNotEnoughToSkipAuth(t *testing.T) {
	skip := skipAuthFor(v2PreLoginRoutes)

	for _, pattern := range []string{
		"/v2/app_management/compose",
		"/v2/app_management/apps",
		"/v1/sys/smb/browse",
		"/v1/batch",
	} {
		if skip(ctxFor(pattern, pattern, nil)) {
			t.Errorf("%s skips authentication for a tokenless request from "+
				"127.0.0.1 — which is every request the gateway forwards, "+
				"including the internet over the tunnel", pattern)
		}
	}
}

// A peer service proves itself with the root-only secret, and must skip — the
// JWT middleware runs before the handler, so without this a peer presenting the
// internal header is refused 401 and never reaches requirePermInternal.
func TestAPeerHoldingTheInternalSecretSkips(t *testing.T) {
	dir := t.TempDir()
	internalauth.SetPathsForTest(dir)
	secret, err := internalauth.Secret()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}

	skip := skipAuthFor(v1PreLoginRoutes)
	c := ctxFor("/v1/notify/:path", "/v1/notify/x",
		map[string]string{internalauth.HeaderName: secret})
	if !skip(c) {
		t.Fatal("a peer presenting the internal secret was not allowed to skip, " +
			"so notify and share between services are now broken")
	}

	// A wrong secret is not a peer.
	bad := ctxFor("/v1/notify/:path", "/v1/notify/x",
		map[string]string{internalauth.HeaderName: secret + "x"})
	if skip(bad) {
		t.Fatal("a WRONG internal secret was accepted")
	}
	if skip(ctxFor("/v1/notify/:path", "/v1/notify/x", nil)) {
		t.Fatal("no secret at all was accepted")
	}
	_ = os.RemoveAll(dir)
}

// The pre-login routes must still answer, or pairing and OAuth break — and both
// break silently, which is why they are tested rather than trusted.
func TestPreLoginRoutesStillSkip(t *testing.T) {
	for pattern, skip := range map[string]func(echo.Context) bool{
		"/v1/recover/:type":          skipAuthFor(v1PreLoginRoutes),
		"/v1/recover/:type/:state":   skipAuthFor(v1PreLoginRoutes),
		"/v2/zimaos/device/info":     skipAuthFor(v2PreLoginRoutes),
		"/v2/zimaos/device/image":    skipAuthFor(v2PreLoginRoutes),
		"/v2/zimaos/zt/info":         skipAuthFor(v2PreLoginRoutes),
		"/v2/mod_management/modules": skipAuthFor(v2PreLoginRoutes),
	} {
		if !skip(ctxFor(pattern, pattern, nil)) {
			t.Errorf("%s no longer answers before login", pattern)
		}
	}
}

// The list is matched against the ROUTE PATTERN, so a crafted URL cannot be
// bent into a listed entry, and a listed parameterised route does not act as a
// prefix that swallows siblings.
func TestTheAllowListCannotBeReachedByCraftingAURL(t *testing.T) {
	skip := skipAuthFor(v2PreLoginRoutes)
	for _, tc := range []struct{ pattern, raw string }{
		{"/v2/app_management/compose", "/v2/app_management/compose?x=/v2/zimaos/device/info"},
		{"/v2/app_management/compose", "/v2/zimaos/device/info/../../app_management/compose"},
		{"/v2/app_management/compose", "/v2/zimaos/device/info%2f..%2fapp_management/compose"},
	} {
		if skip(ctxFor(tc.pattern, tc.raw, nil)) {
			t.Errorf("a request the router matched to %s skipped auth because its "+
				"raw URL mentioned an allowed path (%s)", tc.pattern, tc.raw)
		}
	}
	// A listed route must not shelter children it does not name.
	if skip(ctxFor("/v2/zimaos/device/info/secret", "/v2/zimaos/device/info/secret", nil)) {
		t.Error("a child of a listed route inherited the exemption")
	}
}

// The lists are meant to be short and reviewed. A silent growth spurt is how an
// exemption list stops being a boundary.
func TestThePreLoginListsStaySmall(t *testing.T) {
	if n := len(v1PreLoginRoutes); n > 2 {
		t.Errorf("v1 pre-login list grew to %d entries. Each one answers to the "+
			"internet with no credential; adding to it needs a reviewed reason "+
			"in the comment above the variable, and this bound raised on purpose", n)
	}
	if n := len(v2PreLoginRoutes); n > 6 {
		t.Errorf("v2 pre-login list grew to %d entries — see above", n)
	}
}
