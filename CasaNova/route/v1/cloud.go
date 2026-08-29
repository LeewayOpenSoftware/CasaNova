package v1

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"net/http"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/dropbox"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/google_drive"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/onedrive"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func ListStorages(ctx echo.Context) error {
	// Same family as the OAuth callback, and it was still open: this returns
	// every mounted cloud remote's mount point AND the provider account
	// identity — for the OAuth path, the account e-mail. A tokenless request
	// resolves to guest, and the gateway proxies the whole internet over
	// loopback, so it was an unauthenticated account-enumeration endpoint.
	//
	// Guest does not hold storage:read, which is the intended outcome.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	r, err := service.MyService.Storage().GetStorages()
	if err != nil {
		return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}

	for i := 0; i < len(r.MountPoints); i++ {
		dataMap, err := service.MyService.Storage().GetConfigByName(r.MountPoints[i].Fs)
		if err != nil {
			logger.Error("GetConfigByName", zap.Any("err", err))
			continue
		}
		if dataMap["type"] == "drive" {
			r.MountPoints[i].Icon = google_drive.ICONURL
		}
		if dataMap["type"] == "dropbox" {
			r.MountPoints[i].Icon = dropbox.ICONURL
		}
		if dataMap["type"] == "onedrive" {
			r.MountPoints[i].Icon = onedrive.ICONURL
		}
		r.MountPoints[i].Name = dataMap["username"]
	}
	list := []httper.MountPoint{}

	for _, v := range r.MountPoints {
		list = append(list, httper.MountPoint(v))
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: list})
}

func UmountStorage(ctx echo.Context) error {
	// Unmounting a cloud remote is a storage mutation and had NO gate at all,
	// while its siblings on this group do. A tokenless caller resolves to guest
	// and the gateway proxies every LAN and tunnel request over loopback, so
	// DELETE /v1/cloud with a JSON body was enough to tear down any mounted
	// remote from the internet.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	json := make(map[string]string)
	if err := ctx.Bind(&json); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	mountPoint := json["mount_point"]
	if mountPoint == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.CLIENT_ERROR), Data: "mount_point is empty"})
	}
	err := service.MyService.Storage().UnmountStorage(mountPoint)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	service.MyService.Storage().DeleteConfigByName(strings.ReplaceAll(mountPoint, "/mnt/", ""))
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: "success"})
}

func GetStorage(ctx echo.Context) error {
	// idStr := ctx.QueryParam("id")
	// id, err := strconv.Atoi(idStr)
	// if err != nil {
	// 	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.CLIENT_ERROR), Data: err.Error()})
	// 	return
	// }
	// storage, err := service.MyService.Storage().GetStorageById(uint(id))
	// if err != nil {
	// 	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	// 	return
	// }
	// return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: storage})
	return nil
}
