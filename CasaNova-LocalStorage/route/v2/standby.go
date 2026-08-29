package v2

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// ── PUT /v2/local_storage/standby ───────────────────────────────────
// Sets the HDD standby (spin-down) timer via hdparm -S.
//
// Body: { "minutes": 30 }
//   minutes = 0  → disable standby
//   minutes > 0  → enable standby after N minutes
//
// hdparm -S encoding:
//   value 0       = off (no standby)
//   value 1-240   = value * 5 seconds  (5 s to 20 min)
//   value 241-251 = (value - 240) * 30 minutes  (30 min to 5.5 h)
//   value 252     = 21 minutes
//   value 253     = vendor-defined (8-12 h)
//   value 255     = 21 min 15 s
//
// We map minutes to the closest hdparm value.

// ── GET /v2/local_storage/standby ───────────────────────────────────
// Returns the current standby setting. Since hdparm doesn't persist
// state beyond the running kernel, we keep a process-level default.

var currentStandbyMinutes atomic.Int32 // 0 = disabled

type standbyRequest struct {
	Minutes int `json:"minutes"`
}

func (s *LocalStorage) GetStandby(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 0,
		"message": "ok",
		"data": map[string]int{
			"minutes": int(currentStandbyMinutes.Load()),
		},
	})
}

func (s *LocalStorage) PutStandby(ctx echo.Context) error {
	// Wave 32: gate behind PermStorageWrite. Without this, ANY
	// authenticated user (including read-only viewers) could shell-exec
	// `hdparm -S` against every rotating disk on the host — parking
	// heads on production drives, killing throughput, and accelerating
	// HDD wear. Spinning up a parked array also costs the user a
	// noticeable latency spike on first access. Same blast radius
	// class as a /shutdown call; it must require an admin-level cap.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}

	var req standbyRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "invalid request body: " + err.Error(),
		})
	}

	if req.Minutes < 0 {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"success": 400,
			"message": "minutes must be >= 0",
		})
	}

	hdparmValue := minutesToHdparm(req.Minutes)

	// Apply to all rotational (HDD) devices by reading from /sys.
	devices, err := listRotationalDevices()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": "failed to enumerate disks: " + err.Error(),
		})
	}

	var errors []string
	for _, dev := range devices {
		if err := setStandby(dev, hdparmValue); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", dev, err.Error()))
		}
	}

	if len(errors) > 0 {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"success": 500,
			"message": "some devices failed: " + strings.Join(errors, "; "),
		})
	}

	currentStandbyMinutes.Store(int32(req.Minutes))

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 0,
		"message": "ok",
	})
}

// minutesToHdparm converts a minutes value to the hdparm -S parameter.
func minutesToHdparm(minutes int) int {
	if minutes == 0 {
		return 0 // disable
	}

	seconds := minutes * 60

	// Range 1-240: value * 5 seconds (5s to 1200s = 20 min)
	if seconds <= 1200 {
		v := seconds / 5
		if v < 1 {
			v = 1
		}
		return v
	}

	// Range 241-251: (value - 240) * 30 minutes (30 min to 330 min)
	thirtyMinBlocks := minutes / 30
	if thirtyMinBlocks < 1 {
		thirtyMinBlocks = 1
	}
	if thirtyMinBlocks > 11 {
		thirtyMinBlocks = 11
	}
	return 240 + thirtyMinBlocks
}

// listRotationalDevices returns device paths like /dev/sda for
// spinning disks (rotational=1).
func listRotationalDevices() ([]string, error) {
	// lsblk reads /sys; should be sub-second. 10s bounds against a
	// hung SCSI probe without rejecting healthy multi-disk hosts.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsblk", "-d", "-n", "-o", "NAME,ROTA").Output()
	if err != nil {
		return nil, err
	}

	var devices []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == "1" {
			devices = append(devices, "/dev/"+fields[0])
		}
	}
	return devices, nil
}

// setStandby runs hdparm -S <value> on a single device.
func setStandby(device string, value int) error {
	// Validate device path to prevent injection.
	if !strings.HasPrefix(device, "/dev/") {
		return fmt.Errorf("invalid device path: %s", device)
	}
	// hdparm -S talks ATA on a single device; 30s catches a dying
	// disk that's not responding to commands. Without this, looping
	// over N disks could pin the handler for N × ∞.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hdparm", "-S", fmt.Sprintf("%d", value), device)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err.Error(), strings.TrimSpace(string(out)))
	}
	return nil
}
