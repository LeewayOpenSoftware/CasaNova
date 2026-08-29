package route

import (
	"context"
	"crypto/ecdsa"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/config"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// unixSocketCtxKey is the request-context key used to mark requests
// that arrived on the unix-domain socket listener (i.e. trusted local
// publishers like AppManagement / LocalStorage that hold a file
// descriptor on /run/casanova/internal/message-bus.sock). The JWT middleware checks
// this key to skip auth for those requests.
//
// Wave 32: previously the skipper trusted `c.Request().Host == "unix"`
// to identify the unix-socket path. That header is set by the unix
// client and is fully attacker-controllable from any TCP caller — a
// loopback HTTP request with `Host: unix` bypassed JWT entirely on
// the loopback listener. The fix moves the trust boundary off the
// HTTP layer and onto the actual listener: the socket server's
// ConnContext stamps every connection with this key, and only
// connections that arrived through that listener carry the bypass.
type unixSocketCtxKeyT struct{}

var unixSocketCtxKey = unixSocketCtxKeyT{}

// UnixSocketConnContext is the http.Server.ConnContext value to wire
// onto the unix-socket http.Server in main.go. It tags every accepted
// connection's context as trusted-local, so the JWT middleware can
// skip auth without trusting the spoofable Host header.
func UnixSocketConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, unixSocketCtxKey, true)
}

// isFromUnixSocket reports whether the inbound request was accepted on
// the unix socket listener (and therefore should bypass JWT).
func isFromUnixSocket(c echo.Context) bool {
	v, ok := c.Request().Context().Value(unixSocketCtxKey).(bool)
	return ok && v
}

func NewAPIRouter(swagger *openapi3.T, services *service.Services) (http.Handler, error) {
	apiRoute := NewAPIRoute(services)

	apiPath, err := getAPIPath(getSwaggerURL(swagger))
	if err != nil {
		return nil, err
	}

	healthPath := apiPath + HealthPathSuffix

	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: false, // Cannot use credentials with wildcard origin per CORS spec
	})))

	// NO GZIP ON THE UNIX SOCKET.
	//
	// This process serves TWO listeners through ONE echo handler: a loopback
	// TCP listener the gateway proxies for browsers, and the 0600 unix socket
	// at /run/casanova/internal/message-bus.sock that peer services publish
	// through. Every middleware written for the browser therefore also ran for
	// internal traffic — including compressing a response that travels between
	// two processes on the same kernel, where it cannot save a single byte of
	// anything.
	//
	// Measured on a typical publish (small JSON, client advertising gzip):
	//
	//	with gzip      47 us, 8,924 B/op, 32 allocs
	//	without gzip    8.5 us, 7,129 B/op, 26 allocs
	//
	// So 38 us and ~1.6 KB per publish, a 5.5x latency multiplier on the
	// hottest inter-service path in the box. (An earlier note claimed 814 KB
	// per publish from the deflate window; that does not reproduce — echo
	// v4.12 pools its gzip writers, so the compressor state is amortised. The
	// cost is real but it is microseconds, not megabytes.)
	//
	// Browsers keep compression: the skipper only fires for connections the
	// socket listener accepted, proven by the ConnContext stamp rather than by
	// anything the caller can set. isFromUnixSocket is the same predicate the
	// JWT middleware trusts for exactly that reason — a Host header saying
	// "unix" used to be enough, and it was spoofable from any TCP caller.
	e.Use(echo_middleware.GzipWithConfig(echo_middleware.GzipConfig{
		Skipper: isFromUnixSocket,
	}))
	e.Use(echo_middleware.Recover())
	// Path-only logger to avoid leaking ?token=<JWT> from WebSocket
	// upgrade requests into stdout/journald.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			// Trusted internal publishers (AppManagement, LocalStorage,
			// etc.) reach the bus via the unix socket at
			// /run/casanova/internal/message-bus.sock. We identify those connections by
			// the ConnContext value the unix-socket http.Server stamps
			// onto every accepted conn — NOT by the Host header, which
			// any local TCP client can forge to "unix" to bypass auth.
			if isFromUnixSocket(c) {
				return true
			}

			// NOTE: the previous implementation also skipped JWT for
			// any loopback caller. That made the publish endpoint
			// unauthenticated for any local process — a compromised
			// container sharing the host network (or any binary
			// running on 127.0.0.1) could POST to
			// /v2/message_bus/event/{sourceID}/{name} and forge
			// events with arbitrary sourceIDs, polluting audit
			// streams and UI notifications. The loopback skip is
			// gone; TCP callers must present a valid JWT just like
			// remote callers do.

			// WebSocket connections must authenticate via ?token= query param.
			// No blanket skip — WS endpoints are as sensitive as REST.
			if c.Request().Method == echo.GET && c.Request().Header.Get(echo.HeaderUpgrade) == "websocket" {
				token := c.QueryParam("token")
				if token != "" {
					// Inject token into Authorization header so ParseTokenFunc picks it up.
					c.Request().Header.Set(echo.HeaderAuthorization, "Bearer "+token)
					return false // do NOT skip — let JWT middleware validate
				}
				// No token at all → reject (ParseTokenFunc will return 401)
				return false
			}

			return false
		},
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
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				// Strip the "Bearer " scheme — echo does NOT strip it for custom
				// lookup funcs, so jwt.Validate would otherwise get "Bearer <jwt>"
				// and 401 every TCP/websocket caller (the UI's live event stream).
				// The websocket branch above already injects "Bearer "+token, so
				// this is required for WS auth to work. Case-insensitive per RFC 6750.
				h := c.Request().Header.Get(echo.HeaderAuthorization)
				if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
					h = h[7:]
				}
				return []string{strings.TrimSpace(h)}, nil
			},
		},
	}))

	e.Use(middleware.OapiRequestValidatorWithOptions(swagger, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		// The validator matches against the spec EMBEDDED in codegen
		// (codegen.GetSwagger), not against api/message_bus/openapi.yaml
		// — editing the yaml alone would not teach it about a new path,
		// so an unlisted route would 404 here before reaching its
		// handler. The liveness endpoint is registered by hand below
		// (it must keep working when the generated surface is
		// regenerated), hence an explicit skip for exactly that path.
		Skipper: func(c echo.Context) bool {
			return c.Request().URL.Path == healthPath
		},
	}))

	// Registered before the generated handlers so the skipper above and
	// this route can never drift apart in path. Auth still applies: the
	// JWT middleware runs first, so through the gateway this needs a
	// token, and over the unix socket it does not.
	e.GET(healthPath, apiRoute.Health)

	codegen.RegisterHandlersWithBaseURL(e, apiRoute, apiPath)

	return e, nil
}

func NewDocRouter(swagger *openapi3.T, docHTML string, docYAML string) (http.Handler, error) {
	apiPath, err := getAPIPath(getSwaggerURL(swagger))
	if err != nil {
		return nil, err
	}

	docPath := "/doc" + apiPath

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == docPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == docPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}), nil
}

func getSwaggerURL(swagger *openapi3.T) string {
	return swagger.Servers[0].URL
}

func getAPIPath(swaggerURL string) (string, error) {
	u, err := url.Parse(swaggerURL)
	if err != nil {
		return "", err
	}

	return strings.TrimRight(u.Path, "/"), nil
}
