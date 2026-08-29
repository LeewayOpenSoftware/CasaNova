package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// SecurityHeaders adds standard security headers to all responses.
// Inspired by Cosmos-Server's header hardening.
func SecurityHeaders() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()

			// Prevent MIME type sniffing
			h.Set("X-Content-Type-Options", "nosniff")

			// XSS protection (legacy browsers)
			h.Set("X-XSS-Protection", "1; mode=block")

			// Prevent clickjacking
			h.Set("X-Frame-Options", "SAMEORIGIN")

			// Don't send referrer to external sites
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

			// Restrict browser features
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

			// HSTS if served over HTTPS
			if c.Request().TLS != nil || c.Request().Header.Get("X-Forwarded-Proto") == "https" {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}

			// CSP: allow self and inline (needed for Vue.js), block embedding in other sites
			h.Set("Content-Security-Policy", "frame-ancestors 'self'")

			return next(c)
		}
	}
}

// RequestSizeLimit limits the maximum request body size.
// Prevents DoS via oversized uploads through the gateway.
func RequestSizeLimit(maxBytes int64) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().ContentLength > maxBytes {
				return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{
					"message": "request body too large",
				})
			}
			return next(c)
		}
	}
}

// Cors returns CORS middleware configured for CasaNova.
//
// AllowOriginFunc accepts:
//   - loopback origins (http(s)://localhost, 127.0.0.1, [::1]) — the dashboard UI
//   - private-range LAN origins (RFC1918: 10.x, 172.16-31.x, 192.168.x) and
//     link-local (169.254.x) — typical home server setups where the client
//     browser is on the same LAN as the server
//
// This replaces a previous `return true, nil` implementation which was
// effectively wildcard CORS disguised as validation — any website the user
// visited could read authenticated CasaNova responses cross-origin.
//
// ExposeHeaders includes Content-Range and Accept-Ranges so browsers can
// honor partial-content responses during media playback.
func Cors() echo.MiddlewareFunc {
	return echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOriginFunc: IsAllowedCorsOrigin,
		AllowMethods:    []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders: []string{
			echo.HeaderAuthorization,
			echo.HeaderContentLength,
			echo.HeaderXCSRFToken,
			echo.HeaderContentType,
			echo.HeaderAccessControlAllowOrigin,
			echo.HeaderAccessControlAllowHeaders,
			echo.HeaderAccessControlAllowMethods,
			echo.HeaderConnection,
			echo.HeaderOrigin,
			echo.HeaderXRequestedWith,
			"Range",
		},
		ExposeHeaders: []string{
			echo.HeaderContentLength,
			echo.HeaderAccessControlAllowOrigin,
			echo.HeaderAccessControlAllowHeaders,
			"Content-Range",
			"Accept-Ranges",
		},
		MaxAge:           172800,
		AllowCredentials: true,
	})
}

// MediaStreamingGzipSkipper returns a Gzip middleware skipper that disables
// compression for file streaming endpoints. Gzip corrupts HTTP Range responses
// and wastes CPU on already-compressed media files. Use with echo_middleware.GzipWithConfig.
//
// Example:
//
//	e.Use(echo_middleware.GzipWithConfig(echo_middleware.GzipConfig{
//	    Skipper: middleware.MediaStreamingGzipSkipper(),
//	}))
func MediaStreamingGzipSkipper() echo_middleware.Skipper {
	return func(c echo.Context) bool {
		p := c.Request().URL.Path
		// Skip compression for any file streaming endpoint
		if p == "/v1/file" || p == "/v1/image" || p == "/v1/batch" {
			return true
		}
		if len(p) >= 8 && p[:8] == "/v3/file" {
			return true
		}
		return false
	}
}
