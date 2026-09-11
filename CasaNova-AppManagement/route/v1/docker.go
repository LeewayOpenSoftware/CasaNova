package v1

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/model"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	v2 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/route/v2"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/service/v1"
	modelCommon "github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/port"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/ssh"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/systemctl"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/gorilla/websocket"
	jsoniter "github.com/json-iterator/go"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
	uuid "github.com/satori/go.uuid"
	"go.uber.org/zap"
)

var v1Installing sync.Map

const (
	dockerRootDirFilePath             = "/var/lib/casaos/docker_root"
	dockerDaemonConfigurationFilePath = "/etc/docker/daemon.json"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

const (
	// WebSocket keepalive settings
	wsPingInterval = 25 * time.Second // Send ping every 25s (browsers timeout at ~60s)
	wsPongWait     = 60 * time.Second // Wait up to 60s for pong response
	wsWriteWait    = 10 * time.Second // Max time to write a message
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:   32768, // 32KB - adequate for terminal I/O
	WriteBufferSize:  32768,
	HandshakeTimeout: 10 * time.Second,
	// CheckOrigin uses gorilla's default: same-origin or no-origin (non-browser).
}

// DockerTerminal opens a WebSocket terminal session to a container.
// Includes ping/pong keepalive to prevent disconnection on tab switch or resize.
func DockerTerminal(ctx echo.Context) error {
	// An interactive shell inside a container is effectively root on the
	// host's docker layer; gate it behind admin-grade PermSystemConfig.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	colStr := v2.DefaultQuery(ctx, "cols", "100")
	rowStr := v2.DefaultQuery(ctx, "rows", "30")
	if _, err := strconv.Atoi(colStr); err != nil {
		colStr = "100"
	}
	if _, err := strconv.Atoi(rowStr); err != nil {
		rowStr = "30"
	}
	col := colStr
	row := rowStr
	conn, err := upgrader.Upgrade(ctx.Response().Writer, ctx.Request(), nil)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	defer conn.Close()

	// Configure keepalive + frame size limit. Without SetReadLimit a
	// malicious client can ship a multi-GB frame and OOM the service.
	conn.SetReadLimit(64 * 1024)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	containerID := ctx.Param("id")
	hr, err := service.MyService.Docker().CreateContainerShellSession(containerID, row, col)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	defer hr.Close()
	defer func() {
		if _, err := hr.Conn.Write([]byte("exit\r")); err != nil {
			logger.Error("error when trying `exit` to container", zap.Error(err))
		}
	}()

	// Single mutex shared between the ping-ticker goroutine and the
	// stdout-forward goroutine — Gorilla's *websocket.Conn is NOT safe
	// for concurrent writers and crashes the service with
	// "concurrent write to websocket connection" otherwise.
	var wsMu sync.Mutex

	// Start ping ticker to keep connection alive during idle periods (tab switches, etc.)
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("docker terminal ping panic", zap.Any("panic", r))
			}
		}()
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				wsMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				wsMu.Unlock()
				if err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Forward container stdout → WebSocket (uses the shared mutex).
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("docker terminal stdout panic", zap.Any("panic", r))
			}
			close(done)
		}()
		ssh.WsWriterCopyLocked(hr.Conn, conn, &wsMu)
	}()

	// Forward WebSocket → container stdin (blocks until disconnect)
	ssh.WsReaderCopy(conn, hr.Conn)
	return nil
}

// @Summary app(post json)
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  id path int true "id"
// @Param  port formData int true ""
// @Param  tcp formData string false "tcp"
// @Param  udp formData string false "udp"
// @Param  env formData string false ""
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/install [post]
func InstallApp(ctx echo.Context) error {
	m := model.CustomizationPostData{}
	if err := ctx.Bind(&m); err != nil {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	const CUSTOM = "custom"
	var dockerImage string
	var dockerImageVersion string

	// check app name is exist
	if len(m.Protocol) == 0 {
		m.Protocol = "http"
	}
	m.ContainerName = strings.Replace(m.Label, " ", "_", -1)
	if m.Origin != CUSTOM {
		oldName := m.ContainerName
		oldLabel := m.Label
		for i := 0; i < 10000; i++ {
			if i != 0 {
				m.ContainerName = oldName + "-" + strconv.Itoa(i)
				m.Label = oldLabel + "-" + strconv.Itoa(i)
			}
			if _, err := service.MyService.Docker().GetContainerByName(m.ContainerName); err != nil {
				break
			}
		}
	} else {
		if _, err := service.MyService.Docker().GetContainerByName(m.ContainerName); err == nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.ERROR_APP_NAME_EXIST, Message: common_err.GetMsg(common_err.ERROR_APP_NAME_EXIST)})
		}
	}

	// check port
	if len(m.PortMap) > 0 && m.PortMap != "0" {
		portMap, err := strconv.Atoi(m.PortMap)
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
		}

		if !port.IsPortAvailable(portMap, "tcp") {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "Duplicate port:" + m.PortMap})
		}
	}

	imageArr := strings.Split(m.Image, ":")
	if len(imageArr) == 2 {
		dockerImage = imageArr[0]
		dockerImageVersion = imageArr[1]
	} else {
		dockerImage = m.Image
		dockerImageVersion = "latest"
	}
	m.Image = dockerImage + ":" + dockerImageVersion
	for _, u := range m.Ports {
		protocol := strings.ToLower(u.Protocol)

		if !lo.Contains([]string{"tcp", "udp", "both"}, protocol) {
			return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "Protocol must be tcp or udp or both"})
		}

		protocols := strings.Replace(protocol, "both", "tcp,udp", -1)
		for _, p := range strings.Split(protocols, ",") {
			t, err := strconv.Atoi(u.CommendPort)
			if err != nil || t < 1 || t > 65535 {
				// Skip port availability check if port is invalid (Docker will assign randomly)
				logger.Info("host port is not valid - Docker will pick port number randomly", zap.String("port", u.CommendPort))
				continue
			}

			if !port.IsPortAvailable(t, p) {
				return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "Duplicate port:" + u.CommendPort})
			}
		}
	}

	if m.Origin == CUSTOM {
		for _, device := range m.Devices {
			if file.CheckNotExist(device.Path) {
				return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.DEVICE_NOT_EXIST, Message: device.Path + "," + common_err.GetMsg(common_err.DEVICE_NOT_EXIST)})
			}
		}
	} else {
		dev := []model.PathMap{}
		for _, device := range m.Devices {
			if !file.CheckNotExist(device.Path) {
				dev = append(dev, device)
			}
		}
		m.Devices = dev
	}

	id := uuid.NewV4().String()
	m.CustomID = id

	imageName := dockerImage + ":" + dockerImageVersion

	// Wave 35-X: 30-min deadline + recover(). Previously
	// context.Background() with no deadline meant a stuck pull pinned
	// the goroutine + v1Installing entry forever — the user couldn't
	// retry the install for the same container name until daemon
	// restart, and no error ever propagated to the UI.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	httpCtx := common.WithProperties(baseCtx, v2.PropertiesFromQueryParams(ctx))

	eventProperties := common.PropertiesFromContext(httpCtx)
	eventProperties[common.PropertyTypeAppName.Name] = m.Label
	eventProperties[common.PropertyTypeAppIcon.Name] = m.Icon
	eventProperties[common.PropertyTypeImageName.Name] = imageName

	if _, loaded := v1Installing.LoadOrStore(m.ContainerName, true); loaded {
		baseCancel()
		return ctx.JSON(http.StatusConflict, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: fmt.Sprintf("install already in progress for %s", m.ContainerName)})
	}

	go func() {
		defer baseCancel()
		defer v1Installing.Delete(m.ContainerName)
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in v1 install goroutine",
					zap.Any("panic", r), zap.String("container", m.ContainerName))
				go service.PublishEventWrapper(httpCtx, common.EventTypeAppInstallError, map[string]string{
					common.PropertyTypeMessage.Name: "internal error during install",
				})
			}
		}()

		go service.PublishEventWrapper(httpCtx, common.EventTypeAppInstallBegin, nil)

		defer service.PublishEventWrapper(httpCtx, common.EventTypeAppInstallEnd, nil)

		if err := pullAndInstall(httpCtx, imageName, &m); err != nil {
			go service.PublishEventWrapper(httpCtx, common.EventTypeAppInstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
		}
	}()

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: m.Label})
}

// @Summary app
// @Produce  application/json
// @Accept multipart/form-data
// @Tags app
// @Param  id path string true "id"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/uninstall/{id} [delete]
func UninstallApp(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	containerID := ctx.Param("id")
	if len(containerID) == 0 {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}

	j := make(map[string]bool)
	if err := (&echo.DefaultBinder{}).BindBody(ctx, &j); err != nil {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	isDelete, ok := j["delete_config_folder"]
	if !ok {
		isDelete = false
	}

	// Wave 35-X: 10-min deadline + recover() for uninstall goroutine.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	httpCtx := common.WithProperties(baseCtx, v2.PropertiesFromQueryParams(ctx))

	container, err := service.MyService.Docker().DescribeContainer(httpCtx, containerID)
	if err != nil {
		baseCancel()
		if _, ok := err.(errdefs.ErrNotFound); ok {
			return ctx.JSON(http.StatusNotFound, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	eventProperties := common.PropertiesFromContext(httpCtx)
	eventProperties[common.PropertyTypeAppName.Name] = v1.AppName(container)
	eventProperties[common.PropertyTypeAppIcon.Name] = v1.AppIcon(container)

	go func() {
		defer baseCancel()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in v1 uninstall goroutine",
					zap.Any("panic", r), zap.String("container", containerID))
			}
		}()

		go service.PublishEventWrapper(httpCtx, common.EventTypeAppUninstallBegin, nil)

		defer service.PublishEventWrapper(httpCtx, common.EventTypeAppUninstallEnd, nil)

		if err := uninstall(httpCtx, container, isDelete); err != nil {
			go service.PublishEventWrapper(httpCtx, common.EventTypeAppUninstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
		}
	}()

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary app
// @Produce  application/json
// @Accept multipart/form-data
// @Tags app
// @Param  id path string true "appid"
// @Param  state query string false " start stop restart"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/state/{id} [put]
func ChangAppState(ctx echo.Context) error {
	appID := ctx.Param("id")
	if len(appID) == 0 {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "id should not be empty"})
	}

	js := make(map[string]string)
	if err := ctx.Bind(&js); err != nil {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	state, ok := js["state"]
	if !ok || len(state) == 0 {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "`state` should exist and it should not be empty"})
	}

	switch state {

	case "start":
		if err := service.MyService.Docker().StartContainer(appID); err != nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}
	case "restart":
		if err := service.MyService.Docker().StopContainer(appID); err != nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		if err := service.MyService.Docker().StartContainer(appID); err != nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

	case "stop":
		if err := service.MyService.Docker().StopContainer(appID); err != nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

	default:
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "`state` should be start, stop or restart"})
	}

	info, err := service.MyService.Docker().GetContainer(appID)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: info.State})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  id path string true "appid"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/logs/{id} [get]
func ContainerLog(ctx echo.Context) error {
	appID := ctx.Param("id")

	log, err := service.MyService.Docker().GetContainerLog(appID)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: string(log)})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  id path string true "id"
// @Param  type query string false "type=1"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/state/{id} [get]
func GetContainerState(ctx echo.Context) error {
	id := ctx.Param("id")
	// t := v2.DefaultQuery(ctx, "type", "0")
	containerInfo, e := service.MyService.Docker().GetContainer(id)
	if e != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: e.Error()})
	}

	data := make(map[string]interface{})

	data["state"] = containerInfo.State

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// @Summary
// @Produce  application/json
// @Accept multipart/form-data
// @Tags app
// @Param  id path string true "id"
// @Param  shares formData string false "cpu"
// @Param  mem formData string false "MB"
// @Param  restart formData string false ""
// @Param  label formData string false ""
// @Param  position formData bool true ""
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/update/{id}/setting [put]
func UpdateSetting(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}
	id := ctx.Param("id")
	if len(id) == 0 {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "id should not be empty"})
	}

	m := model.CustomizationPostData{}
	if err := ctx.Bind(&m); err != nil {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}

	// Validate BEFORE stopping the container (prevents leaving container stopped on invalid input)
	for _, u := range m.Ports {
		protocol := strings.ToLower(u.Protocol)
		if !lo.Contains([]string{"tcp", "udp", "both"}, protocol) {
			return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "protocol should be tcp, udp or both"})
		}
	}

	// Only stop container after validation passes
	if err := service.MyService.Docker().StopContainer(id); err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	if err := service.MyService.Docker().RenameContainer(id, id); err != nil {
		logger.Error("rename container error: ", zap.Error(err))
	}

	containerID, err := service.MyService.Docker().CreateContainer(ctx.Request().Context(), m, id)
	if err != nil {
		if err := service.MyService.Docker().RenameContainer(m.ContainerName, id); err != nil {
			logger.Error("rename container error: ", zap.Error(err))
		}

		if err := service.MyService.Docker().StartContainer(id); err != nil {
			return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
		}

		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	// stepStart container
	if err = service.MyService.Docker().StartContainer(containerID); err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	if err := service.MyService.Docker().RemoveContainer(id, true); err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

func GetDockerNetworks(ctx echo.Context) error {
	networks := service.MyService.Docker().GetNetworkList()
	list := []map[string]string{}
	for _, network := range networks {
		if network.Driver != "null" {
			list = append(list, map[string]string{"name": network.Name, "driver": network.Driver, "id": network.ID})
		}
	}

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: list})
}

func ToComposeYAML(ctx echo.Context) error {
	// Wave 25: GET /v1/container/:id/compose returns the full compose
	// representation of a container — including environment variables,
	// which routinely contain database passwords, API keys, and other
	// secrets. The previous implementation was reachable by any user with
	// even PermAppView, so a low-privilege account could fetch the maria
	// container's DB_PASSWORD just by knowing its container ID.
	// PermAppConfigure (admin/user-with-config) is the right gate.
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	appID := ctx.Param("id")

	// Wave 35-X: bounded synchronous read — request handler must not
	// hang forever on a stuck daemon.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer baseCancel()
	httpCtx := common.WithProperties(baseCtx, v2.PropertiesFromQueryParams(ctx))

	info, err := service.MyService.Docker().DescribeContainer(httpCtx, appID)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	m := v1.GetCustomizationPostData(*info)

	return YAML(ctx, http.StatusOK, m.Compose())
}

// @Produce  application/json
// @Accept application/json
// @Tags app
// @Param  id path string true "appid"
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/update/{id}/info [get]
func ContainerUpdateInfo(ctx echo.Context) error {
	// Same data class as ToComposeYAML above — GetCustomizationPostData
	// includes every environment variable on a locally-installed container,
	// which routinely holds database passwords and API keys. Gated the same
	// way for the same reason.
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	appID := ctx.Param("id")

	// Wave 35-X: bounded synchronous read.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer baseCancel()
	httpCtx := common.WithProperties(baseCtx, v2.PropertiesFromQueryParams(ctx))

	info, err := service.MyService.Docker().DescribeContainer(httpCtx, appID)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	m := v1.GetCustomizationPostData(*info)

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: m})
}

// @Summary
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Security ApiKeyAuth
// @Param  index query int false "index"
// @Param  size query int false "size"
// @Param  position query bool false ""
// @Success 200 {string} string "ok"
// @Router /app/my/list [get]
func MyAppList(ctx echo.Context) error {
	name := ctx.QueryParam("name")
	image := ctx.QueryParam("image")
	state := ctx.QueryParam("state")

	casaOSApps, localApps := service.MyService.Docker().GetContainerAppList(&name, &image, &state)
	data := make(map[string]interface{}, 2)
	data["casaos_apps"] = casaOSApps
	data["local_apps"] = localApps

	return ctx.JSON(common_err.SUCCESS, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// NOTE: the API is a temporary and internal API. It will be deleted in the future.
// the API is for archive v1 app for rebuilt v2 app.
func ArchiveContainer(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	appID := ctx.Param("id")

	if err := service.MyService.Docker().StopContainer(appID); err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	// get container name
	container, err := service.MyService.Docker().GetContainer(appID)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	if len(container.Names) == 0 {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "container has no names"})
	}

	if err := service.MyService.Docker().RenameContainer(container.Names[0]+"_old", appID); err != nil {
		return ctx.JSON(http.StatusInternalServerError, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	return ctx.JSON(http.StatusOK, modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS)})
}

// @Summary my app hardware usage list
// @Produce  application/json
// @Accept application/json
// @Tags app
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /app/usage [get]
func AppUsageList(ctx echo.Context) error {
	list := service.MyService.Docker().GetContainerStats()
	return ctx.JSON(common_err.SUCCESS, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: list})
	// return ctx.JSON(common_err.SUCCESS, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: nil})
}

func GetDockerDaemonConfiguration(ctx echo.Context) error {
	// info, err := service.MyService.Docker().GetServerInfo()
	// if err != nil {
	// 	return ctx.JSON(common_err.SERVICE_ERROR, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: err.Error()})
	// 	return
	// }
	data := make(map[string]interface{})

	if file.Exists(dockerRootDirFilePath) {
		buf := file.ReadFullFile(dockerRootDirFilePath)
		err := json.Unmarshal(buf, &data)
		if err != nil {
			return ctx.JSON(common_err.CLIENT_ERROR, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: err})
		}
	}
	return ctx.JSON(common_err.SUCCESS, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

func PutDockerDaemonConfiguration(ctx echo.Context) error {
	// Wave 25: rewriting the docker daemon's data-root and bouncing the
	// service is a host-level operation. Lock it to admins so an account
	// with PermAppInstall alone can't move /var/lib/docker onto a hostile
	// filesystem and force a restart.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}

	request := make(map[string]interface{})
	if err := ctx.Bind(&request); err != nil {
		return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: err})
	}

	value, ok := request["docker_root_dir"]
	if !ok {
		return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: "`docker_root_dir` should not empty"})
	}

	dockerConfig := model.DockerDaemonConfigurationModel{}
	if file.Exists(dockerDaemonConfigurationFilePath) {
		byteResult := file.ReadFullFile(dockerDaemonConfigurationFilePath)
		err := json.Unmarshal(byteResult, &dockerConfig)
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to deserialize " + dockerDaemonConfigurationFilePath, Data: err})
		}
	}

	dockerRootDir, ok := value.(string)
	if !ok {
		return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: "`docker_root_dir` must be a string"})
	}
	if dockerRootDir == "/" {
		dockerConfig.Root = "" // omitempty - empty string will not be serialized
	} else {
		// Validate docker root directory: must be absolute, no traversal,
		// and outside system directories.
		cleanRoot := filepath.Clean(dockerRootDir)
		if !filepath.IsAbs(cleanRoot) || strings.Contains(cleanRoot, "..") {
			return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: "docker root must be an absolute path"})
		}
		// Wave 25: /var was missing — that's where /var/lib/docker, the
		// existing docker state, /var/run/docker.sock, and /var/lib/casanova
		// all live. Pointing daemon root anywhere under /var corrupts host
		// state on the next docker restart. /home, /opt, /tmp added so
		// well-meaning admins don't drop the daemon root onto a tmpfs and
		// lose every container on reboot.
		blockedPrefixes := []string{
			"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64",
			"/boot", "/dev", "/proc", "/sys", "/root",
			"/var", "/run", "/tmp",
		}
		for _, prefix := range blockedPrefixes {
			if cleanRoot == prefix || strings.HasPrefix(cleanRoot, prefix+"/") {
				return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: "docker root must not be in a system directory"})
			}
		}

		if !file.Exists(cleanRoot) {
			return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.DIR_NOT_EXISTS), Data: common_err.GetMsg(common_err.DIR_NOT_EXISTS)})
		}

		dockerConfig.Root = filepath.Join(cleanRoot, "docker")

		if err := file.IsNotExistMkDir(dockerConfig.Root); err != nil {
			return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to create " + dockerConfig.Root, Data: err})
		}
	}

	buf, err := json.Marshal(request)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: "error when trying to serialize docker root json", Data: err})
	}

	if err := file.WriteToFullPath(buf, dockerRootDirFilePath, 0o644); err != nil {
		return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to write " + dockerRootDirFilePath, Data: err})
	}

	buf, err = json.Marshal(dockerConfig)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: "error when trying to serialize docker config", Data: dockerConfig})
	}

	if err := file.WriteToFullPath(buf, dockerDaemonConfigurationFilePath, 0o644); err != nil {
		return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to write to " + dockerDaemonConfigurationFilePath, Data: err})
	}

	if err := systemctl.ReloadDaemon(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to reload systemd daemon"})
	}

	if err := systemctl.StopService("docker"); err != nil {
		return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to stop docker service"})
	}

	if err := systemctl.StartService("docker"); err != nil {
		return ctx.JSON(http.StatusInternalServerError, &modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "error when trying to start docker service"})
	}

	return ctx.JSON(http.StatusOK, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: request})
}

func pullAndInstall(ctx context.Context, imageName string, m *model.CustomizationPostData) error {
	// step
	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeImagePullBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeImagePullEnd, nil)

		if err := service.MyService.Docker().PullImage(ctx, imageName); err != nil {

			go service.PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			return err
		}

		// Wait for image with timeout (max 10 minutes)
		for i := 0; i < 600; i++ {
			if service.MyService.Docker().IsExistImage(m.Image) {
				break
			}
			if i == 599 {
				return fmt.Errorf("timed out waiting for image %s", m.Image)
			}
			time.Sleep(time.Second)
		}

		return nil
	}(); err != nil {
		return err
	}

	var containerID string

	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeContainerCreateBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeContainerCreateEnd, nil)

		_containerID, err := service.MyService.Docker().CreateContainer(ctx, *m, "")
		if err != nil {
			go service.PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			return err
		}

		containerID = _containerID

		eventProperties := common.PropertiesFromContext(ctx)
		eventProperties[common.PropertyTypeContainerID.Name] = containerID

		return nil
	}(); err != nil {
		return err
	}

	// stepStart container
	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, nil)

		if err := service.MyService.Docker().StartContainer(m.ContainerName); err != nil {

			go service.PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	config.CasaOSGlobalVariables.AppChange.Store(true)
	return nil
}

func uninstall(ctx context.Context, ctr *container.InspectResponse, isDelete bool) error {
	// step
	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeContainerStopBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeContainerStopEnd, nil)

		if err := service.MyService.Docker().StopContainer(ctr.ID); err != nil {

			go service.PublishEventWrapper(ctx, common.EventTypeContainerStopError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeContainerRemoveBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeContainerRemoveEnd, nil)

		if err := service.MyService.Docker().RemoveContainer(ctr.ID, false); err != nil {
			go service.PublishEventWrapper(ctx, common.EventTypeContainerRemoveError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	if err := func() error {
		go service.PublishEventWrapper(ctx, common.EventTypeImageRemoveBegin, nil)

		defer service.PublishEventWrapper(ctx, common.EventTypeImageRemoveEnd, nil)

		if err := service.MyService.Docker().RemoveImage(ctr.Config.Image); err != nil {
			logger.Error("error when trying to remove docker image", zap.Error(err), zap.String("image", ctr.Config.Image))

			go service.PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
		}

		return nil
	}(); err != nil {
		return err
	}

	if ctr.Config.Labels["origin"] != "custom" && isDelete {
		// Wave 25: confine config-folder deletion to the apps sandbox.
		//
		// The previous logic split each mount.Source on ctr.Name and joined
		// it back, then handed the result to RMDir without checking the
		// final path. A container name like "../" or a mount whose Source
		// already contained ctr.Name as a substring (e.g. /etc/myapp/extras)
		// could cause RMDir to walk outside the apps directory entirely.
		// Confining the resolved path to AppsPath turns the worst case from
		// "host file deletion" into "no-op + log line".
		//
		// And "no-op" is the ordinary case, not the worst one: an app's mount
		// sources are /DATA/AppData/<name>/…, which is not under AppsPath, so
		// delete_config_folder deletes nothing for a normal app. Same shape and
		// same consequence as the v2 sweep in service/compose_app.go. Repointing
		// it is 27.04 work — converting a silent no-op into a recursive delete
		// of user data is not a frozen-line change.
		//
		// NOTE for whoever does that work: unlike the v2 sweep, the check below
		// admits candidate == appsPath itself (the != is what lets it through),
		// so a candidate that derives to exactly /var/lib/casaos/apps would take
		// every installed app's working dir with it. Not reached today — landing
		// on it needs a mount source containing the apps path, which compose
		// security refuses to bind — but it should be a strict prefix like v2's.
		appsPath := filepath.Clean(config.AppInfo.AppsPath)
		appsPathPrefix := appsPath + string(filepath.Separator)
		for _, v := range ctr.Mounts {
			if !strings.Contains(v.Source, ctr.Name) {
				continue
			}
			candidate := filepath.Clean(filepath.Join(strings.Split(v.Source, ctr.Name)[0], ctr.Name))
			if candidate != appsPath && !strings.HasPrefix(candidate, appsPathPrefix) {
				logger.Error("refusing to delete mount path outside apps directory",
					zap.String("path", candidate),
					zap.String("appsPath", appsPath),
					zap.String("container", ctr.Name),
				)
				continue
			}
			if err := file.RMDir(candidate); err != nil {
				return err
			}
		}
	}
	config.CasaOSGlobalVariables.AppChange.Store(true)

	return nil
}
