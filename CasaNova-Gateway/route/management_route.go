package route

import (
	"crypto/ecdsa"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/loopback"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// isLoopbackTarget validates that a route target URL uses http/https and
// is bound to a literal loopback IP. Wave 34-X residual: the previous
// implementation accepted any hostname whose `net.LookupHost` *currently*
// resolved to loopback. That is a textbook DNS-rebinding bypass: an
// attacker registers `evil.example.com` with a 1-second-TTL A record
// pointing at 127.0.0.1, the gateway validates and accepts the route,
// and on the very next dial the resolver returns 169.254.169.254 (or
// any internal address). The reverse-proxy step happens long after the
// validation snapshot — there is no second check. Locking the field to
// a literal IP that is itself loopback closes the entire window: the IP
// in the URL *is* the address the gateway will dial, no name resolution
// happens, and rebinding is impossible.
func isLoopbackTarget(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

type ManagementRoute struct {
	management *service.Management
	gateway    *GatewayRoute
}

func NewManagementRoute(management *service.Management) *ManagementRoute {
	return &ManagementRoute{
		management: management,
	}
}

// WithGateway wires the GatewayRoute pointer so the gateway
// management endpoints can reach the gateway without exporting
// it on the service layer. Called after both routes are constructed.
func (m *ManagementRoute) WithGateway(g *GatewayRoute) *ManagementRoute {
	m.gateway = g
	return m
}

func (m *ManagementRoute) GetRoute() http.Handler {
	e := echo.New()

	// Wave 31: NO CORS middleware on the management API.
	//
	// This Echo instance is only ever bound to localhost:0 by main.go's
	// fx hook — it's an inter-service control plane consumed by other
	// CasaNova daemons via direct HTTP, never by browsers. The previous
	// `AllowOrigins: []string{"*"}` made the entire route table
	// (/v1/gateway/routes leaks the internal service topology) and
	// gateway port readable cross-origin from any website the operator
	// happened to visit, because the loopback skipper bypasses JWT for
	// any connection whose RemoteAddr is 127.0.0.1 — and a request
	// originated by a browser running on the same host *does* have a
	// loopback RemoteAddr. With wildcard CORS, the response was readable
	// to JavaScript on attacker.com. Dropping CORS entirely is correct
	// because no legitimate caller is a browser.
	e.Use(echo_middleware.Gzip())

	e.GET("/ping", func(ctx echo.Context) error {
		return ctx.JSON(http.StatusOK, echo.Map{
			"message": "pong from management service",
		})
	})

	m.buildV1Group(e)

	return e
}

func (m *ManagementRoute) buildV1Group(e *echo.Echo) {
	v1Group := e.Group("/v1")

	v1Group.Use()
	{
		m.buildV1RouteGroup(v1Group)
	}
}

// managementJWTConfig is the JWT middleware applied to every gateway
// management endpoint. POST/PUT routes previously declared it inline and
// GET routes had no auth at all — meaning GET /routes leaked the entire
// internal service topology to any process on localhost, and GET /port
// leaked the active listen port. Loopback is still skipped because local
// CasaNova services rely on unauthenticated inter-service calls.
//
// Wave 31: tokens are taken from the Authorization header ONLY. The
// previous fallback to ?token=… in the query string ended up in every
// HTTP access log, every reverse-proxy log, and the operator's browser
// history if they ever pasted such a URL. Refresh tokens with a 7-day
// TTL leaked through this surface used to keep the box compromised long
// after the operator thought they had rotated credentials. There is no
// legitimate caller for the management API that cannot set an
// Authorization header.
func (m *ManagementRoute) managementJWTConfig() echo_middleware.JWTConfig {
	return echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			return loopback.IsLoopback(c)
		},
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
				return external.GetPublicKey(m.management.State.GetRuntimePath())
			})
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
				h := c.Request().Header.Get(echo.HeaderAuthorization)
				if h == "" {
					return nil, echo_middleware.ErrJWTMissing
				}
				return []string{h}, nil
			},
		},
	}
}

func (m *ManagementRoute) buildV1RouteGroup(v1Group *echo.Group) {
	v1GatewayGroup := v1Group.Group("/gateway")

	// Apply JWT to the whole group so GET endpoints get the same treatment
	// as POST/PUT. The loopback skipper still allows local service-to-service
	// calls to go through unauthenticated.
	v1GatewayGroup.Use(echo_middleware.JWTWithConfig(m.managementJWTConfig()))
	{
		v1GatewayGroup.GET("/routes", func(ctx echo.Context) error {
			return ctx.JSON(http.StatusOK, m.management.GetRoutes())
		})

		v1GatewayGroup.POST("/routes",
			func(ctx echo.Context) error {
				var route *model.Route
				err := ctx.Bind(&route)
				if err != nil {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: err.Error(),
					})
				}

				if !isLoopbackTarget(route.Target) {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: "route target must use http/https and resolve to a loopback address",
					})
				}

				if err := m.management.CreateRoute(route); err != nil {
					return ctx.JSON(http.StatusInternalServerError, model.Result{
						Success: common_err.SERVICE_ERROR,
						Message: err.Error(),
					})
				}

				return ctx.NoContent(http.StatusCreated)
			})
		// Wave 31: previous code wired a *second*, route-specific JWT
		// middleware on top of the group's managementJWTConfig — same
		// loopback skipper, same Validate(), same query-param token
		// fallback. The duplicate ran the same parse twice on every
		// non-loopback request and was the only place still pulling
		// the JWT from `?token=…`. Group middleware is sufficient.

		v1GatewayGroup.GET("/port", func(ctx echo.Context) error {
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.GetGatewayPort(),
			})
		})

		// Wave 4c: TLS settings (HTTPS + Let's Encrypt).
		// Admin-only via the group JWT + an in-handler role check for
		// defense-in-depth. Loopback callers bypass JWT and are trusted.
		v1GatewayGroup.GET("/tls", func(ctx echo.Context) error {
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.State.GetTLS(),
			})
		})
		v1GatewayGroup.PUT("/tls", func(ctx echo.Context) error {
			// In-handler role check: loopback bypasses, otherwise require
			// PermSystemConfig (admin). The TLS config controls the entire
			// inbound surface, so only admins may touch it.
			if !loopback.IsLoopback(ctx) {
				if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermSystemConfig) {
					return ctx.JSON(http.StatusForbidden, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: "admin role required",
					})
				}
			}
			var settings service.TLSSettings
			if err := ctx.Bind(&settings); err != nil {
				return ctx.JSON(http.StatusBadRequest, model.Result{
					Success: common_err.CLIENT_ERROR,
					Message: err.Error(),
				})
			}
			// Validate domains: non-empty, reasonable length, no path traversal.
			for _, d := range settings.Domains {
				if d == "" || len(d) > 253 || strings.Contains(d, "..") || strings.ContainsAny(d, " /\\") {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: "invalid domain: " + d,
					})
				}
			}
			// Let's Encrypt needs a contact email when domains are set.
			if settings.Enabled && len(settings.Domains) > 0 {
				if settings.Email == "" || !strings.Contains(settings.Email, "@") || len(settings.Email) < 5 {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: "a valid email is required for Let's Encrypt domains",
					})
				}
			}
			if err := m.management.State.SetTLS(settings); err != nil {
				return ctx.JSON(http.StatusInternalServerError, model.Result{
					Success: common_err.SERVICE_ERROR,
					Message: err.Error(),
				})
			}
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    settings,
			})
		})

		v1GatewayGroup.PUT("/port",
			func(ctx echo.Context) error {
				var request *model.ChangePortRequest

				if err := ctx.Bind(&request); err != nil {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: err.Error(),
					})
				}

				portNum, err := strconv.Atoi(request.Port)
				if err != nil || portNum < 1 || portNum > 65535 {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: "port must be a number between 1 and 65535",
					})
				}

				if err := m.management.SetGatewayPort(request.Port); err != nil {
					return ctx.JSON(http.StatusInternalServerError, model.Result{
						Success: common_err.SERVICE_ERROR,
						Message: err.Error(),
					})
				}

				return ctx.JSON(http.StatusOK, model.Result{
					Success: common_err.SUCCESS,
					Message: common_err.GetMsg(common_err.SUCCESS),
				})
			})
		// Wave 31: see /routes — the route-specific JWT middleware that
		// used to wrap PUT /port has been removed. Group middleware
		// handles authentication and no longer accepts ?token=….

		// Global IP allow-list. Persisted via the management service so
		// the gateway middleware can keep an in-memory copy and the file
		// survives gateway restarts. CIDR validation happens both here
		// (fail fast) and in SetWhitelist (single source of truth).
		v1GatewayGroup.GET("/whitelist", func(ctx echo.Context) error {
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.GetWhitelist(),
			})
		})
		v1GatewayGroup.POST("/whitelist", func(ctx echo.Context) error {
			var body struct {
				CIDR string `json:"cidr"`
			}
			if err := ctx.Bind(&body); err != nil {
				return ctx.JSON(http.StatusBadRequest, model.Result{
					Success: common_err.CLIENT_ERROR,
					Message: err.Error(),
				})
			}
			if err := m.management.AddWhitelist(body.CIDR); err != nil {
				return ctx.JSON(http.StatusBadRequest, model.Result{
					Success: common_err.CLIENT_ERROR,
					Message: err.Error(),
				})
			}
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.GetWhitelist(),
			})
		})
		// The SmartShield endpoints were removed together with the subsystem
		// they controlled. Two certifiers rejected it: it derived a client
		// identity from X-Forwarded-For and then banned it, so any LAN device
		// or container could name an arbitrary remote victim. Its two tuning
		// knobs also had no reachable configuration path, and making it govern
		// tunnel traffic required hand-editing a file over SSH.
		//
		// Rate limiting keyed on the observed socket peer is still worth
		// having. It returns as its own change, with its own certification.

		v1GatewayGroup.DELETE("/whitelist", func(ctx echo.Context) error {
			cidr := ctx.QueryParam("cidr")
			if cidr == "" {
				return ctx.JSON(http.StatusBadRequest, model.Result{
					Success: common_err.CLIENT_ERROR,
					Message: "cidr query param required",
				})
			}
			if err := m.management.RemoveWhitelist(cidr); err != nil {
				return ctx.JSON(http.StatusBadRequest, model.Result{
					Success: common_err.CLIENT_ERROR,
					Message: err.Error(),
				})
			}
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.GetWhitelist(),
			})
		})
	}
}
