package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"github.com/labstack/echo/v4"
)

// mgmtCtxFor builds a request as the management server always sees them:
// arriving over loopback. The management listener binds 127.0.0.1 only, so
// EVERY request it can ever receive looks like this one.
func mgmtCtxFor(pattern string, hdr map[string]string) echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, pattern, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "127.0.0.1:54321" // the address that used to be enough
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath(pattern)
	return c
}

func mgmtSkipper() func(echo.Context) bool {
	return NewManagementRoute(service.NewManagementService(service.NewState())).
		managementJWTConfig().Skipper
}

// THE PROPERTY THE OLD SKIPPER FAILED.
//
// It was `return loopback.IsLoopback(c)`. Because the management server binds
// loopback only, that waiver covered every request it can ever receive: the
// JWT middleware and the per-route RBAC checks below it were dead code. And
// loopback is not "internal" — the gateway reverse-proxies the LAN and the
// Cloudflare tunnel over 127.0.0.1, so the exemption reached the internet.
func TestLoopbackWithoutTheSecretDoesNotSkipManagementAuth(t *testing.T) {
	skip := mgmtSkipper()

	for _, pattern := range []string{
		"/v1/gateway/routes",
		"/v1/gateway/port",
		"/v1/gateway/tls",
		"/v1/gateway/whitelist",
	} {
		if skip(mgmtCtxFor(pattern, nil)) {
			t.Errorf("%s skips authentication for a credential-less request from "+
				"127.0.0.1 — which is every request this server can receive", pattern)
		}
	}
}

// A wrong or truncated secret is not a credential either.
func TestAWrongInternalSecretDoesNotSkipManagementAuth(t *testing.T) {
	dir := t.TempDir()
	internalauth.SetPathsForTest(dir)
	secret, err := internalauth.Secret()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}

	skip := mgmtSkipper()
	for _, bad := range []string{"", secret + "x", secret[:len(secret)-1]} {
		if skip(mgmtCtxFor("/v1/gateway/routes", map[string]string{internalauth.HeaderName: bad})) {
			t.Errorf("a request bearing %q as the internal secret skipped authentication", bad)
		}
	}
}

// The peer services in CasaNova-Common/external/gateway.go register their
// routes through this API at boot and hold the root-only secret. If this
// stopped skipping, route registration would fail on every start.
func TestAPeerHoldingTheInternalSecretSkipsManagementAuth(t *testing.T) {
	dir := t.TempDir()
	internalauth.SetPathsForTest(dir)
	secret, err := internalauth.Secret()
	if err != nil {
		t.Fatalf("the internal secret could not be provisioned: %v", err)
	}

	skip := mgmtSkipper()
	if !skip(mgmtCtxFor("/v1/gateway/routes", map[string]string{internalauth.HeaderName: secret})) {
		t.Error("a peer presenting the per-boot internal secret was not allowed to " +
			"skip the user-token check; route registration would fail at boot")
	}
}
