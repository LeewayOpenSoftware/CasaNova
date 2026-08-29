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
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/config"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	echomiddleware "github.com/oapi-codegen/echo-middleware"
)

type JobManagement struct{}

var (
	_swagger *openapi3.T

	APIPath string
	DocPath string
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

	APIPath = strings.TrimRight(u.Path, "/")
	DocPath = "/doc" + APIPath
}

func GetRouter() http.Handler {
	hello := NewJobManagement()

	e := echo.New()

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	}))

	e.Use(middleware.Gzip())

	e.Use(middleware.Logger())

	e.Use(echojwt.WithConfig(echojwt.Config{
		// NOTHING here answers without a credential.
		//
		// The previous skipper let a tokenless loopback request through, and
		// the gateway proxies the LAN and the Cloudflare tunnel over loopback —
		// so "tokenless loopback" was the internet. This service creates and
		// runs scheduled tasks with /bin/sh -c AS ROOT. The per-route RBAC
		// guard was supposed to be the second line; it denied with ctx.JSON,
		// which returns nil, so the handler ran anyway. Measured live before
		// this change: GET :85/v2/job_management/tasks with no credential
		// answered 200.
		//
		// The allowlist is nil: no route in this module has a pre-session
		// contract. A peer service that ever needs to reach it presents
		// internalauth.Header(), which authskip.For checks first.
		Skipper: authskip.For(nil),
		ParseTokenFunc: func(c echo.Context, token string) (any, error) {
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
		TokenLookupFuncs: []middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				if h := c.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
					// Strip the "Bearer " auth scheme so ParseTokenFunc
					// receives the bare JWT. echo does NOT strip the scheme
					// for custom extractors, and jwt.Validate cannot parse
					// "Bearer <jwt>" — it would 401 every real request.
					// Case-insensitive per RFC 6750 §2.1.
					if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
						h = h[7:]
					}
					return []string{h}, nil
				}
				return []string{c.QueryParam("token")}, nil
			},
		},
	}))

	// Wave 32: scheduled-task routes are added by RegisterScheduledTaskRoutes
	// AFTER this middleware, but the codegen swagger has zero `/tasks`
	// operations. The OAPI validator returns HTTP 404 for any path that
	// isn't in the spec — which means every CRUD/run endpoint for
	// scheduled tasks 404s before reaching its handler. The entire
	// scheduled-tasks feature was dead from the network. Skip the
	// validator on the user-route prefix so handlers added outside the
	// codegen surface still work. (Loopback also bypasses validation
	// because it never has the right Authorization headers anyway.)
	e.Use(echomiddleware.OapiRequestValidatorWithOptions(_swagger, &echomiddleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		Skipper: func(c echo.Context) bool {
			p := c.Request().URL.Path
			return strings.HasPrefix(p, APIPath+"/tasks")
		},
	}))

	codegen.RegisterHandlersWithBaseURL(e, hello, APIPath)

	// Wave 4e: user-defined scheduled tasks. Routes are added after the
	// codegen handlers so they live alongside /jobs/* without needing a
	// schema regeneration. All permission enforcement is inside each
	// handler; loopback callers bypass it as usual.
	RegisterScheduledTaskRoutes(e, APIPath)

	return e
}

func GetDocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == DocPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}

func NewJobManagement() codegen.ServerInterface {
	return &JobManagement{}
}
