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
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

func TestHTTPPasswordChangeRejectsAlreadyIssuedAccessToken(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })

	hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	user := model2.UserDBModel{Id: 42, Username: "alice", Password: string(hash), Role: "user"}
	fake := &mockParityUserService{
		privKey: key, pubKey: &key.PublicKey,
		users:     map[string]model2.UserDBModel{"alice": user},
		usersByID: map[string]model2.UserDBModel{"42": user},
	}
	previous := service.MyService
	service.MyService = &mockParityRepo{userSvc: fake}
	t.Cleanup(func() { service.MyService = previous })

	issuedBeforeChange := time.Now().Add(-time.Minute)
	if tokenrevocation.IsRevokedID(user.Id, issuedBeforeChange) {
		t.Fatal("precondition: old access token was rejected before password change")
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/users/current/password", strings.NewReader(`{"old_password":"old-password","password":"new-password"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("user_id", "42")
	rec := httptest.NewRecorder()
	if err := PutUserPassword(e.NewContext(req, rec)); err != nil {
		t.Fatalf("PutUserPassword: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !tokenrevocation.IsRevokedID(user.Id, issuedBeforeChange) {
		t.Fatal("old access token was still accepted after HTTP password change")
	}
}
