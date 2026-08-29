package v1

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ---- Theme pack manager ----
//
// A theme pack is a .zip uploaded by the user that overrides the
// default CSS-variable tokens + ships optional asset images. Layout
// inside the zip:
//
//   theme.json              — metadata: id, name, author, description, version
//   tokens.css              — CSS rules under html.cn-theme-custom-<id>
//   assets/                 — optional images / fonts / etc, served at
//                              /v1/sys/themes/<id>/assets/...
//
// We store extracted packs under /var/lib/casanova/themes/<id>/. The
// frontend lists them via GET /v1/sys/themes, uploads via POST, deletes
// via DELETE, and switches between them via the existing theme store
// (saved in user preferences). Active selection lives in the theme
// store, not server-side — different users on the same install can
// pick different variants.
//
// v0.1 scope: ingest + list + serve assets + token CSS. We do NOT yet
// validate tokens.css for safety (no @import or url(http:...)
// stripping), only enforce that the zip stays in its own subtree
// (zip-slip guard) and cap the unpacked footprint. The token CSS is
// served with `Content-Type: text/css`, so a malicious pack can at
// worst leak its own contents — it cannot execute JS in the CasaNova
// page because <link rel="stylesheet"> doesn't bring scripts along.

const (
	themesDir         = "/var/lib/casanova/themes"
	themePackMaxBytes = 16 * 1024 * 1024 // 16 MiB unpacked cap per pack
	themePackMaxFiles = 256              // sanity cap on files per pack
)

// themeIDRe locks the pack ID to a friendly subset of characters so it
// can safely become a URL slug AND a CSS class suffix.
var themeIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// ThemePackManifest is the on-disk theme.json shape.
type ThemePackManifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// ThemePackInfo is what we hand back to the frontend listing.
type ThemePackInfo struct {
	ThemePackManifest
	InstalledAt string `json:"installed_at"` // RFC3339, derived from dir mtime
	TokensURL   string `json:"tokens_url"`   // served at /v1/sys/themes/{id}/tokens.css
	AssetsURL   string `json:"assets_url"`   // base for assets, /v1/sys/themes/{id}/assets/
}

// GetThemePacks lists installed theme packs.
//
// GET /v1/sys/themes
func GetThemePacks(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	out := []ThemePackInfo{}
	entries, err := os.ReadDir(themesDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := readThemeManifest(filepath.Join(themesDir, e.Name()))
			if err != nil {
				continue
			}
			stat, _ := os.Stat(filepath.Join(themesDir, e.Name()))
			installedAt := ""
			if stat != nil {
				installedAt = stat.ModTime().UTC().Format("2006-01-02T15:04:05Z")
			}
			out = append(out, ThemePackInfo{
				ThemePackManifest: info,
				InstalledAt:       installedAt,
				TokensURL:         "/v1/sys/themes/" + info.ID + "/tokens.css",
				AssetsURL:         "/v1/sys/themes/" + info.ID + "/assets/",
			})
		}
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    out,
	})
}

// PostThemePack accepts a .zip upload. Multipart form field "file".
//
// POST /v1/sys/themes
func PostThemePack(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if err := os.MkdirAll(themesDir, 0o755); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create themes dir: " + err.Error(),
		})
	}

	fh, err := ctx.FormFile("file")
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "missing upload field 'file'",
		})
	}
	src, err := fh.Open()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to open upload: " + err.Error(),
		})
	}
	defer src.Close()

	// Spool the upload to a temp file so we can use the random-access
	// zip.NewReader. Streaming-zip would also work but spooling keeps
	// the install path identical to what we do for OS update tarballs.
	tmp, err := os.CreateTemp("", "casanova-theme-*.zip")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to spool upload: " + err.Error(),
		})
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "upload copy failed: " + err.Error(),
		})
	}
	if err := tmp.Sync(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "upload sync failed: " + err.Error(),
		})
	}

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "uploaded file is not a valid zip: " + err.Error(),
		})
	}
	defer zr.Close()

	// First pass: locate theme.json, validate the ID, count files,
	// bound the unpacked size. We refuse before writing anything to
	// disk so a malformed pack never half-installs.
	var manifest ThemePackManifest
	var manifestFound bool
	var totalSize int64
	if len(zr.File) > themePackMaxFiles {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: fmt.Sprintf("theme pack exceeds %d file limit", themePackMaxFiles),
		})
	}
	for _, f := range zr.File {
		totalSize += int64(f.UncompressedSize64)
		if totalSize > themePackMaxBytes {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "theme pack exceeds size limit",
			})
		}
		name := zipEntryName(f.Name)
		if name == "" || name == "theme.json" {
			if name == "theme.json" {
				rc, err := f.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(io.LimitReader(rc, 64*1024))
				rc.Close()
				if err != nil {
					continue
				}
				if err := json.Unmarshal(data, &manifest); err == nil {
					manifestFound = true
				}
			}
		}
	}
	if !manifestFound {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "theme pack is missing theme.json",
		})
	}
	if !themeIDRe.MatchString(manifest.ID) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "theme.json id must be 1-48 chars, lowercase letters/digits/-/_",
		})
	}

	destRoot := filepath.Join(themesDir, manifest.ID)
	// Replace any prior install of the same ID atomically by writing to
	// a `.tmp` directory and renaming. A crash mid-install leaves the
	// .tmp around but does NOT clobber the live theme.
	stageRoot := destRoot + ".tmp"
	_ = os.RemoveAll(stageRoot)
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create stage dir: " + err.Error(),
		})
	}

	for _, f := range zr.File {
		name := zipEntryName(f.Name)
		if name == "" {
			continue
		}
		// Zip-slip defense — refuse any entry that resolves outside
		// the stage root.
		target := filepath.Join(stageRoot, name)
		rel, err := filepath.Rel(stageRoot, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			_ = os.RemoveAll(stageRoot)
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "theme pack contains a path-traversal entry: " + f.Name,
			})
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				_ = os.RemoveAll(stageRoot)
				return ctx.JSON(http.StatusInternalServerError, model.Result{
					Success: common_err.SERVICE_ERROR,
					Message: "mkdir during extract: " + err.Error(),
				})
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(stageRoot)
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "mkdir parent: " + err.Error(),
			})
		}
		rc, err := f.Open()
		if err != nil {
			_ = os.RemoveAll(stageRoot)
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "open zip entry: " + err.Error(),
			})
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			_ = os.RemoveAll(stageRoot)
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "create file: " + err.Error(),
			})
		}
		// LimitReader belt-and-braces in case UncompressedSize64 lied.
		if _, err := io.Copy(out, io.LimitReader(rc, themePackMaxBytes)); err != nil {
			out.Close()
			rc.Close()
			_ = os.RemoveAll(stageRoot)
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "write file: " + err.Error(),
			})
		}
		out.Close()
		rc.Close()
	}

	// Swap stage → live.
	_ = os.RemoveAll(destRoot)
	if err := os.Rename(stageRoot, destRoot); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "atomic install failed: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: ThemePackInfo{
			ThemePackManifest: manifest,
			TokensURL:         "/v1/sys/themes/" + manifest.ID + "/tokens.css",
			AssetsURL:         "/v1/sys/themes/" + manifest.ID + "/assets/",
		},
	})
}

// DeleteThemePack removes a pack by ID.
//
// DELETE /v1/sys/themes/:id
func DeleteThemePack(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")
	if !themeIDRe.MatchString(id) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid theme id",
		})
	}
	root := filepath.Join(themesDir, id)
	if _, err := os.Stat(root); err != nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "theme not installed",
		})
	}
	if err := os.RemoveAll(root); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "remove failed: " + err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// ServeThemeTokens streams the pack's tokens.css with the right MIME.
//
// GET /v1/sys/themes/:id/tokens.css
func ServeThemeTokens(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	id := ctx.Param("id")
	if !themeIDRe.MatchString(id) {
		return ctx.NoContent(http.StatusNotFound)
	}
	path := filepath.Join(themesDir, id, "tokens.css")
	if _, err := os.Stat(path); err != nil {
		return ctx.NoContent(http.StatusNotFound)
	}
	ctx.Response().Header().Set("Content-Type", "text/css")
	return ctx.File(path)
}

// ServeThemeAsset proxies arbitrary files under the pack's assets/
// subdirectory. The :rest path param is a wildcard catch-all wired
// into the router with "*". Defends against path traversal via the
// same Rel/HasPrefix check used at install time.
//
// GET /v1/sys/themes/:id/assets/*
func ServeThemeAsset(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	id := ctx.Param("id")
	if !themeIDRe.MatchString(id) {
		return ctx.NoContent(http.StatusNotFound)
	}
	rest := ctx.Param("*")
	if rest == "" {
		return ctx.NoContent(http.StatusNotFound)
	}
	assetsRoot := filepath.Join(themesDir, id, "assets")
	target := filepath.Join(assetsRoot, rest)
	rel, err := filepath.Rel(assetsRoot, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ctx.NoContent(http.StatusBadRequest)
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		return ctx.NoContent(http.StatusNotFound)
	}
	return ctx.File(target)
}

func readThemeManifest(packDir string) (ThemePackManifest, error) {
	var m ThemePackManifest
	data, err := os.ReadFile(filepath.Join(packDir, "theme.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if !themeIDRe.MatchString(m.ID) {
		return m, fmt.Errorf("invalid id %q in installed pack", m.ID)
	}
	return m, nil
}

// zipEntryName normalises a zip entry name to a relative POSIX path
// with no leading separators. Returns "" for an entry that should be
// ignored (empty after cleaning, absolute path, etc.).
func zipEntryName(raw string) string {
	if raw == "" {
		return ""
	}
	name := filepath.ToSlash(raw)
	for strings.HasPrefix(name, "/") {
		name = strings.TrimPrefix(name, "/")
	}
	if name == "" || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return ""
	}
	return name
}
