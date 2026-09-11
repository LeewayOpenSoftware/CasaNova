package route

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	dbmodel "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	jwtlib "github.com/golang-jwt/jwt/v4"
)

// fakeUserServiceRoleLookup backs finalizeAccessClaims's DB re-read path.
type fakeUserServiceRoleLookup struct {
	service.UserService
	dbRole string
}

func (f fakeUserServiceRoleLookup) GetUserInfoById(id string) dbmodel.UserDBModel {
	return dbmodel.UserDBModel{Id: 7, Role: f.dbRole}
}

type fakeRepoRoleLookup struct {
	service.Repository
	user fakeUserServiceRoleLookup
}

func (f fakeRepoRoleLookup) User() service.UserService { return f.user }

func withFakeUserService(t *testing.T, dbRole string) {
	t.Helper()
	prev := service.MyService
	service.MyService = fakeRepoRoleLookup{user: fakeUserServiceRoleLookup{dbRole: dbRole}}
	t.Cleanup(func() { service.MyService = prev })
}

// OLA 3 — v1.go rejected refresh-issued tokens used as access tokens; v2.go
// never did, despite its own comment claiming to match v1's ParseTokenFunc.
// A refresh token is only ever meant to be exchanged at POST
// /v1/users/refresh; accepting it directly against an API endpoint lets a
// caller holding only a refresh token (a more sensitive credential, by
// design) act as if it held an access token.
func TestFinalizeAccessClaimsRejectsRefreshIssuedTokens(t *testing.T) {
	claims := &jwt.Claims{
		ID:   7,
		Role: "admin",
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer: "refresh",
		},
	}
	_, ok := finalizeAccessClaims(claims)
	if ok {
		t.Fatal("finalizeAccessClaims accepted a refresh-issued token as an access token")
	}
}

// Positive control: an ordinary access token with a real role passes through
// unchanged — the DB is not even consulted.
func TestFinalizeAccessClaimsPassesThroughARealRole(t *testing.T) {
	withFakeUserService(t, "guest") // if this were consulted, the test would wrongly pass either way
	claims := &jwt.Claims{ID: 7, Role: "admin"}
	role, ok := finalizeAccessClaims(claims)
	if !ok {
		t.Fatal("finalizeAccessClaims rejected an ordinary access token")
	}
	if role != "admin" {
		t.Fatalf("role = %q, want %q — a forged-looking claim must not be overridden by the DB "+
			"when it already names a real role", role, "admin")
	}
}

// Legacy empty/guest claims recover the DB's real role.
func TestFinalizeAccessClaimsRecoversStaleGuestFromDB(t *testing.T) {
	withFakeUserService(t, "admin")
	claims := &jwt.Claims{ID: 7, Role: "guest"}
	role, ok := finalizeAccessClaims(claims)
	if !ok {
		t.Fatal("finalizeAccessClaims rejected a legacy guest-claim token")
	}
	if role != "admin" {
		t.Fatalf("role = %q, want %q (recovered from the DB)", role, "admin")
	}
}

// OAuth2-delegated tokens are minted guest on purpose and must never be
// promoted back to the user's real DB role.
func TestFinalizeAccessClaimsDoesNotRecoverOAuth2DelegatedTokens(t *testing.T) {
	withFakeUserService(t, "admin")
	claims := &jwt.Claims{
		ID:   7,
		Role: "guest",
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer: "oauth2-delegated",
		},
	}
	role, ok := finalizeAccessClaims(claims)
	if !ok {
		t.Fatal("finalizeAccessClaims rejected an oauth2-delegated token")
	}
	if role != "guest" {
		t.Fatalf("role = %q, want %q — an oauth2-delegated guest token must not be "+
			"promoted to the user's real DB role", role, "guest")
	}
}
