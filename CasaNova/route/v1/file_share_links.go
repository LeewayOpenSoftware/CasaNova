package v1

// Public share links — generate a tokenised HTTP URL for a file or
// folder, optionally password-protected and time-limited.
//
//   POST   /v1/file/share-links            { path, password?, expiresHours?, } → { id, url }
//   GET    /v1/file/share-links            → ShareLink[] (active links, admin view)
//   DELETE /v1/file/share-links/:id        → revoke
//   GET    /public/share/:id               → the public download endpoint
//   POST   /public/share/:id               { password } → download when protected
//
// The /public/share route is intentionally outside the JWT'd /v1 tree —
// it's the whole point of a share link. Defence layers instead:
//   - 128-bit random ID (crypto/rand) — unguessable
//   - optional bcrypt-hashed password
//   - optional expiry, checked per request
//   - the shared path is pinned at creation; the handler never accepts
//     a client-supplied path, so traversal is impossible
//   - folders are streamed as zip via the same archiver the download
//     endpoint uses
//
// Persisted to /var/lib/casanova/share-links.json so links survive
// restarts. Single mutex; share volume is human-scale (tens, not
// millions).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

const shareLinksFile = "/var/lib/casanova/share-links.json"

// ShareLink is the persisted record. PasswordHash is never serialised
// to API responses (json:"-" on the response struct below); it lives
// only in the on-disk store.
type ShareLink struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	IsDir        bool   `json:"isDir"`
	PasswordHash string `json:"passwordHash,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // 0 = never
	Downloads    int    `json:"downloads"`
}

// shareLinkView is the API-facing projection — password hash stripped,
// a `protected` boolean in its place.
type shareLinkView struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	IsDir     bool   `json:"isDir"`
	Protected bool   `json:"protected"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Downloads int    `json:"downloads"`
	URL       string `json:"url"`
}

var (
	shareLinksMu sync.Mutex
	shareLinks   map[string]*ShareLink // id → link
)

func loadShareLinks() {
	if shareLinks != nil {
		return
	}
	shareLinks = map[string]*ShareLink{}
	data, err := os.ReadFile(shareLinksFile)
	if err != nil {
		return
	}
	var list []*ShareLink
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	for _, l := range list {
		shareLinks[l.ID] = l
	}
}

func saveShareLinksLocked() error {
	list := make([]*ShareLink, 0, len(shareLinks))
	for _, l := range shareLinks {
		list = append(list, l)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(shareLinksFile), 0o755); err != nil {
		return err
	}
	tmp := shareLinksFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, shareLinksFile)
}

func newShareID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func viewOf(l *ShareLink, ctx echo.Context) shareLinkView {
	scheme := "http"
	if ctx.Request().TLS != nil || ctx.Request().Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := ctx.Request().Host
	return shareLinkView{
		ID:        l.ID,
		Path:      l.Path,
		IsDir:     l.IsDir,
		Protected: l.PasswordHash != "",
		CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt,
		Downloads: l.Downloads,
		URL:       scheme + "://" + host + "/public/share/" + l.ID,
	}
}

// POST /v1/file/share-links
func PostCreateShareLink(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var req struct {
		Path         string `json:"path"`
		Password     string `json:"password"`
		ExpiresHours int    `json:"expiresHours"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS, Message: err.Error(),
		})
	}
	clean, errResp := validatePath(ctx, req.Path)
	if errResp != nil {
		return errResp
	}
	fi, err := os.Stat(clean)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: common_err.GetMsg(common_err.FILE_DOES_NOT_EXIST),
		})
	}

	id, err := newShareID()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR, Message: err.Error(),
		})
	}
	link := &ShareLink{
		ID:        id,
		Path:      clean,
		IsDir:     fi.IsDir(),
		CreatedAt: time.Now().Unix(),
	}
	if req.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR, Message: err.Error(),
			})
		}
		link.PasswordHash = string(hash)
	}
	if req.ExpiresHours > 0 {
		link.ExpiresAt = time.Now().Add(time.Duration(req.ExpiresHours) * time.Hour).Unix()
	}

	shareLinksMu.Lock()
	loadShareLinks()
	shareLinks[id] = link
	err = saveShareLinksLocked()
	shareLinksMu.Unlock()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to persist share link: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    viewOf(link, ctx),
	})
}

// GET /v1/file/share-links
func GetShareLinks(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	shareLinksMu.Lock()
	loadShareLinks()
	now := time.Now().Unix()
	out := make([]shareLinkView, 0, len(shareLinks))
	for _, l := range shareLinks {
		if l.ExpiresAt > 0 && l.ExpiresAt < now {
			continue // expired — hidden from the panel, purged lazily below
		}
		out = append(out, viewOf(l, ctx))
	}
	shareLinksMu.Unlock()
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    out,
	})
}

// DELETE /v1/file/share-links/:id
func DeleteShareLink(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")
	shareLinksMu.Lock()
	loadShareLinks()
	_, ok := shareLinks[id]
	if ok {
		delete(shareLinks, id)
		_ = saveShareLinksLocked()
	}
	shareLinksMu.Unlock()
	if !ok {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR, Message: "share link not found",
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// resolveShare validates id + optional password and returns the link.
// Returns (nil, response) when the request must stop with an error or
// a password prompt.
// resolveShare returns the link or an *echo.HTTPError the caller must return.
// It must NOT write the response itself: ctx.JSON reports only the serializer's
// error, so a helper that denied with `return nil, ctx.JSON(...)` handed back a
// nil error, the caller's `if errResp != nil` never fired, and serveShare was
// called with a nil link — a wrong password panicked the handler instead of
// refusing it.
func resolveShare(ctx echo.Context, id, password string) (*ShareLink, error) {
	shareLinksMu.Lock()
	loadShareLinks()
	link, ok := shareLinks[id]
	if ok && link.ExpiresAt > 0 && link.ExpiresAt < time.Now().Unix() {
		// Lazy purge of expired links on access.
		delete(shareLinks, id)
		_ = saveShareLinksLocked()
		ok = false
	}
	shareLinksMu.Unlock()
	if !ok {
		return nil, echo.NewHTTPError(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR, Message: "link not found or expired",
		})
	}
	if link.PasswordHash != "" {
		if password == "" {
			// 401 with a sentinel so the public page can render a
			// password form instead of an opaque error.
			return nil, echo.NewHTTPError(http.StatusUnauthorized, model.Result{
				Success: common_err.CLIENT_ERROR, Message: "password required",
			})
		}
		if bcrypt.CompareHashAndPassword([]byte(link.PasswordHash), []byte(password)) != nil {
			return nil, echo.NewHTTPError(http.StatusForbidden, model.Result{
				Success: common_err.CLIENT_ERROR, Message: "wrong password",
			})
		}
	}
	return link, nil
}

// serveShare streams the shared path — single file directly, folder as
// a zip assembled on the fly.
func serveShare(ctx echo.Context, link *ShareLink) error {
	fi, err := os.Stat(link.Path)
	if err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.FILE_DOES_NOT_EXIST,
			Message: "shared file no longer exists",
		})
	}

	shareLinksMu.Lock()
	link.Downloads++
	_ = saveShareLinksLocked()
	shareLinksMu.Unlock()

	if !fi.IsDir() {
		name := filepath.Base(link.Path)
		ctx.Response().Header().Set("Content-Disposition",
			"attachment; filename*=utf-8''"+url.PathEscape(name))
		return ctx.File(link.Path)
	}

	// Folder → zip stream.
	_, ar, err := file.GetCompressionAlgorithm("zip")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR, Message: err.Error(),
		})
	}
	name := filepath.Base(link.Path) + ".zip"
	ctx.Response().Header().Set("Content-Disposition",
		"attachment; filename*=utf-8''"+url.PathEscape(name))
	ctx.Response().Header().Set("Content-Type", "application/zip")
	if err := ar.Create(ctx.Response().Writer); err != nil {
		return err
	}
	defer ar.Close()
	commonDir := filepath.Dir(link.Path)
	return file.AddFile(ar, link.Path, commonDir)
}

// GET /public/share/:id — direct download (no password case) or 401.
func GetPublicShare(ctx echo.Context) error {
	link, errResp := resolveShare(ctx, ctx.Param("id"), ctx.QueryParam("password"))
	if errResp != nil {
		return errResp
	}
	return serveShare(ctx, link)
}

// POST /public/share/:id — password form submission.
func PostPublicShare(ctx echo.Context) error {
	var body struct {
		Password string `json:"password" form:"password"`
	}
	_ = ctx.Bind(&body)
	link, errResp := resolveShare(ctx, ctx.Param("id"), body.Password)
	if errResp != nil {
		return errResp
	}
	return serveShare(ctx, link)
}

// suppress unused import when strings usage shifts in future edits
var _ = strings.TrimSpace
