package jwt

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

// NewJTI returns a fresh, opaque, unguessable token identifier suitable
// for the JWT `jti` claim. 16 random bytes → 128 bits of entropy, far
// beyond what an attacker can brute-force in a refresh-token TTL.
func NewJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("jwt: cannot generate jti: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Claims is the CasaNova JWT payload. Role is included so every service
// can authorize without a DB round-trip to UserService. An empty Role on
// a valid token is treated as "admin" by the rbac package for backward
// compatibility with tokens issued before RBAC existed.
type Claims struct {
	Username string `json:"username"`
	ID       int    `json:"id"`
	Role     string `json:"role,omitempty"`
	jwt.RegisteredClaims
}

// GenerateToken signs a new JWT carrying the given identity plus role.
// The role string is stored verbatim; callers should pass a value produced
// by rbac.Normalize to avoid smuggling unexpected strings into claims.
func GenerateToken(username string, privateKey *ecdsa.PrivateKey, id int, role string, issuer string, t time.Duration) (string, error) {
	return GenerateTokenWithJTI(username, privateKey, id, role, issuer, "", t)
}

// GenerateTokenWithJTI is identical to GenerateToken but also stamps a
// caller-supplied jti onto the token. This is what refresh-token rotation
// uses so each issued refresh has a unique, server-trackable identifier.
// Pass jti="" to mint a token without a jti (legacy / access tokens).
func GenerateTokenWithJTI(username string, privateKey *ecdsa.PrivateKey, id int, role string, issuer string, jti string, t time.Duration) (string, error) {
	claims := Claims{
		Username: username,
		ID:       id,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(t)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    issuer,
			ID:        jti,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	signedToken, err := token.SignedString(privateKey)
	return signedToken, err
}

func ParseToken(signedToken string, publicKeyFunc func() (*ecdsa.PublicKey, error)) (*Claims, error) {
	token, err := jwt.ParseWithClaims(signedToken, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return publicKeyFunc()
	})
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

// get AccessToken
//
// Deprecated: use GetAccessTokenWithRole to propagate RBAC role claims.
// Kept for backward compatibility with call sites that don't yet know
// about roles — those tokens get an empty Role and are treated as admin.
func GetAccessToken(username string, privateKey *ecdsa.PrivateKey, id int) (string, error) {
	return GenerateToken(username, privateKey, id, "", "casaos", 3*time.Hour)
}

// GetAccessTokenWithRole issues a short-lived access token that carries
// the user's RBAC role. Every RBAC-aware service reads claims.Role to
// authorize the request without hitting UserService.
func GetAccessTokenWithRole(username string, privateKey *ecdsa.PrivateKey, id int, role string) (string, error) {
	return GenerateToken(username, privateKey, id, role, "casaos", 3*time.Hour)
}

// Deprecated: use GetRefreshTokenWithRole.
func GetRefreshToken(username string, private *ecdsa.PrivateKey, id int) (string, error) {
	return GenerateToken(username, private, id, "", "refresh", 7*24*time.Hour)
}

// GetRefreshTokenWithRole issues a refresh token that carries the role.
// The role is re-baked into the access token on each refresh so a
// promotion/demotion takes effect within the refresh window.
//
// Wave 26: callers should prefer GetRefreshTokenWithRoleAndJTI so the
// server can track and rotate individual refresh tokens. This wrapper is
// kept for old code paths that don't yet plug into the rotation store.
func GetRefreshTokenWithRole(username string, private *ecdsa.PrivateKey, id int, role string) (string, error) {
	return GenerateToken(username, private, id, role, "refresh", 7*24*time.Hour)
}

// GetRefreshTokenWithRoleAndJTI mints a refresh token whose `jti` claim is
// set to the supplied identifier. The caller is expected to register the
// jti in its rotation store before returning the token to the user.
func GetRefreshTokenWithRoleAndJTI(username string, private *ecdsa.PrivateKey, id int, role string, jti string) (string, error) {
	return GenerateTokenWithJTI(username, private, id, role, "refresh", jti, 7*24*time.Hour)
}

// Validate parses an incoming JWT and accepts it ONLY as an access token.
//
// Wave 31: Tokens whose issuer is "refresh" are explicitly rejected here.
// Refresh tokens have a much longer TTL (7 days vs 3 hours) and only the
// /v1/users/refresh handler — which calls ParseToken directly and verifies
// `issuer == "refresh"` itself — should ever accept one. Before this gate
// existed, every service-side JWT middleware (Gateway management, Common
// per-service middleware, the v2 echo groups in CasaNova / AppManagement /
// LocalStorage / UserService / MessageBus / JobManagement) used Validate()
// to authorize requests, which meant a leaked refresh token could be used
// directly as an access token for a full week. Now any caller passing a
// refresh-issuer JWT to Validate() is rejected with errRefreshNotAccess.
//
// Tokens with an empty issuer are still accepted because legacy callers
// minted access tokens before issuer was even a field. Real new access
// tokens carry issuer "casaos" (see GetAccessTokenWithRole).
func Validate(token string, publicKeyFunc func() (*ecdsa.PublicKey, error)) (bool, *Claims, error) {
	claims, err := ParseToken(token, publicKeyFunc)
	if err != nil {
		return false, nil, err
	}

	if claims == nil {
		return false, nil, errors.New("invalid token")
	}

	if claims.Issuer == "refresh" {
		return false, nil, errRefreshNotAccess
	}

	return true, claims, nil
}

// errRefreshNotAccess is returned when a refresh token is presented to
// Validate() (which is the access-token gate). Exported as a sentinel so
// middleware can distinguish "credential confusion attempt" from "expired
// signature" in audit logs if it ever wants to.
var errRefreshNotAccess = errors.New("jwt: refresh token rejected by access-token validator")
