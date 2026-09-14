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
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	v1 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/route/v1"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

func InitV1Router() http.Handler {
	// check if environment variable is set
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
	e.Use(echo_middleware.Recover())
	// Custom logger that omits the query string so stray `?token=<jwt>`
	// parameters (WebSocket / SSE auth) never hit journald.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	// DISABLED ON PURPOSE — do not re-enable without fixing recover.go first.
	//
	// GetRecoverStorage builds its mount point as "/mnt/" + username, where
	// username is what the OAuth PROVIDER returned for the account
	// (route/v1/recover.go:96 and :99, after strings.Split(username, "@")[0]).
	// Nothing checks it for separators or traversal elements, and the one call
	// that looked like it would — fileutil.NameAccumulation — is itself
	// commented out on the line above it. A provider-supplied identity
	// containing "../" therefore places a mount outside /mnt.
	//
	// CasaNova/pkg/oauthflow/ledger.go is where that was solved for the other
	// OAuth flow: SanitizeRemoteName reduces a provider identity to
	// [A-Za-z0-9_-] and MountPointFor refuses anything not landing directly
	// under MountRoot. This route needs the same treatment before it comes
	// back; commenting it out is what is holding the line today, and a line
	// commented without a reason is one housekeeping pass away from returning.
	// r.GET("/v1/recover/:type", v1.GetRecoverStorage)
	v1Group := e.Group("/v1")

	v1Group.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		// Nothing here answers without a credential.
		//
		// The previous skipper passed any tokenless request whose socket peer was
		// 127.0.0.1 — which is EVERY request, because the gateway reverse-proxies
		// the LAN and the Cloudflare tunnel over loopback. This router exposes disk, partition and mount operations.
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
		v1DisksGroup := v1Group.Group("/disks")
		v1DisksGroup.Use()
		{

			v1DisksGroup.GET("", v1.GetDiskList)
			v1DisksGroup.GET("/usb", v1.GetDisksUSBList)
			v1DisksGroup.DELETE("/usb", v1.DeleteDiskUSB)
			v1DisksGroup.DELETE("", v1.DeleteDisksUmount)
			v1DisksGroup.GET("/size", v1.GetDiskSize)
		}

		v1StorageGroup := v1Group.Group("/storage")
		v1StorageGroup.Use()
		{
			v1StorageGroup.POST("", v1.PostAddStorage)

			v1StorageGroup.PUT("", v1.PutFormatStorage)

			v1StorageGroup.DELETE("", v1.DeleteStorage)
			v1StorageGroup.GET("", v1.GetStorageList)

			// Per-disk operations beyond format/mount: online resize
			// (ext4/3/2/xfs/btrfs/f2fs) and a quick benchmark (fio
			// preferred, dd fallback).
			v1StorageGroup.POST("/resize", v1.PostResizeDisk)
			v1StorageGroup.POST("/benchmark", v1.PostBenchmarkDisk)
		}
		v1CloudGroup := v1Group.Group("/cloud")
		v1CloudGroup.Use()
		{
			v1CloudGroup.GET("", v1.ListStorages)
			v1CloudGroup.DELETE("", v1.UmountStorage)
		}
		v1DriverGroup := v1Group.Group("/driver")
		v1DriverGroup.Use()
		{
			v1DriverGroup.GET("", v1.ListDriverInfo)
		}
		v1USBGroup := v1Group.Group("/usb")
		v1USBGroup.Use()
		{
			v1USBGroup.PUT("/usb-auto-mount", v1.PutSystemUSBAutoMount) ///sys/usb/:status
			v1USBGroup.GET("/usb-auto-mount", v1.GetSystemUSBAutoMount) ///sys/usb/status
		}
	}

	return e
}
