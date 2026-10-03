package libvirtconn

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSocket creates a real unix socket at dir/name (ResolveSocket only
// accepts sockets) and returns its path.
func fakeSocket(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatalf("listen %s: %v", p, err)
	}
	t.Cleanup(func() { ln.Close() })
	return p
}

// withFallbacks points the fallback list at a temporary directory, in the
// production order: modular first, monolithic second.
func withFallbacks(t *testing.T, dir string) (modular, monolithic string) {
	t.Helper()
	modular = filepath.Join(dir, "virtqemud-sock")
	monolithic = filepath.Join(dir, "libvirt-sock")
	prev := fallbackSockets
	fallbackSockets = []string{modular, monolithic}
	t.Cleanup(func() { fallbackSockets = prev })
	return modular, monolithic
}

func TestResolveSocketPrefersTheConfiguredSocket(t *testing.T) {
	dir := t.TempDir()
	withFallbacks(t, dir)
	fakeSocket(t, dir, "virtqemud-sock")
	fakeSocket(t, dir, "libvirt-sock")
	custom := fakeSocket(t, dir, "custom-sock")

	got, err := ResolveSocket(custom)
	if err != nil || got != custom {
		t.Fatalf("ResolveSocket(%s) = %q, %v; want the configured socket", custom, got, err)
	}
}

// THE BUG: Ubuntu's monolithic, socket-activated libvirtd creates only
// libvirt-sock, while the default configuration names virtqemud-sock.
func TestResolveSocketFallsBackToMonolithicLibvirtd(t *testing.T) {
	dir := t.TempDir()
	modular, monolithic := withFallbacks(t, dir)
	fakeSocket(t, dir, "libvirt-sock")

	got, err := ResolveSocket(modular)
	if err != nil || got != monolithic {
		t.Fatalf("ResolveSocket(%s) = %q, %v; want %s", modular, got, err, monolithic)
	}
}

func TestResolveSocketTriesFallbacksInOrder(t *testing.T) {
	dir := t.TempDir()
	modular, _ := withFallbacks(t, dir)
	fakeSocket(t, dir, "virtqemud-sock")
	fakeSocket(t, dir, "libvirt-sock")

	got, err := ResolveSocket(filepath.Join(dir, "missing-sock"))
	if err != nil || got != modular {
		t.Fatalf("got %q, %v; want the modular socket %s first", got, err, modular)
	}
}

// A leftover regular file or directory is not a socket and must not be chosen.
func TestResolveSocketIgnoresNonSockets(t *testing.T) {
	dir := t.TempDir()
	modular, monolithic := withFallbacks(t, dir)
	if err := os.WriteFile(modular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSocket(t, dir, "libvirt-sock")

	got, err := ResolveSocket(modular)
	if err != nil || got != monolithic {
		t.Fatalf("got %q, %v; want %s (the regular file is not a socket)", got, err, monolithic)
	}
}

func TestResolveSocketNamesEverythingItTried(t *testing.T) {
	dir := t.TempDir()
	modular, monolithic := withFallbacks(t, dir)
	custom := filepath.Join(dir, "custom-sock")

	_, err := ResolveSocket(custom)
	if !errors.Is(err, ErrNoSocket) {
		t.Fatalf("err = %v, want ErrNoSocket", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "no libvirt socket found: tried ") {
		t.Errorf("message %q does not start with the agreed wording", msg)
	}
	for _, p := range []string{custom, modular, monolithic} {
		if !strings.Contains(msg, p) {
			t.Errorf("message %q does not name %s", msg, p)
		}
	}
	// The configured path equal to a fallback is listed once.
	_, err = ResolveSocket(modular)
	if n := strings.Count(err.Error(), modular); n != 1 {
		t.Errorf("%s listed %d times in %q", modular, n, err.Error())
	}
}

// Conn surfaces the resolver's error rather than dialing a path that is not
// there, so /info can say what is wrong.
func TestConnReportsNoSocket(t *testing.T) {
	dir := t.TempDir()
	withFallbacks(t, dir)

	_, err := New(filepath.Join(dir, "virtqemud-sock")).Conn()
	if !errors.Is(err, ErrNoSocket) {
		t.Fatalf("Conn() err = %v, want ErrNoSocket", err)
	}
}
