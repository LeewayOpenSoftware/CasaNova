package v1

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// antivirusMaxRuntime caps every background AV invocation. ClamAV
// runs unattended from a background goroutine, and the previous
// implementation used exec.Command with no timeout — a hung scan
// pinned the "a scan is already in progress" flag forever and held
// its goroutine across the process lifetime. Two hours is longer
// than any healthy full scan should take on a home NAS but short
// enough that a wedged run unblocks itself before the user notices.
const antivirusMaxRuntime = 2 * time.Hour

var (
	avScanRunning   bool
	avScanMu        sync.Mutex
	avUpdateRunning bool
	avUpdateMu      sync.Mutex
)

type AntivirusStatus struct {
	Detected             bool    `json:"detected"`
	Engine               string  `json:"engine"`
	Version              string  `json:"version"`
	DaemonRunning        bool    `json:"daemonRunning"`
	SignatureDbVersion   string  `json:"signatureDbVersion"`
	SignatureDbUpdatedAt *string `json:"signatureDbUpdatedAt"`
	LastScanAt           *string `json:"lastScanAt"`
	LastScanResult       string  `json:"lastScanResult"`
	ThreatsFound         int     `json:"threatsFound"`
}

// GET /v1/sys/antivirus/status — Probe the host for installed antivirus engines.
// Currently supports ClamAV detection via `clamscan --version` and
// daemon check via `clamd` or `clamav-daemon` service status.
func GetAntivirusStatus(ctx echo.Context) error {
	// requireSystemPrincipal, not requirePerm: guest holds PermSystemRead, and
	// a tokenless caller resolves to guest, so the bare permission check was a
	// no-op against an unauthenticated request. See system.go's comment on
	// requireSystemPrincipal for the full mechanism.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	status := AntivirusStatus{
		Detected:       false,
		Engine:         "",
		LastScanResult: "clean",
	}

	// Probe ClamAV
	if path, err := exec.LookPath("clamscan"); err == nil && path != "" {
		status.Detected = true
		status.Engine = "clamav"

		// Get version
		if out, err := exec.Command("clamscan", "--version").Output(); err == nil {
			version := strings.TrimSpace(string(out))
			// Output is like: ClamAV 1.0.0/27100/Wed Oct 16 08:51:27 2024
			parts := strings.SplitN(version, "/", 3)
			if len(parts) >= 1 {
				status.Version = strings.TrimPrefix(parts[0], "ClamAV ")
			}
			if len(parts) >= 2 {
				status.SignatureDbVersion = parts[1]
			}
			if len(parts) >= 3 {
				// Parse the date from ClamAV output
				t, err := time.Parse("Mon Jan 2 15:04:05 2006", strings.TrimSpace(parts[2]))
				if err == nil {
					iso := t.UTC().Format(time.RFC3339)
					status.SignatureDbUpdatedAt = &iso
				}
			}
		}

		// Check if clamd daemon is running
		// Try multiple service names used by different distros
		for _, svc := range []string{"clamav-daemon", "clamd", "clamd@scan"} {
			if out, err := exec.Command("systemctl", "is-active", svc).Output(); err == nil {
				if strings.TrimSpace(string(out)) == "active" {
					status.DaemonRunning = true
					break
				}
			}
		}

		// Check for last scan log
		scanLogPaths := []string{
			"/var/log/clamav/clamav.log",
			"/var/log/clamav/clamscan.log",
			"/var/log/casanova/clamscan.log",
		}
		for _, logPath := range scanLogPaths {
			if fi, err := os.Stat(logPath); err == nil {
				t := fi.ModTime().UTC().Format(time.RFC3339)
				status.LastScanAt = &t
				break
			}
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    status,
	})
}

// POST /v1/sys/antivirus/scan — Trigger a full system scan.
// Runs clamscan in the background and returns immediately with a job ID.
func PostAntivirusScan(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("clamscan"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "ClamAV is not installed",
		})
	}

	avScanMu.Lock()
	if avScanRunning {
		avScanMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "A scan is already in progress",
		})
	}
	avScanRunning = true
	avScanMu.Unlock()

	// Run scan in background under a capped context so a wedged
	// clamscan can't hold the "running" flag forever. We intentionally
	// don't inherit from ctx.Request().Context() — the HTTP request
	// returns immediately after kicking off the scan.
	go func() {
		// Recover from any panic so a crash inside exec wiring doesn't
		// take the whole daemon down with it. Without this defer, a
		// runtime panic in clamscan setup would propagate to the Go
		// scheduler and kill the CasaNova process — taking every other
		// API along with it. Declared first so it runs last (after the
		// unlock + cancel defers below), guaranteeing the running flag
		// is always cleared even on the panic path.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in antivirus scan goroutine",
					zap.Any("panic", r))
			}
		}()
		runCtx, cancel := context.WithTimeout(context.Background(), antivirusMaxRuntime)
		defer cancel()
		defer func() {
			avScanMu.Lock()
			avScanRunning = false
			avScanMu.Unlock()
		}()
		logFile := "/var/log/casanova/clamscan.log"
		maxBytes, maxAge := logRotationConfig("CASANOVA_CLAMSCAN_LOG")
		rotated, err := rotateLog(logFile, maxBytes, maxAge, false)
		if err != nil {
			logger.Error("antivirus log rotation failed", zap.String("path", logFile), zap.Error(err))
		} else if rotated {
			logger.Info("antivirus log rotated", zap.String("path", logFile), zap.Int64("max_bytes", maxBytes), zap.Duration("retention", maxAge))
		}
		os.MkdirAll("/var/log/casanova", 0755)
		cmd := exec.CommandContext(runCtx, "clamscan", "-r", "--infected", "--log="+logFile, "/DATA")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				logger.Error("antivirus scan timed out", zap.Duration("limit", antivirusMaxRuntime))
			} else {
				logger.Info("antivirus scan exited with error", zap.Error(err))
			}
		}
	}()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Scan started",
		Data:    map[string]string{"status": "scanning"},
	})
}

// POST /v1/sys/antivirus/update-signatures — Update ClamAV signature database.
func PostAntivirusUpdateSigs(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("freshclam"); err != nil {
		return ctx.JSON(http.StatusServiceUnavailable, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "freshclam is not installed",
		})
	}

	avUpdateMu.Lock()
	if avUpdateRunning {
		avUpdateMu.Unlock()
		return ctx.JSON(http.StatusConflict, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "A signature update is already in progress",
		})
	}
	avUpdateRunning = true
	avUpdateMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in antivirus signature-update goroutine",
					zap.Any("panic", r))
			}
		}()
		runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		defer func() {
			avUpdateMu.Lock()
			avUpdateRunning = false
			avUpdateMu.Unlock()
		}()
		cmd := exec.CommandContext(runCtx, "freshclam")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				logger.Error("freshclam timed out")
			} else {
				logger.Info("freshclam exited with error", zap.Error(err))
			}
		}
	}()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Signature update started",
	})
}
