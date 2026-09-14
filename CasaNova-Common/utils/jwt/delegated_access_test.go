package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"
)

func TestDelegatedTokenIsRejectedByGeneralAccessValidator(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token, err := GenerateToken("alice", key, 42, "guest", "oauth2-delegated", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	valid, _, err := Validate(token, func() (*ecdsa.PublicKey, error) { return &key.PublicKey, nil })
	if err == nil || valid {
		t.Fatal("oauth2-delegated token was accepted by the general access validator")
	}
	valid, claims, err := ValidateDelegated(token, func() (*ecdsa.PublicKey, error) { return &key.PublicKey, nil })
	if err != nil || !valid || claims.ID != 42 {
		t.Fatalf("delegated validator broke the legitimate userinfo token: valid=%v claims=%+v err=%v", valid, claims, err)
	}
}
