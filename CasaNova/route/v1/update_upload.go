package v1

// Package upload endpoint for the self-update engine.
//
// POST /v1/sys/update/upload-package — the UI's "Select file" action in
// the About settings load-from-disk dialog. Streams a multipart upload
// straight into /var/lib/casanova/updates/ so the user can then hit
// POST /v1/sys/update/from-disk with the returned absolute path without
// ever needing a shell on the server.
//
// Kept in its own file (not update.go) so diffs stay readable: upload is
// the only handler that needs archive/tar + compress/gzip imports.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

const (
	// uploadDir is also where scan-disks picks up previously-uploaded
	// packages so the user doesn't have to re-upload a file that was
	// already staged earlier. Keep in sync with update_disk.go.
	uploadDir = "/var/lib/casanova/updates"

	// maxUploadBytes caps the multipart body. A full CasaNova installer
	// tarball is ~350 MiB with plenty of headroom for growth; 2 GiB is a
	// paranoid upper bound that still fits on the smallest supported
	// root filesystem.
	maxUploadBytes = int64(2 << 30) // 2 GiB
)

// safeNameRe strips any character that isn't alphanumeric, dot, dash, or
// underscore — protects /var/lib/casanova/updates/ from path traversal or
// shell-meta filenames dropped by a compromised browser.
var safeNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// canonicalPackageName turns a (version, arch) tuple into the filename
// format scan-disks (packageNameRe in update_disk.go) can parse back:
// casanova-<version>[-<arch>].tar.gz. Both fields are sanitized so a
// hostile manifest.json can't escape the updates dir.
func canonicalPackageName(version, arch string) string {
	v := safeNameRe.ReplaceAllString(version, "_")
	if v == "" || v == "." || v == ".." {
		return ""
	}
	a := safeNameRe.ReplaceAllString(arch, "_")
	// Map the manifest's arch alias to what scan-disks expects in the
	// filename: manifest ships "amd64", scan-disks sees "x86_64" in
	// official tarballs. Keep the alias mapping tiny and explicit.
	switch a {
	case "amd64":
		a = "x86_64"
	case "arm64":
		a = "aarch64"
	}
	if a == "" {
		return "casanova-" + v + ".tar.gz"
	}
	return "casanova-" + v + "-" + a + ".tar.gz"
}

// uploadManifest mirrors the manifest.json written by the packager in
// build/full/manifest.json. We only need the fields the UI surfaces.
type uploadManifest struct {
	Version        string `json:"version"`
	DisplayVersion string `json:"display_version"`
	BaseVersion    string `json:"base_version"`
	Channel        string `json:"channel"`
	Arch           string `json:"arch"`
}

// PostUpdateUploadPackage receives a multipart upload of a CasaNova
// update package, saves it under /var/lib/casanova/updates/, parses the
// inner manifest.json, and returns {path, version, arch, sizeBytes}.
// The frontend then turns around and calls POST /v1/sys/update/from-disk
// with the returned path.
func PostUpdateUploadPackage(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	// Bound the request body so a malicious or runaway client can't
	// exhaust disk space in the updates dir. MaxBytesReader returns a
	// proper http.MaxBytesError from the multipart parser downstream.
	ctx.Request().Body = http.MaxBytesReader(
		ctx.Response().Writer,
		ctx.Request().Body,
		maxUploadBytes,
	)

	fileHeader, err := ctx.FormFile("file")
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "file field is required: " + err.Error(),
		})
	}

	// Accept .tar.gz / .tgz natively, or .zip (transcoded to .tar.gz on
	// disk after upload). Failing fast on unknown extensions gives a
	// better error than letting it through and tripping the manifest
	// reader later.
	lower := strings.ToLower(fileHeader.Filename)
	isZip := strings.HasSuffix(lower, ".zip")
	if !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") && !isZip {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "file must be a .tar.gz, .tgz, or .zip archive",
		})
	}

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create updates directory: " + err.Error(),
		})
	}

	// Sanitize the original filename — strip any path components and
	// anything outside [A-Za-z0-9._-]. Used only for the staging temp
	// file; the final destination is re-derived from manifest.json.
	origBase := filepath.Base(fileHeader.Filename)
	origBase = safeNameRe.ReplaceAllString(origBase, "_")
	if origBase == "" || origBase == "." || origBase == ".." {
		origBase = "upload.tar.gz"
	}

	// Stream to a temp file next to the final destination, then atomic
	// rename once the manifest has been validated. That way a failed
	// upload never leaves a half-written .tar.gz in the updates dir
	// that scan-disks would subsequently surface as a "real" package.
	tmpPath := filepath.Join(uploadDir, "."+origBase+".part")

	src, err := fileHeader.Open()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to open upload: " + err.Error(),
		})
	}
	defer src.Close()

	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to create temp file: " + err.Error(),
		})
	}

	written, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()

	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to write upload: " + copyErr.Error(),
		})
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to close temp file: " + closeErr.Error(),
		})
	}

	// If the upload was a .zip, transcode it to .tar.gz in-place so the
	// rest of the chain (manifest read, scan-disks, casanova-update.sh)
	// only ever has to deal with one archive format.
	if isZip {
		tgzPath := strings.TrimSuffix(tmpPath, ".part") + ".tgz.part"
		if err := zipToTarGz(tmpPath, tgzPath); err != nil {
			_ = os.Remove(tmpPath)
			_ = os.Remove(tgzPath)
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.INVALID_PARAMS,
				Message: "failed to convert zip: " + err.Error(),
			})
		}
		_ = os.Remove(tmpPath)
		tmpPath = tgzPath
		origBase = strings.TrimSuffix(origBase, ".zip") + ".tar.gz"
	}

	// Parse manifest.json from inside the archive. This also serves as
	// an integrity check: a truncated or non-tarball upload fails here
	// before we ever rename it into place.
	version, arch, manifestErr := readUploadedManifest(tmpPath)
	if manifestErr != nil {
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid update package: " + manifestErr.Error(),
		})
	}

	// Derive the final filename from the manifest so it matches the
	// canonical casanova-<version>-<arch>.tar.gz format scan-disks
	// recognizes. This way a package uploaded today shows up in
	// tomorrow's scan without re-upload. Fall back to the sanitized
	// original name only if the manifest is unusable (shouldn't happen
	// after the successful readUploadedManifest above, but be safe).
	finalName := canonicalPackageName(version, arch)
	if finalName == "" {
		finalName = origBase
	}
	finalPath := filepath.Join(uploadDir, finalName)

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to finalize upload: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"path":      finalPath,
			"version":   version,
			"arch":      arch,
			"sizeBytes": written,
		},
	})
}

// readUploadedManifest streams a .tar.gz and returns the version + arch
// from manifest.json. The package may be flat (manifest.json at the
// archive root) or nested one level deep inside a "casanova-*" directory.
// Anything deeper is ignored on purpose — the real packager puts
// manifest.json at depth 0 or 1.
func readUploadedManifest(archivePath string) (string, string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", "", fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("tar read: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if filepath.Base(name) != "manifest.json" {
			continue
		}
		// Depth check: at most one directory component in front of
		// manifest.json. Prevents matching a random deeply-nested
		// manifest.json that happens to live inside /www/assets or
		// similar.
		if strings.Count(name, string(filepath.Separator)) > 1 {
			continue
		}

		// Cap the read at 64 KiB — a real manifest is a few hundred
		// bytes; anything larger is malicious or malformed.
		data, err := io.ReadAll(io.LimitReader(tr, 64<<10))
		if err != nil {
			return "", "", fmt.Errorf("read manifest: %w", err)
		}
		var m uploadManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return "", "", fmt.Errorf("parse manifest: %w", err)
		}
		if m.Version == "" {
			return "", "", fmt.Errorf("manifest.json has empty version")
		}
		return m.Version, m.Arch, nil
	}
	return "", "", fmt.Errorf("manifest.json not found in archive")
}

// zipToTarGz repacks a .zip into a .tar.gz with identical entry names so
// the rest of the update chain stays tar-only. Symlinks and absolute /
// traversal paths are rejected; per-entry size is capped to keep an
// adversarial zip from filling the disk during transcode.
func zipToTarGz(zipPath, tgzPath string) error {
	const perEntryCap = int64(512 << 20) // 512 MiB per file
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	out, err := os.OpenFile(tgzPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create tgz: %w", err)
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, zf := range zr.File {
		name := filepath.Clean(zf.Name)
		if name == "." || strings.HasPrefix(name, "..") || strings.HasPrefix(name, "/") {
			return fmt.Errorf("zip entry escapes root: %q", zf.Name)
		}
		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("zip contains symlink %q (not allowed)", zf.Name)
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    int64(mode.Perm()),
			ModTime: zf.Modified,
		}
		if zf.FileInfo().IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Name = name + "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			continue
		}
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(zf.UncompressedSize64)
		if hdr.Size > perEntryCap {
			return fmt.Errorf("zip entry %q exceeds size cap", zf.Name)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("open entry %q: %w", zf.Name, err)
		}
		if _, err := io.Copy(tw, io.LimitReader(rc, perEntryCap)); err != nil {
			rc.Close()
			return fmt.Errorf("copy entry %q: %w", zf.Name, err)
		}
		rc.Close()
	}
	return nil
}
