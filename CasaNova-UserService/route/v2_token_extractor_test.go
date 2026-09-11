package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// OLA 3 — v2TokenExtractor used to be an inline closure whose own comment
// claimed to "mirror v1.go", but v1's extractor falls back to ?token= for
// callers that cannot set a header and this one did not.
func TestV2TokenExtractorFallsBackToQueryParam(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v2/users?token=abc123", nil)
	c := e.NewContext(req, httptest.NewRecorder())

	got, err := v2TokenExtractor(c)
	if err != nil {
		t.Fatalf("v2TokenExtractor returned an error: %v", err)
	}
	if len(got) != 1 || got[0] != "abc123" {
		t.Fatalf("v2TokenExtractor(?token=abc123, no header) = %v, want [\"abc123\"]", got)
	}
}

// Positive control: the header path — the common case — must still work,
// with the Bearer scheme stripped.
func TestV2TokenExtractorPrefersHeaderAndStripsBearer(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v2/users?token=ignored", nil)
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
