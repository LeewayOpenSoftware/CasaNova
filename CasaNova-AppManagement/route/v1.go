package route

import (
	"crypto/ecdsa"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/authskip"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/route/v1"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

const (
	V1APIPath = "/v1/app_management"
	V1DocPath = "/v1doc" + V1APIPath
)

func InitV1Router() http.Handler {
	e := echo.New()
	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: false,
	})))

	e.Use(echo_middleware.Gzip())
	e.Use(echo_middleware.Recover())
	// Path-only logger to avoid leaking ?token=<JWT> from WebSocket
	// upgrade requests into stdout/journald.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	v1Group := e.Group("/v1")

	v1Group.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes compose/app CRUD, container control and backup routes.
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
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				if h := c.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
					// Strip the "Bearer " auth scheme so ParseTokenFunc
					// receives the bare JWT. echo does NOT strip the scheme
					// for custom extractors, and jwt.Validate cannot parse
					// "Bearer <jwt>". Case-insensitive per RFC 6750 §2.1.
					if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
						h = h[7:]
					}
					return []string{h}, nil
				}
				return []string{c.QueryParam("token")}, nil
			},
		},
	}))
	{
		v1ContainerGroup := v1Group.Group("/container")
		v1ContainerGroup.Use()
		{

			// v1ContainerGroup.GET("", v1.MyAppList) ///my/list
			v1ContainerGroup.GET("/usage", v1.AppUsageList)

			// Container lifecycle — registered BEFORE /:id catch-all so
			// Echo matches these specific paths first.
			v1ContainerGroup.POST("/:id/start", v1.PostContainerStart)
			v1ContainerGroup.POST("/:id/stop", v1.PostContainerStop)
			v1ContainerGroup.POST("/:id/restart", v1.PostContainerRestart)

			v1ContainerGroup.GET("/:id", v1.ContainerUpdateInfo)      ///update/:id/info
			v1ContainerGroup.GET("/:id/compose", v1.ToComposeYAML)    // /app/setting/:id
			v1ContainerGroup.GET("/networks", v1.GetDockerNetworks)   // /app/install/config
			v1ContainerGroup.PUT("/archive/:id", v1.ArchiveContainer) // /container/archive/:id

			// WebSocket endpoints. A browser cannot set headers on a WebSocket
			// handshake, so the UI puts the JWT in ?token=. strictJWT()
			// validates the query token and extracts claims so that
			// the in-handler permission checks see the real authenticated role.
			//
			// These two routes must live in this service: they are shaped
			// "/v1/container/:id/...", the gateway routes on literal prefixes,
			// and this service owns "/v1/container".
			v1ContainerGroup.GET("/:id/terminal", v1.DockerTerminal, strictJWT()) // app/terminal/:id
			v1ContainerGroup.GET("/:id/logs", v1.WsContainerLogs, strictJWT())    // app/logs/:id
			// v1ContainerGroup.POST("", v1.InstallApp)                 // app/install

			v1ContainerGroup.PUT("/:id", v1.UpdateSetting) ///update/:id/setting

			// v1ContainerGroup.PUT("/:id/state", v1.ChangAppState) // /app/state/:id
			v1ContainerGroup.DELETE("/:id", v1.UninstallApp) // app/uninstall/:id

			// v1ContainerGroup.GET("/info", v1.GetDockerDaemonConfiguration)
			// v1ContainerGroup.PUT("/info", v1.PutDockerDaemonConfiguration)
		}

		// Wave 4a: Restic-backed backup schedules.
		// Permission checks live inside each handler (requireBackupRead /
		// requireBackupWrite); loopback is no longer trusted as a bypass.
		v1BackupGroup := v1Group.Group("/backup")
		{
			v1BackupGroup.GET("/schedules", v1.ListBackupSchedules)
			v1BackupGroup.PUT("/schedules", v1.PutBackupSchedule)
			v1BackupGroup.DELETE("/schedules/:app", v1.DeleteBackupSchedule)
			v1BackupGroup.POST("/schedules/:app/run", v1.RunBackupNow)
			v1BackupGroup.GET("/schedules/:app/snapshots", v1.ListSnapshots)
			v1BackupGroup.POST("/schedules/:app/restore", v1.RestoreSnapshot)
		}
	}

	return e
}

// strictJWT validates the caller's token with NO loopback escape hatch, and is
// meant for routes the group-level middleware cannot authenticate.
//
// Historically, the group middleware skipped JWT validation for loopback
// requests that carried no Authorization header, which caused WebSocket
// handshakes (unable to set custom headers) to fail authentication.
// strictJWT validates tokens from both Authorization headers and the
// ?token= query parameter without relying on default skippers.
//
// Mounted per-route, it runs after the group middleware has skipped and reads
// the token from the query string, so RoleFromContext reflects the real user.
// An invalid token now fails as 401 instead of silently degrading to guest.
func strictJWT() echo.MiddlewareFunc {
	return echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
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
				if h := c.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
					// Strip the "Bearer " scheme: echo does not do it for a
					// custom extractor and jwt.Validate cannot parse it.
					if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
						return []string{h[7:]}, nil
					}
					return []string{h}, nil
				}
				return []string{c.QueryParam("token")}, nil
			},
		},
	})
}

func InitV1DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V1DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V1DocPath+"/openapi_v1.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
