package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/backup"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// backupScheduler owns the cron engine that fires Restic backups on
// their configured schedules. One instance lives for the lifetime of
// the process; it's initialized lazily so the rest of the service can
// boot even if the backup subsystem has a config error.
type backupScheduler struct {
	store *backup.ScheduleStore
	cron  *cron.Cron
	mu    sync.Mutex
	// entryByApp maps an app name to its active cron entry so we can
	// unregister/replace when the schedule changes.
	entryByApp map[string]cron.EntryID
	// running tracks which apps have a backup in progress to prevent
	// concurrent execution for the same app.
	running sync.Map
}

var (
	backupSchedulerOnce sync.Once
	backupSchedulerImpl *backupScheduler
)

// BackupScheduler returns the process-wide scheduler, creating it on
// first call. The store path lives under the state/runtime directory
// passed in during initial boot; for simplicity we hardcode the
// conventional CasaNova location here.
func BackupScheduler() *backupScheduler {
	backupSchedulerOnce.Do(func() {
		store, err := backup.NewScheduleStore(filepath.Join("/var/lib/casaos", "backups"))
		if err != nil {
			logger.Error("failed to initialize backup schedule store", zap.Error(err))
			return
		}
		backupSchedulerImpl = &backupScheduler{
			store:      store,
			cron:       cron.New(),
			entryByApp: make(map[string]cron.EntryID),
		}
	})
	return backupSchedulerImpl
}

// Start loads every persisted schedule and registers a cron entry for
// each enabled one, then kicks the cron engine. Safe to call once at
// startup after the service is otherwise ready.
func (s *backupScheduler) Start() {
	if s == nil {
		return
	}
	for _, sch := range s.store.All() {
		if sch.Enabled {
			if err := s.register(sch); err != nil {
				logger.Error("failed to register backup schedule",
					zap.String("app", sch.AppName), zap.Error(err))
			}
		}
	}
	s.cron.Start()
	logger.Info("backup scheduler started",
		zap.Int("schedule_count", len(s.store.All())))
}

// Stop drains the cron engine. Called during graceful shutdown.
func (s *backupScheduler) Stop() {
	if s == nil {
		return
	}
	ctx := s.cron.Stop()
	// Wave 28: time.After here would leak its 30s Timer past return when
	// the cron context drains first. NewTimer + defer Stop reclaims it
	// the moment Stop() returns.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		logger.Error("backup scheduler stop timed out")
	}
}

// Store returns the underlying ScheduleStore so HTTP handlers can
// CRUD schedules without reaching through the scheduler struct.
func (s *backupScheduler) Store() *backup.ScheduleStore {
	if s == nil {
		return nil
	}
	return s.store
}

// ApplySchedule persists a new or updated schedule and re-registers it
// with the cron engine. Call from the HTTP handler after Validate.
func (s *backupScheduler) ApplySchedule(sch backup.Schedule) error {
	if s == nil {
		return nil
	}
	if err := s.store.Put(sch); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove any existing entry for this app so we don't end up with
	// two cron entries firing on overlapping schedules.
	if id, ok := s.entryByApp[sch.AppName]; ok {
		s.cron.Remove(id)
		delete(s.entryByApp, sch.AppName)
	}
	if sch.Enabled {
		return s.register(sch)
	}
	return nil
}

// RemoveSchedule deletes the persisted record and cancels the cron entry.
func (s *backupScheduler) RemoveSchedule(appName string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if id, ok := s.entryByApp[appName]; ok {
		s.cron.Remove(id)
		delete(s.entryByApp, appName)
	}
	s.mu.Unlock()
	return s.store.Delete(appName)
}

// RunNow triggers a backup for the named app immediately, outside its
// regular schedule. Blocks until the operation completes so the HTTP
// handler can return a meaningful status code to the client.
func (s *backupScheduler) RunNow(ctx context.Context, appName string) error {
	if s == nil {
		return nil
	}
	sch, ok := s.store.Get(appName)
	if !ok {
		return errNoSchedule(appName)
	}
	// The outcome comes back from the run itself, not from re-reading the
	// persisted state afterwards.
	//
	// Re-reading looked equivalent and was not. runOnce returns early when a
	// backup for the same app is already in flight, so the LastError read
	// back belonged to the PREVIOUS run: a manual trigger during a running
	// backup answered "backup complete" for a backup that never started, or
	// surfaced a stale failure for one that never happened. And when
	// schedules.json could not be written, the failure never reached the
	// store at all, so a FAILED backup read back as success. Both times the
	// screen could not tell "it ran and came out like this" from "it did not
	// run at all".
	return s.runOnce(ctx, sch)
}

// register adds a cron entry for the schedule. Caller must hold s.mu.
func (s *backupScheduler) register(sch backup.Schedule) error {
	id, err := s.cron.AddFunc(sch.Cron, func() {
		// Each run runs in its own background context — cron entries
		// shouldn't be cancelled when an API request finishes.
		//
		// Wave 35-X: 1-hour deadline so a stuck rclone/tar/upload does
		// not pin a goroutine + the per-app `running` lock forever
		// (which would block all future cron triggers for the same app).
		runCtx, runCancel := context.WithTimeout(context.Background(), 1*time.Hour)
		defer runCancel()
		// Nothing to return to on a cron tick; runOnce has already logged
		// and recorded the outcome. Discarded explicitly so it stays visible
		// that an error exists here and is dropped on purpose.
		_ = s.runOnce(runCtx, sch)
	})
	if err != nil {
		return err
	}
	s.entryByApp[sch.AppName] = id
	return nil
}

// runOnce executes a single backup + retention pass and records the
// outcome on the schedule. Emits user-facing notifications for both
// success (info) and failure (error) so the user sees backup status
// without having to open the logs.
func (s *backupScheduler) runOnce(ctx context.Context, sch backup.Schedule) error {
	// Prevent concurrent backups for the same app.
	if _, loaded := s.running.LoadOrStore(sch.AppName, true); loaded {
		logger.Info("backup already in progress, skipping",
			zap.String("app", sch.AppName))
		return ErrBackupAlreadyRunning
	}
	defer s.running.Delete(sch.AppName)

	logger.Info("backup run starting",
		zap.String("app", sch.AppName),
		zap.Strings("paths", sch.Paths))

	mgr := backup.NewRestic(sch.Repo)
	// "Is the repository there?" has three answers, and only two of them are
	// yes and no. Initialising on the third — an unreachable repo, a disk
	// that was not mounted, a password that stopped working — creates an
	// empty repository at the mountpoint and sends every nightly run into it,
	// reporting success while the real history goes invisible. Stop instead,
	// and say which repository could not be reached.
	exists, err := mgr.Exists(ctx)
	if err != nil {
		return s.recordFailure(sch.AppName, fmt.Errorf("cannot determine whether backup repository %s exists, refusing to initialise a new one: %w", sch.Repo.URL, err))
	}
	if !exists {
		if err := mgr.Init(ctx); err != nil {
			return s.recordFailure(sch.AppName, err)
		}
	}

	snap, err := mgr.BackupPaths(ctx, sch.Paths, "app:"+sch.AppName, "kind:scheduled")
	if err != nil {
		return s.recordFailure(sch.AppName, err)
	}

	// Apply retention after a successful backup — doing it before would
	// prune the snapshot we're about to replace and leave a window where
	// the app has zero backups.
	if retErr := mgr.Forget(ctx, sch.Retention, "app:"+sch.AppName); retErr != nil {
		// Retention failure is non-fatal for the schedule — the snapshot
		// exists, we just didn't prune. Log and move on.
		logger.Error("backup retention failed",
			zap.String("app", sch.AppName), zap.Error(retErr))
	}

	// A result nobody could write down is a result nobody can be shown. The
	// snapshot is real, but from here on every screen reads the previous
	// run's outcome, so the caller is told rather than left to assume.
	if err := s.store.UpdateLast(sch.AppName, time.Now(), snap.ShortID, ""); err != nil {
		logger.Error("backup succeeded but its result could not be recorded",
			zap.String("app", sch.AppName),
			zap.String("snapshot", snap.ShortID), zap.Error(err))
		return fmt.Errorf("backup of %s completed (snapshot %s) but the result could not be recorded: %w", sch.AppName, snap.ShortID, err)
	}

	logger.Info("backup run complete",
		zap.String("app", sch.AppName),
		zap.String("snapshot", snap.ShortID))
	return nil
}

// recordFailure persists a failed run and hands the failure back, so a caller
// waiting on the answer gets the same one the store gets.
//
// The write used to be `_ = s.store.UpdateLast(...)`. With schedules.json
// unwritable, that discarded error meant the failure never landed anywhere the
// caller looked, and RunNow — which read the outcome back out of the store —
// reported the run as a success. A failure that could not be recorded is still
// a failure, and now says so twice.
func (s *backupScheduler) recordFailure(appName string, err error) error {
	logger.Error("backup run failed",
		zap.String("app", appName), zap.Error(err))

	if writeErr := s.store.UpdateLast(appName, time.Now(), "", err.Error()); writeErr != nil {
		logger.Error("backup failure could not be recorded",
			zap.String("app", appName), zap.Error(writeErr))
		return fmt.Errorf("%w (and the failure could not be recorded: %v)", err, writeErr)
	}
	return err
}

// small helpers for these one-off error messages. (They predate the use of
// fmt above; kept because the typed error is what callers compare against,
// not because the package avoids fmt.)

type schedNotFoundError struct{ app string }

func (e schedNotFoundError) Error() string { return "no backup schedule configured for " + e.app }

func errNoSchedule(app string) error { return schedNotFoundError{app: app} }

// ErrBackupAlreadyRunning is returned when a backup for the app is already in
// flight, so the requested run did not happen.
//
// It exists because "skipped" had no way to reach the caller: runOnce returned
// nothing, and RunNow inferred the outcome from persisted state that still
// described the previous run. A manual trigger during a running backup
// therefore answered "backup complete". This is not an error in the run — it
// is the absence of a run, and it has to be distinguishable from both success
// and failure.
var ErrBackupAlreadyRunning = errors.New("a backup for this app is already running; the requested run did not start")
