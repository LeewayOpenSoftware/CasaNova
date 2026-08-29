package route

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/gatewayroutes"
	"github.com/labstack/echo/v4"
)

// A handler in this service only ever runs if the gateway routes the request
// here, and the gateway decides that from the literal prefixes each service
// publishes — never from the echo router. So a route can be registered,
// reviewed, and security-hardened while receiving no traffic at all. That is
// how "/v1/container/:id/logs" answered 404 for every caller while a hardened
// WebSocket handler for it sat in this binary, and why the terminal route next
// to it could be "fixed" repeatedly with no observable effect.
//
// This test closes that gap: every route this service registers must resolve
// back to this service through the shared ownership manifest.

// sampleFor turns an echo route pattern into a concrete request path, so it
// can be run through the gateway's prefix matcher.
func sampleFor(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		switch {
		case strings.HasPrefix(seg, ":"):
			// A realistic value: the point is that the gateway sees an
			// arbitrary segment here, which no literal prefix can cover.
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

func registeredPaths(t *testing.T, h http.Handler) []*echo.Route {
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

func TestEveryV1RouteIsReachableThroughTheGateway(t *testing.T) {
	for _, r := range registeredPaths(t, InitV1Router()) {
		sample := sampleFor(r.Path)
		owner, ok := gatewayroutes.Resolve(sample)
		if !ok {
			t.Errorf("%s %s: no gateway prefix matches %q", r.Method, r.Path, sample)
			continue
		}
		if owner != gatewayroutes.OwnerCasaNova {
			t.Errorf("%s %s is registered in casanova but the gateway sends %q to %q. "+
				"The handler will never run. Either publish a prefix that covers it "+
				"(impossible when a path parameter precedes the distinguishing segment) "+
				"or move the handler to the owning service.",
				r.Method, r.Path, sample, owner)
		}
	}
}

// TestContainerIDRoutesAreNotRegisteredHere is the direct regression guard for
// the defect: app-management owns "/v1/container", so no "/v1/container/:id/…"
// route can work from this binary. Re-adding one here fails this test.
func TestContainerIDRoutesAreNotRegisteredHere(t *testing.T) {
	for _, r := range registeredPaths(t, InitV1Router()) {
		if strings.HasPrefix(r.Path, "/v1/container/:") {
			t.Errorf("%s %s: a per-container route registered here is unreachable — "+
				"the gateway hands /v1/container/<anything> to app-management", r.Method, r.Path)
		}
	}
}
