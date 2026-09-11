package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// OLA 3 — v2TokenExtractor used to be an inline closure that read only the
// Authorization header, despite this file's own comment (the custom logger
// config, above InitV2Router's JWT middleware) documenting that ?token=
// query-param auth is expected here for WebSocket/SSE callers.
func TestV2TokenExtractorFallsBackToQueryParam(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v2/storage?token=abc123", nil)
	c := e.NewContext(req, httptest.NewRecorder())

	got, err := v2TokenExtractor(c)
	if err != nil {
		t.Fatalf("v2TokenExtractor returned an error: %v", err)
	}
	if len(got) != 1 || got[0] != "abc123" {
		t.Fatalf("v2TokenExtractor(?token=abc123, no header) = %v, want [\"abc123\"] — a "+
			"WebSocket/SSE caller with no way to set a header must still authenticate",
			got)
	}
}

// Positive control: the header path — the common case — must still work,
// with the Bearer scheme stripped.
func TestV2TokenExtractorPrefersHeaderAndStripsBearer(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v2/storage?token=ignored", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer real-token")
	c := e.NewContext(req, httptest.NewRecorder())

	got, err := v2TokenExtractor(c)
	if err != nil {
		t.Fatalf("v2TokenExtractor returned an error: %v", err)
	}
	if len(got) != 1 || got[0] != "real-token" {
		t.Fatalf("v2TokenExtractor(header set) = %v, want [\"real-token\"]", got)
	}
}
