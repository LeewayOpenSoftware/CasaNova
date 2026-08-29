package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/pkg/scheduled"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// runningTasks tracks task IDs currently executing to prevent overlapping runs.
var runningTasks sync.Map

// scheduledManager owns the cron engine that fires user-defined tasks.
// One per process; lazily initialized so the service can start even if
// the task store has a recoverable error.
type scheduledManager struct {
	store       *scheduled.TaskStore
	cron        *cron.Cron
	mu          sync.Mutex
	entryByTask map[string]cron.EntryID

	// Wave 33: per-task cancel functions. Each cron-fired task derives
	// its run context from this entry, so Remove() can cancel an
	// in-flight execution by name. Without this, a task that took
	// longer than its cron interval (or a runaway script) survived
	// being deleted from the store — the cron entry was removed, the
	// goroutine kept running under the rootCtx, and the operator's
	// only recourse was to restart the daemon. The map is keyed by
	// task ID and entries are cleared in Remove() / Apply() right
	// after the cancel is invoked.
	cancelByTask map[string]context.CancelFunc

	// rootCtx is cancelled when Stop() runs. Every cron-fired task
	// inherits this context so a SIGTERM aborts long-running commands
	// instead of letting them run to completion past the graceful
	// shutdown deadline. Previously tasks ran under context.Background()
	// and the daemon was force-killed mid-execution, leaving partial
	// writes and orphan processes.
	rootCtx    context.Context
	rootCancel context.CancelFunc
}

var (
	schedulerMu   sync.Mutex
	schedulerImpl *scheduledManager
)

// Scheduler returns the process-wide manager, creating it lazily.
// Runtime path comes from MyService so tests can point at a temp dir.
// If initialization fails, returns nil and will retry on next call.
func Scheduler() *scheduledManager {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerImpl != nil {
		return schedulerImpl
	}
	runtimePath := "/var/lib/casaos/jobs"
	if MyService != nil && MyService.runtimePath != "" {
		runtimePath = filepath.Join(MyService.runtimePath, "jobs")
	}
	store, err := scheduled.NewTaskStore(runtimePath)
	if err != nil {
		logger.Error("failed to init scheduled tasks store", zap.Error(err))
		return nil
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	schedulerImpl = &scheduledManager{
		store:        store,
		cron:         cron.New(),
		entryByTask:  make(map[string]cron.EntryID),
		cancelByTask: make(map[string]context.CancelFunc),
		rootCtx:      rootCtx,
		rootCancel:   rootCancel,
	}
	return schedulerImpl
}

// Start registers cron entries for every enabled task and begins firing.
// Called once during service startup.
func (s *scheduledManager) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.store.All() {
		if t.Enabled {
			if err := s.registerLocked(t); err != nil {
				logger.Error("failed to register scheduled task",
					zap.String("task_id", t.ID), zap.Error(err))
			}
		}
	}
	s.cron.Start()
	logger.Info("scheduled tasks manager started",
		zap.Int("task_count", len(s.store.All())))
}

// Stop drains the cron engine. Called during graceful shutdown.
func (s *scheduledManager) Stop() {
	if s == nil {
		return
	}
	// Signal every in-flight task that shutdown is happening so their
	// RunTask(ctx, ...) loops break out of exec.Cmd.Wait. cron.Stop()
	// only stops firing new jobs; it doesn't interrupt running ones.
	if s.rootCancel != nil {
		s.rootCancel()
	}
	ctx := s.cron.Stop()
	// Wave 28: NewTimer + defer Stop instead of time.After so the 30s
	// timer doesn't outlive Stop() when cron drains first.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		logger.Error("scheduled tasks stop timed out")
	}
}

// Store exposes the underlying TaskStore so HTTP handlers can CRUD
// without reaching into the manager struct.
func (s *scheduledManager) Store() *scheduled.TaskStore {
	if s == nil {
		return nil
	}
	return s.store
}

// Apply persists a new or updated task and re-registers its cron entry.
// Returns the stored task with ID + timestamps populated.
//
// Wave 26: actorUserID is the authenticated user that initiated the
// change. It is forwarded to the store so CreatedBy / UpdatedBy can be
// stamped for audit purposes.
func (s *scheduledManager) Apply(t *scheduled.Task, actorUserID int) (*scheduled.Task, error) {
	if s == nil {
		return nil, nil
	}
	stored, err := s.store.Put(t, actorUserID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Replace any existing entry for this task so an edit takes effect
	// on the next tick without duplicating fires. Cancel any in-flight
	// run from the previous incarnation so an edit cleanly supersedes it
	// instead of leaving a stale execution finishing under the old
	// definition.
	if id, ok := s.entryByTask[stored.ID]; ok {
		s.cron.Remove(id)
		delete(s.entryByTask, stored.ID)
	}
	if cancel, ok := s.cancelByTask[stored.ID]; ok {
		cancel()
		delete(s.cancelByTask, stored.ID)
	}
	if stored.Enabled {
		if err := s.registerLocked(stored); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// Remove deletes a task and cancels its cron entry. Wave 33: also
// cancels any in-flight execution of the task — the previous version
// only stopped future fires, so a runaway 30-minute backup script
// kept running for the full 30 minutes after the operator hit
// "Delete" with no UI feedback that the kill had landed. Calling the
// per-task cancel signals the run goroutine via context cancellation
// so RunTask aborts at the next checkpoint (typically the next
// exec.Cmd boundary).
func (s *scheduledManager) Remove(id string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if entryID, ok := s.entryByTask[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entryByTask, id)
	}
	if cancel, ok := s.cancelByTask[id]; ok {
		cancel()
		delete(s.cancelByTask, id)
	}
	s.mu.Unlock()
	return s.store.Delete(id)
}

// RunNow fires a task immediately outside its schedule. Blocks until
// the run finishes (or times out) so the caller can return the result.
func (s *scheduledManager) RunNow(ctx context.Context, id string) (*scheduled.Run, error) {
	if s == nil {
		return nil, nil
	}
	t, ok := s.store.Get(id)
	if !ok {
		return nil, errTaskNotFound(id)
	}
	run := scheduled.RunTask(ctx, t)
	if err := s.store.AppendRun(run); err != nil {
		// Same race tolerance as the cron path: if the task was
		// removed mid-RunNow, swallow the not-found result.
		if err != scheduled.ErrTaskNotFound {
			logger.Error("failed to append task run history",
				zap.String("task_id", id), zap.Error(err))
		}
	}
	return &run, nil
}

// registerLocked wires a cron entry for the task. Caller must hold s.mu.
func (s *scheduledManager) registerLocked(t *scheduled.Task) error {
	taskID := t.ID
	id, err := s.cron.AddFunc(t.Cron, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("scheduled task panic recovered", zap.String("task_id", taskID), zap.Any("panic", r))
			}
		}()
		if _, loaded := runningTasks.LoadOrStore(taskID, struct{}{}); loaded {
			logger.Info("skipping overlapping execution", zap.String("task_id", taskID))
			return
		}
		defer runningTasks.Delete(taskID)

		// Wave 33: derive a per-task cancellable context so Remove() /
		// Apply() can interrupt this run. The cancel func is published
		// under s.mu and cleared on completion so a parallel Remove
		// either finds the cancel (and uses it) or sees nothing (we
		// already finished).
		parent := s.rootCtx
		if parent == nil {
			parent = context.Background()
		}
		runCtx, cancel := context.WithCancel(parent)
		s.mu.Lock()
		s.cancelByTask[taskID] = cancel
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			if cur, ok := s.cancelByTask[taskID]; ok && reflectSameCancel(cur, cancel) {
				delete(s.cancelByTask, taskID)
			}
			s.mu.Unlock()
			cancel()
		}()

		run := scheduled.RunTask(runCtx, t)
		if err := s.store.AppendRun(run); err != nil {
			// ErrTaskNotFound happens when the task was Remove()'d
			// while the run was in flight. That is the expected race
			// after Wave 33's orphan-history fix; suppress the noise.
			if err != scheduled.ErrTaskNotFound {
				logger.Error("failed to append task run history",
					zap.String("task_id", t.ID), zap.Error(err))
			}
		}
		logger.Info("scheduled task fired",
			zap.String("task_id", t.ID),
			zap.String("status", string(run.Status)),
			zap.Int("exit_code", run.ExitCode))
	})
	if err != nil {
		return err
	}
	s.entryByTask[t.ID] = id
	return nil
}

// reflectSameCancel returns true iff `current` is exactly the same
// CancelFunc value as `mine`. We can't compare CancelFunc values
// directly (Go forbids func equality), so wrap a pointer-identity
// check via reflect. The clearer alternative is to key cancellations
// by a per-run nonce, but that adds enough plumbing to obscure the
// fix; the small reflect call is local and only fires on completion.
func reflectSameCancel(current, mine context.CancelFunc) bool {
	if current == nil || mine == nil {
		return false
	}
	return fmt.Sprintf("%p", current) == fmt.Sprintf("%p", mine)
}

// errTaskNotFound is a small typed error for the "id unknown" case so
// HTTP handlers can distinguish it from runner failures and return 404.
type taskNotFoundError struct{ id string }

func (e taskNotFoundError) Error() string { return "task not found: " + e.id }
func errTaskNotFound(id string) error     { return taskNotFoundError{id: id} }

// IsTaskNotFound is the type-asserted check for the not-found sentinel.
func IsTaskNotFound(err error) bool {
	_, ok := err.(taskNotFoundError)
	return ok
}
