package route

// Pins the clickjacking header on the static surface.
//
// History: the UI shipped frame-ancestors inside its <meta> CSP, where the
// spec says browsers must ignore it — every page load logged an error and
// nothing was protected. The directive only counts as an HTTP header, and the
// gateway's static route is the one place that serves the dashboard's HTML,
// so this is where the protection either exists or does not.
//
// 'self', not 'none': the dashboard may frame itself; foreign origins may
// not. A policy carrying only frame-ancestors combines with the page's own
// <meta> policy rather than replacing it, so nothing else tightens or
// loosens by accident.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
)

func staticHandlerOverTempWWW(t *testing.T) http.Handler {
	t.Helper()
	www := t.TempDir()
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<!doctype html><title>x</title>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	state := &service.State{}
	if err := state.SetWWWPath(www); err != nil {
		t.Fatalf("set www path: %v", err)
	}
	return NewStaticRoute(state).GetRoute()
}

func TestStaticServesFrameAncestorsAsARealHeader(t *testing.T) {
	h := staticHandlerOverTempWWW(t)

	for _, path := range []string{"/", "/index.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors") {
			t.Fatalf("%s: no frame-ancestors in CSP header (got %q) — the <meta> variant is ignored by browsers, so without this header there is no clickjacking protection at all", path, csp)
		}
		if !strings.Contains(csp, "'self'") || strings.Contains(csp, "'none'") {
			t.Fatalf("%s: frame-ancestors must be 'self' (dashboard may embed itself), got %q", path, csp)
		}
		if got := rec.Header().Get("X-Frame-Options"); !strings.EqualFold(got, "SAMEORIGIN") {
			t.Fatalf("%s: X-Frame-Options = %q, want SAMEORIGIN", path, got)
		}
	}
}
