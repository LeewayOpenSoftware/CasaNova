package v1

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/labstack/echo/v4"
)

// The id_token handed to a relying party at /v1/oauth2/token is signed with
// the first-party key; it must not double as a CasaNova access token.
func TestIssuedIDTokenIsNotAnAccessToken(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/oauth2/token", nil)
	ctx := echo.New().NewContext(req, httptest.NewRecorder())
	idToken, err := issueIDToken(ctx, 42, "alice", "some-rp", "", key)
	if err != nil {
		t.Fatal(err)
	}
	valid, claims, _ := commonjwt.Validate(idToken, func() (*ecdsa.PublicKey, error) { return &key.PublicKey, nil })
	if valid {
		t.Fatalf("OIDC id_token accepted by jwt.Validate as an access token (iss=%q id=%d)", claims.Issuer, claims.ID)
	}
}
