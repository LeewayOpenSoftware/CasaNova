package v1

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	commonjwt "github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/oauth2"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	gojwt "github.com/golang-jwt/jwt/v4"
	"github.com/labstack/echo/v4"
)

// OAuth2 singletons. The client store is persistent (SQLite would also
// work but JSON keeps parity with the rest of Wave 4), the code store
// is in-memory because authorization codes are short-lived.
var (
	oauth2Once    sync.Once
	oauth2Clients *oauth2.ClientStore
	oauth2Codes   *oauth2.AuthCodeStore
	oauth2InitErr error
)

// initOAuth2 bootstraps both stores on first use. Runtime directory is
// pulled from the service config so the data lives alongside the user
// database rather than in a scattered location.
func initOAuth2() error {
	oauth2Once.Do(func() {
		dir := config.AppInfo.DBPath
		if dir == "" {
			dir = "/var/lib/casaos"
		}
		store, err := oauth2.NewClientStore(dir)
		if err != nil {
			oauth2InitErr = err
			return
		}
		oauth2Clients = store
		oauth2Codes = oauth2.NewAuthCodeStore(context.Background())
	})
	return oauth2InitErr
}

// OAuth2Clients is an accessor for the admin API so admin handlers can
// register/list/delete clients without duplicating the init path.
func OAuth2Clients() *oauth2.ClientStore {
	_ = initOAuth2()
	return oauth2Clients
}

// GetOAuth2Discovery serves the OIDC discovery document at
// /.well-known/openid-configuration. Standards-compliant OAuth2/OIDC
// clients fetch this once and self-configure — issuer, endpoint URLs,
// supported grant types, JWKS URI, etc. This is the single most
// important endpoint for plug-and-play interoperability.
func GetOAuth2Discovery(ctx echo.Context) error {
	if err := initOAuth2(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	issuer := issuerFromRequest(ctx)
	return ctx.JSON(http.StatusOK, echo.Map{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/v1/oauth2/authorize",
		"token_endpoint":                        issuer + "/v1/oauth2/token",
		"userinfo_endpoint":                     issuer + "/v1/oauth2/userinfo",
		"jwks_uri":                              issuer + "/" + commonjwt.JWKSPath,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

// GetOAuth2Authorize handles the /authorize endpoint — the first step
// of the authorization code flow. This endpoint:
//
//  1. Validates the client_id, redirect_uri, response_type, scope, and
//     PKCE parameters
//  2. Checks the caller is authenticated (via JWT middleware above);
//     if not, 401 so the browser can redirect to the CasaNova login page
//  3. Issues a one-shot authorization code and redirects to the client
//     with ?code=... (and ?state=... if the client sent one)
//
// Returning 401 on unauthenticated is deliberate: the proper OAuth2
// flow here is for the dashboard UI to detect the 401, redirect the
// user through the existing login form, then retry /authorize. That
// keeps all auth UI in one place.
func GetOAuth2Authorize(ctx echo.Context) error {
	if err := initOAuth2(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}

	q := ctx.Request().URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	responseType := q.Get("response_type")
	scope := q.Get("scope")
	state := q.Get("state")
	nonce := q.Get("nonce")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")

	if clientID == "" || redirectURI == "" {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_request"})
	}
	if responseType != "code" {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "unsupported_response_type"})
	}

	client, ok := oauth2Clients.Get(clientID)
	if !ok {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_client"})
	}
	if !client.HasRedirect(redirectURI) {
		// Per RFC 6749 §3.1.2.4, redirect_uri mismatches must NOT redirect
		// back to the (untrusted) URL. Show an error page directly.
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_redirect_uri"})
	}

	// Public clients MUST use PKCE. Confidential clients MAY use PKCE
	// and doing so is recommended — we accept either.
	if client.IsPublic() && codeChallenge == "" {
		return redirectErr(ctx, redirectURI, state, "invalid_request", "code_challenge required for public clients")
	}
	if codeChallenge != "" && codeChallengeMethod != "S256" {
		return redirectErr(ctx, redirectURI, state, "invalid_request", "only S256 code_challenge_method is supported")
	}

	// Scope allow-list check.
	scopes := strings.Fields(scope)
	for _, s := range scopes {
		if !client.HasScope(s) {
			return redirectErr(ctx, redirectURI, state, "invalid_scope", "scope not permitted for this client: "+s)
		}
	}

	// Resource owner identity. The JWT middleware on this route group
	// populates the "user_id" request header from the access token it
	// just validated. A missing header means the user isn't logged in —
	// return 401 and let the dashboard drive the login flow.
	uidStr := ctx.Request().Header.Get("user_id")
	if uidStr == "" {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "login_required"})
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "invalid_user"})
	}

	code, err := oauth2Codes.Issue(&oauth2.AuthCode{
		ClientID:            clientID,
		UserID:              uid,
		RedirectURI:         redirectURI,
		Scopes:              scopes,
		Nonce:               nonce,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
	})
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": "server_error"})
	}

	// Success: redirect back to the client with the code (and the state
	// parameter echoed back verbatim for CSRF protection on the client side).
	redir, err := url.Parse(redirectURI)
	if err != nil || (redir.Scheme != "http" && redir.Scheme != "https") || redir.Host == "" {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_redirect_uri"})
	}
	rq := redir.Query()
	rq.Set("code", code)
	if state != "" {
		rq.Set("state", state)
	}
	redir.RawQuery = rq.Encode()
	return ctx.Redirect(http.StatusFound, redir.String())
}

// PostOAuth2Token handles the /token endpoint — the second step of the
// authorization code flow. Exchanges an authorization code for an
// access_token and (when the "openid" scope is present) an id_token.
//
// Client authentication: confidential clients send their secret either
// in the HTTP Basic header or as a form field (client_secret). Public
// clients send only client_id and rely on PKCE.
func PostOAuth2Token(ctx echo.Context) error {
	if err := initOAuth2(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}

	grantType := ctx.FormValue("grant_type")
	if grantType != "authorization_code" {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "unsupported_grant_type"})
	}

	code := ctx.FormValue("code")
	redirectURI := ctx.FormValue("redirect_uri")
	codeVerifier := ctx.FormValue("code_verifier")

	clientID, clientSecret, _ := ctx.Request().BasicAuth()
	if clientID == "" {
		clientID = ctx.FormValue("client_id")
		clientSecret = ctx.FormValue("client_secret")
	}
	if clientID == "" {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_client"})
	}

	client, ok := oauth2Clients.Get(clientID)
	if !ok {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "invalid_client"})
	}
	if !client.IsPublic() && !client.VerifySecret(clientSecret) {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "invalid_client"})
	}

	ac, err := oauth2Codes.Redeem(code, clientID, redirectURI, codeVerifier)
	if err != nil {
		// Every redeem error maps to the spec's "invalid_grant". We
		// include the specific reason in error_description so integrators
		// can debug — it's allowed by the spec and much faster than
		// reading server logs.
		return ctx.JSON(http.StatusBadRequest, echo.Map{
			"error":             "invalid_grant",
			"error_description": err.Error(),
		})
	}

	user := service.MyService.User().GetUserInfoById(strconv.Itoa(ac.UserID))
	if user.Id == 0 {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": "invalid_grant", "error_description": "user no longer exists"})
	}
	privateKey, _ := service.MyService.User().GetKeyPair()

	// Access token: a CasaNova JWT, but minted with a NON-privileged role.
	// A delegated OAuth2 token is for a relying party to identify the user
	// (OIDC /userinfo, which keys off the user id — not the role), NOT to act
	// as them against CasaNova's own admin/management API. Baking in the user's
	// real role let a compromised/malicious RP call admin endpoints (self-
	// promote, delete users, wipe files) with an admin's session. We pass
	// "guest" EXPLICITLY — an empty role reads as ADMIN per the jwt Claims
	// back-compat rule. (Full per-scope/audience enforcement across resource
	// servers would constrain this further; role-drop closes the escalation.)
	// Mint with a DISTINCT issuer ("oauth2-delegated") + guest role. The role
	// alone is not enough: the JWT middleware re-reads the DB role for any
	// "guest"/empty token (legacy-session recovery), which would re-promote
	// this back to the user's real (admin) role. The middleware skips that
	// re-read for this issuer, so a delegated token stays de-privileged.
	accessToken, err := commonjwt.GenerateToken(user.Username, privateKey, user.Id, "guest", "oauth2-delegated", 3*time.Hour)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, echo.Map{"error": "server_error"})
	}

	resp := echo.Map{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int((3 * time.Hour).Seconds()),
		"scope":        strings.Join(ac.Scopes, " "),
	}

	// OIDC id_token: a JWT with the claims standard clients expect
	// (sub, aud, iss, nonce). Issued only when the client asked for
	// the "openid" scope.
	if containsString(ac.Scopes, "openid") {
		idToken, err := issueIDToken(ctx, user.Id, user.Username, client.ID, ac.Nonce, privateKey)
		if err == nil {
			resp["id_token"] = idToken
		}
	}

	return ctx.JSON(http.StatusOK, resp)
}

// GetOAuth2UserInfo returns the OIDC UserInfo document for the caller.
// Expects a Bearer access token in Authorization — the upstream JWT
// middleware has already validated it and populated the user_id header.
func GetOAuth2UserInfo(ctx echo.Context) error {
	uidStr := ctx.Request().Header.Get("user_id")
	if uidStr == "" {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "invalid_token"})
	}
	user := service.MyService.User().GetUserInfoById(uidStr)
	if user.Id == 0 {
		return ctx.JSON(http.StatusUnauthorized, echo.Map{"error": "invalid_token"})
	}
	return ctx.JSON(http.StatusOK, echo.Map{
		"sub":                strconv.Itoa(user.Id),
		"preferred_username": user.Username,
		"name":               user.Nickname,
		"email":              user.Email,
		"picture":            user.Avatar,
		"role":               user.Role,
	})
}

// issueIDToken creates the OIDC id_token JWT using the same ECDSA key
// as CasaNova's regular access tokens. Clients verify this via JWKS.
func issueIDToken(ctx echo.Context, userID int, username, audience, nonce string, privateKey interface{}) (string, error) {
	if privateKey == nil {
		return "", errors.New("no signing key")
	}
	claims := gojwt.MapClaims{
		"iss":                issuerFromRequest(ctx),
		"sub":                strconv.Itoa(userID),
		"aud":                audience,
		"exp":                time.Now().Add(3 * time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": username,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	return token.SignedString(privateKey)
}

// issuerFromRequest derives the OIDC issuer URL from the incoming
// request. This lets discovery work correctly behind reverse proxies
// (the operator can put CasaNova behind a real hostname and the
// discovery doc will reflect that hostname).
func issuerFromRequest(ctx echo.Context) string {
	scheme := "http"
	if ctx.Request().TLS != nil {
		scheme = "https"
	}
	if fwd := ctx.Request().Header.Get("X-Forwarded-Proto"); fwd == "http" || fwd == "https" {
		scheme = fwd
	}
	host := ctx.Request().Host
	return scheme + "://" + host
}

// redirectErr returns an OAuth2 error via redirect, per RFC 6749 §4.1.2.1.
// Used when the error is the client's fault but redirect_uri was valid.
func redirectErr(ctx echo.Context, redirectURI, state, code, description string) error {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, echo.Map{"error": code})
	}
	q := u.Query()
	q.Set("error", code)
	if description != "" {
		q.Set("error_description", description)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return ctx.Redirect(http.StatusFound, u.String())
}

// containsString is a tiny helper that avoids pulling in golang.org/x/exp/slices
// for a single-use lookup.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
