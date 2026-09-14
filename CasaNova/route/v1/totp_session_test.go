package v1

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/labstack/echo/v4"
)

func TestEnablingTOTPRejectsAlreadyIssuedTokens(t *testing.T) {
	dir := t.TempDir()
	previousConfig := totpConfigFile
	totpConfigFile = filepath.Join(dir, "2fa.json")
	t.Cleanup(func() { totpConfigFile = previousConfig })
	tokenrevocation.SetPath(filepath.Join(dir, "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	secret := []byte("01234567890123456789")
	cfg := &totpConfig{Secret: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(totpConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

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
		t.Fatal("precondition: old token was rejected before 2FA activation")
	}

	e := echo.New()
	code := generateTOTP(secret, time.Now().Unix()/totpPeriod)
	req := httptest.NewRequest(http.MethodPost, "/v1/sys/2fa/verify", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), userID)
	if err := PostTOTPVerify(ctx); err != nil {
		t.Fatalf("PostTOTPVerify: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if accepted() {
		t.Fatal("old token was still accepted after 2FA activation")
	}
}
