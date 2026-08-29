package v1

import (
	"net/http"
	"regexp"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/labstack/echo/v4"
)

// validUserID matches numeric user IDs (the DB primary key is an integer).
var validUserID = regexp.MustCompile(`^[0-9]{1,20}$`)

// rbacAdminGuard rejects non-admin HTTP callers. There is NO loopback
// bypass: the gateway proxies every browser request over 127.0.0.1, so
// trusting loopback would let any user (the JWT skipper populates the role
// from their token) self-promote to admin via PUT /users/<id>/role. Genuine
// local automation must present an admin JWT like the UI does.
func rbacAdminGuard(ctx echo.Context) bool {
	return rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage)
}

// ListRoles returns the static list of roles + their permission masks.
// Used by the UI to populate the role dropdown and describe capabilities.
//
// GET /v1/users/roles
func ListRoles(ctx echo.Context) error {
	if !rbacAdminGuard(ctx) {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR, Message: "admin role required",
		})
	}
	type entry struct {
		Role        string `json:"role"`
		Permissions uint64 `json:"permissions"`
	}
	out := make([]entry, 0, len(rbac.AllRoles))
	for _, r := range rbac.AllRoles {
		out = append(out, entry{Role: string(r), Permissions: uint64(rbac.PermissionsFor(r))})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    out,
	})
}

// PutUserRole changes a user's role. Only admins may call it, and the
// request refuses to demote the caller themselves — protecting against
// lockout. The target role must be one of rbac.AllRoles.
//
// PUT /v1/users/:id/role   body: {"role": "user"}
func PutUserRole(ctx echo.Context) error {
	if !rbacAdminGuard(ctx) {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR, Message: "admin role required",
		})
	}
	id := ctx.Param("id")
	if id == "" || !validUserID.MatchString(id) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR, Message: "missing or invalid user id",
		})
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS, Message: err.Error(),
		})
	}
	newRole := rbac.Role(body.Role)
	if !newRole.IsValid() {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "role must be one of: admin, user, guest",
		})
	}

	// Prevent self-demotion: an admin setting their own role to user/guest
	// would lock themselves (and potentially the entire system if they're
	// the only admin) out of user management.
	authedID := ctx.Request().Header.Get("user_id")
	if id == authedID && newRole != rbac.RoleAdmin {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cannot demote your own account",
		})
	}

	target := service.MyService.User().GetUserAllInfoById(id)
	if target.Id == 0 {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR, Message: "user not found",
		})
	}
	target.Role = string(newRole)
	service.MyService.User().UpdateUser(target)

	// Audit the change — role modifications are exactly the kind of thing
	// an operator reviews when investigating "who gained admin last week".
	if l := service.AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventUserRoleChange,
			Severity: auditlog.SeverityWarning,
			Actor:    authedID,
			ActorIP:  ctx.RealIP(),
			Target:   id,
			Message:  "role changed to " + string(newRole),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]string{
			"id":   id,
			"role": string(newRole),
		},
	})
}
