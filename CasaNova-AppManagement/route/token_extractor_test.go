package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// extractorFor pulls the single TokenLookupFunc out of a router's JWT config by
// exercising it directly. Both routers build it inline, so the test reproduces
// the call the middleware makes.
func runExtractor(t *testing.T, f func(echo.Context) ([]string, error), rawURL string, authHeader string) string {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	if authHeader != "" {
		req.Header.Set(echo.HeaderAuthorization, authHeader)
	}
	c := e.NewContext(req, httptest.NewRecorder())
	got, err := f(c)
	if err != nil {
		t.Fatalf("extractor returned an error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("extractor returned %d values, want 1", len(got))
	}
	return got[0]
}

// THE CONTRACT: an SSE stream must be able to authenticate.
//
// GET /v2/app_management/compose/install/:id/progress is Server-Sent Events,
// and EventSource cannot set request headers — the browser puts the JWT in
// ?token= (CasaNova-UI-v2/src/api/containers.ts). The v2 extractor read ONLY
// the Authorization header, so that stream was never authenticated; it worked
// because the loopback skipper waved it through. Tightening the skipper without
// this fallback turns every install progress bar into a silent 401.
//
// This test exists because that failure is invisible: no build breaks, no test
// fails, the bar just never fills.
func TestV2ExtractorAcceptsTheQueryTokenSSEMustUse(t *testing.T) {
	f := v2TokenExtractor

	if got := runExtractor(t, f, "/v2/app_management/compose/install/x/progress?token=abc.def.ghi", ""); got != "abc.def.ghi" {
		t.Fatalf("the v2 extractor ignored ?token= and returned %q. EventSource "+
			"cannot send headers, so install progress cannot authenticate at all.", got)
	}

	// The header still wins, and the Bearer scheme is still stripped.
	if got := runExtractor(t, f, "/v2/app_management/compose", "Bearer hdr.tok.en"); got != "hdr.tok.en" {
		t.Fatalf("header path broken: %q", got)
	}
	if got := runExtractor(t, f, "/v2/app_management/compose", "bearer hdr.tok.en"); got != "hdr.tok.en" {
		t.Fatalf("Bearer must be matched case-insensitively (RFC 6750 §2.1): %q", got)
	}
	if got := runExtractor(t, f, "/v2/app_management/compose", "raw.tok.en"); got != "raw.tok.en" {
		t.Fatalf("a bare token in the header must pass through: %q", got)
	}

	// A header, when present, is NOT overridden by a query token.
	if got := runExtractor(t, f, "/v2/app_management/compose?token=query.one", "Bearer header.one"); got != "header.one" {
		t.Fatalf("the query token overrode a presented header: %q", got)
	}

	// Nothing presented is the empty string, which ParseTokenFunc rejects.
	if got := runExtractor(t, f, "/v2/app_management/compose", ""); got != "" {
		t.Fatalf("a request with no credential produced %q", got)
	}
}
