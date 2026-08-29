package route

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// serveWith runs h behind a real http.Server with NO absolute timeouts, wrapped
// the way main.go wraps the public listeners. A real server is required: the
// deadline reaches through http.ResponseController to the actual connection,
// which httptest's recorder cannot model.
func serveWith(t *testing.T, idle time.Duration, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: WithIdleDeadlines(h, idle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return "http://" + ln.Addr().String()
}

// THE BUG THIS FIXES: a download that is progressing perfectly was killed by an
// absolute WriteTimeout, and the client saw HTTP 200 with a short file.
//
// Here the body takes 6x the idle window to send, but never pauses for longer
// than a fraction of it. Under the old absolute deadline this is exactly the
// case that got truncated. It must now complete, byte for byte.
func TestASlowButProgressingDownloadIsNotTruncated(t *testing.T) {
	const idle = 300 * time.Millisecond
	const chunks, chunkSize = 12, 64 << 10
	const total = chunks * chunkSize

	url := serveWith(t, idle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, chunkSize)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(buf); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(idle / 2) // always progressing, never idle past the window
		}
	}))

	resp, err := http.Get(url + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)

	if err != nil {
		t.Fatalf("a steadily-progressing download failed after %d/%d bytes: %v — "+
			"this is the truncation the idle deadline exists to prevent", n, total, err)
	}
	if n != total {
		t.Fatalf("received %d of %d bytes with status %d: TRUNCATED, and the "+
			"status still says success", n, total, resp.StatusCode)
	}
}

// The other half: the protection must still protect.
//
// The shape that matters is NOT a sleeping handler — a write deadline only
// bites when a write happens, so a handler that pauses is simply cut when it
// resumes, and the first version of this test was really measuring its own
// sleep. The shape the timeout defends against is a client that requests a
// large body, reads the headers, and then STOPS READING. The socket buffers
// fill, the server's Write blocks, and without a deadline that connection is
// pinned for as long as the peer cares to hold it.
func TestAClientThatStopsReadingIsCut(t *testing.T) {
	const idle = 300 * time.Millisecond
	const body = 64 << 20 // far larger than any socket buffer

	writeErr := make(chan error, 1)
	url := serveWith(t, idle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(body))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 256<<10)
		for sent := 0; sent < body; {
			n, err := w.Write(buf)
			if err != nil {
				writeErr <- err
				return
			}
			sent += n
		}
		writeErr <- nil
	}))

	// Raw connection so we can read the headers and then go silent.
	conn, err := net.Dial("tcp", url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n"))
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Deliberately read nothing further.

	start := time.Now()
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("the whole body was written to a client that stopped reading — " +
				"impossible unless nothing bounds it")
		}
		if el := time.Since(start); el > 4*idle {
			t.Errorf("the wedged write was cut after %v, well past the %v window", el, idle)
		}
	case <-time.After(6 * idle):
		t.Fatal("a client that stopped reading pinned the connection past the idle " +
			"window: the deadline is not armed, so a peer can hold a goroutine " +
			"and a socket for as long as it likes")
	}
}

// WEBSOCKET MUST STILL WORK. The wrapper sits between the server and
// httputil.ReverseProxy, which answers a 101 by hijacking the connection. A
// wrapper without Hijack turns every WebSocket in the product into
// "can't switch protocols using non-Hijacker ResponseWriter".
func TestTheWrapperStillSupportsHijack(t *testing.T) {
	hijacked := make(chan error, 1)

	url := serveWith(t, time.Second, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			hijacked <- http.ErrNotSupported
			return
		}
		conn, brw, err := h.Hijack()
		if err != nil {
			hijacked <- err
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		hijacked <- nil
	}))

	conn, err := net.Dial("tcp", url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: x\r\nConnection: Upgrade\r\n\r\n"))

	select {
	case err := <-hijacked:
		if err != nil {
			t.Fatalf("Hijack failed through the wrapper: %v — every WebSocket "+
				"(container terminal, logs, file operations) would break", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never reached Hijack")
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line[:12] != "HTTP/1.1 101" {
		t.Fatalf("upgrade response was %q (%v)", line, err)
	}
}

// And Flush must reach the real writer, or every SSE stream buffers.
func TestTheWrapperForwardsFlush(t *testing.T) {
	got := make(chan bool, 1)
	url := serveWith(t, time.Second, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, isFlusher := w.(http.Flusher)
		got <- isFlusher
		w.(http.Flusher).Flush()
	}))
	resp, err := http.Get(url + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !<-got {
		t.Fatal("the wrapped ResponseWriter is not an http.Flusher: SSE and " +
			"chunked streams would be buffered instead of streamed")
	}
}

// THE TEST THAT THE FIRST FOUR MISSED.
//
// All of them passed while production truncated an 7.8 MB download at 88.9%.
// The difference is the CLIENT: they used http.Get on loopback, which drains
// the response as fast as the server produces it, so the connection is never
// left sitting. Production had curl --limit-rate, a client that reads slowly —
// which is what a phone on hotel WiFi is.
//
// With a slow reader, the server's writes block, the request sits for minutes,
// and any deadline that nothing extends fires. That is exactly what happened:
// the wrapper armed a READ deadline up front, a GET has no body, so nothing
// ever called Read to extend it, and at `idle` the connection was torn down
// mid-response.
//
// A test whose client is faster than the server cannot find this class of bug.
func TestASlowREADINGClientReceivesTheWholeBody(t *testing.T) {
	const idle = 250 * time.Millisecond
	const body = 512 << 10

	url := serveWith(t, idle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(body))
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, newPattern(), body)
	}))

	conn, err := net.Dial("tcp", url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /slowread HTTP/1.1\r\nHost: x\r\n\r\n"))

	br := bufio.NewReader(conn)
	// Consume the header block.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Read the body deliberately slowly: well past `idle` in total, in small
	// steps, exactly like a rate-limited client.
	buf := make([]byte, 4096)
	got := 0
	deadline := time.Now().Add(20 * time.Second)
	for got < body {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after receiving %d of %d bytes", got, body)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := br.Read(buf)
		got += n
		if err != nil {
			t.Fatalf("the connection was cut after %d of %d bytes (%.1f%%): %v — "+
				"this is the truncation the idle deadline exists to prevent, and a "+
				"client that reads slowly is the ONLY shape that exposes it",
				got, body, 100*float64(got)/float64(body), err)
		}
		time.Sleep(12 * time.Millisecond) // ~2.5x idle over the whole transfer
	}
	if got != body {
		t.Fatalf("received %d of %d bytes", got, body)
	}
}

type pattern struct{ n byte }

func newPattern() io.Reader { return &pattern{} }
func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		p.n++
		b[i] = p.n
	}
	return len(b), nil
}

// THE TEST THAT ACTUALLY CATCHES IT.
//
// The three tests above all passed while production truncated a 7.8 MB
// download at 88.9%. The cause was an up-front read deadline that nothing
// extended on a GET — but none of them reproduced it, because none of them put
// a REVERSE PROXY in the path. A plain handler writing to the connection
// behaves differently from httputil.ReverseProxy copying a backend body: the
// proxy's copy loop is what notices the connection going away, and it converts
// that into a panic(ErrAbortHandler) that the caller sees as a short file with
// status 200.
//
// The lesson is in the shape of the test, not the assertion: a wrapper for a
// proxy has to be tested THROUGH a proxy. Testing it against a handler tests a
// different program.
func TestASlowClientThroughAProxyReceivesTheWholeBody(t *testing.T) {
	const idle = 250 * time.Millisecond
	const body = 1 << 20

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(body))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, newPattern(), body)
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = 0

	url := serveWith(t, idle, proxy)

	conn, err := net.Dial("tcp", url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /viaproxy HTTP/1.1\r\nHost: x\r\n\r\n"))

	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	buf := make([]byte, 4096)
	got := 0
	for got < body {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := br.Read(buf)
		got += n
		if err != nil {
			t.Fatalf("the proxied transfer was cut after %d of %d bytes (%.1f%%): "+
				"%v — a deadline that nothing extends fired mid-response, which is "+
				"the production truncation this whole file exists to prevent",
				got, body, 100*float64(got)/float64(body), err)
		}
		time.Sleep(6 * time.Millisecond) // ~1.5s total, 6x the idle window
	}
}
