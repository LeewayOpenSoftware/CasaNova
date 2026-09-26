package v1

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/labstack/echo/v4"
)

// The administrative surface for the per-account system-disk switch: which
// accounts may browse the whole disk from "/".
//
// BOTH endpoints are administrator-only, and that is the substance of the
// feature rather than decoration around it. A common user must not be able to
// read the list (it says which colleagues can see their files) and must
// certainly not be able to switch themselves on. rbac.PermUserManage is the
// same permission that makes a principal an administrator everywhere else in
// this package, so "who may change this" and "who is an administrator" cannot
// drift apart into two different answers.

type systemDiskAccessBody struct {
	// Enabled is the requested state. Absent is false, which is the safe
	// direction for a malformed body: a truncated request turns the switch OFF
	// rather than on.
	Enabled bool `json:"enabled"`
	// Role is the account's current role, needed because the guest account may
	// never receive this and the authority does not hold the role model.
	Role string `json:"role"`
}

// GetStorageSystemDiskAccess lists the accounts the switch is on for.
//
// GET /v1/sys/system-disk-access
func GetStorageSystemDiskAccess(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermUserManage); err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    storageauth.SystemDiskPrincipals(),
	})
}

// PutStorageSystemDiskAccess turns the switch on or off for one account.
//
// PUT /v1/sys/system-disk-access/:id   body: {"enabled": true, "role": "user"}
func PutStorageSystemDiskAccess(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermUserManage); err != nil {
		return err
	}

	id := strings.TrimSpace(ctx.Param("id"))
	// Parsed as a number and handed back as its canonical form, because the
	// authority keys on the principal ID as a string: accepting "007" and
	// storing it verbatim would make a switch that never matches the "7" the
	// request path produces.
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "account id must be a positive integer",
		})
	}

	var body systemDiskAccessBody
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "request body must be {\"enabled\": bool, \"role\": string}",
		})
	}

	if err := storageauth.SetSystemDiskAccess(strconv.Itoa(n), body.Role, body.Enabled); err != nil {
		// The guest refusal is a client error, not a server fault: the
		// administrator asked for something the specification forbids, and the
		// UI needs to say so rather than show a 500.
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    storageauth.SystemDiskPrincipals(),
	})
}
