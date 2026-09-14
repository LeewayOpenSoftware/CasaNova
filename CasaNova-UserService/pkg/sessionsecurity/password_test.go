package sessionsecurity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
)

func TestResetPasswordRejectsAlreadyIssuedToken(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	const userID = 42
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := commonjwt.GenerateToken("alice", key, userID, "admin", "casaos", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	accepted := func() bool {
		valid, claims, err := commonjwt.Validate(oldToken, func() (*ecdsa.PublicKey, error) { return &key.PublicKey, nil })
		return err == nil && valid && !commonjwt.RevokedForClaims(claims)
	}
	if !accepted() {
		t.Fatal("precondition: old token was rejected before password reset")
	}

	updated := false
	if err := ResetPassword(userID, func() { updated = true }); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if !updated {
		t.Fatal("password update was not applied")
	}
	if accepted() {
		t.Fatal("old token was still accepted after CLI password reset")
	}
}
