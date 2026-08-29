package oauth2

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// Authorization codes are short-lived bearer tokens exchanged at the
// token endpoint for an access token. They live in memory because they
// expire in seconds — restart loses them, but that's fine: any client
// mid-flow during a restart retries with a fresh /authorize call.
//
// RFC 6749 mandates a maximum lifetime of 10 minutes; we use 5 to
// reduce the window in which a leaked code can be redeemed.
const authCodeTTL = 5 * time.Minute

// AuthCode is a single in-flight authorization code record.
type AuthCode struct {
	Code        string    // the opaque value returned to the client via redirect
	ClientID    string    // which client issued the /authorize call
	UserID      int       // resource owner (CasaNova user)
	RedirectURI string    // must match at /token time (RFC 6749 §4.1.3)
	Scopes      []string  // what the user consented to
	ExpiresAt   time.Time // hard expiry enforced on exchange
	Used        bool      // one-shot: a code can only be redeemed once
	Nonce       string    // OIDC nonce (if provided) — echoed in id_token

	// PKCE (RFC 7636): the client committed to a code_verifier by sending
	// its SHA-256 hash as code_challenge. The verifier must hash to the
	// stored challenge at exchange time. Public clients MUST use PKCE;
	// confidential clients can optionally use it for extra defense.
	CodeChallenge       string
	CodeChallengeMethod string // "S256" only; "plain" is deprecated and refused
}

// AuthCodeStore holds in-flight codes in memory with a TTL sweep.
// Safe for concurrent access.
type AuthCodeStore struct {
	mu    sync.Mutex
	codes map[string]*AuthCode
	ctx   context.Context
}

// NewAuthCodeStore creates an empty store and starts a background
// sweeper that removes expired codes every minute. The sweeper
// goroutine exits when the provided context is cancelled.
func NewAuthCodeStore(ctx context.Context) *AuthCodeStore {
	s := &AuthCodeStore{codes: make(map[string]*AuthCode), ctx: ctx}
	go s.sweepLoop()
	return s
}

func (s *AuthCodeStore) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *AuthCodeStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.codes {
		if now.After(v.ExpiresAt) {
			delete(s.codes, k)
		}
	}
}

// Issue generates a new code, stores it, and returns the opaque value
// the authorization endpoint should return to the client. The code is
// 32 crypto-random bytes, base64url-encoded.
func (s *AuthCodeStore) Issue(ac *AuthCode) (string, error) {
	code, err := randomToken(32)
	if err != nil {
		return "", err
	}
	ac.Code = code
	ac.ExpiresAt = time.Now().Add(authCodeTTL)
	s.mu.Lock()
	s.codes[code] = ac
	s.mu.Unlock()
	return code, nil
}

// Redeem atomically retrieves a code, marks it used, and validates that
// the client_id and redirect_uri match the /authorize request. PKCE
// verification is performed here too when the original /authorize
// included a code_challenge.
//
// Returns ErrCodeNotFound, ErrCodeExpired, ErrCodeAlreadyUsed, or a
// mismatch error. Never returns the same code twice — one-shot
// semantics are critical: a token-exchange replay with a captured
// code must fail even if the original exchange succeeded.
func (s *AuthCodeStore) Redeem(code, clientID, redirectURI, codeVerifier string) (*AuthCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ac, ok := s.codes[code]
	if !ok {
		return nil, ErrCodeNotFound
	}
	if ac.Used {
		// Per RFC 6749 §10.5, a reused code SHOULD revoke any token
		// already issued for that code. We can't easily implement the
		// "revoke previous" part since JWTs are stateless, but at
		// minimum we refuse the replay.
		delete(s.codes, code)
		return nil, ErrCodeAlreadyUsed
	}
	if time.Now().After(ac.ExpiresAt) {
		delete(s.codes, code)
		return nil, ErrCodeExpired
	}
	if ac.ClientID != clientID {
		return nil, ErrClientMismatch
	}
	if ac.RedirectURI != redirectURI {
		return nil, ErrRedirectMismatch
	}

	// PKCE verification. S256 only — "plain" is deprecated and refused.
	if ac.CodeChallenge != "" {
		if ac.CodeChallengeMethod != "S256" {
			return nil, ErrPKCEMethodUnsupported
		}
		if codeVerifier == "" {
			return nil, ErrPKCEMissing
		}
		if !verifyPKCES256(codeVerifier, ac.CodeChallenge) {
			return nil, ErrPKCEMismatch
		}
	}

	ac.Used = true
	// Keep the record for a moment so a concurrent replay gets
	// ErrCodeAlreadyUsed instead of ErrCodeNotFound (the second form
	// looks like a bug in the client's state rather than an attack).
	// sweep() will clean it up on the next tick.
	return ac, nil
}

// verifyPKCES256 checks that sha256(codeVerifier), base64url-encoded,
// equals the stored challenge. Constant-time byte compare guards
// against timing-attack variants.
func verifyPKCES256(verifier, challenge string) bool {
	h := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// Error sentinels — kept as exported variables so handlers can use
// errors.Is for control-flow branching on specific failure modes.
var (
	ErrCodeNotFound          = errors.New("oauth2: authorization code not found")
	ErrCodeExpired           = errors.New("oauth2: authorization code expired")
	ErrCodeAlreadyUsed       = errors.New("oauth2: authorization code already redeemed")
	ErrClientMismatch        = errors.New("oauth2: client_id does not match authorization request")
	ErrRedirectMismatch      = errors.New("oauth2: redirect_uri does not match authorization request")
	ErrPKCEMethodUnsupported = errors.New("oauth2: unsupported code_challenge_method (S256 only)")
	ErrPKCEMissing           = errors.New("oauth2: code_verifier is required for PKCE flow")
	ErrPKCEMismatch          = errors.New("oauth2: code_verifier does not match code_challenge")
)
