package v1

import (
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTOTPReplayFixture(t *testing.T, secretB32 string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2fa.json")
	data, err := json.Marshal(userTOTPConfig{Enabled: true, Verified: true, Secret: secretB32})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := userTOTPConfigFile
	userTOTPConfigFile = path
	t.Cleanup(func() { userTOTPConfigFile = prev })
}

// A TOTP code accepted at login must not be accepted again: it stays valid
// for its whole ±1-step window, so without last-step tracking a phished or
// logged code is reusable for ~90 s.
func TestTOTPLoginCodeCannotBeReplayed(t *testing.T) {
	raw := []byte("replay-test-secret-0")
	secretB32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	writeTOTPReplayFixture(t, secretB32)

	step := time.Now().Unix() / int64(userTOTPPeriod)
	code := generateUserTOTP(raw, step)
	if err := totpEnforceLogin(code); err != nil {
		t.Fatalf("first use of a fresh code rejected: %v", err)
	}
	if err := totpEnforceLogin(code); !errors.Is(err, errTOTPInvalid) {
		t.Fatalf("replayed TOTP code: got %v, want errTOTPInvalid", err)
	}
	// An older step is also refused once a newer one was accepted.
	if err := totpEnforceLogin(generateUserTOTP(raw, step-1)); !errors.Is(err, errTOTPInvalid) {
		t.Fatalf("earlier-step code after a newer one: got %v, want errTOTPInvalid", err)
	}
	// The next step's code (clock drift) is still accepted.
	if err := totpEnforceLogin(generateUserTOTP(raw, step+1)); err != nil {
		t.Fatalf("next-step code rejected: %v", err)
	}
}

// A new secret (2FA re-enrolled) must not inherit the old secret's last step.
func TestTOTPReplayStateIsPerSecret(t *testing.T) {
	rawA := []byte("replay-test-secret-A")
	writeTOTPReplayFixture(t, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rawA))
	step := time.Now().Unix() / int64(userTOTPPeriod)
	if err := totpEnforceLogin(generateUserTOTP(rawA, step+1)); err != nil {
		t.Fatalf("secret A: %v", err)
	}

	rawB := []byte("replay-test-secret-B")
	writeTOTPReplayFixture(t, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rawB))
	if err := totpEnforceLogin(generateUserTOTP(rawB, step)); err != nil {
		t.Fatalf("fresh code for a new secret refused because of the old secret's step: %v", err)
	}
}
