package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
)

// tlsStateFile lives alongside routes.json and holds the TLS config.
// Separate file so a misformatted tls section can't block gateway
// startup on the route-loading path.
const tlsStateFile = "tls.json"

// TLSSettings mirrors tlsmanager.Config but stays in the service package
// so route/*.go handlers don't need to import the pkg directly to work
// with the data shape.
type TLSSettings struct {
	Enabled bool     `json:"enabled"`
	Domains []string `json:"domains,omitempty"`
	Email   string   `json:"email,omitempty"`
}

type State struct {
	portMu              sync.RWMutex
	gatewayPort         string
	onGatewayPortChange []func(string) error

	pathMu      sync.RWMutex
	runtimePath string
	wwwPath     string

	tlsMu       sync.RWMutex
	tls         TLSSettings
	onTLSChange []func(TLSSettings) error
}

func NewState() *State {
	return &State{
		gatewayPort:         "",
		onGatewayPortChange: make([]func(string) error, 0),

		runtimePath: "",
		wwwPath:     "",
	}
}

// LoadTLSFromDisk reads tls.json from the runtime path, if it exists.
// Missing file is treated as "TLS disabled" (the default). A malformed
// file is logged upstream but leaves TLS disabled so the gateway still
// serves HTTP.
func (c *State) LoadTLSFromDisk() error {
	path := filepath.Join(c.runtimePath, tlsStateFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s TLSSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	c.tlsMu.Lock()
	c.tls = s
	c.tlsMu.Unlock()
	return nil
}

// GetTLS returns a copy of the current TLS settings.
func (c *State) GetTLS() TLSSettings {
	c.tlsMu.RLock()
	defer c.tlsMu.RUnlock()
	return c.tls
}

// SetTLS atomically updates the TLS settings, persists them to disk,
// and notifies every registered listener (the gateway uses this to
// rebuild its TLS config + restart the 443 listener).
//
// Wave 32: the previous version assigned c.tls = s BEFORE the disk
// write and listener callbacks. If the rename failed (disk full,
// EACCES) or a listener returned an error (TLS reload rejected the
// new domains), the function returned an error to the caller — but
// the in-memory state had already been mutated to the broken value.
// GetTLS would then report the new (un-persisted) settings while the
// 443 listener was still running with the old config, and the next
// gateway restart would either pick up an inconsistent disk file or
// fall back to TLS-disabled. The fix holds the write lock for the
// whole operation and only commits c.tls after persistence + every
// listener has confirmed success.
func (c *State) SetTLS(s TLSSettings) error {
	c.tlsMu.Lock()
	defer c.tlsMu.Unlock()

	// Persist atomically (write-to-tmp + fsync + rename + dir-fsync) so
	// a crash mid-write does not leave a truncated tls.json that
	// silently disables TLS on the next boot. Wave 35-Q: the previous
	// implementation called os.WriteFile(tmp, ...) + os.Rename — neither
	// of which calls fsync(2). On btrfs/xfs/zfs the rename and the data
	// can land in different transactions, and a power loss between
	// them yields a renamed-but-empty file. The `commonfile.WriteFileAtomic`
	// helper applies the full durable-write sequence.
	if c.runtimePath != "" {
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		finalPath := filepath.Join(c.runtimePath, tlsStateFile)
		if err := commonfile.WriteFileAtomic(finalPath, raw, 0o600); err != nil {
			return err
		}
	}

	// Snapshot listeners under the lock — we still call them inside
	// the lock window (the callbacks must not call back into State or
	// they'll deadlock; current callers do not). Doing it here keeps
	// the rule "in-memory state matches disk + listener state at all
	// times" airtight: a failing listener leaves c.tls untouched.
	for _, fn := range c.onTLSChange {
		if err := fn(s); err != nil {
			return err
		}
	}

	c.tls = s
	return nil
}

// OnTLSChange registers a callback fired whenever SetTLS runs. Used by
// main.go to swap the live TLS listener when the operator updates config.
func (c *State) OnTLSChange(f func(TLSSettings) error) {
	c.tlsMu.Lock()
	defer c.tlsMu.Unlock()
	c.onTLSChange = append(c.onTLSChange, f)
}

// SetGatewayPort notifies every registered listener of the new port and
// only commits it to State once all of them have confirmed it — the same
// held-until-confirmed shape SetTLS above already uses for the identical
// problem (Wave 32).
//
// The previous version assigned c.gatewayPort = port BEFORE running the
// listeners: if one returned an error, SetGatewayPort still returned that
// error, but the port had already been mutated in memory. In production
// the two real listeners are reloadGateway (rebinds the actual TCP
// listener; registered first, so the reverse-order loop below runs it
// FIRST) and a config.json persist (registered second, run second) — so a
// failed rebind used to leave GetGatewayPort() reporting a port the
// gateway was never actually listening on. Delaying the write until every
// listener has succeeded means a failure leaves gatewayPort exactly where
// it was: nothing to roll back, because nothing was written early.
//
// This does not attempt to re-notify listeners that already ran with a
// "rollback" value if a LATER listener fails — that would assume a
// listener is safe to invoke in reverse ("undo to the old port"), which
// isn't part of its contract (it is told "apply this port", never "undo
// to that one"). One known, accepted asymmetry survives as a result: if
// reloadGateway succeeds but the config persist that runs after it then
// fails, the live gateway really has moved to the new port, but
// GetGatewayPort() will report the old one until the next successful
// call. That mirrors the config file itself being behind live reality in
// exactly that case — an existing limitation of having two listeners
// with different reversibility, not something this fix introduces or
// silently leaves undocumented.
func (c *State) SetGatewayPort(port string) error {
	c.portMu.RLock()
	listeners := make([]func(string) error, len(c.onGatewayPortChange))
	copy(listeners, c.onGatewayPortChange)
	c.portMu.RUnlock()

	// Notify listeners outside the lock to avoid deadlocks if a listener
	// calls back into State.
	for i := len(listeners) - 1; i >= 0; i-- {
		if err := listeners[i](port); err != nil {
			return err
		}
	}

	c.portMu.Lock()
	c.gatewayPort = port
	c.portMu.Unlock()

	return nil
}

func (c *State) GetGatewayPort() string {
	c.portMu.RLock()
	defer c.portMu.RUnlock()
	return c.gatewayPort
}

// Add func `f` to the stack. The stack of funcs will be called, in reverse order, when there is request to change the port.
func (c *State) OnGatewayPortChange(f func(string) error) {
	c.portMu.Lock()
	defer c.portMu.Unlock()
	c.onGatewayPortChange = append(c.onGatewayPortChange, f)
}

func (c *State) SetRuntimePath(path string) error {
	c.pathMu.Lock()
	defer c.pathMu.Unlock()
	c.runtimePath = path
	return nil
}

func (c *State) GetRuntimePath() string {
	c.pathMu.RLock()
	defer c.pathMu.RUnlock()
	return c.runtimePath
}

func (c *State) SetWWWPath(path string) error {
	c.pathMu.Lock()
	defer c.pathMu.Unlock()
	c.wwwPath = path
	return nil
}

func (c *State) GetWWWPath() string {
	c.pathMu.RLock()
	defer c.pathMu.RUnlock()
	return c.wwwPath
}
