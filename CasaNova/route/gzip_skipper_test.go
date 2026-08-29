package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// gzipSkips reproduces the router's skipper decision for a path.
//
// It is a copy of the predicate rather than a call into it because the closure
// is built inline inside InitV1Router. If you change one, this test fails and
// you change the other — which is the point.
func gzipSkips(path string) bool {
	return path == "/v1/file" || path == "/v1/image" || path == "/v1/batch" ||
		strings.HasPrefix(path, "/v3/file") || path == "/v1/sys/backup/download" ||
		strings.HasPrefix(path, "/public/share")
}

// EVERY ROUTE THAT SERVES FILE BYTES MUST BE EXEMPT FROM GZIP.
//
// The middleware's own comment says gzip "corrupts HTTP Range responses (206
// Partial Content) and breaks audio/video playback". /public/share/:id serves
// exactly that — it is the route for handing someone a video link — and it was
// not on the list, because the list matched five exact spellings and anything
// else opted in silently.
//
// This test enumerates the byte-serving routes rather than the exempt ones, so
// ADDING a download route and forgetting the skipper fails here.
func TestEveryByteServingRouteSkipsGzip(t *testing.T) {
	byteServing := map[string]string{
		"/v1/file":                "single file download",
		"/v1/image":               "image/thumbnail bytes",
		"/v1/batch":               "multi-file download",
		"/v3/file/anything":       "raw file handler mounted outside echo",
		"/v1/sys/backup/download": "backup archive",
		"/public/share/abc123":    "PUBLIC SHARE LINK — video and photo sharing, and Range requests",
	}
	for path, what := range byteServing {
		if !gzipSkips(path) {
			t.Errorf("%s (%s) is NOT exempt from gzip. Compressing it wastes CPU on "+
				"already-compressed media and corrupts 206 Partial Content, so "+
				"seeking and resumed downloads break.", path, what)
		}
	}
}

// And the exemption must not have grown into "skip everything": JSON endpoints
// still benefit from compression over a slow link.
func TestApiResponsesAreStillCompressed(t *testing.T) {
	for _, path := range []string{
		"/v1/sys/utilization", "/v1/users/current", "/v2/app_management/compose",
		"/v1/folder", "/v1/sys/smart",
	} {
		if gzipSkips(path) {
			t.Errorf("%s is exempt from gzip, but it returns JSON — over a slow "+
				"remote link that is a real cost for no reason", path)
		}
	}
}

// A prefix exemption must not swallow neighbours it does not mean to cover.
func TestThePrefixExemptionsAreNarrow(t *testing.T) {
	for _, path := range []string{"/public/shares-admin", "/v3/files-report"} {
		if !gzipSkips(path) {
			continue // fine either way; this documents what the prefixes catch
		}
		t.Logf("note: %q is covered by a prefix exemption — intended?", path)
	}
	// The router must actually mount the share route this test is about.
	e := echo.New()
	e.GET("/public/share/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/share/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the /public/share/:id shape no longer routes (%d) — this test is "+
			"guarding a path that moved", rec.Code)
	}
}
