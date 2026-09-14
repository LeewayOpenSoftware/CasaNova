package v1

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
)

func TestRefreshDownstreamFailureDoesNotConsumePresentedToken(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })
	refreshFamiliesMu.Lock()
	refreshFamilies = map[string]*refreshFamily{}
	jtiToFamily = map[string]string{}
	refreshFamiliesMu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jti, err := registerNewRefreshFamily(42, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := jwt.GetRefreshTokenWithRoleAndJTI("alice", key, 42, "user", jti)
	if err != nil {
		t.Fatal(err)
	}
	fake := &mockParityUserService{privKey: key, pubKey: &key.PublicKey, usersByID: map[string]model2.UserDBModel{}}
	previous := service.MyService
	service.MyService = &mockParityRepo{userSvc: fake}
	t.Cleanup(func() { service.MyService = previous })

	call := func() *httptest.ResponseRecorder {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/v1/users/refresh", strings.NewReader(`{"refresh_token":"`+refresh+`"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		if err := PostUserRefreshToken(e.NewContext(req, rec)); err != nil {
			t.Fatalf("PostUserRefreshToken: %v", err)
		}
		return rec
	}

	if first := call(); first.Code != http.StatusUnauthorized {
		t.Fatalf("missing-user status = %d, want 401", first.Code)
	}
	fake.usersByID["42"] = model2.UserDBModel{Id: 42, Username: "alice", Role: "user"}
	if retry := call(); retry.Code != http.StatusOK {
		t.Fatalf("old refresh token was rejected after a downstream failure: status=%d body=%s", retry.Code, retry.Body.String())
	}
}
