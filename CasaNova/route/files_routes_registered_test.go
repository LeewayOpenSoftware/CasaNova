package route

// BUG 1's regression test.
//
// The multi-file download 404'd not because the handler was wrong but because
// its route was never registered: the line was commented out and named a
// handler that no longer exists. No handler-level test can catch that — the
// handler passes its own tests while being unreachable — so this asserts the
// ROUTE, which is the thing that was missing.

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// The endpoints CasaNova-UI-v2/src/api/files.ts calls, as method+path.
func TestFilesUIEndpointsAreRegistered(t *testing.T) {
	h := InitV1Router()
	e, ok := h.(*echo.Echo)
	if !ok {
		t.Fatalf("InitV1Router returned %T, cannot inspect its routes", h)
	}

	registered := make(map[string]bool)
	for _, r := range e.Routes() {
		registered[r.Method+" "+r.Path] = true
	}

	for _, want := range []string{
		// downloadAsArchive — multi-select download. This is the one that 404'd.
		http.MethodGet + " /v1/file/download",
		// compressFiles / extractArchive
		http.MethodPost + " /v1/file/compress",
		http.MethodPost + " /v1/file/extract",
	} {
		if !registered[want] {
			t.Errorf("%s is not registered; the UI calls it and gets 404", want)
		}
	}
}
