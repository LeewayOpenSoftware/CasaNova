package jwt_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJwtFlow(t *testing.T) {
	// Generate a key pair
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	require.NoError(t, err)

	// Generate access and refresh tokens
	username := "testuser"
	id := 1

	accessToken, err := jwt.GetAccessToken(username, privateKey, id)
	require.NoError(t, err)

	refreshToken, err := jwt.GetRefreshToken(username, privateKey, id)
	require.NoError(t, err)

	// Generate JWKS JSON
	jwksJSON, err := jwt.GenerateJwksJSON(publicKey)
	require.NoError(t, err)

	// Serve the JWKS JSON
	server := httptest.NewServer(jwt.JWKSHandler(jwksJSON))
	defer server.Close()

	// Consume the JWKS JSON
	response, err := http.Get(server.URL + "/" + jwt.JWKSPath)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	var jwks jwt.JWKS
	err = json.NewDecoder(response.Body).Decode(&jwks)
	require.NoError(t, err)
	require.Len(t, jwks.Keys, 1)

	// Extract the public key from the JWKS JSON
	consumedPublicKey, err := jwt.PublicKeyFromJwksJSON(jwksJSON)
	require.NoError(t, err)

	// Validate the access token
	valid, claims, err := jwt.Validate(accessToken, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	require.NoError(t, err)
	assert.True(t, valid)
	assert.Equal(t, username, claims.Username)
	assert.Equal(t, id, claims.ID)

	// Wave 31: Validate() is the access-token gate and must REJECT
	// refresh tokens. Callers that legitimately need to consume a
	// refresh token (the /v1/users/refresh handler) parse them via
	// jwt.ParseToken directly and verify the issuer themselves.
	valid, claims, err = jwt.Validate(refreshToken, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	assert.Error(t, err)
	assert.False(t, valid)
	assert.Nil(t, claims)

	// ParseToken still succeeds for refresh tokens (the refresh
	// endpoint relies on this), and the resulting claims carry
	// the "refresh" issuer.
	parsed, err := jwt.ParseToken(refreshToken, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	require.NoError(t, err)
	require.NotNil(t, parsed)
	assert.Equal(t, "refresh", parsed.Issuer)
	assert.Equal(t, username, parsed.Username)
	assert.Equal(t, id, parsed.ID)
}

func TestInvalidToken(t *testing.T) {
	// Generate a key pair
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	require.NoError(t, err)

	// Generate access token
	username := "testuser"
	id := 1

	accessToken, err := jwt.GetAccessToken(username, privateKey, id)
	require.NoError(t, err)

	// Modify the token to make it invalid
	invalidToken := accessToken[:len(accessToken)-5] + "abcde"

	// Validate the invalid token
	valid, claims, err := jwt.Validate(invalidToken, func() (*ecdsa.PublicKey, error) { return publicKey, nil })
	assert.Error(t, err)
	assert.False(t, valid)
	assert.Nil(t, claims)
}

// TestNoExportedEchoMiddlewareConstructor pins the decision that this package
// exports token primitives only, never an echo middleware.
//
// It replaces TestJWTMiddlewareWithValidToken and
// TestJWTMiddlewareWithInvalidToken, which exercised jwt.JWT(). That
// constructor's echojwt Skipper was a bare `loopback.IsLoopback(c)`, and
// httptest requests carry an empty RemoteAddr, which the loopback helper
// treats as a Unix-socket peer. So the "invalid token" test was asserting a
// 401 while the middleware skipped authentication entirely and returned 200 —
// a standing failure that was, in fact, the vulnerability demonstrating
// itself. jwt.JWT() had no caller outside these two tests and has been
// removed; services declare their JWT middleware inline with the hardened
// skipper. See the comment block in jwt_helper.go.
//
// This asserts the shape rather than the name: any exported function here
// returning an echo.MiddlewareFunc would fail it, whatever it is called. The
// broader rule — no loopback-as-trust helper anywhere in this module — is
// enforced by middleware/loopback_trust_sweep_test.go.
func TestNoExportedEchoMiddlewareConstructor(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	require.NotEmpty(t, pkgs, "parsed no non-test sources; the sweep would pass vacuously")

	// Positive control: the detector must fire on the shape it hunts for.
	// Without this, a broken matcher would report "clean" on any input.
	ctrl, err := parser.ParseFile(token.NewFileSet(), "control.go",
		"package jwt\nimport \"github.com/labstack/echo/v4\"\nfunc JWT(k int) echo.MiddlewareFunc { return nil }\n", 0)
	require.NoError(t, err)
	require.NotEmpty(t, echoMiddlewareConstructors(ctrl),
		"detector failed its positive control: it cannot see an exported echo.MiddlewareFunc constructor")

	var offenders []string
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, fn := range echoMiddlewareConstructors(file) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: exported %s returns echo.MiddlewareFunc",
					name, fset.Position(fn.Pos()).Line, fn.Name.Name))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("package jwt must export token primitives only, not echo middleware:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// echoMiddlewareConstructors returns the exported top-level functions in file
// whose result list mentions echo.MiddlewareFunc.
func echoMiddlewareConstructors(file *ast.File) []*ast.FuncDecl {
	var found []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
			continue
		}
		for _, res := range fn.Type.Results.List {
			sel, ok := res.Type.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			if pkgIdent.Name == "echo" && sel.Sel.Name == "MiddlewareFunc" {
				found = append(found, fn)
			}
		}
	}
	return found
}
