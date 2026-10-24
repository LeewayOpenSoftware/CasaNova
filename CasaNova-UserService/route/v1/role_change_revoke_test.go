package v1

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	dbmodel "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
)

// roleChangeTokenLive reports whether tok passes the check every service runs.
func roleChangeTokenLive(key *ecdsa.PrivateKey, tok string) bool {
	valid, claims, err := commonjwt.Validate(tok, func() (*ecdsa.PublicKey, error) { return &key.PublicKey, nil })
	return err == nil && valid && !commonjwt.RevokedForClaims(claims)
}

// A demoted admin must not keep admin through its already-issued tokens:
// services authorise from the role claim, so PutUserRole has to revoke the
// target's access tokens and refresh families.
func TestPutUserRoleRevokesTargetSessions(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	fake := withPutUserRoleFake(map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"8": {Id: 8, Role: "admin"},
	})
	prev := service.MyService
	service.MyService = fakeRepoPutRole{user: fake}
	defer func() { service.MyService = prev }()

	access, err := commonjwt.GenerateToken("bob", key, 8, "admin", "casaos", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jti, err := registerNewRefreshFamily(8, time.Hour, "ua", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !roleChangeTokenLive(key, access) {
		t.Fatal("precondition: access token rejected before the role change")
	}
	time.Sleep(1100 * time.Millisecond) // iat strictly before the revocation instant

	ctx, rec := putUserRoleCtx("8", "7", "guest")
	if err := PutUserRole(ctx); err != nil {
		t.Fatalf("PutUserRole: %v", err)
	}
	if rec.Code != http.StatusOK || fake.updateSeen == nil || fake.updateSeen.Role != "guest" {
		t.Fatalf("demotion did not happen (status %d body %s)", rec.Code, rec.Body.String())
	}
	if roleChangeTokenLive(key, access) {
		t.Fatal("pre-demotion admin access token is still accepted after PUT /v1/users/:id/role")
	}
	if rotateRefreshFamily(jti, "next") == nil {
		t.Fatal("refresh family of the demoted user survived the role change")
	}
}

// Re-submitting the role a user already has is not a change and must not
// log them out.
func TestPutUserRoleSameRoleKeepsSessions(t *testing.T) {
	tokenrevocation.SetPath(filepath.Join(t.TempDir(), "token-revocations.json"))
	t.Cleanup(func() { tokenrevocation.SetPath(tokenrevocation.DefaultPath) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	fake := withPutUserRoleFake(map[string]dbmodel.UserDBModel{
		"7": {Id: 7, Role: "admin"},
		"8": {Id: 8, Role: "user"},
	})
	prev := service.MyService
	service.MyService = fakeRepoPutRole{user: fake}
	defer func() { service.MyService = prev }()

	access, err := commonjwt.GenerateToken("bob", key, 8, "user", "casaos", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	ctx, rec := putUserRoleCtx("8", "7", "user")
	if err := PutUserRole(ctx); err != nil {
		t.Fatalf("PutUserRole: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !roleChangeTokenLive(key, access) {
		t.Fatal("an unchanged role revoked the user's sessions")
	}
}
