package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"

	v2Route "github.com/LeewayOpenSoftware/CasaNova-AppManagement/route/v2"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
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

// v2TokenExtractor is the ONE place v2 decides where a token may come from.
//
// It is a named function rather than a closure inline in the middleware config
// so a test can exercise THE PRODUCTION EXTRACTOR instead of a copy of it. The
// SSE contract it encodes fails silently when broken — no build error, no test
// failure, the install progress bar simply never fills — so it needs a test
// that runs the real thing.
func v2TokenExtractor(c echo.Context) ([]string, error) {
	if h := c.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
		// Strip the "Bearer " auth scheme so ParseTokenFunc
		// receives the bare JWT. echo does NOT strip the scheme
		// for custom extractors, and jwt.Validate cannot parse
		// "Bearer <jwt>". Case-insensitive per RFC 6750 §2.1.
		if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
			return []string{h[7:]}, nil
		}
		return []string{h}, nil
	}

	// FALL BACK TO ?token= — the v1 extractor already does this,
	// and v2 must too before its skipper can be tightened.
	//
	// GET /v2/app_management/compose/install/:id/progress is a
	// Server-Sent Events stream, and EventSource cannot set request
	// headers, so the browser has no way to present a bearer token
	// (CasaNova-UI-v2/src/api/containers.ts:773). Historically that stream
	// worked only because the loopback skipper waved it through
	// unauthenticated. Supporting ?token= query parameter extraction allows
	// SSE streams to authenticate now that authskip.For(nil) closes the skipper.
	//
	// A token in a query string is accepted here because the alternative for
	// browser SSE handshakes is no authentication at all.
	return []string{c.QueryParam("token")}, nil
}

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
	appManagement := v2Route.NewAppManagement()

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

	// Custom logger that omits the query string. The SSE progress
	// endpoint receives `?token=<jwt>` because EventSource cannot
	// set custom headers, and the default Logger() format includes
	// the full URI — every install would leak a live access token
	// to journald and any log shipper. ${path} gives us the routed
	// path without query params.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes the compose API measured serving 47 host volume paths and 46 environment variables, six named like credentials, to an anonymous caller.
		//
		// The allowlist is nil: no route here has a pre-session contract. A peer
		// service presents internalauth.Header(), which authskip.For checks first
		// — it must be checked in the SKIPPER because the JWT middleware runs
		// before any handler-level gate could see it.
		Skipper: authskip.For(nil),
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
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

	// e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
	// 	return func(c echo.Context) error {
	// 		switch c.Request().Header.Get(echo.HeaderContentType) {
	// 		case common.MIMEApplicationYAML: // in case request contains a compose content in YAML
	// 			return middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
	// 				Options: openapi3filter.Options{
	// 					AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
	// 					// ExcludeRequestBody:  true,
	// 					// ExcludeResponseBody: true,
	// 				},
	// 			})(next)(c)

	// 		default:
	// 			return middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
	// 				Options: openapi3filter.Options{
	// 					AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
	// 				},
	// 			})(next)(c)
	// 		}
	// 	}
	// })

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		Skipper: func(c echo.Context) bool {
			p := c.Path()
			return strings.HasPrefix(p, V2APIPath+"/stores") ||
				strings.HasPrefix(p, V2APIPath+"/compose/import") ||
				strings.HasPrefix(p, V2APIPath+"/compose/install/") ||
				strings.HasSuffix(p, "/check_updates") ||
				strings.HasSuffix(p, "/repair") ||
				strings.HasPrefix(p, V2APIPath+"/dockerhub")
		},
	}))

	codegen.RegisterHandlersWithBaseURL(e, appManagement, V2APIPath)

	// ── Store management routes (outside OpenAPI codegen) ────────────
	// These endpoints manage the multi-store registry (stores.json).
	// They skip the OAPI request validator because they aren't in the
	// generated OpenAPI spec. We cast to the concrete type to access
	// the store methods that aren't part of the codegen interface.
	storeMgmt := appManagement.(*v2Route.AppManagement)
	stores := e.Group(V2APIPath + "/stores")
	stores.GET("", storeMgmt.StoreList)
	stores.POST("", storeMgmt.StoreRegister)
	stores.PUT("/:id", storeMgmt.StoreUpdate)
	stores.DELETE("/:id", storeMgmt.StoreUnregister)
	stores.POST("/:id/sync", storeMgmt.StoreSync)

	// ── Compose import + progress routes (outside OpenAPI codegen) ───
	compose := e.Group(V2APIPath + "/compose")
	compose.POST("/import", storeMgmt.PostComposeImport)
	compose.POST("/import-cli", storeMgmt.PostComposeImportCli)
	compose.POST("/:id/check_updates", storeMgmt.PostComposeCheckUpdates)
	// Repair — reconcile an app orphaned from its compose project (loads from
	// the on-disk compose file, so it works even when the app is not in the
	// project list, i.e. the "installed but offline / not found" state).
	compose.POST("/:id/repair", storeMgmt.RepairComposeApp)

	// Docker Hub image search (feeds the install wizard). Outside OpenAPI codegen.
	e.GET(V2APIPath+"/dockerhub/search", storeMgmt.SearchDockerHub)
	compose.GET("/install/:id/progress", storeMgmt.GetComposeInstallProgress)

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
