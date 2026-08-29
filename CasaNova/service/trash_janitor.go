package service

import (
	"context"
	"fmt"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/notifications"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"go.uber.org/zap"
)

// TrashJanitorInterval is how often the background janitor scans trash
// directories for expired items. Daily is plenty — trash retention is
// measured in days, not minutes.
const TrashJanitorInterval = 24 * time.Hour

// StartTrashJanitor launches a background goroutine that periodically
// auto-purges items in the recycle bin that are older than the retention
// period (default 30 days).
//
// Call this once during service startup. The janitor sleeps for a minute
// before the first run so we don't compete with other startup work.
func StartTrashJanitor(ctx context.Context) {
	go func() {
		// Wave 35-X: defer recover() so a panic inside trash.Purge
		// (eg. corrupt metadata, permissions error nil-deref) does
		// not silently kill the janitor for the daemon's lifetime —
		// mirrors the auto_update.go pattern.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("trash janitor goroutine panicked — purge OFFLINE until restart",
					zap.Any("panic", r))
			}
		}()
		// Wave 35-X: initial delay must respect ctx.Done so graceful
		// shutdown isn't pinned for a full minute. Previous
		// time.Sleep(1*time.Minute) ignored cancellation entirely.
		select {
		case <-ctx.Done():
			logger.Info("trash janitor stopped during initial delay")
			return
		case <-time.After(1 * time.Minute):
		}
		runPurge()

		ticker := time.NewTicker(TrashJanitorInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("trash janitor stopped")
				return
			case <-ticker.C:
				runPurge()
			}
		}
	}()
	logger.Info("trash janitor started",
		zap.Duration("interval", TrashJanitorInterval),
		zap.Duration("retention", trash.DefaultRetention),
	)
}

// runPurge scans all trash directories and deletes items past retention.
// Errors are logged but don't stop the loop — next tick will try again.
// On non-zero purges, a user-facing notification is created so the user
// has a visible record of the janitor's work in the notification center.
func runPurge() {
	mgr := trash.NewManager()
	count, err := mgr.Purge(nil)
	if err != nil {
		logger.Error("trash purge failed", zap.Error(err))
		if ns := Notifications(); ns != nil {
			_ = ns.Err(notifications.CategoryStorage,
				"Recycle bin cleanup failed",
				"Automatic purge encountered an error. Check the server logs.")
		}
		return
	}
	if count > 0 {
		logger.Info("trash purge completed", zap.Int("items_removed", count))
		if ns := Notifications(); ns != nil {
			_ = ns.Info(notifications.CategoryStorage,
				"Recycle bin cleaned",
				formatPurgeMessage(count))
		}
	}
}

// formatPurgeMessage pluralizes the purge summary line so the UI copy
// reads correctly for single-item deletions.
func formatPurgeMessage(count int) string {
	if count == 1 {
		return "1 item was automatically removed after its 30-day retention."
	}
	return fmt.Sprintf("%d items were automatically removed after their 30-day retention.", count)
}
