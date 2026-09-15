package docker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
)

// "THE REGISTRY DID NOT ANSWER" IS NOT "THE DIGEST IS EMPTY".
//
// GetDigest returned whatever Docker-Content-Digest held, with a nil error, on
// any 200. Caching proxies and several Harbor/Nexus configurations answer a
// HEAD with 200 and no such header, so the result was ("", nil): a digest that
// matches nothing. CompareDigest then compared "" against well-formed
// RepoDigests, never matched, and reported false WITHOUT AN ERROR — and its
// consumer in appstore_management.go reads that as `!match`, i.e. "an update
// is available". Permanently: the app wore an update badge forever and the
// nightly auto-update re-pulled the same image every night, because nothing
// anywhere said the question had never been answered.
//
// These drive the real GetDigest against a registry that behaves that way.
// Nothing reaches the network: the server is local and the image name points
// at it.

// The token argument is non-empty in every call below: GetDigest refuses an
// empty token before it ever reaches the registry, so an empty one would make
// all three tests pass on a code path that never runs — which is how the
// non-200 test first passed while the other two were still failing on it.
//
// registry stands in for a registry's HEAD endpoint. `digest` is what it puts
// in the Docker-Content-Digest header; empty means the header is absent.
func registry(t *testing.T, status int, digest string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Mutation that must fail this test: go back to
// `return res.Header.Get(ContentDigestHeader), nil`.
func TestA200WithNoDigestHeaderIsAnError(t *testing.T) {
	srv := registry(t, http.StatusOK, "")

	digest, err := docker.GetDigest(context.Background(), srv.URL+"/v2/library/nginx/manifests/latest", "test-token")

	if err == nil {
		t.Fatalf("GetDigest returned (%q, nil) for a 200 with no digest header; an empty digest matches nothing, "+
			"so every caller concludes 'update available' forever", digest)
	}
	if digest != "" {
		t.Errorf("digest = %q; a failure must not also return a value", digest)
	}
	// The message has to say what happened, or an operator reading the log
	// cannot tell this from a registry that refused them.
	if !strings.Contains(err.Error(), "Docker-Content-Digest") {
		t.Errorf("err = %v; it should name the missing header", err)
	}
}

// The success path still works, so the test above is not passing because
// GetDigest refuses everything.
func TestA200WithADigestHeaderIsReturned(t *testing.T) {
	const want = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	srv := registry(t, http.StatusOK, want)

	digest, err := docker.GetDigest(context.Background(), srv.URL+"/v2/library/nginx/manifests/latest", "test-token")
	if err != nil {
		t.Fatalf("GetDigest on a well-behaved registry: %v", err)
	}
	if digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}
}

// And a refusal is still a refusal — the non-200 path is untouched, asserted
// here so a change to the new branch cannot silently swallow it.
func TestANonOKResponseIsStillAnError(t *testing.T) {
	srv := registry(t, http.StatusUnauthorized, "")

	if _, err := docker.GetDigest(context.Background(), srv.URL+"/v2/library/nginx/manifests/latest", "test-token"); err == nil {
		t.Fatal("a 401 from the registry returned no error")
	}
}
