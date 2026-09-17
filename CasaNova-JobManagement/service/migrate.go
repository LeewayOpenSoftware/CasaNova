package service

import (
	"os"
	"path/filepath"

	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

// MigrateLegacyTasksStore copies the tasks.json this daemon has been
// reading from oldBase (wherever CommonInfo.RuntimePath currently points —
// the tmpfs default on a stock install, or an operator's own override) to
// the persisted store now rooted at newBase, exactly once, on a strictly
// best-effort basis: any failure just means the migration is skipped, never
// that startup fails.
//
// It only acts when there is something safe to do:
//   - No-op when oldBase and newBase are already the same location (nothing
//     to migrate: source and destination are one file). This is the case
//     most likely to be got wrong, so it is covered by its own test.
//   - Never overwrites an existing file at the new location, and never even
//     reads the old one in that case.
//   - Silent no-op when there is no old tasks.json to read.
//
// A successful copy logs one line: a migration that leaves no trace is one
// nobody can verify happened.
func MigrateLegacyTasksStore(oldBase, newBase string) {
	oldPath := filepath.Join(oldBase, "jobs", "tasks.json")
	newPath := filepath.Join(newBase, "jobs", "tasks.json")

	if oldPath == newPath {
		return
	}

	if _, err := os.Stat(newPath); err == nil {
		// The new store already has a file. Never overwrite it, and never
		// even look at the old one.
		return
	}

	data, err := os.ReadFile(oldPath)
	if err != nil {
		// Typical case: no legacy store ever existed at the old path.
		return
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		logger.Error("failed to create persistent jobs directory for migration",
			zap.Error(err), zap.String("path", filepath.Dir(newPath)))
		return
	}
	if err := commonfile.WriteFileAtomic(newPath, data, 0o600); err != nil {
		logger.Error("failed to write migrated tasks store",
			zap.Error(err), zap.String("from", oldPath), zap.String("to", newPath))
		return
	}

	logger.Info("migrated scheduled-tasks store off the retired tmpfs default",
		zap.String("from", oldPath), zap.String("to", newPath))
}
