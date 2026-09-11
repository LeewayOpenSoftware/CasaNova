package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	v2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/route/v2"
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
	localStorage := v2.NewLocalStorage()

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
	// parameters (used for WebSocket / SSE auth) never hit journald.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes storage and merge (mergerfs) management.
		//
		// The allowlist is nil: no route here has a pre-session contract. A peer
		// service presents internalauth.Header(), which authskip.For checks first
		// — it must be checked in the SKIPPER because the JWT middleware runs
		// before any handler-level gate could see it.
		Skipper: authskip.For(nil),
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			// claims, code := jwt.Validate(token)
			// if code != common_err.SUCCESS {
			// 	return nil, echo.ErrUnauthorized
			// }
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			// The principal's tokens may have been revoked. Until the shared
			// revocation authority existed this was checked only inside the
			// user service, and only in memory, so a deleted user's token kept
			// working against this service for its full lifetime and survived
			// restarts. Fails closed if the authority cannot be consulted.
			if jwt.RevokedForClaims(claims) {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))
			commonmw.SetRoleInContext(c, claims.Role, claims.ID)

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{v2TokenExtractor},
	}))

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		Skipper: func(c echo.Context) bool {
			// Skip OpenAPI validation for custom (non-codegen) routes.
			p := c.Path()
			return p == V2APIPath+"/storage" || p == V2APIPath+"/standby"
		},
	}))

	codegen.RegisterHandlersWithBaseURL(e, localStorage, V2APIPath)

	// Custom routes (not generated from OpenAPI spec)
	ls := localStorage.(*v2.LocalStorage)
	e.GET(V2APIPath+"/storage", ls.GetStorageRoots)
	e.GET(V2APIPath+"/standby", ls.GetStandby)
	e.PUT(V2APIPath+"/standby", ls.PutStandby)

	return e
}

// v2TokenExtractor reads the bearer token from the Authorization header,
// falling back to ?token= for WebSocket/SSE callers that cannot set a
// header. Named and tested on its own (see v2_token_extractor_test.go)
// because it silently lost that fallback once already: this file's own
// logger-config comment above documents that ?token= is expected here for
// WS/SSE auth, but the inline closure this replaces only ever read the
// header. OLA 3 — same failure mode as CasaNova/route/v1.go's
// wsTokenExtractor was written to prevent, just not yet applied to this
// module.
func v2TokenExtractor(c echo.Context) ([]string, error) {
	h := c.Request().Header.Get(echo.HeaderAuthorization)
	if h == "" {
		return []string{c.QueryParam("token")}, nil
	}
	// Strip the "Bearer " auth scheme so ParseTokenFunc receives the bare
	// JWT. echo does NOT strip the scheme for custom extractors, and
	// jwt.Validate cannot parse "Bearer <jwt>". Case-insensitive per RFC
	// 6750 §2.1.
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		h = h[7:]
	}
	return []string{h}, nil
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
