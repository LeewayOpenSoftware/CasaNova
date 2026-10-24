package route

import (
	"crypto/ecdsa"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova/codegen"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	v2Route "github.com/LeewayOpenSoftware/CasaNova/route/v2"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

var (
	_swagger *openapi3.T

	V2APIPath  string
	V2DocPath  string
	V3FilePath string
)

var v2PreLoginRoutes = map[string]bool{
	"/v2/zimaos/device/info":     true,
	"/v2/zimaos/device/image":    true,
	"/v2/zimaos/zt/info":         true,
	"/v2/mod_management/modules": true,
	"/v2/search/ping":            true,
	"/v2/search/stats":           true,
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
	V3FilePath = "/v3/file"
}

func InitV2Router() http.Handler {
	appManagement := v2Route.NewCasaOS()

	e := echo.New()

	// Outermost: turn any handler panic into a clean 500 instead of an
	// abruptly-closed connection. Cheap insurance for the file/search walkers
	// that touch untrusted filenames and byte offsets.
	e.Use(echo_middleware.Recover())

	// Wave 34-X: previously this used `AllowOrigins: ["*"]` together with
	// `AllowCredentials: true`. Echo's CORS middleware detects that
	// combination at request time and echoes the request's Origin back
	// as Access-Control-Allow-Origin, so browsers DO honour the
	// credentialed cross-origin response despite the spec ban on
	// `*`+credentials. Combined with the JWT skipper that bypasses
	// auth on loopback RemoteAddr, a single visit to a hostile webpage
	// by an admin while logged in to CasaNova on localhost allowed
	// the attacker JS to call /v2/* with full admin authority. v1 was
	// already fixed (see route/v1.go); this is the matching v2 fix.
	//
	// New behaviour: drop AllowCredentials. Browsers will continue to
	// allow non-credentialed cross-origin reads of public endpoints,
	// but the JWT-bearing requests now require an explicit Origin
	// allow-list. Same-origin requests (the normal /v2/* call from the
	// CasaNova UI) are unaffected because Origin matches the document
	// origin.
	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders: []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith, "Range"},
		// ExposeHeaders MUST include Content-Range and Accept-Ranges so browsers
		// can see partial-content responses. Without these, media playback
		// falls back to downloading the entire file before playing.
		ExposeHeaders: []string{
			echo.HeaderContentLength,
			echo.HeaderAccessControlAllowOrigin,
			echo.HeaderAccessControlAllowHeaders,
			"Content-Range",
			"Accept-Ranges",
		},
		MaxAge:           172800,
		AllowCredentials: false,
	})))

	// Gzip is skipped on the file streaming endpoint — compressing a Range
	// response corrupts the partial-content byte offsets and also wastes CPU
	// on already-compressed media files (MP3, MP4, JPEG, etc.).
	e.Use(echo_middleware.GzipWithConfig(echo_middleware.GzipConfig{
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Request().URL.Path, "/v3/file")
		},
	}))

	// Custom logger that omits the query string so stray `?token=<jwt>`
	// parameters (WebSocket / SSE auth) never hit journald.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	// v2PreLoginRoutes: the v2 route patterns that must answer before a session can
	// exist.
	//
	// The JWT here is attached with e.Use, so it covers EVERY route on this echo
	// regardless of registration order — including the ZimaOS compat routes, which
	// registerZimaCompat mounts after it. The Android and Windows clients read the
	// device identity before any login exists; gating those breaks pairing, and it
	// breaks it silently, which is why they are named here rather than left to a
	// prefix rule.
	//
	// The two search endpoints return literal constants ("Pong", and
	// {status:ok, support_ai:false}). They are listed to keep this change from
	// altering behaviour nobody asked about; they disclose nothing the box does not
	// already disclose by answering at all.
	//
	// Everything else on v2 authenticates — including /v2/app_management/*, which
	// has no in-handler RBAC of its own and was, until this change, serving every
	// installed app's compose definition to anyone who asked.
	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: skipAuthFor(v2PreLoginRoutes),
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
			func(ctx echo.Context) ([]string, error) {
				// Prefer Authorization header (standard, secure)
				if h := ctx.Request().Header.Get(echo.HeaderAuthorization); len(h) > 0 {
					// Strip the "Bearer " scheme so ParseTokenFunc receives
					// the bare JWT. echo does NOT strip the scheme for custom
					// extractors, and jwt.Validate cannot parse "Bearer <jwt>"
					// (it would always 401). Case-insensitive per RFC 6750.
					if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
						h = h[7:]
					}
					return []string{h}, nil
				}
				// Fallback: token in query param — required for WebSocket upgrade
				// (browsers cannot send custom headers during WS handshake).
				// NOTE: this means the token may appear in server access logs.
				return []string{ctx.QueryParam("token")}, nil
			},
		},
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
		Skipper: func(c echo.Context) bool {
			// ZimaOS-client compat routes aren't in our OpenAPI spec — skip
			// spec validation for them (see route/zimaos_compat.go).
			p := c.Request().URL.Path
			if strings.HasPrefix(p, "/v2/zimaos/") || strings.HasPrefix(p, "/v2/mod_management/") || strings.HasPrefix(p, "/v2/search") {
				return true
			}
			// jump validate when upload file, because file upload can't pass
			// validate (oapi-codegen#514). Header.Get is nil-safe — indexing
			// the header slice directly panicked on requests with no
			// Content-Type (e.g. a plain GET).
			return strings.Contains(c.Request().Header.Get(echo.HeaderContentType), "multipart/form-data")
		},
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}))

	codegen.RegisterHandlersWithBaseURL(e, appManagement, V2APIPath)
	registerZimaCompat(e)
	registerCasaNovaSearch(e) // /v2/search — global file search (casanova-search)

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

func InitFile() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if len(token) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "token not found"}`))
			return
		}

		valid, claims, errs := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
		if errs != nil || !valid {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "validation failure"}`))
			return
		}
		// REVOCATION. This handler is mounted standalone in main.go, outside
		// InitV1Router/InitV2Router, so it inherits none of their middleware —
		// and it was the only entry point in the process that validated a
		// signature without asking whether the token had since been revoked.
		// A logged-out or deleted principal kept downloading files here for the
		// token's full lifetime, across restarts. Every other entry point
		// (v1Group, strictJWT, the gateway management config, v2) calls this;
		// it fails closed if the revocation authority cannot be consulted.
		if jwt.RevokedForClaims(claims) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "validation failure"}`))
			return
		}
		// AUTHORISATION. A valid signature said only "some principal"; it never
		// said "a principal allowed to read files". PermFileRead is the bit the
		// rest of the file API downloads on (v1 GetDownloadFile), and it is
		// deliberately NOT in guestPerms — that removal is what stopped guests
		// reading anything outside the blocked-prefix list, and this route
		// bypassed it entirely.
		if !rbac.Allows(rbac.Normalize(claims.Role), rbac.PermFileRead) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message": "access denied"}`))
			return
		}
		role := rbac.Normalize(claims.Role)
		serveV3File(w, r, storageauth.Principal{
			ID:    strconv.Itoa(claims.ID),
			Role:  string(role),
			Admin: rbac.Allows(role, rbac.PermUserManage),
		}, r.URL.Query().Get("path"))
	})
}

// serveV3File is the part of GET /v3/file that runs once the token is known to
// be valid, unrevoked and allowed to read files: it decides whether THIS
// principal may read THIS path, the same way v1 GetDownloadFile does.
//
// It used to check only the lexical blocked-prefix list and then call
// http.ServeFile, so the per-user storage authority never ran here: a plain
// user read any path the list did not name (/home/*/.ssh, other users' areas,
// /tmp), and a symlink under /DATA was followed into /etc. SanitizeFilePath judges where the path really lands, and
// OpenAuthorizedStrict asks the authority and opens beneath its boundary, so a
// target swapped after the check is refused too.
func serveV3File(w http.ResponseWriter, r *http.Request, p storageauth.Principal, raw string) {
	filePath, err := utils.SanitizeFilePath(raw)
	if err != nil || !filepath.IsAbs(raw) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message": "invalid path"}`))
		return
	}
	f, _, err := storageauth.OpenAuthorizedStrict(p, storageauth.OpRead, filePath, os.O_RDONLY, 0)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message": "access denied"}`))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message": "invalid path"}`))
		return
	}
	fileName := path.Base(filePath)
	w.Header().Add("Content-Disposition", "attachment; filename*=utf-8''"+url.PathEscape(fileName))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, fileName, info.ModTime(), f)
}

// writeJSONError answers a raw http.Handler with the same envelope the echo
// handlers use.
//
// Every error path in InitDir below used to be a bare `return` with the
// refusal COMMENTED OUT: no status, no body, and the octet-stream headers
// already set. The client got 200 and zero bytes, so a rejected request was
// indistinguishable from an empty archive -- which is how a bad request
// surfaced to the user as a silently broken download.
func writeJSONError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(model.Result{Success: code, Message: message})
}

func InitDir() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if len(token) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "token not found"}`))
			return
		}

		valid, claims, errs := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
		if errs != nil || !valid {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "validation failure"}`))
			return
		}
		// The same two holes InitFile had, in the same shape, for the same
		// reason: both are mounted standalone in main.go, outside
		// InitV1Router/InitV2Router, so neither inherits their middleware and
		// both discarded the claims they had already decoded.
		//
		// REVOCATION. A signature check alone says the token was once issued,
		// never that it still stands. A logged-out or deleted principal kept
		// downloading here for the token's full lifetime, across restarts.
		// Fails closed if the revocation authority cannot be consulted.
		if jwt.RevokedForClaims(claims) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message": "validation failure"}`))
			return
		}
		// AUTHORISATION. PermFileRead, the same bit InitFile and v1
		// GetDownloadFile require, and deliberately not in guestPerms. If
		// anything this route needs it MORE than InitFile does: that one
		// serves a single named path, while this one takes a comma-separated
		// LIST and streams an archive, so one unauthorised request drains as
		// much of the tree as the blocked-prefix list leaves reachable.
		if !rbac.Allows(rbac.Normalize(claims.Role), rbac.PermFileRead) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message": "access denied"}`))
			return
		}
		t := r.URL.Query().Get("format")
		files := r.URL.Query().Get("files")

		if len(files) == 0 {
			writeJSONError(w, http.StatusBadRequest, common_err.INVALID_PARAMS,
				common_err.GetMsg(common_err.INVALID_PARAMS))
			return
		}
		list := strings.Split(files, ",")
		for i, v := range list {
			cleaned := filepath.Clean(v)
			if !filepath.IsAbs(cleaned) || strings.Contains(cleaned, "..") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, prefix := range utils.BlockedSystemPrefixes {
				if cleaned == prefix || strings.HasPrefix(cleaned, prefix+"/") {
					w.WriteHeader(http.StatusForbidden)
					return
				}
			}
			list[i] = cleaned
			if !file.Exists(cleaned) {
				writeJSONError(w, http.StatusNotFound, common_err.FILE_DOES_NOT_EXIST,
					common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST))
				return
			}
		}
		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Transfer-Encoding", "binary")
		w.Header().Add("Cache-Control", "no-cache")
		extension, ar, err := file.GetCompressionAlgorithm(t)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, common_err.INVALID_PARAMS,
				common_err.GetMsg(common_err.INVALID_PARAMS))
			return
		}

		err = ar.Create(w)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, common_err.SERVICE_ERROR,
				common_err.GetMsg(common_err.SERVICE_ERROR))
			return
		}
		defer ar.Close()
		commonDir := file.CommonPrefix(filepath.Separator, list...)

		currentPath := filepath.Base(commonDir)

		name := "_" + currentPath
		name += extension
		w.Header().Add("Content-Disposition", "attachment; filename*=utf-8''"+url.PathEscape(name))
		for _, fname := range list {
			err = file.AddFile(ar, fname, commonDir)
			if err != nil {
				log.Printf("Failed to archive %s: %v", fname, err)
			}
		}
	})
}
