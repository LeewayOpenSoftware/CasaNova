// Package oauth2 implements a minimal OAuth2/OIDC provider for CasaNova.
// It's deliberately NOT a full Fosite install — a home-server that needs
// "Login with CasaNova" for a handful of locally-hosted apps doesn't
// need dynamic client registration, RP-initiated logout, or every grant
// type. What we support:
//
//   - Authorization Code flow with PKCE (RFC 7636) — the modern default
//     for web apps and SPAs
//   - /token exchange returns a signed JWT access token and an OIDC
//     id_token when the client requested the "openid" scope
//   - /userinfo returns the authenticated user's profile
//   - /.well-known/openid-configuration discovery endpoint so standards-
//     compliant clients can wire themselves up automatically
//   - JWKS is already served by the existing jwt package at
//     /.well-known/jwks.json, so we just reference it in discovery
//
// Clients are registered by an admin through the UserService API; no
// dynamic registration. Client secrets are stored as bcrypt hashes.
package oauth2

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"golang.org/x/crypto/bcrypt"
)

// Client is a registered OAuth2 relying party. Created via the admin API.
type Client struct {
	// ID is the public client identifier sent in authorize + token requests.
	ID string `json:"client_id"`

	// Name is the human-readable label shown on the consent screen.
	Name string `json:"name"`

	// SecretHash is a bcrypt hash of the client secret. The plaintext is
	// shown to the admin ONCE at creation time and never stored.
	// Empty SecretHash means "public client" (mobile/SPA) — auth is
	// enforced purely by PKCE and redirect_uri matching.
	SecretHash string `json:"secret_hash,omitempty"`

	// RedirectURIs is the exact-match allow-list for redirect_uri. A
	// request redirecting elsewhere is rejected — this is the primary
	// OAuth2 defense against phishing.
	RedirectURIs []string `json:"redirect_uris"`

	// Scopes the client is allowed to request. "openid" is required for
	// id_token issuance; "profile" and "email" are standard OIDC claims.
	AllowedScopes []string `json:"allowed_scopes,omitempty"`

	// Created timestamp, stamped by the store.
	Created time.Time `json:"created"`
}

// IsPublic reports whether the client uses PKCE-only auth (no secret).
// Public clients must always include a PKCE challenge in /authorize.
func (c *Client) IsPublic() bool {
	return c.SecretHash == ""
}

// VerifySecret checks a plaintext secret against the stored bcrypt hash.
// Constant-time by virtue of bcrypt's own comparison.
func (c *Client) VerifySecret(plaintext string) bool {
	if c.IsPublic() {
		return false // public clients never verify a secret — callers
		// should check IsPublic() first and route to PKCE verification
	}
	return bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(plaintext)) == nil
}

// HasRedirect reports whether a redirect URI is in the allow-list.
// Exact-match comparison — no wildcard support. Required by RFC 6749 §3.1.2.
func (c *Client) HasRedirect(uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

// HasScope reports whether the client is allowed to request a scope.
// Empty AllowedScopes means "any scope permitted" for backward compat
// with simple setups where the admin doesn't scope-lock clients.
func (c *Client) HasScope(s string) bool {
	if len(c.AllowedScopes) == 0 {
		return true
	}
	for _, a := range c.AllowedScopes {
		if a == s {
			return true
		}
	}
	return false
}

// ClientStore is a JSON-backed persistence layer for registered clients.
// Patterns mirror the backup and scheduled-task stores: single file,
// atomic rewrite, thread-safe.
type ClientStore struct {
	path string
	mu   sync.RWMutex
	data map[string]*Client
}

// NewClientStore opens or creates a store under dir.
func NewClientStore(dir string) (*ClientStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("oauth2 client store mkdir: %w", err)
	}
	s := &ClientStore{
		path: filepath.Join(dir, "oauth2_clients.json"),
		data: make(map[string]*Client),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ClientStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*Client
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range list {
		s.data[c.ID] = c
	}
	return nil
}

// saveLocked persists the OAuth2 client list.
//
// Wave 35-Q: previously this was os.WriteFile(tmp) + os.Rename — neither
// of which calls fsync(2). On btrfs/xfs/zfs the rename and the data
// can land in different transactions, and a power loss between them
// yields a renamed-but-empty file. The result was every registered
// OAuth2 client (with its bcrypt-hashed secret) silently disappearing
// at once. commonfile.WriteFileAtomic does the full durable-write
// sequence so the file is always either the previous content or the
// new content.
func (s *ClientStore) saveLocked() error {
	list := make([]*Client, 0, len(s.data))
	for _, c := range s.data {
		list = append(list, c)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return commonfile.WriteFileAtomic(s.path, raw, 0o600)
}

// Register creates a new client and returns the client along with its
// generated plaintext secret. The secret is ONLY returned here and is
// not recoverable — the admin must copy it immediately.
func (s *ClientStore) Register(name string, redirectURIs, scopes []string, isPublic bool) (*Client, string, error) {
	if name == "" {
		return nil, "", errors.New("client name is required")
	}
	if len(redirectURIs) == 0 {
		return nil, "", errors.New("at least one redirect_uri is required")
	}

	clientID, err := randomToken(16)
	if err != nil {
		return nil, "", err
	}

	var secretPlain string
	var secretHash string
	if !isPublic {
		secretPlain, err = randomToken(32)
		if err != nil {
			return nil, "", err
		}
		h, err := bcrypt.GenerateFromPassword([]byte(secretPlain), bcrypt.DefaultCost)
		if err != nil {
			return nil, "", err
		}
		secretHash = string(h)
	}

	client := &Client{
		ID:            clientID,
		Name:          name,
		SecretHash:    secretHash,
		RedirectURIs:  redirectURIs,
		AllowedScopes: scopes,
		Created:       time.Now(),
	}

	s.mu.Lock()
	s.data[clientID] = client
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		return nil, "", err
	}
	s.mu.Unlock()

	return client, secretPlain, nil
}

// Get returns a client by ID. Second return is false for unknown IDs.
func (s *ClientStore) Get(id string) (*Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data[id]
	return c, ok
}

// Delete removes a registered client. Idempotent.
func (s *ClientStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return s.saveLocked()
}

// All returns every registered client. The secret_hash is omitted from
// serialization by the caller (the admin UI listing should never leak
// even the hash).
func (s *ClientStore) All() []*Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Client, 0, len(s.data))
	for _, c := range s.data {
		out = append(out, c)
	}
	return out
}

// randomToken returns a crypto-random URL-safe string of n bytes.
// Used for client IDs and secrets — never seed from math/rand.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
