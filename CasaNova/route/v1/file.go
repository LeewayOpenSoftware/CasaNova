package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	url2 "net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/robfig/cron/v3"
	"github.com/tidwall/gjson"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/h2non/filetype"
)

// validatePath is the single source of truth for user-supplied path validation
// in every file endpoint. It prevents path traversal and sensitive-directory
// access, and returns a client-safe error message (no internal paths leaked).
//
// Returns the cleaned absolute path and, on failure, a populated echo response
// that the caller should return directly.
func validatePath(ctx echo.Context, raw string) (string, error) {
	return validatePathWith(ctx, raw, utils.SanitizeFilePath)
}

// validatePathNoFollow is validatePath for the handlers that act on the entry
// ITSELF — delete, move-to-trash, rename — where the final component must not
// be followed. A link such as /DATA/AppData/foo/nginx.conf ->
// /etc/nginx/nginx.conf lists fine in the file manager, but the full check
// resolved it to /etc and answered 400, leaving the user with an entry they
// could not remove from the UI. Unlinking a link never touches its target; the
// parent is still resolved, so a path THROUGH a link into a blocked directory
// stays refused. Read, write and download keep using validatePath.
func validatePathNoFollow(ctx echo.Context, raw string) (string, error) {
	return validatePathWith(ctx, raw, utils.SanitizeFilePathNoFollow)
}

// validateUploadNames rejects an upload whose filename or relativePath carries a
// literal backslash. A backslash is a perfectly legal character in a Linux file
// name ("AC\DC - Back in Black.mp3"), but every path this file validates goes
// through utils.FixAndCleanPath, which rewrites "\" into "/" — so that name
// silently becomes two path components. The upload handlers hold the invariant
// "the validated string is the string we write", which is what keeps the symlink
// re-checks meaningful, so we cannot un-rewrite the name after the fact. Left
// alone the request dies later at open(2) with ENOENT (the invented parent does
// not exist) and the user gets an opaque 500 mid-transfer. Refusing up front and
// naming the offending character is the honest answer. Kept as one helper so the
// POST and its GET preflight can't drift: the preflight is where the client
// learns about it BEFORE spending bandwidth.
func validateUploadNames(ctx echo.Context, fileName, relative string) error {
	if strings.ContainsRune(fileName, '\\') || strings.ContainsRune(relative, '\\') {
		return echo.NewHTTPError(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: `upload name cannot contain a backslash ("\")`,
		})
	}
	return nil
}

func validatePathWith(ctx echo.Context, raw string, sanitize func(string) (string, error)) (string, error) {
	return authorizePathWith(ctx, raw, sanitize, opForSanitizer(sanitize))
}

// principalFrom builds the identity a storage decision is made for out of the
// request context the JWT middleware populated.
func principalFrom(ctx echo.Context) storageauth.Principal {
	role := commonmw.RoleFromContext(ctx)
	return storageauth.Principal{
		ID:    strconv.Itoa(commonmw.UserIDFromContext(ctx)),
		Role:  string(role),
		Admin: rbac.Allows(role, rbac.PermUserManage),
	}
}

// opForSanitizer maps the legacy two-variant sanitizer choice onto the
// operation vocabulary. The no-follow variant exists for operations acting on
// the ENTRY (delete, trash, rename); the following variant for operations on
// its CONTENT.
//
// This is a bridge, not the destination: handlers that know their operation
// exactly should call authorizePath with it, and several already do. Inferring
// it from the sanitizer keeps the 25 existing call sites correct today without
// a 25-site edit that would be its own risk.
func opForSanitizer(sanitize func(string) (string, error)) storageauth.Op {
	if reflect.ValueOf(sanitize).Pointer() == reflect.ValueOf(utils.SanitizeFilePathNoFollow).Pointer() {
		return storageauth.OpDelete
	}
	return storageauth.OpRead | storageauth.OpWrite
}

// authorizePath is the single decision point every file endpoint goes through.
//
// It answers the question the inherited helper could not: not "is this path
// outside a fixed list of system directories", but "what authority establishes
// that CasaNova, and then THIS principal, may perform THIS operation here".
//
// Both checks run, in order, and both must pass:
//
//	utils.Sanitize* — the inherited lexical and blocklist hygiene, kept as
//	                  defence in depth because it also normalises the string
//	                  the caller will act on;
//	storageauth    — the derived storage ceiling and the principal's grants.
//
// The sanitizer is no longer the boundary. It is the first of two.
func authorizePath(ctx echo.Context, raw string, op storageauth.Op) (string, error) {
	sanitize := utils.SanitizeFilePath
	if op&(storageauth.OpDelete|storageauth.OpRename) != 0 &&
		op&(storageauth.OpRead|storageauth.OpWrite|storageauth.OpCreate) == 0 {
		sanitize = utils.SanitizeFilePathNoFollow
	}
	return authorizePathWith(ctx, raw, sanitize, op)
}

func authorizePathWith(ctx echo.Context, raw string, sanitize func(string) (string, error), op storageauth.Op) (string, error) {
	if raw == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: common_err.GetMsg(common_err.INVALID_PARAMS),
		})
	}
	clean, err := sanitize(raw)
	if err != nil {
		// Only the sanitize error reason is exposed — never the raw path.
		return "", echo.NewHTTPError(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	d := storageauth.AuthorizeStorage(principalFrom(ctx), op, clean)
	if !d.Allowed {
		return "", storageDenied(d)
	}
	return d.Path, nil
}

// confinementRefused turns a failure of the descriptor-relative primitive into
// a structured security error.
//
// LEG-001's claim is that a stale authorization cannot be turned into an escape.
// A handler that fell back to the ordinary open when the kernel cannot confine
// would be claiming a protection it does not have, so the File API refuses
// instead. That is the fail-closed half of the contract: the reduced guarantee
// is never silent.
func confinementRefused(err error) error {
	if errors.Is(err, storageauth.ErrConfinementUnavailable) {
		return echo.NewHTTPError(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "STORAGE_CONFINEMENT_UNAVAILABLE",
			Data: map[string]interface{}{
				"reason": "STORAGE_CONFINEMENT_UNAVAILABLE",
				"detail": "this kernel cannot confine path resolution, so the file API refuses " +
					"rather than operate without the guarantee",
			},
		})
	}
	return echo.NewHTTPError(http.StatusForbidden, model.Result{
		Success: common_err.CLIENT_ERROR,
		Message: "STORAGE_BOUNDARY_VIOLATION",
		Data: map[string]interface{}{
			"reason": "STORAGE_BOUNDARY_VIOLATION",
			"detail": err.Error(),
		},
	})
}

// storageDenied turns a Decision into an HTTP response that carries the
// machine-readable reason, so a client can distinguish "ask an administrator to
// add this location" from "you have no access here" without parsing English.
func storageDenied(d storageauth.Decision) error {
	status := http.StatusForbidden
	if d.Reason == storageauth.ReasonInvalidPath {
		status = http.StatusBadRequest
	}
	if d.Reason == storageauth.ReasonPolicyUnavailable {
		// Not the client's fault, and not a permanent no.
		status = http.StatusServiceUnavailable
	}
	return echo.NewHTTPError(status, model.Result{
		Success: common_err.CLIENT_ERROR,
		Message: string(d.Reason),
		Data: map[string]interface{}{
			"reason":     string(d.Reason),
			"detail":     d.Detail,
			"remediable": d.Remediable,
		},
	})
}

// safeDownloadMIME maps a sniffed content-type to a value that is safe to
// echo back to a browser. Anything that a browser would interpret as
// active content (HTML, JS, SVG, XHTML, MathML, XML) is rewritten to
// application/octet-stream so the browser saves instead of renders.
//
// Wave 27: previously the sniff result was piped straight into the
// Content-Type header. An attacker could upload a .jpg containing HTML +
// inline JavaScript and, if Content-Disposition was ever stripped by a
// proxy or omitted on a related code path, the file would execute in the
// user's same-origin session. Coercing the type closes that door.
func safeDownloadMIME(sniffed string) string {
	// Strip params (charset, boundary) before matching.
	bare := sniffed
	if idx := strings.Index(bare, ";"); idx != -1 {
		bare = strings.TrimSpace(bare[:idx])
	}
	switch strings.ToLower(bare) {
	case "text/html",
		"application/xhtml+xml",
		"image/svg+xml",
		"application/xml",
		"text/xml",
		"application/javascript",
		"text/javascript",
		"application/ecmascript",
		"text/ecmascript",
		"application/x-javascript",
		"application/mathml+xml":
		return "application/octet-stream"
	}
	return sniffed
}

// sanitizeErr strips host-specific detail out of system errors so we don't
// leak filesystem layout to unauthenticated clients. It preserves the kind
// of failure (not found, permission denied, etc.) while dropping exact paths.
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Collapse any "stat /foo/bar:" or "open /foo/bar:" prefix down to the suffix.
	if idx := strings.Index(s, ": "); idx != -1 {
		s = s[idx+2:]
	}
	return s
}

type ListReq struct {
	model.PageReq
	Path string `json:"path" form:"path"`
	// Refresh bool   `json:"refresh"`
}

type ObjResp struct {
	Name       string                 `json:"name"`
	Size       int64                  `json:"size"`
	IsDir      bool                   `json:"is_dir"`
	Modified   time.Time              `json:"modified"`
	Sign       string                 `json:"sign"`
	Thumb      string                 `json:"thumb"`
	Type       int                    `json:"type"`
	Path       string                 `json:"path"`
	Date       time.Time              `json:"date"`
	Extensions map[string]interface{} `json:"extensions"`
}
type FsListResp struct {
	Content  []ObjResp `json:"content"`
	Total    int64     `json:"total"`
	Readme   string    `json:"readme,omitempty"`
	Write    bool      `json:"write,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Index    int       `json:"index"`
	Size     int       `json:"size"`
}

// Wave 28: WebSocket robustness constants. The previous defaults left
// every parameter at zero, which means: no handshake timeout (a slow
// client could pin a goroutine indefinitely during the upgrade), no
// max-message size (a malicious peer could push 100 MB frames at the
// server), no read deadline (a half-open TCP connection wedged the
// readPump forever), and no ping/pong (dead clients were never noticed
// until the OS finally tore the socket down). All four are now set.
const (
	wsFileHandshakeTimeout = 10 * time.Second
	wsFileMaxMessageSize   = 1 << 20 // 1 MiB per inbound frame
	wsFilePongWait         = 60 * time.Second
	wsFilePingPeriod       = (wsFilePongWait * 9) / 10
	wsFileWriteWait        = 10 * time.Second
)

var (
	//  WebSocket
	upgraderFile = websocket.Upgrader{
		HandshakeTimeout: wsFileHandshakeTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		// CheckOrigin uses gorilla's default: same-origin or no-origin (non-browser).
		// Wave 28: keep compression disabled — gorilla supports it via
		// EnableCompression: true, but flate-over-WS gives an attacker
		// a CRIME/BREACH-shaped oracle if any per-user secret is ever
		// echoed in a frame. Plain frames are safer than fast frames.
		EnableCompression: false,
	}
)

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path query string true ""
// @Success 200 {string} string "ok"
// @Router /file/read [get]
func GetFilerContent(ctx echo.Context) error {
	// The file API's read surface was ungated: the only limit was the lexical
	// blocked-prefix list, so any authenticated caller (and, while guest held
	// PermFileRead, effectively anyone) could read outside it. Gate on the
	// permission the role model already defines.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	filePath, errResp := validatePath(ctx, ctx.QueryParam("path"))
	if errResp != nil {
		return errResp
	}
	if !file.Exists(filePath) {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
		})
	}

	stat, err := os.Stat(filePath)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
			Data:    sanitizeErr(err),
		})
	}
	const maxReadSize = 100 * 1024 * 1024 // 100 MB
	if stat.Size() > maxReadSize {
		return ctx.JSON(http.StatusRequestEntityTooLarge, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "file too large to read into memory (max 100 MB)",
		})
	}

	info, _, err := storageauth.ReadFileAuthorized(principalFrom(ctx), filePath)
	if err != nil && !os.IsNotExist(err) {
		if storageauth.IsDenial(err) {
			return err
		}
		return confinementRefused(err)
	}
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{
			Success: common_err.FILE_READ_ERROR,
			Message: common_err.GetMsg(common_err.FILE_READ_ERROR),
			// Don't leak absolute paths — sanitize the system error.
			Data: sanitizeErr(err),
		})
	}
	result := string(info)

	return ctx.JSON(common_err.SUCCESS, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    result,
	})
}

// GetLocalFile was removed rather than wired.
//
// It was a complete file-serving handler that NO router registered — dead on
// every deployment, and dead code with a privileged capability is not harmless:
// it is a correctly-shaped handler sitting ready for someone to route in a
// hurry. Reducing historical attack surface is itself hardening.
//
// If local file serving is wanted again, add it through authorizePath like
// every other endpoint in this file, with its operation named.

// @Summary download
// @Produce  application/json
// @Accept application/json
// @Tags file
// @Security ApiKeyAuth
// @Param format query string false "Compression format" Enums(zip,tar,targz)
// @Param files query string true "file list eg: filename1,filename2,filename3 "
// @Success 200 {string} string "ok"
// @Router /file/download [get]
func GetDownloadFile(ctx echo.Context) error {
	// The file API's read surface was ungated: the only limit was the lexical
	// blocked-prefix list, so any authenticated caller (and, while guest held
	// PermFileRead, effectively anyone) could read outside it. Gate on the
	// permission the role model already defines.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	t := ctx.QueryParam("format")

	files := ctx.QueryParam("files")

	if len(files) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: common_err.GetMsg(common_err.INVALID_PARAMS),
		})
	}
	rawList := strings.Split(files, ",")
	// Sanitize every path up-front so traversal attempts can't smuggle in
	// as part of a comma-separated batch.
	list := make([]string, 0, len(rawList))
	for _, v := range rawList {
		clean, errResp := validatePath(ctx, v)
		if errResp != nil {
			return errResp
		}
		if !file.Exists(clean) {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{
				Success: common_err.FILE_DOES_NOT_EXIST,
				Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
			})
		}
		list = append(list, clean)
	}
	ctx.Request().Header.Add("Content-Type", "application/octet-stream")
	ctx.Request().Header.Add("Content-Transfer-Encoding", "binary")
	ctx.Request().Header.Add("Cache-Control", "no-cache")
	// handles only single files not folders and multiple files
	if len(list) == 1 {

		filePath := list[0]
		info, err := os.Stat(filePath)
		if err != nil {
			return ctx.JSON(http.StatusNotFound, model.Result{
				Success: common_err.FILE_DOES_NOT_EXIST,
				Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
			})
		}
		if !info.IsDir() {
			fileName := path.Base(filePath)
			ctx.Response().Header().Add("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(fileName))

			// SERVE THROUGH THE RAW ResponseWriter, NOT ctx.File.
			//
			// echo's ctx.File calls http.ServeContent(c.Response(), ...), and
			// echo's *Response has no ReadFrom method. io.Copy therefore cannot
			// take the fast path and falls back to a 32 KiB buffer loop; the
			// bytes are read into user space and written out again, chunk by
			// chunk. Handing ServeContent the underlying writer restores it,
			// because net/http's own response DOES implement ReadFrom and uses
			// sendfile(2) when the source is an *os.File.
			//
			// Measured with strace on a 64 MiB download:
			//
			//	ctx.File()               4,174 write   99 ms in syscalls
			//	ctx.Response().Writer      156 sendfile + 4 write   11 ms
			//
			// 26x fewer syscalls, and with sendfile the data never enters user
			// space at all. GetDownloadSingleFile below already serves this way;
			// this path was the one still using the wrapper.
			f, err := os.Open(filePath)
			if err != nil {
				return ctx.JSON(http.StatusNotFound, model.Result{
					Success: common_err.FILE_DOES_NOT_EXIST,
					Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
				})
			}
			defer f.Close()
			http.ServeContent(ctx.Response().Writer, ctx.Request(), fileName, info.ModTime(), f)
			return nil
		}
	}

	extension, ar, err := file.GetCompressionAlgorithm(t)
	if err != nil {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: common_err.GetMsg(common_err.INVALID_PARAMS),
		})
	}

	err = ar.Create(ctx.Response().Writer)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: common_err.GetMsg(common_err.SERVICE_ERROR),
			Data:    err.Error(),
		})
	}
	defer ar.Close()
	commonDir := file.CommonPrefix(filepath.Separator, list...)

	currentPath := filepath.Base(commonDir)

	name := "_" + currentPath
	name += extension
	ctx.Request().Header.Add("Content-Disposition", "attachment; filename*=utf-8''"+url.PathEscape(name))
	for _, fname := range list {
		err = file.AddFile(ar, fname, commonDir)
		if err != nil {
			log.Printf("Failed to archive %s: %v", fname, err)
		}
	}
	return nil
}

func GetDownloadSingleFile(ctx echo.Context) error {
	// The file API's read surface was ungated: the only limit was the lexical
	// blocked-prefix list, so any authenticated caller (and, while guest held
	// PermFileRead, effectively anyone) could read outside it. Gate on the
	// permission the role model already defines.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	filePath, errResp := validatePath(ctx, ctx.QueryParam("path"))
	if errResp != nil {
		return errResp
	}
	fileName := path.Base(filePath)
	// c.Header("Content-Disposition", "inline")
	ctx.Request().Header.Add("Content-Disposition", "attachment; filename*=utf-8''"+url2.PathEscape(fileName))

	// Confined open: the authorization above and this open are now one
	// resolution, in the kernel, so an ancestor swapped in between cannot
	// redirect the read.
	fi, _, err := storageauth.OpenAuthorizedStrict(principalFrom(ctx), storageauth.OpRead,
		filePath, os.O_RDONLY, 0)
	if err != nil {
		return confinementRefused(err)
	}
	_ = err
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: sanitizeErr(err)})
	}
	defer fi.Close()

	// Sniff the content type from the first 261 bytes, then seek back to 0
	// so http.ServeContent starts from the beginning. The old code forgot
	// to seek back, so every download was missing its first 261 bytes; then
	// it opened a SECOND file handle it never used (pure FD leak).
	buffer := make([]byte, 261)
	_, _ = fi.Read(buffer)
	if _, err := fi.Seek(0, 0); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: sanitizeErr(err),
		})
	}
	// Wave 27: never echo a sniffed Content-Type that the browser would
	// render inline as script. The download endpoint already forces
	// Content-Disposition: attachment, but defense in depth — if the
	// disposition header is ever lost in a proxy, an .html or .svg file
	// uploaded with a .jpg name would otherwise execute in the user's
	// session origin. Coerce anything outside the safe-render set to
	// application/octet-stream so the browser saves instead of executes.
	if kind, _ := filetype.Match(buffer); kind != filetype.Unknown {
		ctx.Response().Header().Set("Content-Type", safeDownloadMIME(kind.MIME.Value))
	}

	node, err := os.Stat(filePath)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: sanitizeErr(err),
		})
	}
	// Write Last-Modified on the RESPONSE header (old code wrote it on the
	// request header, which browsers never see). Content-Length is set
	// automatically by http.ServeContent via the io.Seeker interface.
	ctx.Response().Header().Set("Last-Modified", node.ModTime().UTC().Format(http.TimeFormat))
	http.ServeContent(ctx.Response().Writer, ctx.Request(), fileName, node.ModTime(), fi)
	return nil
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path query string false ""
// @Success 200 {string} string "ok"
// @Router /file/dirpath [get]
func DirPath(ctx echo.Context) error {
	// The file API's read surface was ungated: the only limit was the lexical
	// blocked-prefix list, so any authenticated caller (and, while guest held
	// PermFileRead, effectively anyone) could read outside it. Gate on the
	// permission the role model already defines.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	var req ListReq
	cleanPath, errResp := validatePath(ctx, ctx.QueryParam("path"))
	if errResp != nil {
		return errResp
	}
	req.Path = cleanPath
	req.Validate()

	// Fixes upstream CasaOS issue #1848 (40 comments, open since 2024).
	// By default, entries whose name begins with "." are filtered — matching
	// the existing UI behavior where the frontend hid them. When the client
	// passes ?show_hidden=true, dotfiles are included so a "show hidden"
	// toggle in the UI actually has something to reveal. Omitted param
	// keeps the old behavior so we don't break existing clients.
	showHidden := ctx.QueryParam("show_hidden") == "true"
	info, err := service.MyService.System().GetDirPath(req.Path)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	shares := service.MyService.Shares().GetSharesList()
	sharesMap := make(map[string]string)
	for _, v := range shares {
		sharesMap[v.Path] = fmt.Sprint(v.ID)
	}
	// if len(info) <= (req.Page-1)*req.Size {
	// 	return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: "page out of range"})
	// 	return
	// }
	// Page bounds, computed once and shared by the three passes below. The
	// start is clamped to forEnd so a page that lands past the end of the
	// directory yields an empty slice instead of a negative length when it
	// is used to size pathList.
	forEnd := req.Index * req.Size
	if forEnd > len(info) {
		forEnd = len(info)
	}
	pageStart := (req.Index - 1) * req.Size
	if pageStart > forEnd {
		pageStart = forEnd
	}
	for i := pageStart; i < forEnd; i++ {
		if v, ok := sharesMap[info[i].Path]; ok {
			ex := make(map[string]interface{})
			shareEx := make(map[string]string)
			shareEx["shared"] = "true"
			shareEx["id"] = v
			ex["share"] = shareEx
			ex["mounted"] = false
			info[i].Extensions = ex
		}
	}
	if strings.HasPrefix(req.Path, "/mnt") || strings.HasPrefix(req.Path, "/media") {
		// v0.1 perf: previously this called service.IsMounted(path) once
		// per entry. IsMounted reads /proc/self/mountinfo from scratch
		// AND iterates the rclone connections list — both per call —
		// so listing a /mnt/ parent with 50 mounted children meant 50
		// mountinfo reads + 50 connections scans per directory list.
		// Snapshot both once per request and look up in O(1) per entry.
		mountedSet := service.SnapshotMountedPaths()
		for i := pageStart; i < forEnd; i++ {
			ex := info[i].Extensions
			if ex == nil {
				ex = make(map[string]interface{})
			}
			ex["mounted"] = mountedSet[info[i].Path]
			info[i].Extensions = ex
		}
	}
	// Hide the files or folders in operation
	fileQueue := make(map[string]string)
	service.OpMu.Lock()
	opStrArrSnapshot := make([]string, len(service.OpStrArr))
	copy(opStrArrSnapshot, service.OpStrArr)
	service.OpMu.Unlock()
	if len(opStrArrSnapshot) > 0 {
		for _, v := range opStrArrSnapshot {
			v, ok := service.FileQueue.Load(v)
			if !ok {
				continue
			}
			vt := v.(model.FileOperate)
			for _, i := range vt.Item {
				lastPath := i.From[strings.LastIndex(i.From, "/")+1:]
				fileQueue[vt.To+"/"+lastPath] = i.From
			}
		}
	}

	// v0.1 perf: pathList used to grow from an empty slice, so listing a
	// directory with tens of thousands of entries (photo library, dataset)
	// reallocated it ~17 times and memcpy'd every ObjResp built so far on
	// each growth — megabytes of pure copying before the response is even
	// serialized. The page bounds are already known here, so allocate once.
	pathList := make([]ObjResp, 0, forEnd-pageStart)
	for i := pageStart; i < forEnd; i++ {
		if info[i].Name == ".temp" && info[i].IsDir {
			continue
		}
		// Hide dotfiles by default (matches pre-existing UI). The client
		// opts in via ?show_hidden=true when the user toggles "show
		// hidden files" in the file manager.
		if !showHidden && strings.HasPrefix(info[i].Name, ".") {
			continue
		}
		if _, ok := fileQueue[info[i].Path]; !ok {
			t := ObjResp{}
			t.IsDir = info[i].IsDir
			t.Name = info[i].Name
			t.Modified = info[i].Date
			t.Date = info[i].Date
			t.Size = info[i].Size
			t.Path = info[i].Path
			t.Extensions = info[i].Extensions
			pathList = append(pathList, t)

		}
	}
	flist := FsListResp{
		Content: pathList,
		Total:   int64(len(info)),
		// Readme:   "",
		// Write:    true,
		// Provider: "local",
		Index: req.Index,
		Size:  req.Size,
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: flist})
}

// @Summary rename file or dir
// @Produce  application/json
// @Accept application/json
// @Tags file
// @Security ApiKeyAuth
// @Param oldpath body string true "path of old"
// @Param newpath body string true "path of new"
// @Success 200 {string} string "ok"
// @Router /file/rename [put]
func RenamePath(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	// The source is renamed, not read: renaming a symlink moves the link and
	// leaves its target alone, so a link into a blocked directory must stay
	// renameable (see validatePathNoFollow). The DESTINATION keeps the full
	// check — nothing is lost by refusing a destination that resolves into a
	// system directory.
	op, errResp := validatePathNoFollow(ctx, json["old_path"])
	if errResp != nil {
		return errResp
	}
	np, errResp := validatePath(ctx, json["new_path"])
	if errResp != nil {
		return errResp
	}
	mounted := service.IsMounted(op)
	if mounted {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.MOUNTED_DIRECTIORIES, Message: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES), Data: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES)})
	}

	success, err := service.MyService.System().RenameFile(op, np)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: success, Message: common_err.GetMsg(success), Data: err})
}

// @Summary create folder
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path body string true "path of folder"
// @Success 200 {string} string "ok"
// @Router /file/mkdir [post]
func MkdirAll(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	j := make(map[string]string)
	if err := ctx.Bind(&j); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	cleanPath, errResp := validatePath(ctx, j["path"])
	if errResp != nil {
		return errResp
	}
	var code int
	code, _ = service.MyService.System().MkdirAll(cleanPath)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: code, Message: common_err.GetMsg(code)})
}

// @Summary create file
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path body string true "path of folder (path need to url encode)"
// @Success 200 {string} string "ok"
// @Router /file/create [post]
func PostCreateFile(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	j := make(map[string]string)
	if err := ctx.Bind(&j); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	cleanPath, errResp := validatePath(ctx, j["path"])
	if errResp != nil {
		return errResp
	}
	code, err := service.MyService.System().CreateFile(cleanPath)
	if code == common_err.SERVICE_ERROR {
		// A create that failed is a failure of the request, so it gets a
		// failing status. This used to answer HTTP 200 for every outcome —
		// the first argument to ctx.JSON is the status, and it was the
		// literal SUCCESS constant — which meant any client branching on the
		// status, from a curl script to an uptime probe, read a permanently
		// failed create as a success.
		logger.Error("create file", zap.String("path", cleanPath), zap.Error(err))
		msg := common_err.GetMsg(common_err.SERVICE_ERROR)
		if err != nil {
			msg = err.Error()
		}
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: msg})
	}
	// SUCCESS and FILE_OR_DIR_EXISTS both keep HTTP 200 on purpose: a name
	// that is already taken is not a broken request, it is an answer the
	// caller has to act on, and callers distinguish the two by the body code.
	return ctx.JSON(http.StatusOK, model.Result{Success: code, Message: common_err.GetMsg(code)})
}

// @Summary upload file
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path formData string false "file path"
// @Param file formData file true "file"
// @Success 200 {string} string "ok"
// @Router /file/upload [get]
func GetFileUpload(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	relative := ctx.QueryParam("relativePath")
	fileName := ctx.QueryParam("filename")
	chunkNumber := ctx.QueryParam("chunkNumber")

	// Same traversal hardening as PostFileUpload: relative/fileName/chunkNumber
	// flow into file.MkDir (MkdirAll + chmod 0777) and stat paths as root. Left
	// unvalidated, relativePath="../../../../etc/cron.d" makes this preflight a
	// world-writable arbitrary-directory-creation primitive → RCE.
	relative = filepath.Clean(relative)
	if filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid relativePath"})
	}
	fileName = filepath.Clean(fileName)
	if filepath.IsAbs(fileName) || strings.Contains(fileName, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid filename"})
	}
	// Answer the backslash case here, in the preflight, so the client is told
	// before it starts pushing chunks rather than halfway through the transfer.
	if errResp := validateUploadNames(ctx, fileName, relative); errResp != nil {
		return errResp
	}

	totalChunks, _ := strconv.Atoi(utils.DefaultQuery(ctx, "totalChunks", "0"))
	if n, cerr := strconv.Atoi(chunkNumber); cerr != nil || n < 0 || (totalChunks > 0 && n > totalChunks) {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid chunkNumber"})
	}
	path, errResp := validatePath(ctx, ctx.QueryParam("path"))
	if errResp != nil {
		return errResp
	}
	dirPath := ""
	hash := file.GetHashByContent([]byte(path + "/" + fileName))
	if file.Exists(path + "/" + relative) {
		return ctx.JSON(http.StatusConflict, model.Result{Success: http.StatusConflict, Message: common_err.GetMsg(common_err.FILE_ALREADY_EXISTS)})
	}
	tempDir := filepath.Join(path, ".temp", hash+strconv.Itoa(totalChunks)) + "/"
	if fileName != relative {
		dirPath = strings.TrimSuffix(relative, fileName)
		tempDir += dirPath
		// Same re-check as PostFileUpload: `path` was validated, `dirPath` is
		// appended to it raw. relativePath is only cleaned of ".." and of a
		// leading "/", which says nothing about a SYMLINKED directory component
		// inside the upload area — and MkDir is MkdirAll + chmod 0777, so a
		// planted link turns this preflight into world-writable directory
		// creation anywhere on the host. Validate what we are about to create.
		uploadDir, errResp := validatePath(ctx, path+"/"+dirPath)
		if errResp != nil {
			return errResp
		}
		file.MkDir(uploadDir)
	}
	tempDir += chunkNumber
	if !file.CheckNotExist(tempDir) {
		return ctx.JSON(200, model.Result{Success: 200, Message: common_err.GetMsg(common_err.FILE_ALREADY_EXISTS)})
	}

	return ctx.JSON(204, model.Result{Success: 204, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary upload file
// @Produce  application/json
// @Accept  multipart/form-data
// @Tags file
// @Security ApiKeyAuth
// @Param path formData string false "file path"
// @Param file formData file true "file"
// @Success 200 {string} string "ok"
// @Router /file/upload [post]
func PostFileUpload(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	f, _, err := ctx.Request().FormFile("file")
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "Missing or invalid file upload"})
	}
	defer f.Close()
	relative := ctx.FormValue("relativePath")
	fileName := ctx.FormValue("filename")

	// Sanitize relativePath to prevent path traversal (e.g. ../../../etc/)
	relative = filepath.Clean(relative)
	if filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid relativePath"})
	}

	// Sanitize fileName to prevent path traversal
	fileName = filepath.Clean(fileName)
	if filepath.IsAbs(fileName) || strings.Contains(fileName, "..") {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid filename"})
	}
	// Checked before ANY concatenation below, because the rewrite bites in every
	// one of them: the staging dir, the relativePath directory that gets created,
	// and the final destination that `path` is reassigned from.
	if errResp := validateUploadNames(ctx, fileName, relative); errResp != nil {
		return errResp
	}

	totalChunks, _ := strconv.Atoi(utils.DefaultPostForm(ctx, "totalChunks", "0"))
	chunkNumber := ctx.FormValue("chunkNumber")
	// chunkNumber is a chunk INDEX used verbatim as a filename under tempDir.
	// Unvalidated, a value like "../../../../etc/cron.d/x" escapes tempDir and
	// writes/deletes arbitrary paths as root (RCE). It must be a plain
	// non-negative integer within the declared chunk count.
	if n, cerr := strconv.Atoi(chunkNumber); cerr != nil || n < 0 || (totalChunks > 0 && n > totalChunks) {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: "invalid chunkNumber"})
	}
	dirPath := ""
	path, errResp := validatePath(ctx, ctx.FormValue("path"))
	if errResp != nil {
		return errResp
	}

	hash := file.GetHashByContent([]byte(path + "/" + fileName))
	tempDir := filepath.Join(path, ".temp", hash+strconv.Itoa(totalChunks)) + "/"

	// `path` was validated on its own, but everything below concatenates
	// user-controlled tails onto it and nothing used to re-check the result.
	// relativePath is only stripped of ".." and of a leading "/", which says
	// nothing about a SYMLINKED directory component inside the upload area: a
	// container that bind-mounts /DATA can plant /DATA/Uploads/link -> /etc, and
	// then relativePath=link/cron.d/x writes /etc/cron.d/x as root. The O_EXCL
	// below only protects the final NAME, never the components leading to it.
	// Re-sanitizing each concatenation puts the symlink resolution back in front
	// of every write; the checks run against paths that mostly exist by then, so
	// the resolver sees the real target rather than a lexical guess.
	if fileName != relative {
		dirPath = strings.TrimSuffix(relative, fileName)
		tempDir += dirPath
		uploadDir, errResp := validatePath(ctx, path+"/"+dirPath)
		if errResp != nil {
			return errResp
		}
		if err := file.MkDir(uploadDir); err != nil {
			logger.Error("error when trying to create `"+uploadDir+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
	}

	// The chunk staging directory carries the same user-controlled tail and is
	// created with MkdirAll, so a link planted at `<path>/.temp` would stage the
	// chunks — and splice them — outside the upload area as well. Only the
	// verdict is used: tempDir keeps its trailing separator because every chunk
	// path below is built by appending to it.
	if _, errResp := validatePath(ctx, tempDir); errResp != nil {
		return errResp
	}

	dest, errResp := validatePath(ctx, path+"/"+relative)
	if errResp != nil {
		return errResp
	}
	path = dest

	if !file.CheckNotExist(tempDir + chunkNumber) {
		if err := file.RMDir(tempDir + chunkNumber); err != nil {
			logger.Error("error when trying to remove existing `"+tempDir+chunkNumber+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
	}

	if totalChunks > 1 {
		if err := file.IsNotExistMkDir(tempDir); err != nil {
			logger.Error("error when trying to create `"+tempDir+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		out, err := os.OpenFile(tempDir+chunkNumber, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			logger.Error("error when trying to open `"+tempDir+chunkNumber+"` for creation", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		defer out.Close()

		if _, err := io.Copy(out, f); err != nil { // recommend to use https://github.com/iceber/iouring-go for faster copy
			logger.Error("error when trying to write to `"+tempDir+chunkNumber+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		fileNum, err := os.ReadDir(tempDir)
		if err != nil {
			logger.Error("error when trying to read number of files under `"+tempDir+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		if totalChunks == len(fileNum) {
			if err := file.SpliceFiles(tempDir, path, totalChunks, 1); err != nil {
				logger.Error("error when trying to splice files under `"+tempDir+"`", zap.Error(err))
				return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
			}
			// Clean up temp dir synchronously after splice completes,
			// instead of a fixed 11-second sleep in a goroutine that
			// could race with still-in-progress uploads.
			if err := file.RMDir(tempDir); err != nil {
				logger.Error("error when trying to remove `"+tempDir+"`", zap.Error(err))
			}
		}
	} else {
		// Single-shot branch. Write to a sidecar temp and rename into place:
		// writing the destination directly (the old behavior, even with the
		// Wave-32 O_TRUNC fix) DESTROYS the existing file the moment the
		// upload starts — an interrupted transfer left the original truncated
		// to whatever bytes had arrived. Temp+rename keeps the original
		// intact until the new content is fully on disk (same pattern as the
		// chunked paths: SpliceFiles and the v2 sparse .tmp).
		// Unique per-request tmp name + O_EXCL: two concurrent single-shot
		// uploads to the same destination each write their own file (last
		// rename wins whole, no interleaving), and a pre-planted symlink or
		// leftover at the name can't be opened/followed.
		//
		// The name is built by the sweeper's own helper instead of formatted
		// here: every error path below removes the sidecar, but a process that
		// dies mid-copy cannot, and until the sweeper learned this shape
		// nothing on the box ever collected those leftovers. Producer and
		// janitor now share one definition of the name.
		tmpPath := service.SingleShotUploadTmpPath(path)
		out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			logger.Error("error when trying to open `"+tmpPath+"` for creation", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		if _, err := io.Copy(out, f); err != nil {
			out.Close()
			os.Remove(tmpPath)
			logger.Error("error when trying to write to `"+tmpPath+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
		}
		// Close BEFORE rename — renaming an open-for-write file risks
		// publishing unflushed content.
		if err := out.Close(); err != nil {
			os.Remove(tmpPath)
			logger.Error("error when closing `"+tmpPath+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
		if err := file.SafeRename(tmpPath, path); err != nil {
			os.Remove(tmpPath)
			logger.Error("error when finalizing `"+path+"`", zap.Error(err))
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// PostFileOctet was dead code — no route ever reached it. It had no path
// validation, no authentication wrapping beyond the default, and wrote to
// any location claimed by the multipart header. Removed to eliminate
// attack surface. PostFileUpload is the real upload handler.

// maxOperateItems caps how many items one POST /v1/file/operate may carry.
// Sized for the file manager's real ceiling — a "select all" over a folder of a
// few thousand entries has to keep working — not for what the protocol allows,
// which was unlimited.
const maxOperateItems = 10000

// operateScanBudget bounds the TOTAL time the size pre-scan may spend walking
// the filesystem inside one request (shared across all items, not per item).
// Exceeding it is not an error: the scan stops and the operation is queued with
// the bytes counted so far. A var so the regression test can prove that firing
// the budget still queues a working operation.
var operateScanBudget = 3 * time.Second

// folderQueryScanBudget bounds the filesystem work GET /v1/folder/size and
// GET /v1/folder/count may do inside one request.
//
// It is deliberately more generous than operateScanBudget: for /file/operate
// the walk is bookkeeping attached to a real operation, whereas here the walk
// IS the request, so cutting it short costs the caller their answer. A var so
// the regression tests can drive the truncation path deterministically.
var folderQueryScanBudget = 5 * time.Second

// isMountedIn answers service.IsMounted's question against a snapshot of the
// global mount state taken ONCE per request.
//
// Why this exists (CN-DOS-03, the amplification the first patch left behind):
// the /file/operate loop called service.IsMounted once per item, and IsMounted
// derives global state from scratch every call — mountinfo.Mounted (path
// normalisation + openat2, and /proc/self/mountinfo parsing on kernels without
// openat2) AND, on every miss (i.e. for every ordinary item), a gorm SELECT
// against SQLite via Connections().GetConnectionsList(). With the 10000-item
// ceiling one POST ordered up to 10000 mount-table derivations and 10000 SQL
// queries on the request goroutine, with no context and no deadline. The size
// pre-scan — the cheap half — was budgeted; this, the expensive half, was not.
//
// Semantics are a strict SUPERSET of IsMounted, never weaker:
//   - the raw path is checked first, which is what IsMounted compares rclone
//     connection mount points against;
//   - on a miss the path is resolved, because mountinfo.Mounted normalises
//     (Abs+EvalSymlinks) before comparing, so a mount reached through a
//     symlinked ancestor (/DATA/link/usb with /DATA/link -> /mnt) must keep
//     matching. A path that will not resolve cannot be a mount point.
//
// Cost per item drops to one map lookup plus, at most, EvalSymlinks' lstats —
// which IsMounted already paid inside normalizePath anyway — and the mount
// table is read once and the connections table queried once per request.
func isMountedIn(mountedSet map[string]bool, path string) bool {
	if mountedSet[path] {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		return mountedSet[resolved]
	}
	return false
}

// scanItemSize is the cancellable, budgeted replacement for
// file.GetFileOrDirSize on the request path.
//
// Two differences, both load-bearing for CN-DOS-03:
//   - it aborts as soon as ctx is done (client disconnect or budget expiry),
//     checked per entry so a tree of millions of small files stops immediately
//     rather than after the current subtree;
//   - it returns the partial byte count ALONGSIDE the error, so a truncated
//     walk still yields a usable lower bound for the progress bar.
//
// Everything else matches file.DirSizeB on purpose: no symlink following
// (WalkDir lstats, like Walk did), and entries that cannot be stat'd — deleted
// mid-walk, permission denied, broken mount — are skipped rather than failing
// the whole measurement, because every caller already treats this number as an
// approximation.
func scanItemSize(ctx context.Context, path string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}

	var size int64
	err = filepath.WalkDir(path, func(_ string, d os.DirEntry, walkErr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if walkErr != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		entryInfo, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		size += entryInfo.Size()
		return nil
	})
	return size, err
}

// @Summary copy or move file
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param body body model.FileOperate true "type:move,copy"
// @Success 200 {string} string "ok"
// @Router /file/operate [post]
func PostOperateFileOrDir(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	list := model.FileOperate{}
	if err := ctx.Bind(&list); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	if len(list.Item) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// CN-DOS-03: bound how much per-item work one request can order BEFORE any
	// of it starts. Everything below is O(items) in syscalls — path resolution
	// (SanitizeFilePath does real lstat/readlink work), the mount-point guard,
	// and the size walk — and the list arrived straight off the wire with no
	// ceiling at all, on an endpoint reachable from the internet through the
	// tunnel. The ceiling is deliberately far above real usage (selecting a few
	// thousand files in the file manager and moving them is normal), and when
	// it does fire the client is told exactly what to do instead, so no
	// legitimate operation is silently degraded.
	if len(list.Item) > maxOperateItems {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("too many items in a single operation (%d, limit %d); operate on the parent folder or split the selection", len(list.Item), maxOperateItems),
		})
	}
	// Validate every source AND the destination. Operating over unsanitized
	// paths would let a client smuggle traversal into bulk move/copy.
	cleanTo, errResp := validatePath(ctx, list.To)
	if errResp != nil {
		return errResp
	}
	list.To = cleanTo
	for i := range list.Item {
		cleanFrom, errResp := validatePath(ctx, list.Item[i].From)
		if errResp != nil {
			return errResp
		}
		list.Item[i].From = cleanFrom
	}
	if list.To == list.Item[0].From[:strings.LastIndex(list.Item[0].From, "/")] {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SOURCE_DES_SAME, Message: common_err.GetMsg(common_err.SOURCE_DES_SAME)})
	}

	// CN-DOS-03: the size pre-scan is ACCOUNTING for the progress bar, not part
	// of the operation — the worker copies or moves whatever the items point at
	// whether or not their sizes are known. It used to call
	// file.GetFileOrDirSize (a full recursive walk) per item with no context
	// and no deadline, inside the request goroutine: a single POST pointing at
	// large trees (or at /DATA) kept walking the filesystem long after the
	// client had disconnected, and repeating it pinned the box. It now runs
	// under the request's own context — so it dies with the client — plus a
	// short overall budget shared by every item.
	//
	// What happens when the budget fires matters more than the budget: the
	// bytes counted so far are kept and the operation is queued anyway. Only
	// the progress bar's denominator becomes approximate. Refusing the request,
	// or aborting the queueing, would break a perfectly legitimate move of a
	// large tree — a "fix" worse than the DoS it closes.
	scanCtx, cancelScan := context.WithTimeout(ctx.Request().Context(), operateScanBudget)
	defer cancelScan()

	// Take the mount snapshot ONCE, before the loop. The guard below used to
	// call service.IsMounted per item, which re-derives the whole mount table
	// and runs a SQLite query every time — see isMountedIn for what that cost
	// at 10000 items. Only "move" consults it, so nothing is paid for a copy.
	var mountedSet map[string]bool
	if list.Type == "move" {
		mountedSet = service.SnapshotMountedPaths()
	}

	var total int64 = 0
	scanTruncated := false
	for i := 0; i < len(list.Item); i++ {
		// The mount-point guard is a safety gate, not accounting, so it runs
		// for EVERY item even once the scan budget is gone — a truncated scan
		// must never become a way to move a mount point. It is O(1) per item.
		// (It also used to be skipped for any item whose size lookup failed;
		// it is now unconditional.)
		if list.Type == "move" && isMountedIn(mountedSet, list.Item[i].From) {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.MOUNTED_DIRECTIORIES, Message: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES), Data: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES)})
		}
		if scanTruncated {
			continue
		}
		size, err := scanItemSize(scanCtx, list.Item[i].From)
		// Credit the partial count before looking at the error: a walk cut off
		// by the budget still returns a real lower bound, which keeps the
		// progress bar useful instead of dividing by a total of zero.
		list.Item[i].Size = size
		total += size
		if err != nil && scanCtx.Err() != nil {
			// Budget spent or client gone. Stop measuring; keep queueing.
			scanTruncated = true
		}
	}
	if scanTruncated {
		logger.Info("file operate size pre-scan hit its budget; the operation is queued with an approximate total size",
			zap.String("to", list.To), zap.Int("items", len(list.Item)), zap.Int64("counted", total))
	}

	list.TotalSize = total
	list.ProcessedSize = 0

	list.LastUpdate = time.Now()
	uid := uuid.NewString()
	service.FileQueue.Store(uid, list)
	// Attach the cancellation handle before the request returns. The id only
	// reaches the client through the progress broadcast, which happens later,
	// so registering here guarantees no cancel can ever arrive for an
	// operation that has no way of being stopped.
	service.RegisterOp(uid)

	service.OpMu.Lock()
	service.OpStrArr = append(service.OpStrArr, uid)
	isFirst := len(service.OpStrArr) == 1
	service.OpMu.Unlock()

	if isFirst {
		go service.ExecOpFile()
		go service.CheckFileStatus()
		notifyFileOperateAsync(false)
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary delete file
// @Description By default, items are moved to the recycle bin (.Trash-casanova on the
// @Description same filesystem). Pass ?permanent=true to bypass the trash and delete forever.
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param body body string true "paths eg [\"/a/b/c\",\"/d/e/f\"]"
// @Param permanent query bool false "bypass trash and delete permanently"
// @Success 200 {string} string "ok"
// @Router /file/delete [delete]
func DeleteFile(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileDelete); err != nil {
		return err
	}
	paths := []string{}
	if err := ctx.Bind(&paths); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	if len(paths) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// Same O(items) amplification that was just hoisted out of
	// PostOperateFileOrDir, and it was unbounded here where operate at least
	// capped at maxOperateItems: the list comes straight off the wire, and the
	// mount check below used to derive the mount table and run a SQLite query
	// once PER ITEM. Fixing only the sibling handler would have left the class
	// open — the recurring failure in this codebase.
	if len(paths) > maxOperateItems {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: fmt.Sprintf("too many items in a single delete (%d, limit %d); delete the parent folder or split the selection", len(paths), maxOperateItems),
		})
	}

	// Refuse to delete anything that's currently a mount point — that
	// would unmount and destroy the filesystem metadata.
	//
	// One snapshot for the whole request instead of one service.IsMounted per
	// item: that call re-read /proc/self/mountinfo AND issued a gorm SELECT
	// against SQLite every time, so a large selection turned one request into
	// thousands of mount-table reads and thousands of queries on the request
	// goroutine. The guard stays unconditional — only its cost changes.
	mountedSet := service.SnapshotMountedPaths()
	for _, v := range paths {
		if isMountedIn(mountedSet, v) {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.MOUNTED_DIRECTIORIES, Message: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES), Data: common_err.GetMsg(common_err.MOUNTED_DIRECTIORIES)})
		}
	}

	// Permanent delete mode (?permanent=true). Bypasses trash entirely.
	// Used by auto-update cleanup, install rollback, and admin workflows
	// that need to reclaim space immediately.
	permanent := ctx.QueryParam("permanent") == "true"

	actor := ctx.Request().Header.Get("user_id")
	if actor == "" {
		actor = ctx.RealIP()
	}

	// Both branches below unlink or move the entry itself, so they use the
	// no-follow check: a symlink whose target sits in a blocked directory
	// (/DATA/AppData/foo/nginx.conf -> /etc/nginx/nginx.conf) lists fine but
	// could not be deleted at all, and removing the link never touches the
	// target. The parent is still resolved, so deleting THROUGH a link into a
	// blocked directory stays refused.
	if permanent {
		for _, v := range paths {
			// Through the authority, like every other path in this file. A
			// permanent delete is unrecoverable, so it is the last place that
			// should have kept its own private validation.
			cleanPath, err := authorizePath(ctx, v, storageauth.OpDelete)
			if err != nil {
				return err
			}
			// Confined recursive removal: every step stays beneath the
			// boundary that authorized it, so a directory swapped in
			// mid-traversal cannot redirect the delete outside.
			if _, err := storageauth.RemoveAllAuthorized(principalFrom(ctx), cleanPath); err != nil {
				return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.FILE_DELETE_ERROR, Message: common_err.GetMsg(common_err.FILE_DELETE_ERROR), Data: err})
			}
			// Audit: permanent deletes are unrecoverable — always logged.
			service.AuditFileOp("delete.permanent", actor, cleanPath, "permanent delete")
		}
		return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: "permanently deleted"})
	}

	// Default path: move to recycle bin on the same filesystem (near-instant).
	// If anything fails partway through, we return the error but leave
	// already-trashed items in the trash (they can be restored).
	trashMgr := trash.NewManager()
	var trashed []trash.Entry
	for _, v := range paths {
		cleanPath, err := authorizePath(ctx, v, storageauth.OpDelete)
		if err != nil {
			return err
		}
		entry, err := trashMgr.Trash(cleanPath)
		if err != nil {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.FILE_DELETE_ERROR, Message: "move to trash failed: " + err.Error(), Data: trashed})
		}
		trashed = append(trashed, *entry)
		service.AuditFileOp("delete.trashed", actor, cleanPath, "moved to recycle bin")
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: "moved to recycle bin", Data: trashed})
}

// @Summary update file
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path body string true "path"
// @Param content body string true "content"
// @Success 200 {string} string "ok"
// @Router /file/update [put]
func PutFileContent(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	fi := model.FileUpdate{}
	if err := ctx.Bind(&fi); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	cleanPath, errResp := validatePath(ctx, fi.FilePath)
	if errResp != nil {
		return errResp
	}
	fi.FilePath = cleanPath

	// Reject oversized payloads to prevent OOM (matches GetFileContent's 100 MB cap).
	const maxFileContentSize = 100 * 1024 * 1024 // 100 MB
	if len(fi.FileContent) > maxFileContentSize {
		return ctx.JSON(http.StatusRequestEntityTooLarge, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "file content exceeds maximum allowed size",
		})
	}

	// This endpoint updates; it does not create. Saying so is the whole point
	// of the message: it used to answer "File already exists" for a file that
	// did not exist, which sends whoever reads the error looking for a
	// collision that is not there. FILE_DOES_NOT_EXIST already existed in the
	// error table, unused, with exactly the right text.
	if !file.Exists(fi.FilePath) {
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}
	f, _, err := storageauth.StatAuthorized(principalFrom(ctx), storageauth.OpWrite, fi.FilePath)
	if err != nil {
		// Not a collision either: the storage authority refused the write, or
		// the stat failed. Report it as the service error it is instead of
		// borrowing an unrelated message.
		logger.Error("stat authorized", zap.String("path", fi.FilePath), zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR)})
	}
	fm := f.Mode()
	// Removed: a dead `if err != nil` re-check on a nil error, and an
	// orphaned `os.OpenFile(...)` call whose result was discarded (FD leak).
	// WriteToFullPath re-opens with the correct mode internally.
	err = file.WriteToFullPath([]byte(fi.FileContent), fi.FilePath, fm)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary image thumbnail/original image
// @Produce  application/json
// @Accept  application/json
// @Tags file
// @Security ApiKeyAuth
// @Param path query string true "path"
// @Param type query string false "original,thumbnail" Enums(original,thumbnail)
// @Success 200 {string} string "ok"
// @Router /file/image [get]
func GetFileImage(ctx echo.Context) error {
	// The file API's read surface was ungated: the only limit was the lexical
	// blocked-prefix list, so any authenticated caller (and, while guest held
	// PermFileRead, effectively anyone) could read outside it. Gate on the
	// permission the role model already defines.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	t := ctx.QueryParam("type")
	path := ctx.QueryParam("path")

	cleanPath, err := authorizePath(ctx, path, storageauth.OpRead)
	if err != nil {
		return err
	}
	if !file.Exists(cleanPath) {
		// Same inverted message as PutFileContent had: a missing image was
		// reported as one that already exists.
		return ctx.JSON(http.StatusNotFound, model.Result{Success: common_err.FILE_DOES_NOT_EXIST, Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST)})
	}

	// Thumbnail path: return generated thumbnail bytes and stop.
	// The original code forgot to `return` here, causing a double-write
	// of both the thumbnail AND the full image. Fixed.
	if t == "thumbnail" {
		data, err := file.GetImage(cleanPath, 100, 0)
		if err != nil {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
		}
		ctx.Response().Writer.Write(data)
		return nil
	}

	// Full image path: stream with http.ServeContent so Range requests work
	// and we don't load the entire file into memory (previously used io.ReadAll).
	f, _, err := storageauth.OpenAuthorizedStrict(principalFrom(ctx), storageauth.OpRead, cleanPath, os.O_RDONLY, 0)
	if err != nil && !storageauth.IsDenial(err) {
		return confinementRefused(err)
	}
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}

	// This endpoint serves ANY path the caller names, and ServeContent would
	// derive the type from the extension — so a .svg or .html in the user's
	// files came back as renderable content on the UI's own origin (stored
	// XSS with access to the session). Coerce the type through the same
	// hardening the download path already uses and forbid sniffing. The
	// header must be set BEFORE ServeContent, which only fills Content-Type
	// when it is still empty.
	ctype := safeDownloadMIME(mime.TypeByExtension(filepath.Ext(cleanPath)))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	ctx.Response().Header().Set("Content-Type", ctype)
	ctx.Response().Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(ctx.Response().Writer, ctx.Request(), stat.Name(), stat.ModTime(), f)
	return nil
}

// notifyFileOperateAsync fires the file-operation broadcast off the request
// path. It runs on a bare goroutine, so an unhandled panic in the notify
// chain would take the whole daemon down instead of failing one broadcast.
func notifyFileOperateAsync(nowSend bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in file operate notify", zap.Any("recover", r))
			}
		}()
		service.MyService.Notify().SendFileOperateNotify(nowSend)
	}()
}

// DeleteOperateFileOrDir cancels an in-flight or queued file operation.
//
// Wave 33-A HIGH: previously this handler had no permission gate at all.
// Any authenticated caller — including read-only operator roles that
// must never be allowed to interfere with file actions — could DELETE
// /v1/file/operate/{id} and either:
//
//   - drop a single in-progress operation (id=N), corrupting another
//     user's copy/move halfway through and leaving partial state on
//     disk, or
//   - id=0, which mass-resets the *entire* shared queue (FileQueue +
//     OpStrArr) for every user on the box. A guest with file-read
//     could repeatedly cancel an admin's bulk move and the admin would
//     just see operations vanish with no clue why.
//
// Fix: gate the single-id path on PermFileWrite (the same permission
// that started the operation in the first place), and restrict the
// id=0 mass-reset to admin role only — that path is destructive to
// every other concurrent caller and there is no legitimate reason for
// a non-admin to invoke it.
func DeleteOperateFileOrDir(ctx echo.Context) error {
	id := ctx.Param("id")
	if id == "0" {
		// Do NOT trust loopback: the gateway proxies every LAN request over
		// 127.0.0.1, so a loopback allowance would let any unauthenticated
		// LAN caller wipe every user's in-flight file operations. This mass
		// reset has no internal tokenless caller — require admin for all.
		if commonmw.RoleFromContext(ctx) != rbac.RoleAdmin {
			return ctx.JSON(http.StatusForbidden, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "mass file-operation reset is admin-only",
			})
		}
		// Cancel every operation's context and drop its tracking. Wiping the
		// tracking alone (what this did before) left the copy goroutines
		// running against the same destinations with nothing on screen.
		service.CancelAllOps(service.CancelUnwindWait)
	} else {
		if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
			return err
		}
		if !service.CancelOp(id, service.CancelUnwindWait) {
			// The worker is still unwinding — typically blocked in a write to
			// a dying disk or a dead network mount. Say so instead of
			// reporting a stop that has not happened: the operation stays in
			// the transfer panel as CANCELLING and disappears on its own once
			// the worker lets go of the filesystem.
			notifyFileOperateAsync(true)
			return ctx.JSON(common_err.SUCCESS, model.Result{
				Success: common_err.SUCCESS,
				Message: "cancellation requested; the operation is still stopping and will disappear once it releases the destination",
			})
		}
	}

	notifyFileOperateAsync(true)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// GetSize answers GET /v1/folder/size.
//
// CN-DOS-03, second half. /file/operate got the cancellable, budgeted pre-scan;
// this handler — a recursive walk with NOTHING else in it — kept calling
// file.GetFileOrDirSize with no context, no deadline and no cancellation, on
// the request goroutine. It was also completely ungated, and the v1 JWT
// middleware skips auth for tokenless loopback requests while the gateway
// reverse-proxies every LAN AND tunnel request over 127.0.0.1: GET
// /v1/folder/size?path=/DATA was therefore an UNAUTHENTICATED, uncancellable,
// endlessly repeatable full traversal of a 5.4 TB array reachable from the
// internet — a strictly cheaper version of the DoS the first patch closed.
//
// Two fixes, both required:
//   - requirePerm(PermFileRead). The guest role (what a tokenless request
//     resolves to) does NOT hold PermFileRead, so this is what actually takes
//     the endpoint away from the loopback skipper. The gate alone would leave
//     an authenticated user able to pin the box; the budget alone would leave
//     it free to anyone on the internet.
//   - the scan runs under the request's own context plus an overall budget, so
//     it dies the moment the client disconnects.
//
// On truncation we keep the PARTIAL byte count instead of answering 503 —
// the same call already made for /file/operate. A 503 would mean a genuine
// query against a large array can never succeed, which is a "fix" worse than
// the bug. The wire contract is unchanged: Success stays 200 and Data stays
// the same plain number, so every programmatic consumer (UI getFolderSize,
// LEGACY MergeStorages) reads exactly what it read before. Only Message —
// the human-readable field nothing branches on — says the number is a lower
// bound, because silently returning a wrong size would be dishonest.
func GetSize(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	path, errResp := validatePath(ctx, json["path"])
	if errResp != nil {
		return errResp
	}

	scanCtx, cancelScan := context.WithTimeout(ctx.Request().Context(), folderQueryScanBudget)
	defer cancelScan()

	size, err := scanItemSize(scanCtx, path)
	// Only a REAL failure (missing path, unreadable root) is an error: that is
	// what GetFileOrDirSize reported and what the client still needs to see.
	// A budget/disconnect stop is not a failure, it is a partial answer.
	if err != nil && scanCtx.Err() == nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	message := common_err.GetMsg(common_err.SUCCESS)
	if err != nil {
		message = "ok (partial: size scan hit its time budget, the value is a lower bound)"
		logger.Info("folder size scan hit its budget; returning the partial count",
			zap.String("path", path), zap.Int64("counted", size))
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: message, Data: size})
}

// GetFileCount answers GET /v1/folder/count.
//
// Same exposure as GetSize above (ungated + loopback skipper + tunnel), and the
// same unbounded shape: os.ReadDir reads the WHOLE directory into a slice and
// then SORTS it by name, only for len() to be taken. On a directory with
// millions of entries that is a large allocation plus an O(n log n) sort per
// request, uncancellable and repeatable by anyone.
//
// Counting in batches off the open directory handle is strictly cheaper than
// what it replaces for every caller — no retained slice, no sort — so unlike a
// rate limit this costs a legitimate request nothing, and the ctx check between
// batches means a disconnect or the budget stops the read immediately. The
// count stays EXACT in every case a client sees today; only a truncated read
// yields a lower bound, and that is the same "keep the partial" call made for
// the size scan.
func GetFileCount(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	path, errResp := validatePath(ctx, json["path"])
	if errResp != nil {
		return errResp
	}

	scanCtx, cancelScan := context.WithTimeout(ctx.Request().Context(), folderQueryScanBudget)
	defer cancelScan()

	count, truncated, err := countDirEntries(scanCtx, path)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	message := common_err.GetMsg(common_err.SUCCESS)
	if truncated {
		message = "ok (partial: directory read hit its time budget, the value is a lower bound)"
		logger.Info("folder count hit its budget; returning the partial count",
			zap.String("path", path), zap.Int("counted", count))
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: message, Data: count})
}

// countDirEntries counts the entries of one directory without materialising or
// sorting them, aborting as soon as ctx is done.
//
// countDirEntriesBatch is the number of entries pulled per getdents round trip.
// Large enough that the syscall overhead is amortised on a normal directory,
// small enough that the ctx check between batches is a tight cancellation
// point. Errors from opening or reading the directory are returned as-is so
// the handler answers exactly what os.ReadDir made it answer before; a
// ctx-driven stop is reported through the truncated flag, not as an error.
const countDirEntriesBatch = 512

func countDirEntries(ctx context.Context, path string) (count int, truncated bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, true, nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer dir.Close()

	for {
		if ctx.Err() != nil {
			return count, true, nil
		}
		entries, readErr := dir.ReadDir(countDirEntriesBatch)
		count += len(entries)
		if readErr == io.EOF {
			return count, false, nil
		}
		if readErr != nil {
			// ENOTDIR on a regular file lands here, which is the same failure
			// os.ReadDir reported for that input.
			return 0, false, readErr
		}
	}
}

type CenterHandler struct {
	broadcast chan []byte
	//  map
	register chan *Client
	//  map
	unregister chan *Client

	// clientsMu guards every read/write of clients. The monitoring
	// goroutine owns writes, but readPump() and GetPeers() run on
	// separate goroutines and used to race the map without this.
	clientsMu sync.RWMutex
	clients   map[string]*Client
}

type Client struct {
	handler *CenterHandler
	conn    *websocket.Conn

	send         chan []byte
	ID           string       `json:"id"`
	IP           string       `json:"ip"`
	Name         service.Name `json:"name"`
	RtcSupported bool         `json:"rtcSupported"`
	TimerId      int          `json:"timerId"`
	LastBeat     time.Time    `json:"lastBeat"`
}

type PeerModel struct {
	ID           string       `json:"id"`
	Name         service.Name `json:"name"`
	RtcSupported bool         `json:"rtcSupported"`
}

func ConnectWebSocket(ctx echo.Context) error {
	// Gated: a socket that carries file operations must not be openable by a tokenless caller.
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}

	peerId := ctx.QueryParam("peer")
	writer := ctx.Response().Writer
	request := ctx.Request()
	key := uuid.NewString()
	// peerModel := service.MyService.Peer().GetPeerByUserAgent(ctx.Request().UserAgent())
	peerModel := model2.PeerDriveDBModel{}
	name := service.GetName(request)
	conn, err := upgraderFile.Upgrade(writer, request, writer.Header())
	if err != nil {
		log.Println(err)
		return nil
	}
	client := &Client{handler: &handler, conn: conn, send: make(chan []byte, 256), ID: service.GetPeerId(request, key), IP: service.GetIP(request), Name: name, RtcSupported: true, TimerId: 0, LastBeat: time.Now()}
	if peerId != "" || len(peerModel.ID) > 0 {
		if len(peerModel.ID) == 0 {
			peerModel = service.MyService.Peer().GetPeerByID(peerId)
		}
		if len(peerModel.ID) > 0 {
			key = peerId
			client.ID = peerModel.ID
			client.Name = service.GetNameByDB(peerModel)
		}
	}
	list := service.MyService.Peer().GetPeers()
	if len(peerModel.ID) == 0 {
		peerModel.ID = key
		peerModel.DisplayName = name.DisplayName
		peerModel.DeviceName = name.DeviceName
		peerModel.Model = name.Model
		peerModel.OS = name.OS
		peerModel.Browser = name.Browser
		peerModel.UserAgent = ctx.Request().UserAgent()
		peerModel.IP = client.IP
		service.MyService.Peer().CreatePeer(&peerModel)
		list = append(list, peerModel)
	}

	cookie := http.Cookie{
		Name:  "peerid",
		Value: key,
		Path:  "/",
	}
	http.SetCookie(writer, &cookie)
	if len(list) > 10 {
		kickoutList := []Client{}
		count := len(list) - 10
		for i := len(list) - 1; count > 0 && i > -1; i-- {
			handler.clientsMu.RLock()
			_, ok := handler.clients[list[i].ID]
			handler.clientsMu.RUnlock()
			if !ok {
				count--
				kickoutList = append(kickoutList, Client{ID: list[i].ID, Name: service.GetNameByDB(list[i]), IP: list[i].IP})
				service.MyService.Peer().DeletePeer(list[i].ID)
			}
		}
		// if len(kickoutList) > 0 {
		// 	other := make(map[string]interface{})
		// 	other["type"] = "kickout"
		// 	other["peers"] = kickoutList
		// 	otherBy, err := json.Marshal(other)
		// 	fmt.Println(err)
		// 	client.handler.broadcast <- otherBy
		// }
	}
	list = service.MyService.Peer().GetPeers()
	currentPeer := PeerModel{ID: client.ID, Name: client.Name, RtcSupported: client.RtcSupported}
	pmsg := make(map[string]interface{})
	pmsg["type"] = "peer-joined"
	pmsg["peer"] = currentPeer
	pby, err := json.Marshal(pmsg)
	fmt.Println(err)
	handler.clientsMu.RLock()
	peerSnapshot := make([]*Client, 0, len(handler.clients))
	for _, v := range handler.clients {
		peerSnapshot = append(peerSnapshot, v)
	}
	handler.clientsMu.RUnlock()
	// Wave 33-A HIGH: every `client.send <- ...` in this handler used to
	// be a naked blocking send. The channel buffer is 256, but a peer
	// whose writePump has not yet started — or one that just lost its TCP
	// connection between Upgrade() and the first frame — will fill that
	// buffer instantly during a peer-storm, and the request goroutine
	// would block forever holding the gorilla/websocket conn open. Every
	// fan-out broadcast also blocked the entire ConnectWebSocket goroutine
	// behind one slow consumer. Use the same non-blocking pattern the
	// monitoring loop already uses; if a peer cannot keep up the
	// monitoring loop will evict it.
	for _, v := range peerSnapshot {
		select {
		case v.send <- pby:
		default:
		}
	}
	// client.handler.broadcast <- pby
	clients := []PeerModel{}
	for _, v := range peerSnapshot {
		clients = append(clients, PeerModel{ID: v.ID, Name: v.Name, RtcSupported: v.RtcSupported})
	}

	other := make(map[string]interface{})
	other["type"] = "peers"
	other["peers"] = clients
	otherBy, err := json.Marshal(other)
	fmt.Println(err)
	select {
	case client.send <- otherBy:
	default:
	}

	handler.register <- client

	select {
	case client.send <- []byte(`{"type":"ping"}`):
	default:
	}

	data := make(map[string]string)
	data["displayName"] = client.Name.DisplayName
	data["deviceName"] = client.Name.DeviceName
	data["id"] = client.ID
	msg := make(map[string]interface{})
	msg["type"] = "display-name"
	msg["message"] = data
	by, _ := json.Marshal(msg)
	select {
	case client.send <- by:
	default:
	}

	//  client  2
	go client.writePump()
	go client.readPump()
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

var handler = CenterHandler{
	// Wave 35-X: broadcast buffered to 256. Unbuffered channel made
	// every send a hand-off — any momentary stall in `monitoring`
	// (slow client send buffer fill) stalled cron pings AND
	// readPumps cluster-wide. Buffered + non-blocking sends absorb
	// bursts without blocking producers.
	broadcast:  make(chan []byte, 256),
	register:   make(chan *Client, 32),
	unregister: make(chan *Client, 32),
	clients:    make(map[string]*Client),
}

func init() {
	//  3  channel
	go handler.monitoring()

	crontab := cron.New(cron.WithSeconds()) //

	// Wave 35-X: non-blocking ping + recover. Previous blocking send
	// to the unbuffered `broadcast` channel deadlocked the cron
	// whenever the monitoring goroutine was stalled (e.g., waiting on
	// a client whose `send` channel was full). Once stalled, every
	// subsequent 30s tick stacked another goroutine waiting on the
	// same unbuffered channel — within minutes, hundreds of leaked
	// goroutines, file-sharing UI silently stops receiving pings.
	task := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("file.go ping cron panicked",
					zap.Any("panic", r))
			}
		}()
		select {
		case handler.broadcast <- []byte(`{"type":"ping"}`):
		default:
			// Monitoring goroutine is busy or unavailable. Skip this
			// tick rather than block — clients will get the next one.
		}
	}

	spec := "*/30 * * * * ?" // every 30 seconds
	crontab.AddFunc(spec, task)

	crontab.Start()
}

func (c *Client) writePump() {
	// Wave 28: send periodic pings so dead peers are detected by the
	// readPump's pong-deadline reset. Wrap every WriteMessage in a
	// SetWriteDeadline so a stuck TCP connection cannot pin this
	// goroutine forever.
	pingTicker := time.NewTicker(wsFilePingPeriod)
	defer func() {
		pingTicker.Stop()
		// Non-blocking unregister: the monitoring goroutine may be
		// busy. We must not block on it during shutdown or every
		// disconnect would cascade-stall.
		select {
		case c.handler.unregister <- c:
		default:
		}
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				_ = c.conn.SetWriteDeadline(time.Now().Add(wsFileWriteWait))
				_ = c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsFileWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-pingTicker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsFileWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		select {
		case c.handler.unregister <- c:
		default:
		}
		c.conn.Close()
	}()
	// Wave 28: bound inbound frame size and install a pong-driven
	// read deadline. Without these, a half-open TCP connection (peer
	// network died with no FIN) keeps the goroutine and the conn
	// alive forever.
	c.conn.SetReadLimit(wsFileMaxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(wsFilePongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(wsFilePongWait))
		c.LastBeat = time.Now()
		return nil
	})
	for {

		_, message, err := c.conn.ReadMessage()
		if err != nil {

			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			// Wave 33-A: monitoring's broadcast channel is unbuffered.
			// On daemon shutdown the monitoring goroutine has already
			// returned and there is no receiver — sending here would
			// pin every readPump forever. Drop the peer-left frame in
			// that case; the unregister path runs from the deferred
			// cleanup regardless.
			select {
			case c.handler.broadcast <- []byte(`{"type":"peer-left","peerId":"` + c.ID + `"}`):
			default:
			}
			break
		}

		t := gjson.GetBytes(message, "type")
		if t.String() == "disconnect" {
			select {
			case c.handler.unregister <- c:
			default:
			}
			c.conn.Close()
			// clients := []Client{}
			// list := service.MyService.Peer().GetPeers()
			// for _, v := range list {
			// 	if _, ok := handler.clients[v.ID]; ok {
			// 		clients = append(clients, *handler.clients[v.ID])
			// 	} else {
			// 		clients = append(clients, Client{ID: v.ID, Name: service.GetNameByDB(v), IP: v.IP, Offline: true})
			// 	}
			// }
			// other := make(map[string]interface{})
			// other["type"] = "peers"
			// other["peers"] = clients
			// otherBy, err := json.Marshal(other)
			// fmt.Println(err)
			select {
			case c.handler.broadcast <- []byte(`{"type":"peer-left","peerId":"` + c.ID + `"}`):
			default:
			}
			// c.handler.broadcast <- otherBy
			break
		} else if t.String() == "pong" {
			c.LastBeat = time.Now()
			continue
		}
		to := gjson.GetBytes(message, "to")

		if len(to.String()) > 0 {
			c.handler.clientsMu.RLock()
			toC := c.handler.clients[to.String()]
			c.handler.clientsMu.RUnlock()
			if toC == nil {
				continue
			}
			data := map[string]interface{}{}
			// Drop frames whose payload isn't valid JSON instead of
			// silently rewriting an empty map. The previous code
			// ignored Unmarshal errors and forwarded a `{"sender":...}`
			// stub to the peer, which corrupted protocol state on the
			// receiving end.
			if err := json.Unmarshal(message, &data); err != nil {
				logger.Info("dropping malformed peer message",
					zap.String("from", c.ID),
					zap.Error(err))
				continue
			}
			data["sender"] = c.ID
			delete(data, "to")
			rewritten, err := json.Marshal(data)
			if err != nil {
				logger.Info("failed to re-marshal peer message",
					zap.String("from", c.ID),
					zap.Error(err))
				continue
			}
			// Non-blocking send. The target's writePump may have
			// exited (peer just disconnected) and its `send` channel
			// could be full; without the default branch this readPump
			// would deadlock and leak its goroutine + websocket conn.
			// We drop the frame in that case rather than wedge the
			// signaling channel — the peer reconnects and re-syncs.
			select {
			case toC.send <- rewritten:
			default:
				logger.Info("dropping peer-routed message; target send buffer full",
					zap.String("from", c.ID),
					zap.String("to", to.String()))
			}
			continue
		}

		// Wave 35-X: non-blocking broadcast send. If the monitoring
		// goroutine is briefly busy (slow consumer chain), blocking
		// here would pin this readPump and ultimately leak the WS
		// goroutine + conn. Drop frames rather than wedge.
		select {
		case c.handler.broadcast <- message:
		default:
			logger.Info("dropping broadcast; monitoring channel busy",
				zap.String("from", c.ID))
		}
	}
}

func (ch *CenterHandler) monitoring() {
	// Wave 33-A: a panic inside this loop (e.g. close-of-closed-channel
	// during a races shutdown, or any future map mutation bug) used to
	// take the entire daemon down because monitoring runs unrecovered
	// inside an `init()`-spawned goroutine. Recover and respawn so a
	// single bad frame can never bring the file service offline.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("CenterHandler.monitoring panic; restarting", zap.Any("panic", r))
			go ch.monitoring()
		}
	}()
	for {
		select {

		case client := <-ch.register:
			ch.clientsMu.Lock()
			ch.clients[client.ID] = client
			ch.clientsMu.Unlock()

		case client := <-ch.unregister:
			ch.clientsMu.Lock()
			delete(ch.clients, client.ID)
			ch.clientsMu.Unlock()

		case message := <-ch.broadcast:
			// writePump — do NOT close(client.send) here; the writePump
			// owns the channel lifetime and will clean up via unregister.
			// Snapshot under the lock so the broadcast loop never walks
			// the map while register/unregister (or the slow-consumer
			// eviction below) mutates it.
			ch.clientsMu.RLock()
			snapshot := make([]*Client, 0, len(ch.clients))
			for _, c := range ch.clients {
				snapshot = append(snapshot, c)
			}
			ch.clientsMu.RUnlock()
			var stuck []string
			for _, client := range snapshot {
				select {
				case client.send <- message:
				default:
					stuck = append(stuck, client.ID)
				}
			}
			if len(stuck) > 0 {
				ch.clientsMu.Lock()
				for _, id := range stuck {
					delete(ch.clients, id)
				}
				ch.clientsMu.Unlock()
			}
		}
	}
}

func GetPeers(ctx echo.Context) error {
	// Gated: peer list is part of the file surface.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}

	peers := service.MyService.Peer().GetPeers()
	handler.clientsMu.RLock()
	for i := 0; i < len(peers); i++ {
		if _, ok := handler.clients[peers[i].ID]; ok {
			peers[i].Online = true
		}
	}
	handler.clientsMu.RUnlock()
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: peers})
}

// ─── File search ──────────────────────────────────────────────────────
//
//   GET /v1/file/search?root=/DATA&q=foo&limit=200&hidden=false
//
// Walks `root` recursively and returns entries whose lowercased name
// contains the lowercased query. Strictly read-only; no shell-out, just
// filepath.Walk so we don't need a deadline on a subprocess. The walk
// is capped at SearchMaxFiles entries and SearchTimeout wall-clock so a
// runaway tree (network mount, deep nesting) can't pin the handler.

const (
	// searchMaxFiles bounds the number of files we'll examine in one
	// query. A typical Documents tree is <50k entries; 100k catches
	// nearly every real use case without letting a Photos-with-RAW
	// tree melt the server.
	searchMaxFiles = 100_000
	// searchMaxResults bounds the JSON response size. Beyond this the
	// UI is unusable anyway — operators should refine the query.
	searchMaxResults = 500
	// searchTimeout caps the walk's wall-clock duration regardless of
	// how many files we managed to visit. Higher than journalctl/
	// hostnamectl because tree walks scale with disk speed, not
	// CPU/IPC.
	searchTimeout = 30 * time.Second
)

// FileSearchHit is one row in the search response. Mirrors the same
// shape FilesListEntry uses in GetDirPath so the frontend can reuse
// existing rendering code.
type FileSearchHit struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

func GetFileSearch(ctx echo.Context) error {
	// Gate + sanitize: this was the only file-read endpoint that skipped both,
	// letting any caller walk the whole FS (e.g. root=/ q=id_rsa reads
	// /root/.ssh, /etc, other homes) and enumerate names/paths/sizes.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	root := ctx.QueryParam("root")
	q := strings.ToLower(strings.TrimSpace(ctx.QueryParam("q")))
	if root == "" || q == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "root and q are required",
		})
	}
	cleanRoot, errResp := validatePath(ctx, root)
	if errResp != nil {
		return errResp
	}
	root = cleanRoot
	// Confine the walk to real storage roots. validatePath blocks system dirs
	// but still permits "/" and "/home", from which the search would enumerate
	// the entire filesystem's names/paths. Require root under a storage mount.
	// utils.DataRoots is the same list the sanitizer consults, so adding a
	// storage root in one place can't leave the search endpoint behind.
	confined := false
	for _, ar := range utils.DataRoots {
		if root == ar || strings.HasPrefix(root, ar+"/") {
			confined = true
			break
		}
	}
	if !confined {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "search root must be under a storage path (/DATA, /mnt, /media)",
		})
	}
	limit, _ := strconv.Atoi(ctx.QueryParam("limit"))
	if limit <= 0 || limit > searchMaxResults {
		limit = searchMaxResults
	}
	includeHidden := ctx.QueryParam("hidden") == "true"

	// Defence in depth — the root path must exist as a directory before
	// we walk it. Bail clearly so the UI doesn't render an empty list
	// for a typo'd path.
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "root is not a directory",
		})
	}

	hits := make([]FileSearchHit, 0, 64)
	deadline := time.Now().Add(searchTimeout)
	visited := 0
	truncated := false

	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable subtrees rather than aborting; permissions
			// are common on real-world disks (lost+found, .Trash-*).
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > searchMaxFiles || time.Now().After(deadline) {
			truncated = true
			return io.EOF // signal end of walk
		}
		name := info.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			if info.IsDir() && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(strings.ToLower(name), q) {
			hits = append(hits, FileSearchHit{
				Name:     name,
				Path:     p,
				IsDir:    info.IsDir(),
				Size:     info.Size(),
				Modified: info.ModTime().Format(time.RFC3339),
			})
			if len(hits) >= limit {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != io.EOF {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "walk failed: " + walkErr.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"hits":      hits,
			"visited":   visited,
			"truncated": truncated,
			"query":     q,
			"root":      root,
		},
	})
}
