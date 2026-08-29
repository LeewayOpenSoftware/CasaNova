package v1

import (
	"fmt"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

func GetSearchResult(ctx echo.Context) error {
	// SSRF guard: this proxied an arbitrary user URL and reflected the body,
	// with no auth. Require an authenticated admin at minimum.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	url := json["url"]

	if url == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: "key is empty"})
	}
	// data, err := service.MyService.Other().Search(key)
	data, err := service.MyService.Other().AgentSearch(url)
	if err != nil {
		fmt.Println(err)
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}
