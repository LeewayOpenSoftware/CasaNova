package loopback

import "testing"

// OLA 3 — this is the shared home for a decision that used to be computed
// independently in CasaNova-Gateway's isLoopbackTarget and CasaNova's
// embed_probe.go loopbackHost, and had drifted: the latter also accepted the
// literal string "localhost", which depends on name resolution rather than
// being the exact bytes that get dialed.
func TestIsLoopbackHostRejectsHostnames(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "evil.example.com", ""} {
		if IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = true, want false — only a literal loopback IP "+
				"may pass, never a name", host)
		}
	}
}

// Positive control: real loopback IPs, in both families, still pass.
func TestIsLoopbackHostAllowsLoopbackIPs(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "127.255.255.255", "::1"} {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true", host)
		}
	}
}

// Non-loopback IPs must be refused — this is the actual SSRF boundary.
func TestIsLoopbackHostRejectsNonLoopbackIPs(t *testing.T) {
	for _, host := range []string{"10.0.0.1", "192.168.1.1", "8.8.8.8", "169.254.169.254"} {
		if IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = true, want false", host)
		}
	}
}
