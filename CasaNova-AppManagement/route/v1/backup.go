package v1

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/backup"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/labstack/echo/v4"
)

// validSnapshotID matches restic snapshot IDs (hex) or the keyword "latest".
var validSnapshotID = regexp.MustCompile(`^(?:latest|[a-f0-9]{8,64})$`)

// Permission policy for backup endpoints:
//   - Read (list schedules, list snapshots) → PermBackupRead
//   - Write (create/update/delete schedule, manual trigger, restore) → PermBackupWrite
//
// These gates deliberately do NOT trust a loopback RemoteAddr: the CasaNova
// gateway reverse-proxies LAN traffic over 127.0.0.1, so a loopback peer is
// not evidence of a trusted internal caller. Scheduled backups are fired by
// this service's own in-process cron engine (service/backup_scheduler.go) and
// never traverse HTTP, so no internal tokenless caller depends on a bypass.
// DENY WITH echo.NewHTTPError, NEVER WITH ctx.JSON.
//
// ctx.JSON returns the SERIALIZER's error, which is nil once the bytes are
// written — so `if err := requireBackupRead(ctx); err != nil { return err }`
// receives nil, the 403 is written, AND THE HANDLER RUNS ANYWAY. Every backup
// route guarded this way performed its work while reporting a denial.
//
// The sibling copy of this mistake in CasaNova-JobManagement guarded task
// creation and execution, which run /bin/sh -c as root.
func requireBackupRead(ctx echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermBackupRead) {
		return echo.NewHTTPError(http.StatusForbidden,
			map[string]string{"message": "permission denied: backup read required"})
	}
	return nil
}

func requireBackupWrite(ctx echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermBackupWrite) {
		return echo.NewHTTPError(http.StatusForbidden,
			map[string]string{"message": "permission denied: backup write required"})
	}
	return nil
}

// ListBackupSchedules returns every configured backup plan.
// GET /v1/backup/schedules
func ListBackupSchedules(ctx echo.Context) error {
	if err := requireBackupRead(ctx); err != nil {
		return err
	}
	sched := service.BackupScheduler()
	if sched == nil || sched.Store() == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	// Strip repo passwords before returning — never expose secrets in API responses.
	schedules := sched.Store().All()
	for i := range schedules {
		schedules[i].Repo.Password = ""
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"schedules": schedules,
	})
}

// PutBackupSchedule creates or updates a schedule for an app.
// PUT /v1/backup/schedules   body: backup.Schedule
func PutBackupSchedule(ctx echo.Context) error {
	if err := requireBackupWrite(ctx); err != nil {
		return err
	}
	var sch backup.Schedule
	if err := ctx.Bind(&sch); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	if err := sch.Validate(); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	sched := service.BackupScheduler()
	if sched == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	if err := sched.ApplySchedule(sch); err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	// Return schedule without the repo password — never echo secrets.
	sch.Repo.Password = ""
	return ctx.JSON(http.StatusOK, sch)
}

// DeleteBackupSchedule removes a schedule.
// DELETE /v1/backup/schedules/:app
func DeleteBackupSchedule(ctx echo.Context) error {
	if err := requireBackupWrite(ctx); err != nil {
		return err
	}
	app := ctx.Param("app")
	if app == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "app name required"})
	}
	sched := service.BackupScheduler()
	if sched == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	if err := sched.RemoveSchedule(app); err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return ctx.NoContent(http.StatusNoContent)
}

// RunBackupNow triggers an immediate backup outside the schedule.
// POST /v1/backup/schedules/:app/run
func RunBackupNow(ctx echo.Context) error {
	if err := requireBackupWrite(ctx); err != nil {
		return err
	}
	app := ctx.Param("app")
	if app == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "app name required"})
	}
	sched := service.BackupScheduler()
	if sched == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	// Give manually-triggered backups a generous but bounded timeout.
	bgCtx, cancel := context.WithTimeout(ctx.Request().Context(), 2*time.Hour)
	defer cancel()
	if err := sched.RunNow(bgCtx, app); err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return ctx.JSON(http.StatusOK, map[string]string{"message": "backup complete"})
}

// ListSnapshots lists all snapshots for an app from its configured repo.
// GET /v1/backup/schedules/:app/snapshots
func ListSnapshots(ctx echo.Context) error {
	if err := requireBackupRead(ctx); err != nil {
		return err
	}
	app := ctx.Param("app")
	if app == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "app name required"})
	}
	sched := service.BackupScheduler()
	if sched == nil || sched.Store() == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	sch, ok := sched.Store().Get(app)
	if !ok {
		return ctx.JSON(http.StatusNotFound, map[string]string{"message": "no schedule configured for app"})
	}
	mgr := backup.NewRestic(sch.Repo)
	snaps, err := mgr.Snapshots(ctx.Request().Context(), "app:"+app)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"snapshots": snaps,
		"count":     len(snaps),
	})
}

// RestoreSnapshot restores a snapshot to the target path. Target defaults
// to the first path in the schedule's Paths when not supplied, so a user
// clicking "restore" in the UI gets in-place restoration by default.
//
// POST /v1/backup/schedules/:app/restore   body: {"snapshot_id":"xxx","target":"/..."}
func RestoreSnapshot(ctx echo.Context) error {
	if err := requireBackupWrite(ctx); err != nil {
		return err
	}
	app := ctx.Param("app")
	if app == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "app name required"})
	}
	var req struct {
		SnapshotID string `json:"snapshot_id"`
		Target     string `json:"target"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	sched := service.BackupScheduler()
	if sched == nil || sched.Store() == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{"message": "backup scheduler not initialized"})
	}
	sch, ok := sched.Store().Get(app)
	if !ok {
		return ctx.JSON(http.StatusNotFound, map[string]string{"message": "no schedule configured for app"})
	}
	// Validate snapshot ID — must be hex or "latest" to prevent argument
	// injection. The empty string used to be exempt from this check and was
	// then promoted to "latest" deeper down, which in a repository shared by
	// several apps means "whichever app backed up most recently". Absent is
	// now as invalid as malformed.
	if !validSnapshotID.MatchString(req.SnapshotID) {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "invalid snapshot_id: must be hex or 'latest'"})
	}

	// Default target to the first scheduled path (the app's own data directory).
	// NEVER default to "/" — that would overwrite the entire filesystem.
	target := req.Target
	if target == "" && len(sch.Paths) > 0 {
		target = sch.Paths[0]
	}
	if target == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "target path is required"})
	}

	// Path traversal guard: reject "..", ensure absolute, block sensitive prefixes.
	cleanTarget := filepath.Clean(target)
	if strings.Contains(cleanTarget, "..") || !filepath.IsAbs(cleanTarget) {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "invalid target path"})
	}
	blockedPrefixes := []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/boot", "/dev", "/proc", "/sys", "/root"}
	for _, prefix := range blockedPrefixes {
		if cleanTarget == prefix || strings.HasPrefix(cleanTarget, prefix+"/") {
			return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "restore target cannot be a system directory"})
		}
	}
	// Additionally reject restoring to filesystem root.
	if cleanTarget == "/" {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": "restore target cannot be /"})
	}

	// Scope the restore to this app's own snapshots. The tag is the same one
	// runOnce stamps on every scheduled backup, and without it "latest"
	// resolves across every app sharing the repository.
	mgr := backup.NewRestic(sch.Repo)
	if err := mgr.Restore(ctx.Request().Context(), req.SnapshotID, cleanTarget, "app:"+app); err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return ctx.JSON(http.StatusOK, map[string]string{"message": "restore complete"})
}
