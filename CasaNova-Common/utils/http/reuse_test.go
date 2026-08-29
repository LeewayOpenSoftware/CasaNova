package http

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener counts accepted TCP connections. The COUNT is the property:
// a duration assertion would be flaky and would get deleted, a connection count
// is exact.
type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (c countingListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err == nil {
		c.n.Add(1)
	}
	return conn, err
}

func serveCounting(t *testing.T, h http.HandlerFunc) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = countingListener{ln, &n}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// THE DEFECT: every peer client in this tree reads response.StatusCode and
// closes the body without reading it. net/http only returns a connection to the
// idle pool once the body has been read to EOF, so every inter-service call
// paid a fresh TCP handshake and teardown — measured at 200 connections for 200
// calls. The route watchdog alone re-registers 51 prefixes every 30 seconds for
// the life of the box.
func TestPeerCallsReuseOneConnection(t *testing.T) {
	const calls = 100
	url, conns := serveCounting(t, func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"success":200,"message":"ok"}`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	for i := 0; i < calls; i++ {
		// Exactly what every caller does: check the status, close the body.
		resp, err := Get(url+"/v1/notify/x", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	if got := conns.Load(); got > 3 {
		t.Fatalf("%d calls opened %d TCP connections. The body is not being "+
			"drained before Close, so the transport discards the connection every "+
			"time and every peer call pays a handshake.", calls, got)
	} else {
		t.Logf("%d calls over %d connection(s)", calls, got)
	}
}

// THE SAFETY HALF: Close must not start waiting on a body whose size the
// upstream never declared. A caller that abandons a slow or streaming response
// used to get an instant Close; draining unconditionally would make it block
// for the whole request timeout, which is a worse failure than the one being
// fixed.
func TestCloseDoesNotBlockOnAnUndeclaredBody(t *testing.T) {
	release := make(chan struct{})

	url, _ := serveCounting(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK) // chunked: no Content-Length
		w.(http.Flusher).Flush()
		<-release // never sends the rest
	})

	// Registered AFTER serveCounting on purpose: t.Cleanup is LIFO, so this
	// releases the blocked handler BEFORE httptest.Server.Close waits for it.
	// The other order deadlocks the test suite, which is how the first version
	// of this file hung for ten minutes.
	t.Cleanup(func() { close(release) })

	resp, err := Get(url+"/stream", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentLength >= 0 {
		t.Fatalf("setup: expected an undeclared length, got %d", resp.ContentLength)
	}

	done := make(chan struct{})
	go func() { resp.Body.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a body whose length was never declared — the " +
			"drain must only run for a small, KNOWN length, or abandoning a " +
			"stream costs the caller the whole request timeout")
	}
}

// And the context must still be cancelled by Close, which is what the wrapper
// existed for before the drain was added.
func TestCloseStillCancelsTheContext(t *testing.T) {
	url, _ := serveCounting(t, func(w http.ResponseWriter, r *http.Request) {
		body := []byte("ok")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})

	var captured context.Context
	resp, err := Do(func(ctx context.Context) (*http.Request, error) {
		captured = ctx
		return http.NewRequestWithContext(ctx, http.MethodGet, url+"/x", nil)
	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Err() != nil {
		t.Fatal("the context was cancelled before the body was closed")
	}
	resp.Body.Close()
	if captured.Err() == nil {
		t.Fatal("Close no longer cancels the request context, so every call leaks " +
			"a timer until its deadline expires")
	}
}
