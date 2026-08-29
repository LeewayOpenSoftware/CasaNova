package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/labstack/echo/v4"
)

// prodRouter builds THE REAL ROUTER, not a re-creation of it.
//
// The first version of this file built its own echo instance and attached the
// gzip middleware the way routers.go attaches it. Every test passed — and all
// three mutations of the production code survived, because the tests were
// exercising a copy of the intent rather than the wiring. A test that rebuilds
// the thing it is testing tests nothing.
// prodHealthPath derives the same path routers.go computes, using the same
// helpers, so the test cannot drift from the router it exercises.
func prodHealthPath(t *testing.T) string {
	t.Helper()
	swagger, err := codegen.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	apiPath, err := getAPIPath(getSwaggerURL(swagger))
	if err != nil {
		t.Fatal(err)
	}
	return apiPath + HealthPathSuffix
}

func prodRouter(t *testing.T) http.Handler {
	t.Helper()
	swagger, err := codegen.GetSwagger()
	if err != nil {
		t.Fatalf("the embedded spec is unreadable, so the real router cannot be "+
			"built and this test would silently become a no-op: %v", err)
	}
	h, err := NewAPIRouter(swagger, &service.Services{})
	if err != nil {
		t.Fatalf("NewAPIRouter failed: %v", err)
	}
	return h
}

// THE POINT: this process serves one handler on two listeners. Compressing a
// response that goes from one local process to another over a unix socket
// cannot save a byte — it is 38 us and ~1.6 KB of pure waste per publish, on
// the hottest inter-service path in the box.
func TestPublishOverTheUnixSocketIsNotCompressed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, prodHealthPath(t), nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip") // what a Go client sends
	// Stamp the context exactly as the socket listener's ConnContext does.
	req = req.WithContext(UnixSocketConnContext(req.Context(), nil))

	rec := httptest.NewRecorder()
	prodRouter(t).ServeHTTP(rec, req)

	if enc := rec.Header().Get(echo.HeaderContentEncoding); enc == "gzip" {
		t.Fatal("a publish accepted on the unix socket was gzip-compressed. " +
			"Compression between two processes on the same kernel saves nothing " +
			"and costs 38 us and ~1.6 KB per publish.")
	}
}

// THE OTHER HALF: browsers reach this service over loopback TCP through the
// gateway, and for them compression is the whole point. A skipper that fires
// for everyone would trade a microsecond win for a real bandwidth loss.
func TestBrowserTrafficIsStillCompressed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, prodHealthPath(t), nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	// No ConnContext stamp: this is the TCP listener.

	rec := httptest.NewRecorder()
	prodRouter(t).ServeHTTP(rec, req)

	if enc := rec.Header().Get(echo.HeaderContentEncoding); enc != "gzip" {
		t.Fatalf("a browser-path response was NOT compressed (Content-Encoding %q): "+
			"the skipper is firing for TCP traffic too, which costs real bandwidth "+
			"over the Cloudflare tunnel", enc)
	}
}

// THE TRUST BOUNDARY: the decision must come from the listener that accepted
// the connection, never from something the caller can set. A Host header
// saying "unix" was once enough to bypass JWT here, from any TCP caller.
func TestAHeaderCannotClaimToBeTheSocket(t *testing.T) {
	for _, h := range []struct{ k, v string }{
		{"Host", "unix"},
		{"X-Forwarded-For", "unix"},
		{"X-Real-IP", "/run/casanova/internal/message-bus.sock"},
	} {
		req := httptest.NewRequest(http.MethodGet, prodHealthPath(t), nil)
		req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
		req.Header.Set(h.k, h.v)
		if h.k == "Host" {
			req.Host = h.v
		}
		rec := httptest.NewRecorder()
		prodRouter(t).ServeHTTP(rec, req)

		if rec.Header().Get(echo.HeaderContentEncoding) != "gzip" {
			t.Errorf("a TCP request claiming %s: %q was treated as socket traffic. "+
				"The socket predicate must depend only on the ConnContext stamp, "+
				"which no caller can forge.", h.k, h.v)
		}
	}
}
