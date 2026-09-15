package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
)

// countingWriter counts Write calls and records whether Flush was asked for.
// The COUNT is the property under test: how many times the proxy's copy loop
// crosses the writer boundary for one body.
type countingWriter struct {
	hdr     http.Header
	writes  int
	flushes int
	total   int
}

func (c *countingWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}
func (c *countingWriter) Write(b []byte) (int, error) {
	c.writes++
	c.total += len(b)
	return len(b), nil
}
func (c *countingWriter) WriteHeader(int) {}
func (c *countingWriter) Flush()          { c.flushes++ }

// THE REGRESSION THIS PINS, and why it is a count and not a stopwatch.
//
// ReverseProxy allocates a 32 KiB buffer per request when no BufferPool is set,
// and moves the body in 32 KiB steps. Measured on the production box, a 512 MiB
// download went at 3.18 GB/s straight from the backend and 1.55 GB/s through
// this gateway — a 2x loss, and 867 ms of gateway CPU per GiB merely forwarded.
// The overhead was a constant ~10.9 us per 32 KiB chunk across an 8x range of
// sizes, so it is per-chunk, and the fix is to make the chunks bigger.
//
// A duration assertion here would be flaky on a shared machine and would be
// deleted the first time CI hiccuped. The number of writes is exact, stable,
// and fails loudly the moment somebody drops the BufferPool.
func TestProxyMovesBodiesInLargeChunks(t *testing.T) {
	const bodySize = 4 << 20 // 4 MiB

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(bodySize))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, devZero{}, bodySize)
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newConfiguredProxy(target)

	rec := &countingWriter{}
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	proxy.ServeHTTP(rec, req)

	if rec.total != bodySize {
		t.Fatalf("proxied %d bytes, want %d", rec.total, bodySize)
	}

	// With the 32 KiB default this is 128 writes. With a 256 KiB pool it is 16.
	// Allow generous slack for however the transport frames reads — the point is
	// the ORDER OF MAGNITUDE, which is what the buffer size controls.
	const with32k = bodySize / (32 << 10) // 128
	if rec.writes >= with32k {
		t.Fatalf("the proxy performed %d writes for a %d MiB body — that is the "+
			"32 KiB default (%d writes), so the BufferPool is gone or too small. "+
			"Measured cost of that regression: gateway throughput halves, from "+
			"3.18 GB/s to 1.55 GB/s, and 867 ms of CPU per GiB forwarded.",
			rec.writes, bodySize>>20, with32k)
	}

	// And it must not be flushing after every write on a plain, sized body.
	if rec.flushes > rec.writes {
		t.Errorf("%d flushes for %d writes: FlushInterval is negative again, "+
			"which costs a flush per chunk on bulk transfers", rec.flushes, rec.writes)
	}
	t.Logf("4 MiB body: %d writes, %d flushes (32 KiB default would be %d writes)",
		rec.writes, rec.flushes, with32k)
}

// The buffers themselves must be the size the measurement assumed.
func TestProxyBufferPoolHandsOutLargeBuffers(t *testing.T) {
	b := proxyBuffers.Get()
	defer proxyBuffers.Put(b)
	if len(b) != proxyCopyBufferSize {
		t.Fatalf("pool handed out %d bytes, want %d", len(b), proxyCopyBufferSize)
	}
	if proxyCopyBufferSize <= 32<<10 {
		t.Fatalf("proxyCopyBufferSize is %d, which is not larger than the 32 KiB "+
			"default it exists to replace", proxyCopyBufferSize)
	}
}

// STREAMING MUST STILL BE IMMEDIATE. Go overrides FlushInterval to -1 for
// text/event-stream and for responses with unknown length; this asserts that
// the behaviour we are relying on is really there, so a Go upgrade that changed
// it would fail here rather than silently delaying every progress bar.
func TestStreamingResponsesAreStillFlushedImmediately(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 4; i++ {
			_, _ = w.Write([]byte("data: tick\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	rec := &countingWriter{}
	newConfiguredProxy(target).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sse", nil))

	if rec.flushes == 0 {
		t.Fatal("an SSE response was proxied with ZERO flushes: install progress " +
			"bars and log streams would stall until the buffer filled")
	}
	t.Logf("SSE: %d writes, %d flushes", rec.writes, rec.flushes)
}

type devZero struct{}

func (devZero) Read(p []byte) (int, error) { return len(p), nil }

// The internal header is accepted by requirePermInternal as a peer identity;
// a public gateway request must therefore lose it before it can reach a
// backend's internal gate. Use non-canonical map keys deliberately: a client
// may choose any header casing on the wire.
func TestProxyDirectorStripsForgedInternalPeerHeader(t *testing.T) {
	target, err := url.Parse("http://backend.invalid")
	if err != nil {
		t.Fatal(err)
	}

	for _, casing := range []string{
		internalauth.HeaderName,
		"x-casanova-internal",
		"X-CASANOVA-INTERNAL",
	} {
		t.Run(casing, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://gateway.invalid/private", nil)
			req.Header = http.Header{casing: {"forged-peer-secret"}}
			newConfiguredProxy(target).Director(req)
			for key, values := range req.Header {
				if strings.EqualFold(key, internalauth.HeaderName) {
					t.Fatalf("forged internal identity reached backend header map as %q: %q", key, values)
				}
			}
		})
	}
}
