// Package backup provides per-app backup and restore functionality.
// Inspired by Runtipi's BackupManager — creates tar.gz snapshots of app data
// before updates, allowing rollback if something breaks.
package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

const (
	// DefaultBackupDir is where app backups are stored.
	DefaultBackupDir = "/var/lib/casaos/backups"

	// DefaultMaxBackups is the maximum number of backups to keep per app.
	DefaultMaxBackups = 5
)

// Manager handles backup and restore operations for apps.
type Manager struct {
	backupDir  string
	maxBackups int
}

// NewManager creates a backup manager.
func NewManager() *Manager {
	return &Manager{
		backupDir:  DefaultBackupDir,
		maxBackups: DefaultMaxBackups,
	}
}

// BackupApp creates a tar.gz backup of an app's data directory.
// Returns the path to the created backup file.
func (m *Manager) BackupApp(appName string, dataDir string) (string, error) {
	if appName == "" || dataDir == "" {
		return "", fmt.Errorf("appName and dataDir are required")
	}

	// Validate appName: reject path separators and traversal sequences.
	if strings.ContainsAny(appName, "/\\") || appName == ".." || strings.Contains(appName, "..") {
		return "", fmt.Errorf("invalid app name: must not contain path separators or '..'")
	}

	// Validate data directory exists (use Lstat to not follow symlinks)
	info, err := os.Lstat(dataDir)
	if err != nil {
		return "", fmt.Errorf("data directory does not exist: %s", dataDir)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("data directory is a symlink: %s", dataDir)
	}

	// Create backup directory for this app
	appBackupDir := filepath.Join(m.backupDir, appName)
	// Verify the resolved path stays within the backup directory.
	if !strings.HasPrefix(filepath.Clean(appBackupDir)+"/", filepath.Clean(m.backupDir)+"/") {
		return "", fmt.Errorf("path traversal detected in app name")
	}
	if err := os.MkdirAll(appBackupDir, 0o750); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Generate timestamped filename
	timestamp := time.Now().Format("20060102-150405")
	backupFile := filepath.Join(appBackupDir, fmt.Sprintf("%s-%s.tar.gz", appName, timestamp))

	// Create tar.gz using system tar (more efficient than Go's archive/tar for large directories)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "-czf", backupFile, "-C", filepath.Dir(dataDir), filepath.Base(dataDir))
	if output, err := cmd.CombinedOutput(); err != nil {
		// Clean up partial archive on failure
		os.Remove(backupFile)
		return "", fmt.Errorf("tar failed: %w (output: %s)", err, string(output))
	}

	logger.Info("app backup created",
		zap.String("app", appName),
		zap.String("backup", backupFile),
	)

	// Enforce max backups
	if err := m.pruneOldBackups(appName); err != nil {
		logger.Error("failed to prune old backups", zap.Error(err), zap.String("app", appName))
		// Non-fatal — backup was still created
	}

	return backupFile, nil
}

// RestoreApp restores an app's data from a backup file.
func (m *Manager) RestoreApp(backupFile string, targetDir string) error {
	// Use Lstat to detect symlinks — don't follow them.
	info, err := os.Lstat(backupFile)
	if err != nil {
		return fmt.Errorf("backup file does not exist: %s", backupFile)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup file is a symlink: %s", backupFile)
	}

	// Validate backup file is within the expected backup directory.
	cleanBackup := filepath.Clean(backupFile)
	if !strings.HasPrefix(cleanBackup, filepath.Clean(m.backupDir)+"/") {
		return fmt.Errorf("backup file is outside backup directory: %s", backupFile)
	}

	// Validate target directory is not a sensitive system path.
	cleanTarget := filepath.Clean(targetDir)
	blockedPrefixes := []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/boot", "/dev", "/proc", "/sys", "/root"}
	for _, prefix := range blockedPrefixes {
		if cleanTarget == prefix || strings.HasPrefix(cleanTarget, prefix+"/") {
			return fmt.Errorf("restore target cannot be a system directory: %s", targetDir)
		}
	}
	if cleanTarget == "/" {
		return fmt.Errorf("restore target cannot be /")
	}

	// Extract tar.gz to target directory's parent (tar preserves the directory name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "-xzf", cleanBackup, "--no-overwrite-dir", "-C", filepath.Dir(cleanTarget))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restore failed: %w (output: %s)", err, string(output))
	}

	logger.Info("app restored from backup",
		zap.String("backup", backupFile),
		zap.String("target", targetDir),
	)

	return nil
}

// ListBackups returns all backup files for an app, newest first.
func (m *Manager) ListBackups(appName string) ([]string, error) {
	if strings.ContainsAny(appName, "/\\") || strings.Contains(appName, "..") {
		return nil, fmt.Errorf("invalid app name")
	}
	appBackupDir := filepath.Join(m.backupDir, appName)
	entries, err := os.ReadDir(appBackupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			backups = append(backups, filepath.Join(appBackupDir, e.Name()))
		}
	}

	// Sort newest first
	sort.Sort(sort.Reverse(sort.StringSlice(backups)))
	return backups, nil
}

// pruneOldBackups removes backups exceeding maxBackups, oldest first.
func (m *Manager) pruneOldBackups(appName string) error {
	backups, err := m.ListBackups(appName)
	if err != nil {
		return err
	}

	if len(backups) <= m.maxBackups {
		return nil
	}

	// Remove oldest backups (list is newest-first)
	for _, old := range backups[m.maxBackups:] {
		if err := os.Remove(old); err != nil {
			logger.Error("failed to remove old backup", zap.String("file", old), zap.Error(err))
		} else {
			logger.Info("pruned old backup", zap.String("file", old))
		}
	}

	return nil
}
