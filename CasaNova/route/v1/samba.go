/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-26 11:08:48
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-17 18:25:42
 * @FilePath: /CasaOS/route/v1/samba.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package v1

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/systemctl"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"

	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/samba"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
)

// blockedSharePrefixes lists system directories that must never be exposed as
// Samba shares. Paths supplied by the user are rejected if they start with any
// of these prefixes.
var blockedSharePrefixes = []string{
	"/etc", "/root", "/usr", "/var", "/sys", "/proc", "/dev", "/boot", "/bin", "/sbin", "/lib",
}

// validShareName matches share names that contain only safe characters:
// alphanumeric, dash, underscore, and dot.
var validShareName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// service

func GetSambaStatus(ctx echo.Context) error {
	if status, err := systemctl.IsServiceRunning("smbd"); err != nil || !status {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_NOT_RUNNING,
			Message: common_err.GetMsg(common_err.SERVICE_NOT_RUNNING),
		})
	}

	needInit := true
	if file.Exists("/etc/samba/smb.conf") {
		str := file.ReadLine(1, "/etc/samba/smb.conf")
		if strings.Contains(str, "# Copyright (c) 2021-2022 CasaOS Inc. All rights reserved.") {
			needInit = false
		}
	}
	data := make(map[string]string, 1)
	data["need_init"] = fmt.Sprintf("%v", needInit)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

func GetSambaSharesList(ctx echo.Context) error {
	// Gated: share definitions carry the host paths behind them.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}

	shares := service.MyService.Shares().GetSharesList()
	shareList := []model.Shares{}
	for _, v := range shares {
		shareList = append(shareList, model.Shares{
			Anonymous: v.Anonymous,
			Path:      v.Path,
			ID:        v.ID,
		})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: shareList})
}

func PostSambaSharesCreate(ctx echo.Context) error {
	// Wave 33: gate behind PermStorageWrite. Previously this handler
	// was reachable by any role with a valid JWT — including Guest —
	// which let an unprivileged session expose any non-blacklisted
	// directory as an *anonymous* (Anonymous: true, chmod 0755) Samba
	// share readable by anyone on the LAN. The only previous safety
	// net was the path-prefix block-list, which left /DATA, /home,
	// /srv, /opt, etc. wide open. Match the gate every other write-
	// side handler in this package uses.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	shares := []model.Shares{}
	if err := ctx.Bind(&shares); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	for i, v := range shares {
		if v.Path == "" {
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
		}

		// Sanitize and validate the path to prevent path-injection attacks.
		cleanPath := filepath.Clean(v.Path)
		if strings.Contains(cleanPath, "..") {
			logger.Error("samba share path contains '..': rejected", zap.String("path", v.Path))
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "path contains disallowed traversal"})
		}
		// Reject paths under sensitive system directories.
		for _, prefix := range blockedSharePrefixes {
			if cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
				logger.Error("samba share path under blocked directory: rejected", zap.String("path", cleanPath), zap.String("blocked", prefix))
				return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: "sharing this directory is not allowed"})
			}
		}
		shares[i].Path = cleanPath

		if !file.Exists(cleanPath) {
			return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.DIR_NOT_EXISTS, Message: common_err.GetMsg(common_err.DIR_NOT_EXISTS)})
		}
		if len(service.MyService.Shares().GetSharesByPath(cleanPath)) > 0 {
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.SHARE_ALREADY_EXISTS, Message: common_err.GetMsg(common_err.SHARE_ALREADY_EXISTS)})
		}

		// Sanitize the share name: only allow alphanumeric, dash, underscore, dot.
		shareName := filepath.Base(cleanPath)
		if !validShareName.MatchString(shareName) {
			logger.Error("samba share name contains invalid characters: rejected", zap.String("name", shareName))
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: "share name contains invalid characters (only alphanumeric, dash, underscore, dot allowed)"})
		}
		if len(service.MyService.Shares().GetSharesByPath(shareName)) > 0 {
			return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.SHARE_NAME_ALREADY_EXISTS, Message: common_err.GetMsg(common_err.SHARE_NAME_ALREADY_EXISTS)})
		}
	}
	for _, v := range shares {
		shareDBModel := model2.SharesDBModel{}
		shareDBModel.Anonymous = true
		shareDBModel.Path = v.Path
		shareDBModel.Name = filepath.Base(v.Path)
		os.Chmod(v.Path, 0o755)
		service.MyService.Shares().CreateShare(shareDBModel)
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: shares})
}

func DeleteSambaShares(ctx echo.Context) error {
	// CasaNova-LocalStorage deletes a share when its backing disk is removed
	// (external.ShareService.DeleteShare -> disk.go), presenting the per-boot
	// internal peer secret; authenticated callers still need PermStorageWrite.
	if err := requirePermInternal(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	id := ctx.Param("id")
	if id == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INSUFFICIENT_PERMISSIONS, Message: common_err.GetMsg(common_err.INSUFFICIENT_PERMISSIONS)})
	}
	service.MyService.Shares().DeleteShare(id)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: id})
}

// client
func GetSambaConnectionsList(ctx echo.Context) error {
	// Gated: names who is connected from where.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}

	connections := service.MyService.Connections().GetConnectionsList()
	connectionList := []model.Connections{}
	for _, v := range connections {
		connectionList = append(connectionList, model.Connections{
			ID:         v.ID,
			Username:   v.Username,
			Port:       v.Port,
			Host:       v.Host,
			MountPoint: v.MountPoint,
		})
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: connectionList})
}

func PostSambaConnectionsCreate(ctx echo.Context) error {
	// Wave 33: same gate as the share handlers above. Mounting a
	// remote SMB server with attacker-supplied credentials at
	// /mnt/&lt;host&gt; persists in the connections DB and survives
	// reboot — Guest must not be able to plant a watering-hole
	// remote mount that admin browsing then traverses.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	connection := model.Connections{}
	if err := ctx.Bind(&connection); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	if connection.Port == "" {
		connection.Port = "445"
	}
	if connection.Username == "" || connection.Host == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.CHARACTER_LIMIT, Message: common_err.GetMsg(common_err.CHARACTER_LIMIT)})
	}

	// if ok, _ := regexp.MatchString(`^[\w@#*.]{4,30}$`, connection.Password); !ok {
	// 	return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.CHARACTER_LIMIT, Message: common_err.GetMsg(common_err.CHARACTER_LIMIT)})
	// 	return
	// }
	// if ok, _ := regexp.MatchString(`^[\w@#*.]{4,30}$`, connection.Username); !ok {
	// 	return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	// 	return
	// }
	// if !ip_helper.IsIPv4(connection.Host) && !ip_helper.IsIPv6(connection.Host) {
	// 	return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	// 	return
	// }
	// if ok, _ := regexp.MatchString("^[0-9]{1,6}$", connection.Port); !ok {
	// 	return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	// 	return
	// }

	connection.Host = strings.Split(connection.Host, "/")[0]
	// check is exists
	connections := service.MyService.Connections().GetConnectionByHost(connection.Host)
	if len(connections) > 0 {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.Record_ALREADY_EXIST, Message: common_err.GetMsg(common_err.Record_ALREADY_EXIST), Data: common_err.GetMsg(common_err.Record_ALREADY_EXIST)})
	}
	// check connect is ok
	directories, err := samba.GetSambaSharesList(connection.Host, connection.Port, connection.Username, connection.Password)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}

	connectionDBModel := model2.ConnectionsDBModel{}
	connectionDBModel.Username = connection.Username
	connectionDBModel.Password = connection.Password
	connectionDBModel.Host = connection.Host
	connectionDBModel.Port = connection.Port
	connectionDBModel.Directories = strings.Join(directories, ",")
	baseHostPath := "/mnt/" + connection.Host
	connectionDBModel.MountPoint = baseHostPath
	connection.MountPoint = baseHostPath
	file.IsNotExistMkDir(baseHostPath)
	for _, v := range directories {
		mountPoint := baseHostPath + "/" + v
		file.IsNotExistMkDir(mountPoint)
		service.MyService.Connections().MountSmaba(connectionDBModel.Username, connectionDBModel.Host, v, connectionDBModel.Port, mountPoint, connectionDBModel.Password)
	}

	service.MyService.Connections().CreateConnection(&connectionDBModel)

	connection.ID = connectionDBModel.ID
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: connection})
}

func DeleteSambaConnections(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	id := ctx.Param("id")
	connection := service.MyService.Connections().GetConnectionByID(id)
	if connection.Username == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.Record_NOT_EXIST, Message: common_err.GetMsg(common_err.Record_NOT_EXIST)})
	}
	mountPointList, err := samba.GetSambaSharesList(connection.Host, connection.Port, connection.Username, connection.Password)
	// mountPointList, err := service.MyService.System().GetDirPath(connection.MountPoint)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	}
	baseHostPath := "/mnt/" + connection.Host
	for _, v := range mountPointList {
		if service.IsMounted(baseHostPath + "/" + v) {
			err := service.MyService.Connections().UnmountSmaba(baseHostPath + "/" + v)
			if err != nil {
				logger.Error("unmount smaba error", zap.Error(err), zap.Any("path", baseHostPath+"/"+v))
				return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
			}
		}
	}
	dir, _ := os.ReadDir(connection.MountPoint)
	if len(dir) == 0 {
		os.RemoveAll(connection.MountPoint)
	}
	service.MyService.Connections().DeleteConnection(id)
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: id})
}
