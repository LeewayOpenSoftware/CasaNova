package service

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A failed proxy request answers 502 with a generic body; the backend's
// internal address stays in the log.
func TestProxyErrorDoesNotLeakBackendAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	ln.Close() // closed port -> connection refused

	target, _ := url.Parse("http://" + addr)
	p := newConfiguredProxy(target)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{addr, port, "127.0.0.1", "dial tcp", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Errorf("502 body %q discloses %q", strings.TrimSpace(body), leak)
		}
	}
	if got := strings.TrimSpace(body); got != "Backend unavailable" {
		t.Errorf("502 body = %q, want %q", got, "Backend unavailable")
	}
}
