package route

import (
	"net/http"
	"regexp"
	"strconv"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/pkg/scheduled"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// actorUserID extracts the authenticated user id stamped by the JWT
// middleware. Returns 0 for loopback / unauthenticated callers; the
// store treats 0 as "no actor" and skips the audit-stamp side effects.
func actorUserID(c echo.Context) int {
	raw := c.Request().Header.Get("user_id")
	if raw == "" {
		return 0
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return id
}

// validTaskID matches the format produced by task.go: "task_" + hex digits, max 50 chars.
var validTaskID = regexp.MustCompile(`^task_[0-9a-f]{1,16}$`)

func validateTaskID(c echo.Context) (string, error) {
	id := c.Param("id")
	if !validTaskID.MatchString(id) {
		return "", c.JSON(http.StatusBadRequest, map[string]string{"message": "invalid task ID format"})
	}
	return id, nil
}

// Permission policy for scheduled task endpoints:
//   - Read-only endpoints (list tasks, list run history) → PermSystemRead
//     since tasks can expose sensitive command output
//   - Write endpoints (CRUD, manual trigger) → PermSystemConfig
//     because running arbitrary shell commands on the host is
//     equivalent to a system configuration change — admin-only
//
// No loopback bypass — the JWT skipper (route.go) now validates any token,
// including gateway-proxied browser requests, so RoleFromContext is set and
// the RBAC check below is authoritative. A tokenless loopback caller reaches
// here with an empty role and is correctly denied (blocks the unauthenticated
// root-RCE path via task create/run).
// DENY WITH echo.NewHTTPError, NEVER WITH ctx.JSON.
//
// ctx.JSON returns the SERIALIZER's error, which is nil once the bytes are
// written. So `if err := requireTaskRead(c); err != nil { return err }` receives nil,
// the 403 is written to the response, AND THE HANDLER CONTINUES. The caller
// sees a permission denial while the guarded work runs anyway.
//
// Verified live on the production box before this fix:
// GET :85/v2/job_management/tasks with no token at all answered 200 {"tasks":[]}
// instead of 403 — and the same shape guarded PUT /tasks and
// POST /tasks/:id/run, which execute an arbitrary command with /bin/sh -c as
// root. That is remote unauthenticated root execution, reachable from the
// internet through the Cloudflare tunnel.
//
// Returning an error VALUE is what aborts an echo handler chain. This is the
// third time this convention has been wrong in this codebase; it is now
// asserted by a test that fails on any gate whose denial path calls ctx.JSON.
func requireTaskRead(c echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(c), rbac.PermSystemRead) {
		return echo.NewHTTPError(http.StatusForbidden,
			map[string]string{"message": "permission denied"})
	}
	return nil
}

// DENY WITH echo.NewHTTPError, NEVER WITH ctx.JSON.
//
// ctx.JSON returns the SERIALIZER's error, which is nil once the bytes are
// written. So `if err := requireTaskWrite(c); err != nil { return err }` receives nil,
// the 403 is written to the response, AND THE HANDLER CONTINUES. The caller
// sees a permission denial while the guarded work runs anyway.
//
// Verified live on the production box before this fix:
// GET :85/v2/job_management/tasks with no token at all answered 200 {"tasks":[]}
// instead of 403 — and the same shape guarded PUT /tasks and
// POST /tasks/:id/run, which execute an arbitrary command with /bin/sh -c as
// root. That is remote unauthenticated root execution, reachable from the
// internet through the Cloudflare tunnel.
//
// Returning an error VALUE is what aborts an echo handler chain. This is the
// third time this convention has been wrong in this codebase; it is now
// asserted by a test that fails on any gate whose denial path calls ctx.JSON.
func requireTaskWrite(c echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(c), rbac.PermSystemConfig) {
		return echo.NewHTTPError(http.StatusForbidden,
			map[string]string{"message": "permission denied: admin role required"})
	}
	return nil
}

// ListScheduledTasks returns every configured task.
// GET /v2/job_management/tasks
func ListScheduledTasks(c echo.Context) error {
	if err := requireTaskRead(c); err != nil {
		return err
	}
	s := service.Scheduler()
	if s == nil || s.Store() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"tasks": s.Store().All(),
	})
}

// PutScheduledTask creates a new task or updates an existing one.
// PUT /v2/job_management/tasks   body: scheduled.Task
func PutScheduledTask(c echo.Context) error {
	if err := requireTaskWrite(c); err != nil {
		return err
	}
	var t scheduled.Task
	if err := c.Bind(&t); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	if err := t.Validate(); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	s := service.Scheduler()
	if s == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	actor := actorUserID(c)
	stored, err := s.Apply(&t, actor)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	logger.Info("scheduled task created/updated",
		zap.String("task_id", stored.ID),
		zap.Int("actor_user_id", actor),
		zap.Bool("enabled", stored.Enabled))
	return c.JSON(http.StatusOK, stored)
}

// GetScheduledTask returns a single task by ID.
// GET /v2/job_management/tasks/:id
func GetScheduledTask(c echo.Context) error {
	if err := requireTaskRead(c); err != nil {
		return err
	}
	id, err := validateTaskID(c)
	if err != nil {
		return err
	}
	s := service.Scheduler()
	if s == nil || s.Store() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	t, ok := s.Store().Get(id)
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"message": "task not found"})
	}
	return c.JSON(http.StatusOK, t)
}

// DeleteScheduledTask removes a task and its run history.
// DELETE /v2/job_management/tasks/:id
func DeleteScheduledTask(c echo.Context) error {
	if err := requireTaskWrite(c); err != nil {
		return err
	}
	id, err := validateTaskID(c)
	if err != nil {
		return err
	}
	s := service.Scheduler()
	if s == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	if err := s.Remove(id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	logger.Info("scheduled task deleted",
		zap.String("task_id", id),
		zap.Int("actor_user_id", actorUserID(c)))
	return c.NoContent(http.StatusNoContent)
}

// RunScheduledTaskNow fires a task immediately and returns the run record.
// POST /v2/job_management/tasks/:id/run
func RunScheduledTaskNow(c echo.Context) error {
	if err := requireTaskWrite(c); err != nil {
		return err
	}
	id, err := validateTaskID(c)
	if err != nil {
		return err
	}
	s := service.Scheduler()
	if s == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	run, err := s.RunNow(c.Request().Context(), id)
	if err != nil {
		if service.IsTaskNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]string{"message": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return c.JSON(http.StatusOK, run)
}

// ListTaskRuns returns the run history for a task, newest first.
// GET /v2/job_management/tasks/:id/runs
func ListTaskRuns(c echo.Context) error {
	if err := requireTaskRead(c); err != nil {
		return err
	}
	id, err := validateTaskID(c)
	if err != nil {
		return err
	}
	s := service.Scheduler()
	if s == nil || s.Store() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "scheduler not initialized"})
	}
	runs := s.Store().History(id)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"runs":  runs,
		"count": len(runs),
	})
}

// RegisterScheduledTaskRoutes attaches the scheduled-task routes to an
// existing Echo instance. Called from GetRouter() after the codegen
// handlers are wired — we stay off the oapi-codegen path so schema
// regeneration isn't blocked on our additions.
func RegisterScheduledTaskRoutes(e *echo.Echo, basePath string) {
	g := e.Group(basePath + "/tasks")
	g.GET("", ListScheduledTasks)
	g.PUT("", PutScheduledTask)
	g.GET("/:id", GetScheduledTask)
	g.DELETE("/:id", DeleteScheduledTask)
	g.POST("/:id/run", RunScheduledTaskNow)
	g.GET("/:id/runs", ListTaskRuns)
}
