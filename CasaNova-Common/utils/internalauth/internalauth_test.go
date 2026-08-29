package internalauth

import (
	"os"
	"testing"
)

func withTempSecret(t *testing.T) {
	t.Helper()
	setPathsForTest(t.TempDir())
	t.Cleanup(func() {
		mu.Lock()
		cached = ""
		mu.Unlock()
	})
}

// Secret must be stable within a boot: two reads return the same value, and it
// is created exactly once.
func TestSecretIsStableAndCreatedOnce(t *testing.T) {
	withTempSecret(t)

	a, err := Secret()
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Fatalf("secret length = %d, want 64", len(a))
	}
	b, err := Secret()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("secret changed between reads: %q vs %q", a, b)
	}

	// The on-disk file must be root-only.
	fi, err := os.Stat(secretFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", perm)
	}
}

// A second process starting against an existing secret adopts it rather than
// clobbering it — the get-or-create race resolves to one shared value.
func TestSecretAdoptsExistingFile(t *testing.T) {
	dir := t.TempDir()
	setPathsForTest(dir)
	first, err := Secret()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a fresh process: same file, empty cache.
	mu.Lock()
	cached = ""
	mu.Unlock()

	second, err := Secret()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("a restart minted a new secret: %q vs %q", first, second)
	}
}

// Valid is the gate the endpoints lean on: the right secret passes, an empty
// header (the attacker who simply omits it) and a wrong value both fail.
func TestValid(t *testing.T) {
	withTempSecret(t)
	s, err := Secret()
	if err != nil {
		t.Fatal(err)
	}

	if !Valid(s) {
		t.Fatal("the correct secret was rejected")
	}
	if Valid("") {
		t.Fatal("an empty header was accepted")
	}
	if Valid(s + "x") {
		t.Fatal("a wrong secret was accepted")
	}
	if Valid("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("a same-length wrong secret was accepted")
	}
}
