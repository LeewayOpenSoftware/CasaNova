// Package tlsmanager provides HTTPS for CasaNova with zero manual cert
// management. Two modes, both usable side-by-side:
//
//   - Self-signed (default, LAN): a long-lived self-signed cert is
//     generated on first boot with the server's IP addresses and
//     hostname in its SANs. Browsers will warn once and the user adds
//     the cert to their trust store. No internet required.
//
//   - Let's Encrypt (public domains): golang.org/x/crypto/acme/autocert
//     handles the HTTP-01 challenge and renewal. Only activated when the
//     operator sets a domain and email via the management API — until
//     then it's purely the self-signed fallback.
//
// The returned *tls.Config does SNI-based selection so both modes can
// coexist: a request for the public domain gets the Let's Encrypt cert;
// anything else (IP, *.local, LAN hostname) gets the self-signed one.
package tlsmanager

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"golang.org/x/crypto/acme/autocert"
)

// Config holds the tunables read from disk at startup and patched via
// the management API at runtime. A Manager is keyed off this struct.
type Config struct {
	// Enabled toggles TLS serving. When false, the Manager is inert and
	// the caller should serve plain HTTP on its existing listener.
	Enabled bool `json:"enabled"`

	// Domains is the allow-list for Let's Encrypt. Every domain here gets
	// a real cert on first request; others fall back to self-signed.
	Domains []string `json:"domains,omitempty"`

	// Email is the ACME account contact. Let's Encrypt requires one.
	Email string `json:"email,omitempty"`

	// CertDir is where both the self-signed material and the autocert
	// cache live. Created with 0700 permissions if missing.
	CertDir string `json:"cert_dir"`
}

// Manager owns the TLS state for a gateway. It's safe for concurrent
// reads via the methods exposed below.
type Manager struct {
	cfg Config
	mu  sync.RWMutex

	// Self-signed cert and its backing x509 template, regenerated
	// lazily on first use and cached.
	selfSignedCert *tls.Certificate

	// Autocert manager, lazily constructed when Enabled && len(Domains) > 0.
	acm *autocert.Manager
}

// New builds a Manager. It does not start anything — call TLSConfig()
// when you're ready to wire a TLS listener.
func New(cfg Config) (*Manager, error) {
	if cfg.CertDir == "" {
		return nil, errors.New("tlsmanager: CertDir is required")
	}
	if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
		return nil, fmt.Errorf("tlsmanager: create cert dir: %w", err)
	}
	return &Manager{cfg: cfg}, nil
}

// Update atomically replaces the active configuration. Call after the
// operator edits TLS settings via the management API. Existing TLS
// listeners automatically pick up the new cert on the next handshake
// because TLSConfig() returns a closure over m.cfg guarded by mu.
func (m *Manager) Update(cfg Config) error {
	if cfg.CertDir == "" {
		return errors.New("tlsmanager: CertDir is required")
	}
	if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
		return fmt.Errorf("tlsmanager: create cert dir: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	// Drop cached autocert manager so the next TLSConfig rebuild sees the
	// new domain set.
	m.acm = nil
	// Wave 33: also drop the cached self-signed cert. The previous
	// comment claimed "Self-signed cert is domain-independent and can
	// stay" — that's only true if CertDir is unchanged AND no SAN-
	// affecting host state has shifted. In practice Update() is called
	// from the management API when the operator edits TLS settings,
	// which can absolutely change CertDir, and we'd then keep serving
	// a cert+key pair loaded from the OLD directory while persisting
	// nothing to the new one. After the next process restart that
	// cert no longer exists, a brand-new self-signed is generated,
	// and every browser that previously trusted the imported root
	// gets a fresh certificate-mismatch warning. Clearing the cache
	// here forces a re-load (or re-generate) against the new CertDir
	// on the very next handshake, which writes the right files to
	// the right place and keeps the on-disk and in-memory state in
	// sync. Cert generation is cheap; this is the conservative call.
	m.selfSignedCert = nil
	return nil
}

// Config returns a copy of the active configuration.
func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// TLSConfig returns a *tls.Config whose GetCertificate does SNI-based
// selection. Let's Encrypt is consulted first for domains the operator
// listed; everything else falls through to the self-signed cert.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert, err := m.certForSNI(hello); err == nil && cert != nil {
				return cert, nil
			}
			// Fallback — always return something rather than leaking a
			// handshake error. Self-signed is universally safe for LAN.
			return m.getSelfSigned()
		},
	}
}

// ACMEHTTPHandler returns an http.Handler that serves HTTP-01 challenges
// from the autocert manager. Mount it on the plain-HTTP listener (port 80
// in production) so Let's Encrypt can reach it. When ACME is not active
// (no domains configured) the returned handler is a 404 no-op.
func (m *Manager) ACMEHTTPHandler(fallback http.Handler) http.Handler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.acm == nil {
		return fallback
	}
	return m.acm.HTTPHandler(fallback)
}

// certForSNI routes a ClientHelloInfo to the right cert source. Returns
// (nil, nil) when no better source than self-signed is available, so
// the caller can transparently fall back.
func (m *Manager) certForSNI(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	cfg := m.cfg
	acm := m.acm
	m.mu.RUnlock()

	if acm == nil && cfg.Enabled && len(cfg.Domains) > 0 {
		m.mu.Lock()
		if m.acm == nil { // double-check under write lock
			m.acm = &autocert.Manager{
				Cache:      autocert.DirCache(filepath.Join(cfg.CertDir, "acme")),
				Prompt:     autocert.AcceptTOS,
				Email:      cfg.Email,
				HostPolicy: autocert.HostWhitelist(cfg.Domains...),
			}
		}
		acm = m.acm
		m.mu.Unlock()
	}

	if acm != nil {
		// Only serve via autocert if the SNI name is in the whitelist —
		// otherwise fall through to self-signed.
		for _, d := range cfg.Domains {
			if d == hello.ServerName {
				return acm.GetCertificate(hello)
			}
		}
	}
	return nil, nil
}

// getSelfSigned returns the cached self-signed cert, generating it on
// first use. The generated cert is valid for 10 years so browsers
// that trust it once don't need re-adding.
func (m *Manager) getSelfSigned() (*tls.Certificate, error) {
	m.mu.RLock()
	cert := m.selfSignedCert
	m.mu.RUnlock()
	if cert != nil {
		return cert, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.selfSignedCert != nil {
		return m.selfSignedCert, nil
	}

	certPath := filepath.Join(m.cfg.CertDir, "self_signed.crt")
	keyPath := filepath.Join(m.cfg.CertDir, "self_signed.key")

	// Reuse an existing self-signed cert if one is already on disk.
	if loaded, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		m.selfSignedCert = &loaded
		return m.selfSignedCert, nil
	}

	generated, err := generateSelfSigned()
	if err != nil {
		return nil, err
	}
	if err := writePEM(certPath, keyPath, generated); err != nil {
		return nil, err
	}
	m.selfSignedCert = generated
	return m.selfSignedCert, nil
}

// generateSelfSigned produces a fresh ECDSA P-256 self-signed cert whose
// SANs cover every local IP, loopback, and ".casanova.local". This is
// enough to satisfy browsers doing modern hostname verification when
// the user imports the cert into their trust store.
func generateSelfSigned() (*tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CasaNova",
			Organization: []string{"Leeway Open Software"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              []string{"localhost", "casanova.local", "*.casanova.local"},
		IPAddresses:           collectLocalIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        parsed,
	}, nil
}

// collectLocalIPs returns every non-loopback IP bound to an interface on
// the host, plus 127.0.0.1 / ::1. Baking these into the self-signed cert
// means "https://192.168.1.42" works without an invalid-host warning.
func collectLocalIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	return ips
}

// writePEM serializes a cert + key pair to disk at standard PEM paths.
// Files are written with 0600 (key) / 0644 (cert) permissions.
//
// Wave 35-X: atomic + ordered. Previously plain os.WriteFile twice in
// sequence — a crash between cert write and key write left an
// inconsistent pair on disk that wedged Gateway TLS boot forever.
// Order matters: write KEY first (its absence is recoverable — Gateway
// regenerates; a stale key with new cert mismatches and fails handshake
// without regen). Both writes are atomic temp+rename.
func writePEM(certPath, keyPath string, cert *tls.Certificate) error {
	priv, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("tlsmanager: self-signed private key is not ECDSA")
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := file.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	der := cert.Certificate[0]
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return file.WriteFileAtomic(certPath, certPEM, 0o644)
}
