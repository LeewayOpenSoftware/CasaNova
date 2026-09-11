package v1

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

const (
	defaultLogMaxBytes = int64(50 << 20)
	defaultLogMaxAge   = 90 * 24 * time.Hour
)

// logRotationConfig is deliberately read at the point of use so operators can
// tune the cap without rebuilding the daemon. Invalid values fail closed to
// the explicit defaults; zero or negative values disable rotation for that
// dimension rather than making every file appear eligible.
func logRotationConfig(prefix string) (int64, time.Duration) {
	maxBytes := defaultLogMaxBytes
	if value, err := strconv.ParseInt(os.Getenv(prefix+"_MAX_BYTES"), 10, 64); err == nil && value > 0 {
		maxBytes = value
	}
	maxAge := defaultLogMaxAge
	if value, err := strconv.ParseInt(os.Getenv(prefix+"_RETENTION_DAYS"), 10, 64); err == nil && value > 0 {
		maxAge = time.Duration(value) * 24 * time.Hour
	}
	return maxBytes, maxAge
}

// rotateLog renames a complete log to .1 before a new writer is launched.
// Callers must prove that no process currently owns the log; this function
// intentionally has no unsafe best-effort fallback. Unknown timestamps and
// stat errors are retained rather than treated as ancient files.
func rotateLog(path string, maxBytes int64, maxAge time.Duration, writerActive bool) (bool, error) {
	if writerActive || (maxBytes <= 0 && maxAge <= 0) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat log: %w", err)
	}
	modTime := info.ModTime()
	if modTime.IsZero() || modTime.Unix() <= 0 {
		return false, nil
	}
	tooLarge := maxBytes > 0 && info.Size() > maxBytes
	tooOld := maxAge > 0 && time.Since(modTime) > maxAge
	if !tooLarge && !tooOld {
		return false, nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return false, fmt.Errorf("rotate log: %w", err)
	}
	return true, nil
}

// systemdUnitActive returns an error when the state cannot be established.
// In particular, a missing systemctl must not turn an indeterminate writer
// state into permission to rename a live log.
func systemdUnitActive(unit string) (bool, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false, err
	}
	err := exec.Command("systemctl", "is-active", "--quiet", unit).Run()
	if err == nil {
		return true, nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 3 {
		return false, nil
	}
	return false, err
}

func prepareLogRotation(path, prefix, unit string) {
	active, err := systemdUnitActive(unit)
	if err != nil {
		logger.Info("log rotation skipped: writer state unknown", zap.String("path", path), zap.Error(err))
		return
	}
	maxBytes, maxAge := logRotationConfig(prefix)
	rotated, err := rotateLog(path, maxBytes, maxAge, active)
	if err != nil {
		logger.Error("log rotation failed", zap.String("path", path), zap.Error(err))
		return
	}
	if rotated {
		logger.Info("log rotated", zap.String("path", path), zap.Int64("max_bytes", maxBytes), zap.Duration("retention", maxAge))
	}
}
