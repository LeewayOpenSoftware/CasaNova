package v1

// Disk-based update scanning and application.
//
// Two endpoints:
//   GET  /v1/sys/update/scan-disks  — lists mounted storage + found packages
//   POST /v1/sys/update/from-disk   — (already in update.go, this file adds scan)

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ─── Types ─────────────────────────────────────────────────────────────────

// DiskPackage describes a single CasaNova update package found on disk.
type DiskPackage struct {
	Filename   string `json:"filename"`
	Path       string `json:"path"`
	Version    string `json:"version"`
	Arch       string `json:"arch"`
	SizeBytes  int64  `json:"sizeBytes"`
	ModifiedAt string `json:"modifiedAt"`
}

// DiskMount describes a mounted storage device and any packages found on it.
type DiskMount struct {
	MountPoint string        `json:"mountPoint"`
	Device     string        `json:"device"`
	Label      string        `json:"label"`
	Packages   []DiskPackage `json:"packages"`
}

// packageNameRe matches filenames like casanova-5.0.0.dev11.0408-x86_64.tar.gz
var packageNameRe = regexp.MustCompile(
	`^casanova-(.+?)(?:-([a-zA-Z0-9_]+))?\.tar\.gz$`,
)

// mountPrefixes are the directories we scan for mounted storage.
var mountPrefixes = []string{"/media/", "/mnt/", "/run/media/"}

// ─── Handlers ──────────────────────────────────────────────────────────────

// GetScanDisks scans mounted storage for CasaNova update packages.
// GET /v1/sys/update/scan-disks
func GetScanDisks(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	mounts, err := findExternalMounts()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Failed to read mount points: " + err.Error(),
		})
	}

	// Always surface the internal staging directory first so packages
	// uploaded via POST /update/upload-package (or dropped in by the
	// packaging build) are pickable without re-uploading. List it even
	// when empty so the UI can render a friendly "nothing staged yet"
	// row instead of hiding the option entirely.
	var results []DiskMount
	results = append(results, DiskMount{
		MountPoint: uploadDir,
		Device:     "internal",
		Label:      "Server storage",
		Packages:   scanDirForPackages(uploadDir),
	})

	// Scan each external mount point for packages
	for _, m := range mounts {
		pkgs := scanMountForPackages(m.MountPoint)
		dm := DiskMount{
			MountPoint: m.MountPoint,
			Device:     m.Device,
			Label:      m.Label,
			Packages:   pkgs,
		}
		results = append(results, dm)
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"mounts": results,
		},
	})
}

// ─── Internal helpers ──────────────────────────────────────────────────────

type mountInfo struct {
	Device     string
	MountPoint string
	Label      string
}

// findExternalMounts reads /proc/mounts and returns mount points under
// /media/, /mnt/, and /run/media/.
func findExternalMounts() ([]mountInfo, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool)
	var mounts []mountInfo

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		device := fields[0]
		mountPoint := fields[1]

		// Only consider mount points under our known prefixes
		isExternal := false
		for _, prefix := range mountPrefixes {
			if strings.HasPrefix(mountPoint, prefix) {
				isExternal = true
				break
			}
		}
		if !isExternal {
			continue
		}

		// Deduplicate by mount point
		if seen[mountPoint] {
			continue
		}
		seen[mountPoint] = true

		// Derive a human-readable label from the mount point basename
		label := filepath.Base(mountPoint)

		mounts = append(mounts, mountInfo{
			Device:     device,
			MountPoint: mountPoint,
			Label:      label,
		})
	}

	return mounts, scanner.Err()
}

// scanMountForPackages searches a mount point for casanova-*.tar.gz files.
// It scans both the root of the mount point and one level of subdirectories.
func scanMountForPackages(root string) []DiskPackage {
	var pkgs []DiskPackage

	// Scan root directory
	pkgs = append(pkgs, scanDirForPackages(root)...)

	// Also scan one level of subdirectories (user might put packages in a folder)
	entries, err := os.ReadDir(root)
	if err != nil {
		return pkgs
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(root, e.Name())
			// Skip symlinks to prevent following crafted links on untrusted media
			info, err := os.Lstat(sub)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			pkgs = append(pkgs, scanDirForPackages(sub)...)
		}
	}

	// Sort by modified date descending (newest first)
	sort.Slice(pkgs, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, pkgs[i].ModifiedAt)
		tj, _ := time.Parse(time.RFC3339, pkgs[j].ModifiedAt)
		return ti.After(tj)
	})

	return pkgs
}

// scanDirForPackages globs for casanova-*.tar.gz in a single directory.
func scanDirForPackages(dir string) []DiskPackage {
	pattern := filepath.Join(dir, "casanova-*.tar.gz")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}

	var pkgs []DiskPackage
	for _, path := range matches {
		fi, err := os.Lstat(path)
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}

		base := filepath.Base(path)
		m := packageNameRe.FindStringSubmatch(base)
		if m == nil {
			continue
		}

		version := m[1]
		arch := ""
		if len(m) > 2 {
			arch = m[2]
		}

		pkgs = append(pkgs, DiskPackage{
			Filename:   base,
			Path:       path,
			Version:    version,
			Arch:       arch,
			SizeBytes:  fi.Size(),
			ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339),
		})
	}

	return pkgs
}
