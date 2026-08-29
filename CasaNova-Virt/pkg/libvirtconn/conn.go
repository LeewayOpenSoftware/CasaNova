// Package libvirtconn manages the RPC connection to libvirt.
//
// It uses the pure-Go github.com/digitalocean/go-libvirt client, which speaks
// the libvirt RPC protocol directly over the unix socket. This is a deliberate
// departure from the ZimaOS oracle, which linked the CGO bindings
// (libvirt.org/go/libvirt). CasaNova services build CGO_ENABLED=0 static, so
// the CGO bindings are unusable here; go-libvirt is the drop-in replacement.
package libvirtconn

import (
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// Manager owns a lazily-established, self-healing connection to libvirt.
type Manager struct {
	socketPath string
	mu         sync.Mutex
	l          *libvirt.Libvirt
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

	dialer := dialers.NewLocal(
		dialers.WithSocket(m.socketPath),
		dialers.WithLocalTimeout(10*time.Second),
	)
	l := libvirt.NewWithDialer(dialer)
	if err := l.ConnectToURI(libvirt.QEMUSystem); err != nil {
		return nil, err
	}
	m.l = l
	return m.l, nil
}

// Close tears down the connection (used on graceful shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.l != nil {
		_ = m.l.Disconnect()
		m.l = nil
	}
}

// SocketPath reports the configured socket path.
func (m *Manager) SocketPath() string { return m.socketPath }
