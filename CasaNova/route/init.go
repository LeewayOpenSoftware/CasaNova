/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-11-15 15:51:44
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-11-15 15:55:16
 * @FilePath: /CasaOS/route/init.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package route

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	file1 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/samba"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/encryption"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	v1 "github.com/LeewayOpenSoftware/CasaNova/route/v1"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"go.uber.org/zap"
)

func InitFunction() {
	go InitNetworkMount()
	go InitInfo()
	//go InitZerotier()
}

func InitInfo() {
	mb := model.BaseInfo{}
	if file.Exists(config.AppInfo.DBPath + "/baseinfo.conf") {
		err := json.Unmarshal(file.ReadFullFile(config.AppInfo.DBPath+"/baseinfo.conf"), &mb)
		if err != nil {
			logger.Error("baseinfo.conf", zap.String("error", err.Error()))
		}
	}
	if file.Exists("/etc/CHANNEL") {
		channel := file.ReadFullFile("/etc/CHANNEL")
		mb.Channel = string(channel)
	}
	mac, err := service.MyService.System().GetMacAddress()
	if err != nil {
		logger.Error("GetMacAddress", zap.String("error", err.Error()))
	}
	mb.Hash = encryption.GetMD5ByStr(mac)
	mb.Version = common.VERSION
	osRelease, _ := file1.ReadOSRelease()

	mb.DriveModel = osRelease["MODEL"]
	if len(mb.DriveModel) == 0 {
		mb.DriveModel = "Casa"
	}
	os.Remove(config.AppInfo.DBPath + "/baseinfo.conf")
	by, err := json.Marshal(mb)
	if err != nil {
		logger.Error("init info err", zap.Any("err", err))
		return
	}
	// Wave 33-A: was 0o666 — exposes device fingerprint to every UID on
	// the host. The CasaNova daemon runs as root and is the only consumer.
	file.WriteToFullPath(by, config.AppInfo.DBPath+"/baseinfo.conf", 0o600)
}

// initNetworkMountDelay is how long InitNetworkMount waits for the network to
// come up before it tries to reach the saved NAS hosts. A package variable so
// a test can drive the function without waiting ten real seconds.
var initNetworkMountDelay = 10 * time.Second

// unmountAllStale unmounts every mount point found under a host directory and
// reports whether ANY of them is still mounted afterwards.
//
// It exists to make the error observable. The loop this replaces called
// UnmountSmaba and discarded its error, then os.RemoveAll'd the parent
// regardless -- so a share that refused to unmount (EBUSY, the normal answer
// for one in use) got its live CIFS tree walked and deleted, over the network,
// on the user's NAS.
func unmountAllStale(conns service.ConnectionsService, mountPoints []string) (stillMounted bool) {
	for _, mp := range mountPoints {
		if err := conns.UnmountSmaba(mp); err != nil {
			stillMounted = true
			logger.Error("could not unmount samba share - NOT removing the parent directory",
				zap.Any("err", err), zap.String("mount point", mp))
		}
	}
	return stillMounted
}

func InitNetworkMount() {
	time.Sleep(initNetworkMountDelay)
	connections := service.MyService.Connections().GetConnectionsList()
	for _, v := range connections {
		connection := service.MyService.Connections().GetConnectionByID(fmt.Sprint(v.ID))
		directories, err := samba.GetSambaSharesList(connection.Host, connection.Port, connection.Username, connection.Password)
		if err != nil {
			// This used to DeleteConnection here, which read a failure to ASK
			// as an answer of "no shares". The two are not the same state and
			// the API cannot conflate them: GetSambaSharesList fails at three
			// separate stages -- net.Dial (host unreachable), smb2 Dial (the
			// session/NTLM handshake), ListSharenames (the server answered with
			// an error) -- and NOT ONE of them establishes that the share is
			// gone. The only evidence of absence this API can produce is a
			// SUCCESSFUL listing that omits the share.
			//
			// InitNetworkMount runs 10s after the daemon starts, which on a
			// power-cut reboot is while the switch is still doing STP and the
			// NAS has not started smbd. Deleting there destroys the user's
			// credentials permanently for a NAS that comes back 40s later.
			//
			// So: keep the row, say so, and try again next boot. A stale
			// connection costs a log line; a deleted one costs the user their
			// credentials with no warning and no undo.
			logger.Error("samba share list unavailable at boot - KEEPING the saved connection and skipping this mount attempt",
				zap.Any("err", err), zap.String("host", connection.Host), zap.Uint("id", connection.ID))
			continue
		}
		baseHostPath := "/mnt/" + connection.Host

		mountPointList, err := service.MyService.System().GetDirPath(baseHostPath)
		if err != nil {
			logger.Error("get mount point err", zap.Any("err", err))
			continue
		}
		// UnmountSmaba returns an error and this loop used to discard it, then
		// os.RemoveAll'd the parent unconditionally. When a share is still
		// mounted (EBUSY is the normal case for one that is in use) RemoveAll
		// walks INTO the live CIFS mount and deletes the user's files on the
		// NAS, over the network. Losing the saved credentials was the reported
		// bug; this is the same mistake one directory up and it destroys data
		// that is not even on this machine.
		mountPoints := make([]string, 0, len(mountPointList))
		for _, v := range mountPointList {
			mountPoints = append(mountPoints, v.Path)
		}

		// Only prune the host directory once every child is proven unmounted.
		if unmountAllStale(service.MyService.Connections(), mountPoints) {
			logger.Error("skipping remount for this host: at least one share is still mounted",
				zap.String("host", connection.Host), zap.String("path", baseHostPath))
			continue
		}
		os.RemoveAll(baseHostPath)

		file.IsNotExistMkDir(baseHostPath)
		for _, v := range directories {
			mountPoint := baseHostPath + "/" + v
			file.IsNotExistMkDir(mountPoint)
			service.MyService.Connections().MountSmaba(connection.Username, connection.Host, v, connection.Port, mountPoint, connection.Password)
		}
		connection.Directories = strings.Join(directories, ",")
		service.MyService.Connections().UpdateConnection(&connection)
	}
	err := service.MyService.Storage().CheckAndMountAll()
	if err != nil {
		logger.Error("mount storage err", zap.Any("err", err))
	}
}
func InitZerotier() {
	v1.CheckNetwork()
}
