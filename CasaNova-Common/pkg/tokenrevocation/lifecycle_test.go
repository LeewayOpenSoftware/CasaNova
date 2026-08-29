package tokenrevocation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The operator-independence gate. A security subsystem that needs a human to
// create its directory, fix its permissions or run a validation command is not
// finished. These two tests walk the sequences that gate demands, end to end,
// with no manual step anywhere in them.

// BOOT: detect required state -> provision -> validate the invariant -> expose
// healthy state -> the service is usable.
func TestBootSequenceNeedsNoOperator(t *testing.T) {
	// A machine that has never run CasaNova: no directory, no file, no
	// permissions set by anyone.
	base := filepath.Join(t.TempDir(), "var", "lib", "casanova")
	path := filepath.Join(base, "token-revocations.json")
	SetPath(path)
	t.Cleanup(func() { SetPath(DefaultPath) })

	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s should not exist yet", base)
	}

	// Exactly what a service main() does.
	if err := Ensure(); err != nil {
		t.Fatalf("boot failed on a clean machine — an operator would have to intervene: %v", err)
	}

	// It created its own state, with the mode every service needs to read it.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("boot did not create its own state: %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("mode = %#o, want %#o — an operator would have to chmod it",
			info.Mode().Perm(), fileMode)
	}

	// It validated the property, not merely its own existence.
	h := CurrentHealth()
	if h.Status != StatusHealthy || !h.Enforcing {
		t.Fatalf("health after boot = %+v, want healthy and enforcing", h)
	}

	// And the service is usable: a valid token is not refused.
	if IsRevoked("1", time.Now()) {
		t.Fatal("a freshly booted box refuses valid tokens")
	}
}

// BROKEN COMPONENT: detect failure -> the dangerous operation fails closed ->
// diagnosis names the failed invariant -> recovery converges -> no insecure
// fallback at any point.
func TestBrokenComponentFailsClosedAndRecovers(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(500); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	// Break it the way a disk fault or tampering would.
	if err := os.WriteFile(path, []byte("\x00\x01 truncated garbage"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	// A service restarts into the broken state.
	SetPath(path)

	// 1. The dangerous operation fails closed — accepting a token is the
	//    dangerous operation here, and it is refused.
	if !IsRevoked("999", time.Now()) {
		t.Fatal("INSECURE FALLBACK: a broken authority accepted a token it could not vet")
	}

	// 2. Diagnosis names the invariant, in terms an operator can act on.
	h := CurrentHealth()
	if h.Enforcing {
		t.Fatal("health claims enforcement while broken")
	}
	if h.Invariant == "" || h.Detail == "" {
		t.Fatalf("diagnosis does not say what failed: %+v", h)
	}
	if h.Path == "" {
		t.Fatal("diagnosis does not say which file to look at")
	}

	// 3. The failure is stable: restarting must not silently convert it into
	//    an empty, healthy, everyone-un-revoked list.
	SetPath(path)
	if !IsRevoked("999", time.Now()) {
		t.Fatal("INSECURE FALLBACK: a restart healed the break into an empty list")
	}

	// 4. Recovery, once the operator removes the unreadable file as the
	//    diagnosis instructs, converges on its own with no further steps.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	SetPath(path)
	if err := Ensure(); err != nil {
		t.Fatalf("recovery did not converge: %v", err)
	}
	if h := CurrentHealth(); h.Status != StatusHealthy || !h.Enforcing {
		t.Fatalf("health after recovery = %+v, want healthy and enforcing", h)
	}
	if IsRevoked("999", time.Now()) {
		t.Fatal("the box is still refusing valid tokens after recovery")
	}
}

// Interrupted setup: a zero-length file is what a crash mid-write would leave
// behind if writes were not atomic. Converge rather than fail on it — it
// carries no revocations to lose, so it is the same content provisioning would
// have created.
func TestInterruptedSetupConverges(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Ensure(); err != nil {
		t.Fatalf("an interrupted setup did not converge: %v", err)
	}
	if h := CurrentHealth(); h.Status != StatusHealthy {
		t.Fatalf("health = %+v, want healthy", h)
	}
	// And the mode it was left with gets repaired, or the other services
	// would all silently go fail-closed.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("mode = %#o was not repaired to %#o", info.Mode().Perm(), fileMode)
	}
}

// Version upgrade: a box that ran an older build must keep its revocations.
func TestUpgradeFromAnEarlierBuildKeepsRevocations(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(31337); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	// The upgrade: a new process, same state on disk.
	SetPath(path)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure after upgrade: %v", err)
	}
	if !IsRevoked("31337", time.Now().Add(-time.Hour)) {
		t.Fatal("upgrading dropped an existing revocation")
	}
}
