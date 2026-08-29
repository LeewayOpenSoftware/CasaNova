package v2

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	"github.com/labstack/echo/v4"
)

// StorageRoot is a user-visible mount point shown in the file browser sidebar.
type StorageRoot struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	Kind       string `json:"kind"` // internal | external | cloud | network
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
	// UsageUnknown says the two counts above are not an answer: the filesystem
	// did not respond to statfs in time. A mount that is unreachable and a
	// mount that is empty produce the same zeros, and the sidebar must not
	// present the first as the second.
	UsageUnknown bool `json:"usageUnknown"`
	// UsageStale says the counts are the last successful measurement of a
	// filesystem that has since stopped answering.
	UsageStale bool `json:"usageStale"`
}

// GetStorageRoots returns all meaningful mount points for the file browser.
// It queries LSBLK via the Disk service and builds a list of mounted volumes,
// classifying each as internal, external (USB), cloud, or network.
func (s *LocalStorage) GetStorageRoots(ctx echo.Context) error {
	blkList := service.MyService.Disk().LSBLK(false)
	roots := []StorageRoot{}
	seen := map[string]bool{}

	for _, disk := range blkList {
		children := disk.Children
		if len(children) == 0 {
			// Disk with no partitions -- treat the disk itself as a partition
			children = append(children, disk)
		}

		for _, part := range children {
			mp := part.MountPoint
			if mp == "" || mp == "/" || mp == "[SWAP]" || strings.HasPrefix(mp, "/boot") || strings.HasPrefix(mp, "/snap") {
				continue
			}
			if seen[mp] {
				continue
			}
			seen[mp] = true

			label := part.Label
			if label == "" {
				label = filepath.Base(mp)
			}

			kind := "internal"
			if disk.Tran == "usb" || part.Tran == "usb" {
				kind = "external"
			}
			// Network mounts
			if strings.HasPrefix(part.FsType, "cifs") || strings.HasPrefix(part.FsType, "nfs") || strings.HasPrefix(part.FsType, "smb") {
				kind = "network"
			}
			// Cloud (rclone/fuse) mounts
			if strings.HasPrefix(part.FsType, "fuse.rclone") {
				kind = "cloud"
			}

			// Bounded probe instead of a bare statfs: this loop walks every
			// mount point the box exposes, network shares included, and a bare
			// statfs on one that stopped answering parks this handler forever
			// while the file browser keeps polling it.
			usage, usageErr := service.MountUsage(mp)

			id := strings.ToLower(strings.ReplaceAll(label, " ", "-"))
			if id == "" {
				id = filepath.Base(part.Path)
			}

			roots = append(roots, StorageRoot{
				ID:           id,
				Label:        label,
				Path:         mp,
				Kind:         kind,
				TotalBytes:   usage.TotalBytes,
				UsedBytes:    usage.UsedBytes,
				UsageUnknown: usageErr != nil,
				UsageStale:   usage.Stale,
			})
		}
	}

	// Also add /DATA if it exists and was not already picked up from LSBLK
	// (it could be a mergerfs mount not visible in LSBLK)
	if !seen["/DATA"] {
		if fi, err := os.Stat("/DATA"); err == nil && fi.IsDir() {
			usage, usageErr := service.MountUsage("/DATA")
			roots = append([]StorageRoot{{
				ID:           "data",
				Label:        "DATA",
				Path:         "/DATA",
				Kind:         "internal",
				TotalBytes:   usage.TotalBytes,
				UsedBytes:    usage.UsedBytes,
				UsageUnknown: usageErr != nil,
				UsageStale:   usage.Stale,
			}}, roots...)
		}
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 0,
		"message": "ok",
		"data":    roots,
	})
}
