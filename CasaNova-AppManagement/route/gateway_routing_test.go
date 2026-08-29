package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/gatewayroutes"
	"github.com/labstack/echo/v4"
)

// The gateway routes on literal path prefixes, so whether a handler in this
// binary ever runs is decided by the ownership manifest, not by the echo
// router. Two checks follow from that, and between them they pin the defect
// that left container log streaming answering 404 while a hardened handler for
// it lived in another service:
//
//   - nothing registered here may resolve to a different service, and
//   - everything the gateway sends here must actually be handled here.

func sampleFor(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		switch {
		case strings.HasPrefix(seg, ":"):
			segments[i] = "9b1deadbeef0"
		case strings.HasPrefix(seg, "*"):
			segments[i] = "sample"
		}
	}
	return strings.Join(segments, "/")
}

func isHTTPMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return true
	}
	return false
}

func registeredRoutes(t *testing.T, h http.Handler) []*echo.Route {
	t.Helper()
	e, ok := h.(*echo.Echo)
	if !ok {
		t.Fatalf("router is %T, expected *echo.Echo", h)
	}
	var out []*echo.Route
	for _, r := range e.Routes() {
		if !isHTTPMethod(r.Method) {
			continue // echo's internal not-found / method-not-allowed entries
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("router registered no routes; the check would pass vacuously")
	}
	return out
}

// matches reports whether an echo route pattern matches a concrete path.
// Implemented here rather than by driving echo's router so the test never
// executes a handler (these handlers talk to dockerd).
func matches(pattern, path string) bool {
	p := strings.Split(strings.Trim(pattern, "/"), "/")
	c := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range p {
		if strings.HasPrefix(seg, "*") {
			return true
		}
		if i >= len(c) {
			return false
		}
		if strings.HasPrefix(seg, ":") {
			if c[i] == "" {
				return false
			}
			continue
		}
		if seg != c[i] {
			return false
		}
	}
	return len(p) == len(c)
}

func TestEveryRouteIsReachableThroughTheGateway(t *testing.T) {
	for _, r := range registeredRoutes(t, InitV1Router()) {
		sample := sampleFor(r.Path)
		owner, ok := gatewayroutes.Resolve(sample)
		if !ok {
			t.Errorf("%s %s: no gateway prefix matches %q", r.Method, r.Path, sample)
			continue
		}
		if owner != gatewayroutes.OwnerAppManagement {
			t.Errorf("%s %s is registered in app-management but the gateway sends %q to %q. "+
				"The handler will never run.", r.Method, r.Path, sample, owner)
		}
	}
}

// TestClientContainerEndpointsAreHandled walks the container URLs the web UI
// builds (CasaNova-UI-v2/src/api/containers.ts) and requires that every one the
// gateway routes to this service has a handler.
//
// "/v1/container/:id/logs" was the one that did not: the route was commented
// out here, so the gateway delivered the request and this service answered 404,
// while the real implementation sat unreachable in the main service.
func TestClientContainerEndpointsAreHandled(t *testing.T) {
	clientCalls := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/container/all"},
		{http.MethodGet, "/v1/container/stats"},
		{http.MethodGet, "/v1/container/image/tags"},
		{http.MethodGet, "/v1/container/usage"},
		{http.MethodGet, "/v1/container/9b1deadbeef0/logs"},
		{http.MethodGet, "/v1/container/9b1deadbeef0/terminal"},
		{http.MethodPost, "/v1/container/9b1deadbeef0/start"},
		{http.MethodPost, "/v1/container/9b1deadbeef0/stop"},
		{http.MethodPost, "/v1/container/9b1deadbeef0/restart"},
	}

	routes := registeredRoutes(t, InitV1Router())
	checked := 0
	for _, call := range clientCalls {
		owner, ok := gatewayroutes.Resolve(call.path)
		if !ok {
			t.Errorf("%s %s: no gateway prefix matches", call.method, call.path)
			continue
		}
		if owner != gatewayroutes.OwnerAppManagement {
			continue // another service owns it; its own test covers it
		}
		checked++
		found := false
		for _, r := range routes {
			if r.Method == call.method && matches(r.Path, call.path) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the gateway routes %s %s to app-management, but this service has no handler for it: "+
				"callers get a 404 no matter what any other service implements", call.method, call.path)
		}
	}
	if checked == 0 {
		t.Fatal("no client endpoint resolved to app-management; the check proved nothing")
	}
}

// TestWebSocketRoutesRejectABadQueryToken guards the other half of the fix.
//
// A browser cannot set headers on a WebSocket handshake, so the UI puts the JWT
// in ?token=. The group's JWT middleware skips validation for loopback requests
// that carry no Authorization header — and the gateway reverse-proxies every
// request over 127.0.0.1, so that describes every real WebSocket request. The
// token was therefore never looked at: the role stayed guest, which happens to
// hold PermAppView, so a forged or expired token would have been served logs.
//
// The request below is shaped exactly like the gateway's: loopback peer, no
// Authorization header, a junk ?token=. With strictJWT mounted it is rejected
// at the middleware. Drop strictJWT from the route and the request falls
// through to the handler instead, which is what this test exists to catch.
func TestWebSocketRoutesRejectABadQueryToken(t *testing.T) {
	router := InitV1Router()

	for _, path := range []string{
		"/v1/container/9b1deadbeef0/logs?tail=500&token=not-a-jwt",
		"/v1/container/9b1deadbeef0/terminal?token=not-a-jwt",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		// The gateway proxies over loopback; that is what makes the group
		// middleware's skipper fire.
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s returned %d; an unverifiable token must be rejected before the handler runs "+
				"(a loopback request with no Authorization header is exactly what every gateway-proxied "+
				"WebSocket looks like)", path, rec.Code)
		}
	}
}
