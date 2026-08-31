package route

import (
	"crypto/ecdsa"
	"errors"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/config"
	v2 "github.com/LeewayOpenSoftware/CasaNova-Virt/route/v2"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/service"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// V2APIPath is the gateway-facing base path for the virtualization API.
//
// The ZimaOS oracle mounted its handlers under a base URL via oapi-codegen's
// RegisterHandlersWithBaseURL; the exact prefix was not recoverable from the
// stripped binary (types+server codegen, no embedded spec; the VM UI was not
// present in the shipped rootfs). We use the CasaOS v2 convention here. The
// service registers this prefix with the gateway on startup, so changing it is
// a single-line edit.
const V2APIPath = "/v2/virtualization"

// InitV2Router builds the echo handler for the virtualization API. Middleware
// (CORS, gzip, structured logging, loopback-skipping JWT) mirrors
// CasaNova-LocalStorage/route.InitV2Router.
func InitV2Router() http.Handler {
	h := v2.NewVirtHandler(service.MyService)

	e := echo.New()
	e.HideBanner = true

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

	g := e.Group(V2APIPath)

	// Skip JWT ONLY for tokenless loopback (genuine internal service hops).
	// A presented token is ALWAYS validated so the role is set on the context
	// — critical because the gateway reverse-proxies LAN traffic over
	// 127.0.0.1, so every proxied request looks loopback; blanket-skipping it
	// would drop a real user's token and leave RoleFromContext at guest, which
	// the per-handler requireConfig/requireRead gates (route/v2/rbac_gate.go)
	// then evaluate. Tokenless callers fail closed to guest.
	g.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// This skipper already took more care than its siblings — it refused to
		// skip when a ?token= was present, so the WebSocket VNC console would
		// get its role — but the base condition was still "tokenless loopback",
		// and the gateway reverse-proxies the LAN and the Cloudflare tunnel over
		// loopback. So the careful part protected authenticated callers while
		// the rest admitted the internet to libvirt VM control.
		//
		// The ?token= handling is preserved by the extractor below, which reads
		// the query as a fallback; it does not belong in the skipper, whose only
		// job is to say who may proceed with NO credential. Answer here: a peer
		// service holding the internal secret, and nobody else.
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
				if auth == "" {
					return nil, errors.New("no authorization header")
				}
				return []string{strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))}, nil
			},
			func(c echo.Context) ([]string, error) {
				// WebSocket clients (the VNC console) can't set an Authorization
				// header, so they carry the JWT in ?token=, matching the main
				// service's WS auth convention. Tried only when there's no header.
				t := c.QueryParam("token")
				if t == "" {
					return nil, errors.New("no token query param")
				}
				return []string{t}, nil
			},
		},
	}))

	// ---- host / dashboard ----
	g.GET("/info", h.GetInfo)
	g.GET("/host", h.GetHost)

	// ---- domains (VMs) ----
	g.GET("/domain", h.ListDomains)
	g.POST("/domain", h.CreateDomain)
	g.GET("/domain/:name", h.GetDomain)
	g.DELETE("/domain/:name", h.DeleteDomain)
	g.GET("/domain/:name/status", h.GetDomainStatus)
	g.PUT("/domain/:name/status", h.SetDomainStatus)
	g.POST("/domain/:name/autostart", h.SetAutostart)
	g.GET("/domain/:name/vnc", h.GetConsole)
	g.GET("/domain/:name/vnc/ws", h.VncWebSocket) // websockify bridge for noVNC

	// ---- ISO images (for the create-VM wizard) ----
	g.GET("/iso", h.ListISOs)
	g.POST("/iso/download", h.DownloadISO)
	// Registered before "/iso/:name" so "downloads" is not swallowed as a
	// filename by the parameterised route.
	g.GET("/iso/downloads", h.ListISODownloads)
	g.DELETE("/iso/downloads/:name", h.CancelISODownload)
	g.DELETE("/iso/:name", h.DeleteISO)

	// ---- storage ----
	g.GET("/pool", h.ListPools)
	g.GET("/pool/:name/volume", h.ListVolumes)
	g.POST("/pool/:name/volume", h.CreateVolume)
	g.DELETE("/pool/:name/volume/:volume", h.DeleteVolume)

	return e
}
