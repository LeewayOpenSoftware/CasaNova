package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
)

type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type JWKS struct {
	Keys []JWK `json:"keys"`
}

const JWKSPath = ".well-known/jwks.json"

// RevokedForClaims reports whether these claims name a principal whose tokens
// have been revoked — account deleted, or sessions terminated.
//
// This replaces IsRevokedFunc, a hook that was declared for this purpose,
// checked here behind a nil guard, and assigned by nobody in any module. It
// never rejected a request. The revocation state it was meant to reach lived
// in a sync.Map inside the user service: unreachable from the other six
// services and lost on restart. tokenrevocation is the shared authority that
// actually holds it.
//
// Nil-safe on iat: a token minted without an issue time yields the zero
// instant, which the authority treats as "cannot be placed relative to the
// revocation" and refuses for a revoked principal.
func RevokedForClaims(claims *Claims) bool {
	if claims == nil {
		return true
	}
	var issued time.Time
	if claims.IssuedAt != nil {
		issued = claims.IssuedAt.Time
	}
	return tokenrevocation.IsRevokedID(claims.ID, issued)
}

// This package deliberately exports no echo JWT middleware constructor.
//
// It used to export JWT(publicKeyFunc), whose echojwt Skipper was a bare
// `return loopback.IsLoopback(c)`: authentication was skipped for every
// loopback peer, even one that presented a token. The gateway
// reverse-proxies every LAN request — and, over the Cloudflare tunnel, every
// internet request — to the backends over 127.0.0.1, so that skipper handed
// the whole internet an unauthenticated path. The JWT middlewares services
// actually install are declared inline, and the hardened form they should use
// skips only when no credential was presented at all:
//
//	loopback.IsLoopback(c) && c.Request().Header.Get(echo.HeaderAuthorization) == ""
//
// Nine of the inline skippers were rewritten to that form. This comment does
// not claim every one of them was: as of this change,
// CasaNova-Gateway/route/management_route.go still skips on a bare
// loopback.IsLoopback(c), and lives in another module.
//
// This shared copy never caught up and had no caller in any of the ten
// modules; the Go linker had already eliminated the symbol from every
// shipped binary. It was removed rather than repaired, because a shared
// constructor cannot do the job correctly anyway: the RBAC role must be
// stashed with middleware.SetRoleInContext inside ParseTokenFunc, and this
// package cannot import that one (import cycle). Services own their
// middleware; this package owns the token primitives (Validate, ParseToken,
// RevokedForClaims) that those middlewares call.
//
// middleware/loopback_trust_sweep_test.go enforces that no replacement helper
// reintroduces the loopback-as-trust shape anywhere in this module.

func GenerateKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("error generating key pair: %w", err)
	}

	publicKey := &privateKey.PublicKey

	return privateKey, publicKey, nil
}

func GenerateJwksJSON(publicKey *ecdsa.PublicKey) ([]byte, error) {
	// RFC 7518 requires the EC coordinates be the FIXED field size (32 bytes
	// for P-256), left-padded with zeros. big.Int.Bytes() strips leading
	// zeros, producing a 31-byte value ~0.8% of the time that strict external
	// OIDC libraries reject — breaking "Login with CasaNova" id_token verify.
	// FillBytes left-pads to the exact length.
	byteLen := (publicKey.Curve.Params().BitSize + 7) / 8
	xb := make([]byte, byteLen)
	yb := make([]byte, byteLen)
	publicKey.X.FillBytes(xb)
	publicKey.Y.FillBytes(yb)
	jwk := JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(xb),
		Y:   base64.RawURLEncoding.EncodeToString(yb),
	}

	jwks := JWKS{
		Keys: []JWK{jwk},
	}

	return json.Marshal(jwks)
}

func PublicKeyFromJwksJSON(jwksJSON []byte) (*ecdsa.PublicKey, error) {
	var jwks JWKS
	err := json.Unmarshal(jwksJSON, &jwks)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling JWKS JSON: %w", err)
	}

	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("no keys in JWKS")
	}

	jwk := jwks.Keys[0]

	x, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("error decoding X: %w", err)
	}

	y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("error decoding Y: %w", err)
	}

	publicKey := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}

	return publicKey, nil
}

func JWKSHandler(jwksJSON []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(jwksJSON)
		if err != nil {
			http.Error(w, "Error writing JWKS JSON", http.StatusInternalServerError)
			return
		}
	})
}
