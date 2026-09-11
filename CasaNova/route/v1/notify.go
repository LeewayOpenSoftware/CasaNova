package v1

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// maxNotifyBodyBytes caps the JSON payload that PostNotifyMessage and
// PostSystemStatusNotify will accept. Without this, an authenticated
// caller could post a multi-gigabyte JSON blob and OOM the daemon while
// ctx.Bind decodes it into an unbounded map[string]interface{}.
// 64 KiB is generous for any real notification payload (the system temp
// data we observe in the wild is well under 4 KiB).
const maxNotifyBodyBytes = 64 << 10

func PostNotifyMessage(ctx echo.Context) error {
	// CasaNova-LocalStorage posts here (disk/USB status) presenting the
	// per-boot internal peer secret via requirePermInternal; authenticated
	// callers still need PermSystemConfig.
	if err := requirePermInternal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, maxNotifyBodyBytes)

	name := ctx.Param("name")
	message := make(map[string]interface{})
	if err := ctx.Bind(&message); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	service.MyService.Notify().SendNotify(name, message)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

func PostSystemStatusNotify(ctx echo.Context) error {
	// CasaNova-LocalStorage posts system status here (external.NotifyService.
	// SendSystemStatusNotify) presenting the per-boot internal peer secret;
	// authenticated callers still need PermSystemConfig.
	if err := requirePermInternal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, maxNotifyBodyBytes)

	message := make(map[string]interface{})
	if err := ctx.Bind(&message); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	service.MyService.Notify().SettingSystemTempData(message)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}
