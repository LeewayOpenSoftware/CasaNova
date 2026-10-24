package route

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	jwtlib "github.com/golang-jwt/jwt/v4"
)

// An id_token-shaped claim set (issuer = request host, no "id") names no
// user and must not be honoured as an access token by the middleware.
func TestFinalizeAccessClaimsRejectsTokenWithoutUserID(t *testing.T) {
	withFakeUserService(t, "admin")
	if _, ok := finalizeAccessClaims(&jwt.Claims{ID: 0,
		RegisteredClaims: jwtlib.RegisteredClaims{Issuer: "http://nas.local"}}); ok {
		t.Fatal("finalizeAccessClaims accepted a claim set with no user id")
	}
}
