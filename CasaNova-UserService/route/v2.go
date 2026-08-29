package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	codegen "github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/user_service"
	v1 "github.com/LeewayOpenSoftware/CasaNova-UserService/route/v1"
	v2 "github.com/LeewayOpenSoftware/CasaNova-UserService/route/v2"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

var (
	_swagger *openapi3.T

	V2APIPath string
	V2DocPath string
)

func init() {
	swagger, err := codegen.GetSwagger()
	if err != nil {
		panic(err)
	}

	_swagger = swagger

	u, err := url.Parse(_swagger.Servers[0].URL)
	if err != nil {
		panic(err)
	}

	V2APIPath = strings.TrimRight(u.Path, "/")
	V2DocPath = "/doc" + V2APIPath
}

func InitV2Router() http.Handler {
	UserService := v2.NewUserService()

	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	})))

	e.Use(echo_middleware.Gzip())

	// Custom logger that omits the query string so stray `?token=<jwt>`
	// parameters never hit journald. v1 already does this; v2 needs
	// the same treatment since it sits behind the same gateway and
	// sees the same WebSocket/SSE auth patterns.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes account data; the pre-login routes (login/register/status/refresh) are registered outside this middleware and are unaffected.
		//
		// The allowlist is nil: no route here has a pre-session contract. A peer
		// service presents internalauth.Header(), which authskip.For checks first
		// — it must be checked in the SKIPPER because the JWT middleware runs
		// before any handler-level gate could see it.
		Skipper: authskip.For(nil),
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

			// Reject tokens for deleted users. The shared authority is the
			// durable, cross-service record; the in-memory map only
			// short-circuits it inside this process. Fails closed if the
			// authority cannot be consulted.
			_, revokedLocally := v1.RevokedUsers.Load(strconv.Itoa(claims.ID))
			if revokedLocally || jwt.RevokedForClaims(claims) {
				return nil, echo.ErrUnauthorized
			}

			// Wave 35-U: match v1's role-recovery path so v2 endpoints
			// (e.g. /v2/users/*) don't silently 403 for sessions that
			// were issued before the login handler was fixed. See
			// v1.go's ParseTokenFunc comment for the full rationale.
			role := claims.Role
			// Skip the DB re-read for OAuth2-delegated tokens (minted "guest"
			// on purpose to de-privilege a relying party). See v1.go.
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
				h := c.Request().Header.Get(echo.HeaderAuthorization)
				// Strip the "Bearer " auth scheme so ParseTokenFunc receives
				// the bare JWT. echo does NOT strip the scheme for custom
				// extractors, and jwt.Validate cannot parse "Bearer <jwt>".
				// Case-insensitive per RFC 6750 §2.1. Mirrors v1.go:161-172.
				// (Previously latent: the blanket loopback skip meant the JWT
				// middleware never ran on the Gateway-proxied path, so this
				// extractor was never exercised for real traffic.)
				if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
					h = h[7:]
				}
				return []string{h}, nil
			},
		},
	}))

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}))

	codegen.RegisterHandlersWithBaseURL(e, UserService, V2APIPath)

	return e
}

func InitV2DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V2DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V2DocPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
