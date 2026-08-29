package v1

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

const backupDir = "/var/lib/casaos/backups"

// Permission bits allowed inside an extracted backup tarball.
//
// Wave 34-X: previously `restoreFromTarball` wrote files with
// `os.FileMode(header.Mode)` unmasked. A maliciously crafted backup
// (or a genuine backup made on a developer box where someone had
// chmod 4755'd a shell script) could therefore plant setuid/setgid
// binaries, sticky-bit traps, or world-writable directories during
// a seemingly benign "restore my config" click. We now mask to just
// the read/write/execute bits; special bits are dropped silently.
const tarballSafeModeMask = 0o777

var backupFilenameRe = regexp.MustCompile(`^backup-\d{8}-\d{6}\.tar\.gz$`)

// Directories included in the backup. Only directories that exist are
// archived — missing ones are silently skipped.
var backupSources = []string{
	"/etc/casaos",
	"/etc/casanova",
	"/var/lib/casaos/db",
	"/var/lib/casaos/conf",
}

// ---- manifest embedded in every backup ----

type backupManifest struct {
	Hostname  string   `json:"hostname"`
	Version   string   `json:"version"`
	CreatedAt string   `json:"created_at"`
	Apps      []string `json:"apps"`
}

// ---- response shapes ----

type backupInfo struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Created  string `json:"created"`
	Path     string `json:"path"`
}

// POST /v1/sys/backup/create — Create a system backup tarball.
func PostBackupCreate(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	// Wave 34-X: backup directory MUST be 0700. A backup tarball
	// contains the bcrypt password hashes, the JWT signing key, all
	// OAuth client secrets, and every /etc/casanova/*.conf file. Under
	// the old 0755 any local unprivileged user on the box could
	// `ls /var/lib/casaos/backups/` and `cat` the latest tarball, then
	// walk away with every credential on the system. Also chmod on
	// every create so existing installs get clamped down on the next
	// backup request.
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create backup directory: " + err.Error(),
		})
	}
	_ = os.Chmod(backupDir, 0o700)

	now := time.Now()
	filename := fmt.Sprintf("backup-%s.tar.gz", now.Format("20060102-150405"))
	backupPath := filepath.Join(backupDir, filename)

	// Build manifest.
	hostname, _ := os.Hostname()
	manifest := backupManifest{
		Hostname:  hostname,
		Version:   common.FullVersion(),
		CreatedAt: now.UTC().Format(time.RFC3339),
		Apps:      discoverApps(),
	}

	// Create the tarball.
	if err := createBackupTarball(backupPath, manifest); err != nil {
		// Clean up partial file.
		os.Remove(backupPath)
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "backup failed: " + err.Error(),
		})
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "cannot stat backup file: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: backupInfo{
			Filename: filename,
			Size:     info.Size(),
			Created:  now.UTC().Format(time.RFC3339),
			Path:     backupPath,
		},
	})
}

// GET /v1/sys/backup/list — List existing backups, newest first.
func GetBackupList(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: common_err.GetMsg(common_err.SUCCESS),
			Data:    []backupInfo{},
		})
	}

	matches, _ := filepath.Glob(filepath.Join(backupDir, "backup-*.tar.gz"))
	var list []backupInfo
	for _, m := range matches {
		base := filepath.Base(m)
		if !backupFilenameRe.MatchString(base) {
			continue
		}
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		list = append(list, backupInfo{
			Filename: base,
			Size:     info.Size(),
			Created:  info.ModTime().UTC().Format(time.RFC3339),
			Path:     m,
		})
	}

	// Sort newest first.
	sort.Slice(list, func(i, j int) bool {
		return list[i].Filename > list[j].Filename
	})

	if list == nil {
		list = []backupInfo{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    list,
	})
}

// GET /v1/sys/backup/download — Serve a backup file for download.
//
// Backups contain the full SQLite databases and /etc/casanova configs —
// effectively a credential dump. Gating this on PermSystemConfig keeps
// the same trust level as creating/restoring a backup; PermSystemRead is
// not enough because read-only operators should not be able to walk off
// with the password hashes.
func GetBackupDownload(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	filename := ctx.QueryParam("filename")
	if !backupFilenameRe.MatchString(filename) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid filename",
		})
	}

	backupPath := filepath.Join(backupDir, filename)
	// Verify the resolved path is still inside backupDir (no traversal).
	abs, err := filepath.Abs(backupPath)
	if err != nil || !strings.HasPrefix(abs, backupDir) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "path traversal not allowed",
		})
	}

	if _, err := os.Stat(abs); os.IsNotExist(err) {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "backup not found",
		})
	}

	ctx.Response().Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	return ctx.File(abs)
}

// POST /v1/sys/backup/restore — Restore configuration from a backup.
func PostBackupRestore(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if !backupFilenameRe.MatchString(body.Filename) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid filename",
		})
	}

	backupPath := filepath.Join(backupDir, body.Filename)
	abs, err := filepath.Abs(backupPath)
	if err != nil || !strings.HasPrefix(abs, backupDir) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "path traversal not allowed",
		})
	}

	if _, err := os.Stat(abs); os.IsNotExist(err) {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "backup not found",
		})
	}

	count, err := restoreFromTarball(abs)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "restore failed: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"restored":         true,
			"files_count":      count,
			"requires_restart": true,
		},
	})
}

// DELETE /v1/sys/backup — Delete a backup file.
func DeleteBackup(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if !backupFilenameRe.MatchString(body.Filename) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid filename",
		})
	}

	backupPath := filepath.Join(backupDir, body.Filename)
	abs, err := filepath.Abs(backupPath)
	if err != nil || !strings.HasPrefix(abs, backupDir) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "path traversal not allowed",
		})
	}

	if _, err := os.Stat(abs); os.IsNotExist(err) {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "backup not found",
		})
	}

	if err := os.Remove(abs); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to delete: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// ---- helpers ----

// discoverApps shells out to docker to list running containers (app names).
func discoverApps() []string {
	out, err := execOutBounded(10*time.Second, "docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		return []string{}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var apps []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			apps = append(apps, l)
		}
	}
	return apps
}

// createBackupTarball writes a gzipped tarball containing all backup
// sources plus an embedded manifest.json.
//
// Wave 34-X: previously used `os.Create(dest)` which applies the
// process umask to 0666. On a default umask of 0022 that yielded a
// world-readable 0644 tarball containing every secret on the host.
// Use O_CREATE|O_EXCL|0o600 so only root (the casanova service user)
// can read the file and so we refuse to overwrite an existing backup
// with the same timestamp-based name (which would otherwise silently
// clobber a legitimate backup on a clock-skew edge case).
func createBackupTarball(dest string, manifest backupManifest) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Write the manifest first.
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    "manifest.json",
		Size:    int64(len(manifestBytes)),
		Mode:    0644,
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		return err
	}

	// Walk each source directory and add files.
	for _, src := range backupSources {
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip unreadable files
			}
			// Only archive regular files and directories.
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}

			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return nil
			}
			// Use the absolute path so restore puts files back in the
			// right place. Strip the leading "/" for tar convention, but
			// keep the full directory structure.
			header.Name = strings.TrimPrefix(path, "/")

			if err := tw.WriteHeader(header); err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return nil // skip files we can't read
			}
			defer file.Close()

			_, err = io.Copy(tw, file)
			return err
		})
		if err != nil {
			return fmt.Errorf("walk %s: %w", src, err)
		}
	}

	return nil
}

// restoreFromTarball extracts a backup tarball to the filesystem root.
// Returns the count of files restored.
func restoreFromTarball(src string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	count := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}

		// Skip the manifest — it's metadata, not a config file.
		if header.Name == "manifest.json" {
			continue
		}

		// Restore to the absolute path by prepending "/", then clean
		// to prevent path traversal (e.g. "etc/casaos/../../etc/shadow").
		target := filepath.Clean("/" + header.Name)

		// Safety: only restore into known prefixes (re-check after Clean).
		// Use a "/"-suffixed prefix check so /etc/casaos does not also
		// match a sibling like /etc/casaosX — without the boundary check
		// a crafted tar could escape the allowlist via near-miss names.
		allowed := false
		for _, src := range backupSources {
			if target == src || strings.HasPrefix(target, src+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}

		// Wave 34-X: mask the tar entry mode to just r/w/x bits. This
		// strips setuid, setgid, sticky, and any other special bits a
		// crafted (or genuine-but-dangerous) tarball might carry. The
		// header.Uid / header.Gid fields are also ignored entirely —
		// the extracted files are owned by whatever user the casanova
		// service runs as, which avoids "restore the backup, now the
		// file belongs to UID 0 of a foreign host" surprises.
		safeMode := os.FileMode(header.Mode) & tarballSafeModeMask

		switch header.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			// Reject symlinks and hardlinks — a malicious tar could use them
			// to point outside the allowed restore directories.
			continue
		case tar.TypeDir:
			// Clamp directory modes: never create a world-writable or
			// sticky directory from a tarball. 0o755 is the upper bound
			// for config directories; the intersection with the tar's
			// own bits lets well-formed restrictive tarballs stay
			// restrictive.
			dirMode := safeMode & 0o755
			if dirMode == 0 {
				dirMode = 0o755
			}
			if err := os.MkdirAll(target, dirMode); err != nil {
				return count, err
			}
		case tar.TypeReg:
			// Reject files larger than 100MB to prevent zip-bomb type
			// attacks. Also reject negative sizes — a malformed/crafted
			// tar header with Size = -1 would otherwise pass the upper
			// bound and be passed straight into io.LimitReader, where
			// LimitReader's int64 N would still be ≤0 (a no-op read) but
			// we'd have already created and truncated the destination
			// file. Drop these entries entirely.
			if header.Size < 0 || header.Size > 100*1024*1024 {
				continue
			}
			dir := filepath.Dir(target)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return count, err
			}
			// O_NOFOLLOW: refuse to follow a pre-existing symlink at the
			// target path. Without it, an attacker who can pre-plant
			// /etc/casanova/gateway.ini → /etc/shadow between tarball
			// creation and restore could trick us into writing backup
			// contents through a symlink to an arbitrary root-owned
			// file. The allowlist check above already restricts the
			// destination tree, but O_NOFOLLOW closes the TOCTOU gap
			// on every individual write. Fallback mode 0o600 on files
			// with no usable bits in the header.
			fileMode := safeMode
			if fileMode == 0 {
				fileMode = 0o600
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, fileMode)
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, header.Size+1)); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
			count++
		}
	}

	return count, nil
}
