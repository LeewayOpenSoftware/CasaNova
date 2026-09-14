package sessionsecurity

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
)

func TestResetPasswordRejectsAlreadyIssuedToken(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	const userID = 42
	issuedBeforeReset := time.Now().Add(-time.Minute)
	if tokenrevocation.IsRevokedID(userID, issuedBeforeReset) {
		t.Fatal("precondition: old token was rejected before password reset")
	}

	updated := false
	if err := ResetPassword(userID, func() { updated = true }); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if !updated {
		t.Fatal("password update was not applied")
	}
	if !tokenrevocation.IsRevokedID(userID, issuedBeforeReset) {
		t.Fatal("old token was still accepted after CLI password reset")
	}
}
