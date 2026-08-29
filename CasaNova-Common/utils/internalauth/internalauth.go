// Package internalauth proves that a request came from a peer CasaNova service
// on this host, WITHOUT trusting the network.
//
// The gateway reverse-proxies every LAN (and, over the Cloudflare tunnel,
// internet) request to the backend over 127.0.0.1, so a handler that trusts
// `IsLoopback(RemoteAddr)` trusts the whole internet. That mistake made
// DELETE /v1/samba/shares/:id and POST /v1/notify/* reachable unauthenticated
// from anywhere. The fix is a shared secret that lives only in a root-only
// file under /run (tmpfs, wiped and regenerated every boot): a peer service
// reads it and presents it in a header; an external attacker cannot, because
// they cannot read the file. Loopback is no longer a trust signal anywhere.
package internalauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// HeaderName carries the per-boot secret between peer services.
const HeaderName = "X-CasaNova-Internal"

// secretDir is 0700 root-only; secretFile is 0600 root-only. Both peers run as
// root on the same host, so both can read it and no other uid can. They are
// vars, not consts, only so a test can point them at a temp directory.
var (
	secretDir  = "/run/casanova/internal"
	secretFile = "/run/casanova/internal/service.secret"
)

var (
	mu     sync.Mutex
	cached string
)

// Secret returns the per-boot shared secret, creating it on first use.
//
// Get-or-create is race-safe across the two services that both call this at
// startup: the writer stages to a temp file and renames atomically, and
// creation uses O_EXCL so a loser falls back to reading the winner's value —
// there is no window in which a reader sees a half-written secret.
func Secret() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	if s, err := readSecret(); err == nil && s != "" {
		cached = s
		return s, nil
	}

	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)

	// Stage + atomic rename so a concurrent reader never sees a torn file.
	tmp, err := os.CreateTemp(secretDir, "secret-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(secret); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// O_EXCL semantics via link: if the file already exists (the other service
	// won the race), keep its value instead of clobbering it.
	if err := os.Link(tmpName, secretFile); err != nil {
		if s, rerr := readSecret(); rerr == nil && s != "" {
			cached = s
			return s, nil
		}
		return "", err
	}
	cached = secret
	return secret, nil
}

func readSecret() (string, error) {
	raw, err := os.ReadFile(secretFile)
	if err != nil {
		return "", err
	}
	s := string(raw)
	if s == "" {
		return "", errors.New("internal secret file is empty")
	}
	return s, nil
}

// Valid reports whether a presented secret matches, in constant time. An empty
// presented secret is always invalid, so a request that simply omits the header
// is rejected rather than trusted.
func Valid(presented string) bool {
	if presented == "" {
		return false
	}
	want, err := Secret()
	if err != nil || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// Header returns the header map a peer service attaches to an internal request.
func Header() (map[string]string, error) {
	s, err := Secret()
	if err != nil {
		return nil, err
	}
	return map[string]string{HeaderName: s}, nil
}

// SetPathsForTest redirects the secret to a temp dir and clears the cache, so a
// test in another package can exercise a genuine peer handshake instead of
// asserting against a stub.
//
// It REFUSES OUTSIDE A TEST BINARY. Exporting a function that relocates the
// root-only secret is otherwise a way to disarm peer authentication at runtime:
// point it at a world-writable directory and every caller becomes a peer. The
// guard makes the capability exist only where it is legitimate, so this is a
// test hook rather than a switch someone can reach in production.
func SetPathsForTest(dir string) {
	if !testing.Testing() {
		panic("internalauth.SetPathsForTest called outside a test binary: this " +
			"would relocate the peer-authentication secret in a running system")
	}
	setPathsForTest(dir)
}

// setPathsForTest redirects the secret to a temp dir and clears the cache.
func setPathsForTest(dir string) {
	mu.Lock()
	defer mu.Unlock()
	secretDir = dir
	secretFile = filepath.Join(dir, "service.secret")
	cached = ""
}
