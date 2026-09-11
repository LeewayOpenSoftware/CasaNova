package cmd

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCLIAuthTransportRefusesNonRoot(t *testing.T) {
	oldEUID, oldHeader := cliEUID, cliInternalHeader
	t.Cleanup(func() { cliEUID, cliInternalHeader = oldEUID, oldHeader })
	cliEUID = func() int { return 1000 }
	cliInternalHeader = func() (map[string]string, error) {
		t.Fatal("credential reader must not run for non-root")
		return nil, nil
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (cliAuthTransport{}).RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "must be run as root") {
		t.Fatalf("non-root transport error = %v, want refusal", err)
	}
}

func TestCLIAuthTransportAttachesInternalHeader(t *testing.T) {
	oldEUID, oldHeader := cliEUID, cliInternalHeader
	t.Cleanup(func() { cliEUID, cliInternalHeader = oldEUID, oldHeader })
	cliEUID = func() int { return 0 }
	cliInternalHeader = func() (map[string]string, error) {
		return map[string]string{"X-CasaNova-Internal": "test-secret"}, nil
	}
	seen := ""
	transport := cliAuthTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = req.Header.Get("X-CasaNova-Internal")
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("root transport = (%v, %v), want 204 response", resp, err)
	}
	if seen != "test-secret" {
		t.Fatalf("internal header = %q, want test-secret", seen)
	}
}
