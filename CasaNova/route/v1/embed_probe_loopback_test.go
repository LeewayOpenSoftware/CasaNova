package v1

import "testing"

// OLA 3 — duplicated security decision: loopbackHost here used to accept the
// literal string "localhost" in addition to loopback IPs, independently of
// CasaNova-Gateway's isLoopbackTarget (the same conceptual guard, in a
// different module), which rejects any hostname on purpose — a hostname's
// resolution is not pinned at the time this check runs, only at the time the
// probe actually dials it. Now both call the shared
// CasaNova-Common/utils/loopback.IsLoopbackHost.
func TestLoopbackHostRejectsHostnames(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "evil.example.com", "attacker.local", ""} {
		if loopbackHost(host) {
			t.Errorf("loopbackHost(%q) = true, want false — a redirect target must be a "+
				"literal loopback IP, never a name that depends on resolution", host)
		}
	}
}

// Positive control: the actual loopback IPs the probe legitimately redirects
// within must still pass, or the embed probe breaks for every well-behaved
// app that redirects to itself by IP (e.g. nextcloud's 302 to 127.0.0.1:PORT).
func TestLoopbackHostAllowsLoopbackIPs(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1"} {
		if !loopbackHost(host) {
			t.Errorf("loopbackHost(%q) = false, want true — this is exactly the case the "+
				"embed probe exists to follow", host)
		}
	}
}
