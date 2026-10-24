package v1

import (
	"net/http"
	"regexp"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
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

	// Refuse to demote the box's last admin account. The self-demotion check
	// above already makes this unreachable today (a non-admin cannot call
	// this handler at all — rbacAdminGuard — and an admin demoting THEMSELF
	// is already refused unconditionally), but that safety is an emergent
	// property of two separately-motivated checks, not something this
	// decision examines on its own — exactly the shape this sweep exists to
	// close. Counts by the RESOLVED role (countOtherAdmins), matching
	// DeleteUser's identical guard, so a legacy admin row is protected the
	// same way a literal "admin" row is.
	if resolveEffectiveRole(target.Role) == string(rbac.RoleAdmin) &&
		newRole != rbac.RoleAdmin &&
		countOtherAdmins(id) == 0 {
		return ctx.JSON(http.StatusForbidden, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cannot demote the last admin account",
		})
	}

	roleChanged := target.Role != string(newRole)
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

	// Every service authorises from the role claim in the signed token, so a
	// role change must cut off the target's existing sessions — otherwise a
	// demoted admin keeps admin until the access token expires and can keep
	// refreshing (mirrors the password change and DeleteUser).
	if roleChanged {
		revokeAllRefreshFamiliesForUser(target.Id)
		if err := tokenrevocation.RevokeID(target.Id); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: "role changed but existing sessions could not be revoked: " + err.Error(),
			})
		}
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
