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

// validTaskID matches the format produced by newTaskID() in task.go:
// "task_" + hex digits. It is what this API will MINT and, from now on, the
// only shape it will ACCEPT from a client that supplies its own ID.
var validTaskID = regexp.MustCompile(`^task_[0-9a-f]{1,16}$`)

// addressableTaskID is what the management endpoints will accept in :id, and
// it is deliberately WIDER than validTaskID.
//
// THE BUG THIS SPLIT FIXES. PutScheduledTask accepted any client-supplied ID
// and TaskStore stored it verbatim, but GET/DELETE/run/runs all required
// validTaskID. A task created as {"id":"backup_diario"} was scheduled, ran as
// root via /bin/sh, and then could not be read, triggered or DELETED through
// the API at all -- every management call answered 400. The only way out was
// SSH and editing tasks.json by hand.
//
// Two changes are needed and only one of them is obvious. Rejecting free-form
// IDs at creation closes the door, but on its own it makes things WORSE for
// anyone who already has such a task: it would be unmanageable forever. So the
// management side is widened to whatever a URL path segment can carry.
//
// That is safe because a task ID is ONLY a map key: ts.tasks[t.ID]. It is
// never joined into a path (the sole path in the store is tasks.json itself),
// so there is no traversal to reintroduce. The bound and the exclusion of
// control characters are there to keep it sane in logs and JSON.
//
// DECLARED RESIDUE: a task whose stored ID contains "/" or a control character
// stays unreachable through this API -- not because of this regexp, but
// because such an ID cannot be a URL path segment in the first place. No new
// one can be created now. An existing one still needs tasks.json edited by
// hand, and nothing here touches a real tasks.json.
var addressableTaskID = regexp.MustCompile(`^[^\x00-\x1f/]{1,128}$`)

// DENY WITH echo.NewHTTPError, NEVER WITH c.JSON — the same rule the permission
// gates below carry, and for the same reason.
//
// This returned `"", c.JSON(...)`. c.JSON returns the SERIALIZER's error, which
// is nil once the bytes are written, so every caller's
// `id, err := validateTaskID(c); if err != nil { return err }` received nil and
// CARRIED ON with id == "" after a 400 had already been committed to the
// response. DeleteScheduledTask then reached store.Delete("") and rewrote
// tasks.json for a request it had already rejected; the other three attempted a
// second write on a committed response.
//
// The gates were fixed and a test was added for them, but that test only looked
// at permission gates, so this sibling helper in the same file kept the bug.
// The rule is about any error-returning helper, not about permissions.
func validateTaskID(c echo.Context) (string, error) {
	id := c.Param("id")
	if !addressableTaskID.MatchString(id) {
		return "", echo.NewHTTPError(http.StatusBadRequest,
			map[string]string{"message": "invalid task ID format"})
	}
	return id, nil
}

// Permission policy for scheduled task endpoints:
//   - Read-only endpoints (list tasks, list run history) → PermSystemConfig
//   - Write endpoints (CRUD, manual trigger)             → PermSystemConfig
//
// Reads sit at the SAME bit as writes, and that is not an oversight. A task is
// a command line run with /bin/sh -c as root, and a run record carries its
// stdout, so reading the list is reading the box's root command inventory and
// its output. There is no permission bit between "guest" and "admin" that
// expresses "may read root command output but may not schedule one", and
// inventing one is a scheme change this line does not take.
//
// WHAT WAS WRONG BEFORE, because the shape survived review twice:
// reads were gated on PermSystemRead, and guestPerms HOLDS PermSystemRead
// (CasaNova-Common/pkg/rbac/rbac.go — deliberately, so tokenless internal reads
// and the read-only dashboard widgets keep working). rbac.Normalize also fails
// closed to RoleGuest, so "no role" is not "no permissions": it is guest, and
// guest passed. The gate could not refuse anybody the JWT middleware admitted;
// route/denial_convention_test.go said so in a comment rather than a case.
//
// AND requireSystemPrincipal WOULD NOT HAVE FIXED IT. That helper
// (CasaNova/route/v1/system.go) demands UserIDFromContext != 0 before checking
// the permission, which closes the TOKENLESS hole — the right tool where the
// skipper still lets tokenless traffic through. It is the wrong tool here for
// two reasons. First, this module's skipper is already authskip.For(nil): no
// path answers without a credential, so the tokenless hole is shut upstream.
// Second, and decisively, the callers that must be refused ARE authenticated
// and DO have a non-zero user id: CasaNova-UserService mints OAuth2-delegated
// tokens with role "guest" ON PURPOSE to de-privilege a relying party
// (route/v1.go, route/v2.go — the DB re-read that would promote guest back is
// explicitly skipped for issuer "oauth2-delegated"). An identity check admits
// exactly those principals. Only a permission guest does not hold refuses them.
//
// So the fix is the bit, not the identity check, and no new helper is needed:
// PermSystemConfig is outside guestPerms and outside userPerms, which is the
// codebase's own documented rule for when plain requirePerm/rbac.Allows is
// sufficient (see requireSystemPrincipal's comment: "use requirePerm when the
// permission is already beyond guest").
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
	if !rbac.Allows(commonmw.RoleFromContext(c), rbac.PermSystemConfig) {
		return echo.NewHTTPError(http.StatusForbidden,
			map[string]string{"message": "permission denied: admin role required"})
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
	// An empty ID means "mint one" and is the normal create path. A supplied ID
	// must be one this API could have minted; anything else used to be accepted
	// here and then rejected by every endpoint that manages it. The check lives
	// in the handler and NOT in Task.Validate() on purpose: TaskStore.load()
	// does not validate, so a tasks.json that already holds a free-form ID
	// keeps loading and keeps running. Tightening Validate() would have made
	// those tasks vanish from the scheduler on the next restart, which is a
	// worse failure than the one being fixed.
	if t.ID != "" && !validTaskID.MatchString(t.ID) {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"message": "task ID must be of the form task_<hex> or be omitted so the server assigns one",
		})
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
