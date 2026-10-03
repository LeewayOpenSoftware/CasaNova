// Package libvirtconn manages the RPC connection to libvirt.
//
// It uses the pure-Go github.com/digitalocean/go-libvirt client, which speaks
// the libvirt RPC protocol directly over the unix socket. This is a deliberate
// departure from the ZimaOS oracle, which linked the CGO bindings
// (libvirt.org/go/libvirt). CasaNova services build CGO_ENABLED=0 static, so
// the CGO bindings are unusable here; go-libvirt is the drop-in replacement.
package libvirtconn

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// ErrNoSocket is wrapped by the error Conn returns when none of the candidate
// sockets exists, so callers can answer "libvirt is not reachable here" rather
// than an opaque 500.
var ErrNoSocket = errors.New("no libvirt socket found")

// fallbackSockets are tried, in order, when the configured socket does not
// exist. The default configuration (inherited from ZimaOS) points at the
// modular daemon's virtqemud-sock, but a stock Ubuntu/Debian host runs the
// MONOLITHIC libvirtd, socket-activated, which only creates libvirt-sock. With
// no fallback the service could never connect there although libvirt was up.
// A variable so tests can point it at a temporary directory.
var fallbackSockets = []string{
	"/var/run/libvirt/virtqemud-sock",
	"/var/run/libvirt/libvirt-sock",
}

// ResolveSocket picks the socket to dial: the configured one if it exists,
// otherwise the first fallback that exists. Only an actual unix socket counts:
// a leftover regular file or directory at that path would only produce a
// confusing dial error later. It is resolved on every (re)connect, because a
// socket-activated daemon can create its socket after this service started.
func ResolveSocket(configured string) (string, error) {
	candidates := make([]string, 0, 1+len(fallbackSockets))
	seen := map[string]bool{}
	for _, c := range append([]string{configured}, fallbackSockets...) {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: tried %s (is libvirtd or virtqemud running?)", ErrNoSocket, strings.Join(candidates, ", "))
}

// Manager owns a lazily-established, self-healing connection to libvirt.
type Manager struct {
	socketPath string
	mu         sync.Mutex
	l          *libvirt.Libvirt
	active     string // the socket the live connection was dialed on
}

// New returns a Manager targeting the given unix socket path. Connection is
// deferred until the first Conn() call so the service can start even when
// virtqemud is not yet up.
func New(socketPath string) *Manager {
	return &Manager{socketPath: socketPath}
}

// Conn returns a connected *libvirt.Libvirt, (re)connecting on demand. Safe for
// concurrent use.
func (m *Manager) Conn() (*libvirt.Libvirt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.l != nil && m.l.IsConnected() {
		return m.l, nil
	}

	// Drop a stale handle before reconnecting.
	if m.l != nil {
		_ = m.l.Disconnect()
		m.l = nil
	}

	sock, err := ResolveSocket(m.socketPath)
	if err != nil {
		return nil, err
	}
	dialer := dialers.NewLocal(
		dialers.WithSocket(sock),
		dialers.WithLocalTimeout(10*time.Second),
	)
	l := libvirt.NewWithDialer(dialer)
	if err := l.ConnectToURI(libvirt.QEMUSystem); err != nil {
		return nil, fmt.Errorf("connect to libvirt at %s: %w", sock, err)
	}
	m.l = l
	m.active = sock
	return m.l, nil
}

// Close tears down the connection (used on graceful shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.l != nil {
		_ = m.l.Disconnect()
		m.l = nil
		m.active = ""
	}
}

// SocketPath reports the configured socket path.
func (m *Manager) SocketPath() string { return m.socketPath }

// ActiveSocket reports the socket the current connection was dialed on, or ""
// when not connected.
func (m *Manager) ActiveSocket() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}
