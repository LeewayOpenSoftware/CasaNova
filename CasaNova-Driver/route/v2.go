package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/config"
	v2 "github.com/LeewayOpenSoftware/CasaNova-Driver/route/v2"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// Route prefixes registered with the gateway. Kept generic per naming policy.
const (
	V2APIPath = "/v2/driver"
	V2DocPath = "/doc/v2/driver"
)

// baseMiddleware installs CORS, gzip, a query-stripping logger, and the shared
// JWT auth that is skipped for loopback (gateway-proxied) requests — identical
// to CasaNova-LocalStorage so behaviour matches across services.
func baseMiddleware(e *echo.Echo) {
	e.Use(echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	}))
	e.Use(echo_middleware.Gzip())
	e.Use(echo_middleware.Recover())
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))
}

func jwtConfig() echo_middleware.JWTConfig {
	return echo_middleware.JWTConfig{
		// Skip JWT ONLY for tokenless loopback (genuine internal service hops).
		// A presented token is ALWAYS validated so the role is set on the
		// context — critical because the gateway reverse-proxies LAN traffic
		// over 127.0.0.1, so every proxied request looks loopback; blanket-
		// skipping it would drop a real user's token and leave RoleFromContext
		// at guest, which the per-handler requireConfig/requireRead gates
		// (route/v2/rbac_gate.go) then evaluate. Tokenless callers fail closed.
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes sysfs tuning: cpu governor, swappiness, io scheduler, sysctl, zram.
		//
		// The allowlist is nil: no route here has a pre-session contract. A peer
		// service presents internalauth.Header(), which authskip.For checks first
		// — it must be checked in the SKIPPER because the JWT middleware runs
		// before any handler-level gate could see it.
		Skipper: authskip.For(nil),
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
				return external.GetPublicKey(config.CommonInfo.RuntimePath)
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
				// Strip the "Bearer " scheme — with custom lookup funcs echo does
				// NOT strip it, so jwt.Validate would otherwise receive
				// "Bearer <jwt>" and fail. A raw token (no prefix) passes through.
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				return []string{strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))}, nil
			},
		},
	}
}

func InitV2Router() http.Handler {
	e := echo.New()
	baseMiddleware(e)

	g := e.Group(V2APIPath)
	g.Use(echo_middleware.JWTWithConfig(jwtConfig()))

	v2.NewDriverAPI().RegisterUnder(g)
	return e
}
