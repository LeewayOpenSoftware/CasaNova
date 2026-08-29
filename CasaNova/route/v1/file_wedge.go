package v1

import (
	"net/http"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// THE PARKED-WORKER SURFACE.
//
// service.WedgedFileOps was written "for the health and diagnosis surfaces" and
// had zero callers, so a reservation held by a dead mount was not merely
// unreleasable — it was invisible. The operator saw file operations refused
// with a path they could not account for, and nothing anywhere named the
// worker holding it. This is the same defect as everything else in this
// subsystem's history: a correct mechanism no production caller reaches.

// GetWedgedFileOps lists operations whose worker is parked inside the kernel.
//
// ADMIN ONLY. The first version of this shipped with no gate at all, on the
// reasoning that it "names paths, not contents". That reasoning was wrong twice
// over. First, absolute paths and mount points ARE disclosure: they map the
// operator's storage layout, name shares, and reveal which remote hosts this
// box mounts. Second, and worse, "readable by anyone who may manage files" was
// enforced by nothing — the handler had no check, so a tokenless GET to
// :85/v1/batch/wedged answered 200, and because the gateway proxies the
// Cloudflare tunnel over loopback, "tokenless" includes the entire internet.
// It was confirmed live on the production box before this gate was added.
//
// The honest reason it is admin-only rather than owner-scoped: WedgedOp records
// no principal, so there is nothing to scope by. A per-owner view needs the
// operation's owner captured at claim time first; until then the fail-closed
// answer is the correct one.
func GetWedgedFileOps(ctx echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage) {
		return echo.NewHTTPError(http.StatusForbidden, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "listing stuck file operations is an administrator action",
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    service.WedgedFileOps(),
	})
}

// DeleteWedgeReservation frees the tree a parked worker still holds.
//
// ADMIN ONLY, and gated on PermUserManage like every other administrative
// action here. It cancels the operation and releases its reservation; it
// cannot kill a thread blocked in the kernel, and the response says so rather
// than implying the worker is gone.
func DeleteWedgeReservation(ctx echo.Context) error {
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage) {
		// echo.NewHTTPError, not ctx.JSON: a helper that denies by writing a
		// body and returning nil hands nil to `if err != nil` and the handler
		// runs anyway. That convention error was an exploitable bypass in this
		// very file's neighbours; it is not repeated here.
		return echo.NewHTTPError(http.StatusForbidden, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "releasing a stuck file operation is an administrator action",
		})
	}

	id := ctx.Param("id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "an operation id is required",
		})
	}

	info, ok := service.DiscardWedgeReservation(id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "no parked worker is holding a reservation for that operation",
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "the reservation on " + info.Holder + " was released and the operation cancelled. " +
			"The worker itself cannot be interrupted while it is blocked in the kernel: if " +
			info.Mount + " ever responds again it will unwind and exit without resuming.",
		Data: info,
	})
}
