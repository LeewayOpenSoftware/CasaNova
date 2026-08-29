package v1

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// GET /v1/sys/host/power — current UPS power state (from the Forza daemon) +
// orchestrator health. Read-only; drives the UI power card.
func GetHostPower(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	reading, lastErr := service.Power().Status()
	cfg := service.Power().Config()
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"power":     reading,
			"error":     lastErr,
			"reachable": lastErr == "" && reading.Reachable,
			"enabled":   cfg.Enabled,
			"peer":      service.Power().PeerState(),
			"ladder":    service.Power().LadderState(),
		},
	})
}

// GET /v1/sys/host/power/config
func GetHostPowerConfig(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    service.Power().RedactedConfig(),
	})
}

// PUT /v1/sys/host/power/config — enable/disable, Forza URL, auto-shutdown,
// essential apps. AutoShutdown can power the box off, so it's PermSystemConfig.
func PutHostPowerConfig(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var cfg service.PowerConfig
	if err := ctx.Bind(&cfg); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: err.Error()})
	}
	if err := service.Power().SetConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: service.Power().RedactedConfig()})
}
