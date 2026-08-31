package docker_test

// Checking one image for updates makes three outbound registry calls —
// challenge, token, digest — and none of them carried a context. Each was
// bounded only by the shared client's 30 second timeout, so a slow or
// rate-limited registry cost up to 90 seconds per image that nothing could
// cancel. That ran inside a loop over every installed app, and the cache in
// front of it is purged by any store sync, so a cold sweep put every app down
// that path at once. The caller's own 15 second deadline was created and then
// never attached to the requests it was meant to bound.
//
// These pin the threading, not the timing: a context that is already done must
// stop the call before it reaches the network. If the deadline is ignored
// again, the calls go out and these take real seconds instead of failing at
// once.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
)

// Well above the microseconds a context check costs, far below the seconds a
// real registry round trip takes — so this distinguishes the two without
// being flaky on a slow machine.
const immediate = 2 * time.Second

func TestGetChallenge_HonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := docker.GetChallenge(ctx, "docker.io/library/alpine:latest")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled context must abort the challenge request")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("want a context error, got %v — the request may have gone out anyway", err)
	}
	if elapsed > immediate {
		t.Errorf("took %v: the context is not reaching the request", elapsed)
	}
}

func TestGetDigest_HonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := docker.GetDigest(ctx, "https://registry-1.docker.io/v2/library/alpine/manifests/latest", "token")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled context must abort the digest request")
	}
	// The error has to be the CONTEXT's, not the registry's. Without this the
	// test passes against the unfixed code too: the request goes out, the
	// placeholder token is rejected, and "some error, quickly" looks the same
	// as "stopped before the network".
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("want a context error, got %v — the request went out anyway", err)
	}
	if elapsed > immediate {
		t.Errorf("took %v: the context is not reaching the request", elapsed)
	}
}

func TestGetDigest_StillRefusesAnEmptyToken(t *testing.T) {
	// Guards the guard: if every call failed on the context alone, the tests
	// above would pass against a function that had stopped doing its job.
	// This one must fail for its own reason, with a live context.
	if _, err := docker.GetDigest(context.Background(), "https://example.invalid/v2/x/manifests/latest", ""); err == nil {
		t.Fatal("an empty token must be refused before any request is made")
	}
}

func TestCompareDigest_HonoursADeadlineAlreadyPassed(t *testing.T) {
	// The whole chain, from the entry point the update sweep actually calls.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	start := time.Now()
	_, err := docker.CompareDigest(ctx, "docker.io/library/alpine:latest", []string{"alpine@sha256:deadbeef"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an expired deadline must stop the comparison")
	}
	if elapsed > immediate {
		t.Errorf("took %v: the deadline is not reaching the registry calls", elapsed)
	}
}
