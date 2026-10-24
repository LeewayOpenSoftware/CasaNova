package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamBody yields a JSON prefix followed by filler up to size bytes without
// allocating it, and records how many bytes the server consumed.
type streamBody struct {
	prefix []byte
	size   int64
	read   int64
}

func (b *streamBody) Read(p []byte) (int, error) {
	if b.read >= b.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && b.read < b.size {
		if b.read < int64(len(b.prefix)) {
			p[n] = b.prefix[b.read]
		} else {
			p[n] = 'a'
		}
		n++
		b.read++
	}
	return n, nil
}

func (b *streamBody) Close() error { return nil }

// The pre-auth JSON routes must stop reading an oversized body instead of
// letting ctx.Bind buffer all of it.
func TestPreAuthJSONRoutesCapBodySize(t *testing.T) {
	h := InitRouter()
	const size = 64 << 20
	for _, path := range []string{"/v1/users/login", "/v1/users/register", "/v1/users/refresh", "/v1/users/logout"} {
		// Chunked (no Content-Length): the limit must hold while reading.
		body := &streamBody{prefix: []byte(`{"username":"`), size: size}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Body = body
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if body.read > 2<<20 {
			t.Errorf("%s: server consumed %d bytes of a %d byte body; want it capped near 1M", path, body.read, int64(size))
		}

		// Declared Content-Length over the limit: refused up front.
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader("x")) // invalid JSON: never reaches the service layer
		req.ContentLength = 2 << 20
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: declared 2 MiB body got status %d, want 413", path, rec.Code)
		}
	}
}
