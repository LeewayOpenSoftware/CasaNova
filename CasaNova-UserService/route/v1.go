package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"strconv"
	"strings"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	v1 "github.com/LeewayOpenSoftware/CasaNova-UserService/route/v1"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// jwtLoopbackSkipper reports whether JWT validation should be skipped for a
// request. Loopback callers bypass auth ONLY when they present no token at all
// (truly anonymous local/CLI calls, e.g. the public /v1/users/status probe).
//
// jwtLoopbackSkipper decides who reaches /v1/users/* and the protected oauth2
// routes WITHOUT a token. The answer is: a peer service holding the per-boot
// internal secret, and nobody else.
//
// It used to skip any tokenless request whose socket peer was 127.0.0.1. The
// gateway reverse-proxies the LAN and the Cloudflare tunnel over loopback, so
// that meant "anybody who omits the header" — the tenth copy of this same
// boolean in this tree. It survived the first sweep because it is a NAMED
// function rather than the one-line expression the sweep grepped for.
//
// The routes it guards are account data. The pre-login routes — login,
// register, refresh, logout, status, image and the OIDC discovery document —
// are registered on the echo instance itself, outside this middleware, so they
// are unaffected. /v1/oauth2/token likewise: it authenticates with
// client_id+secret on a group that carries no JWT at all. JWKS is served by a
// separate mux in main.go, so GetPublicKey — which every service calls to
// validate tokens — never passes through here. The allowlist is therefore nil.
var jwtLoopbackSkipper = authskip.For(nil)

func InitRouter() http.Handler {
	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:  []string{"*"},
		AllowMethods:  []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:  []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders: []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:        172800,
		// Wildcard origin + credentials is rejected by browsers anyway;
		// keep this false so the misconfiguration is loud, not silent.
		AllowCredentials: false,
	})))

	e.Use(echo_middleware.Gzip())

	// Defense in depth: strip identity headers from every incoming
	// request so the JWT middleware (or its absence on public routes)
	// is the sole source of truth. The Gateway already strips these
	// in production, but a service exposed directly to the network
	// must not trust them either.
	e.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Request().Header.Del("user_id")
			c.Request().Header.Del("User_id")
			c.Request().Header.Del("User-Id")
			c.Request().Header.Del("role")
			c.Request().Header.Del("Role")
			return next(c)
		}
	})

	// Custom logger that omits the query string — JWTs travel as
	// ?token=<...> on WebSocket upgrade and any non-WS callers should
	// use the Authorization header. Logging full URI leaks live tokens
	// to journald and any log shipper.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.POST("/v1/users/register", v1.PostUserRegister)
	e.POST("/v1/users/login", v1.PostUserLogin)
	// Wave 28: GET /v1/users/name was previously public — anyone could
	// pull the entire username list, which is a perfect enumeration
	// oracle that defeats every login-side timing-attack defense. The
	// route is now mounted inside v1UsersGroup (JWT-required) below,
	// AND the handler enforces an admin RBAC check so non-admin
	// callers cannot iterate other users either.
	e.POST("/v1/users/refresh", v1.PostUserRefreshToken)
	// Logout is unauthenticated by design: the access token may have
	// already expired by the time the user clicks "log out", so a JWT
	// gate would render the endpoint unreachable in exactly the cases
	// where it matters most. The handler verifies the *refresh* token
	// signature itself before revoking the family.
	e.POST("/v1/users/logout", v1.PostUserLogout)
	// No short-term modifications
	e.GET("/v1/users/image", v1.GetUserImage)

	e.GET("/v1/users/status", v1.GetUserStatus) // init/check

	v1Group := e.Group("/v1")

	jwtConfig := echo_middleware.JWTConfig{
		Skipper: jwtLoopbackSkipper,
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(
				token,
				func() (*ecdsa.PublicKey, error) {
					_, publicKey := service.MyService.User().GetKeyPair()
					return publicKey, nil
				})
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			// Reject refresh tokens used as access tokens. Refresh tokens
			// are issued with Issuer="refresh"; only the dedicated
			// /v1/users/refresh handler may consume them.
			if claims.Issuer == "refresh" {
				return nil, echo.ErrUnauthorized
			}

			// Reject tokens for deleted users. The shared authority is the
			// durable, cross-service record; the in-memory map only
			// short-circuits it inside this process. Fails closed if the
			// authority cannot be consulted.
			_, revokedLocally := v1.RevokedUsers.Load(strconv.Itoa(claims.ID))
			if revokedLocally || jwt.RevokedForClaims(claims) {
				return nil, echo.ErrUnauthorized
			}

			// Wave 35-S / 35-U: tokens issued before Wave 35-U's
			// login fix may carry either:
			//   • an empty role claim (pre-role-claim builds), or
			//   • "guest" (Wave 35-R..S where PostUserLogin was calling
			//     rbac.Normalize("") and getting RoleGuest back).
			// In both cases the DB row is the authoritative answer —
			// the AutoMigrate backfill in pkg/sqlite/db.go has already
			// promoted legacy empty rows to "admin" by the time any
			// middleware runs. Re-read the DB whenever the token's
			// role is empty OR "guest" so existing sessions don't
			// need to force-logout to recover from the old bug. Real
			// (non-empty, non-guest) roles continue to come straight
			// from the signed claim, so a forged "admin" claim from a
			// stolen signing key still cannot be elevated through
			// this path — the DB lookup only ever promotes guest→
			// (whatever the DB says), it does not let the token
			// override the DB when the claim already says admin.
			role := claims.Role
			// Skip the DB re-read for OAuth2-delegated tokens: they are
			// minted "guest" ON PURPOSE to de-privilege a relying party, so
			// promoting them back to the user's DB role would re-open the
			// escalation. Only legacy first-party ("casaos") guest/empty
			// tokens get recovered.
			if (role == "" || role == "guest") && claims.ID != 0 && claims.Issuer != "oauth2-delegated" {
				if dbUser := service.MyService.User().GetUserInfoById(strconv.Itoa(claims.ID)); dbUser.Id != 0 && dbUser.Role != "" {
					role = dbUser.Role
				}
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))
			commonmw.SetRoleInContext(c, role, claims.ID)

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				if h := c.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
					// Strip the "Bearer " auth scheme so ParseTokenFunc
					// receives the bare JWT. echo does NOT strip the scheme
					// for custom extractors, and jwt.Validate cannot parse
					// "Bearer <jwt>". Case-insensitive per RFC 6750 §2.1.
					// (Previously latent: the loopback skip meant the JWT
					// middleware never ran on the Gateway-proxied path, so
					// this extractor was never exercised for real traffic.)
					if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
						h = h[7:]
					}
					return []string{h}, nil
				}
				return []string{c.QueryParam("token")}, nil
			},
		},
	}

	v1UsersGroup := v1Group.Group("/users")
	v1UsersGroup.Use(echo_middleware.JWTWithConfig(jwtConfig))
	{
		v1UsersGroup.Use()
		v1UsersGroup.GET("/current", v1.GetUserInfo)
		v1UsersGroup.PUT("/current", v1.PutUserInfo)
		v1UsersGroup.PUT("/current/password", v1.PutUserPassword)

		v1UsersGroup.GET("/current/custom/:key", v1.GetUserCustomConf)
		v1UsersGroup.POST("/current/custom/:key", v1.PostUserCustomConf)
		v1UsersGroup.DELETE("/current/custom/:key", v1.DeleteUserCustomConf)

		v1UsersGroup.POST("/current/image/:key", v1.PostUserUploadImage)
		v1UsersGroup.PUT("/current/image/:key", v1.PutUserImage)
		// v1UserGroup.POST("/file/image/:key", v1.PostUserFileImage)
		v1UsersGroup.DELETE("/current/image", v1.DeleteUserImage)

		v1UsersGroup.PUT("/avatar", v1.PutUserAvatar)
		v1UsersGroup.GET("/avatar", v1.GetUserAvatar)

		// Active Sessions — surfaces every active refresh family for the
		// caller's user_id. The DELETE marks one revoked; the access
		// token remains valid until its exp (3h max) but can no longer
		// refresh, so the session effectively dies on next rotation.
		v1UsersGroup.GET("/sessions", v1.GetUserSessions)
		v1UsersGroup.DELETE("/sessions/:id", v1.DeleteUserSession)

		// Wave 28: relocated from public root. The handler enforces
		// admin via PermUserManage so non-admins cannot enumerate
		// either.
		v1UsersGroup.GET("/name", v1.GetUserAllUsername)

		v1UsersGroup.DELETE("/:id", v1.DeleteUser)
		v1UsersGroup.GET("/:username", v1.GetUserInfoByUsername)
		v1UsersGroup.DELETE("", v1.DeleteUserAll)

		// Wave 4b: RBAC administration. Admin-only via in-handler checks.
		v1UsersGroup.GET("/roles", v1.ListRoles)
		v1UsersGroup.PUT("/:id/role", v1.PutUserRole)
	}

	// Wave 4d: OAuth2 / OIDC provider endpoints.
	//
	// Discovery + JWKS live at root-level /.well-known paths (outside the
	// /v1 JWT group) so unauthenticated clients can fetch them. The JWKS
	// endpoint is already mounted by main.go; here we only add discovery.
	//
	// /authorize requires a logged-in user (JWT) so it stays inside
	// v1Group where the JWT middleware is already applied. /token and
	// /userinfo use their own auth paths: /token uses client_id+secret
	// (no JWT), /userinfo uses the access token which IS a JWT so it
	// naturally works inside the group.
	e.GET("/.well-known/openid-configuration", v1.GetOAuth2Discovery)

	// /token uses client_id+secret auth — must be unauthenticated.
	v1Group.POST("/oauth2/token", v1.PostOAuth2Token)

	// /authorize and /userinfo are JWT-protected — they identify the
	// caller via the access token, not via a client-supplied user_id
	// header. Without this group an attacker could mint codes for any
	// user simply by setting `user_id: 1` in the request.
	v1OAuth2Protected := v1Group.Group("/oauth2")
	v1OAuth2Protected.Use(echo_middleware.JWTWithConfig(jwtConfig))
	{
		v1OAuth2Protected.GET("/authorize", v1.GetOAuth2Authorize)
		v1OAuth2Protected.GET("/userinfo", v1.GetOAuth2UserInfo)
	}

	// Admin-only OAuth2 client management — MUST be inside the JWT
	// middleware so RoleFromContext returns the actual authenticated
	// role. Without JWT middleware, the in-handler RBAC guard would
	// always see RoleGuest (no token parsed), accidentally blocking
	// legitimate admins and providing no real security (the guard
	// relies on accurate role data from the JWT).
	v1OAuth2Admin := v1UsersGroup.Group("/oauth2")
	{
		v1OAuth2Admin.GET("/clients", v1.ListOAuth2Clients)
		v1OAuth2Admin.POST("/clients", v1.PostOAuth2RegisterClient)
		v1OAuth2Admin.DELETE("/clients/:id", v1.DeleteOAuth2Client)
	}

	return e
}
