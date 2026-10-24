package jwt_test

import (
	"crypto/ecdsa"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	gojwt "github.com/golang-jwt/jwt/v4"
)

// An OIDC id_token is signed with the first-party key but names no user in
// the "id" claim. Validate must not accept it as an access token: it would
// authorise as principal 0, which no revocation ever matches.
func TestValidateRejectsTokenWithoutUserID(t *testing.T) {
	priv, pub, err := jwt.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	keyFunc := func() (*ecdsa.PublicKey, error) { return pub, nil }

	idToken, err := gojwt.NewWithClaims(gojwt.SigningMethodES256, gojwt.MapClaims{
		"iss":                "http://nas.local",
		"sub":                "42",
		"aud":                "some-rp",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "alice",
	}).SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	if valid, claims, _ := jwt.Validate(idToken, keyFunc); valid {
		t.Fatalf("id_token-shaped token accepted as access token (id=%d iss=%q)", claims.ID, claims.Issuer)
	}

	zero, err := jwt.GenerateToken("alice", priv, 0, "admin", "casaos", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if valid, _, _ := jwt.Validate(zero, keyFunc); valid {
		t.Fatal("access token for principal 0 accepted")
	}

	access, err := jwt.GetAccessTokenWithRole("alice", priv, 42, "user")
	if err != nil {
		t.Fatal(err)
	}
	if valid, claims, err := jwt.Validate(access, keyFunc); !valid || err != nil || claims.ID != 42 {
		t.Fatalf("ordinary access token rejected: valid=%v err=%v", valid, err)
	}
}
