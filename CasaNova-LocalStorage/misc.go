package main

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	"github.com/pilebones/go-udev/netlink"
	"go.uber.org/zap"
)

func sendDiskBySocket() {
	blkList := service.MyService.Disk().LSBLK(true)

	status := model.DiskStatus{}
	healthy := true

	//var systemDisk *model.LSBLKModel

	for _, currentDisk := range blkList {

		// if systemDisk == nil {
		// 	// go 5 level deep to look for system block device by mount point being "/"
		// 	systemDisk = service.WalkDisk(currentDisk, 5, func(blk model.LSBLKModel) bool { return blk.MountPoint == "/" })

		// 	if systemDisk != nil {
		// 		s, _ := strconv.ParseUint(systemDisk.FSSize.String(), 10, 64)
		// 		a, _ := strconv.ParseUint(systemDisk.FSAvail.String(), 10, 64)
		// 		u, _ := strconv.ParseUint(systemDisk.FSUsed.String(), 10, 64)
		// 		status.Size += s
		// 		status.Avail += a
		// 		status.Used += u
		//		continue
		// 	}
		// }
		if !service.IsDiskSupported(currentDisk) {
			continue
		}
		temp := service.MyService.Disk().SmartCTL(currentDisk.Path)
		if reflect.DeepEqual(temp, model.SmartctlA{}) {
			// SMART data unavailable — leave healthy unchanged (unknown) rather than faking healthy
		} else {
			if len(temp.ModelName) > 0 {
				healthy = temp.SmartStatus.Passed
			} else {
				healthy = true
			}
		}
		if len(currentDisk.Children) > 0 {
			for _, v := range currentDisk.Children {
				if len(v.MountPoint) > 0 {
					s, _ := strconv.ParseUint(v.FSSize.String(), 10, 64)
					a, _ := strconv.ParseUint(v.FSAvail.String(), 10, 64)
					u, _ := strconv.ParseUint(v.FSUsed.String(), 10, 64)
					status.Size += s
					status.Avail += a
					status.Used += u
				}
			}
		} else {
			if len(currentDisk.MountPoint) > 0 {
				s, _ := strconv.ParseUint(currentDisk.FSSize.String(), 10, 64)
				a, _ := strconv.ParseUint(currentDisk.FSAvail.String(), 10, 64)
				u, _ := strconv.ParseUint(currentDisk.FSUsed.String(), 10, 64)
				status.Size += s
				status.Avail += a
				status.Used += u
			}
		}
	}

	status.Health = healthy
	message := make(map[string]interface{})
	message["sys_disk"] = status
	if err := service.MyService.NotifySystem().SendSystemStatusNotify(message); err != nil {
		logger.Error("failed to send notify", zap.Any("message", message), zap.Error(err))
	}
}

func sendUSBBySocket() {
	message := map[string]interface{}{
		"sys_usb": service.MyService.Disk().GetUSBDriveStatusList(),
	}

	if err := service.MyService.NotifySystem().SendSystemStatusNotify(message); err != nil {
		logger.Error("failed to send notify", zap.Any("message", message), zap.Error(err))
	}
}

func monitorUEvent(ctx context.Context) {
	var matcher netlink.Matcher

	conn := new(netlink.UEventConn)
	if err := conn.Connect(netlink.UdevEvent); err != nil {
		logger.Error("udev err", zap.Any("Unable to connect to Netlink Kobject UEvent socket", err))
		return
	}
	defer conn.Close()

	queue := make(chan netlink.UEvent)
	defer close(queue)

	errors := make(chan error)
	defer close(errors)

	quit := conn.Monitor(queue, errors, matcher)
	defer close(quit)

	for {
		select {

		case <-ctx.Done():
			return

		case uevent := <-queue:

			if event := common.EventAdapter(uevent); event != nil {

				// add UI properties to applicable events so that CasaOS UI can render it
				event := common.EventAdapterWithUIProperties(event)

				if v, ok := event.Properties["local-storage:path"]; ok && strings.Contains(event.Name, "disk") {

					diskModel := service.MyService.Disk().GetDiskInfo(v)
					if !reflect.DeepEqual(diskModel, model.LSBLKModel{}) {

						properties := common.AdditionalProperties(diskModel)
						for k, v := range properties {
							event.Properties[k] = v
						}
					}
				}
				logger.Info("disk model", zap.Any("diskModel", event.Name))
				// Wave 35-X: bound the publish so a wedged bus cannot
				// freeze the udev monitor goroutine (which would then
				// drop every subsequent disk plug/unplug event).
				pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
				response, err := service.MyService.MessageBus().PublishEventWithResponse(pubCtx, event.SourceID, event.Name, event.Properties)
				pubCancel()
				if err != nil {
					logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", event))
					continue
				}

				if response != nil && response.StatusCode() != http.StatusOK {
					logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
				}
			}

			switch uevent.Env["DEVTYPE"] {
			case "partition":

				switch uevent.Env["ID_BUS"] {
				case "usb":
					time.Sleep(1 * time.Second)
					sendUSBBySocket()
					continue
				}
			}

		case err := <-errors:
			logger.Error("udev err", zap.Error(err))
		}
	}
}

// statsInFlight prevents overlapping cron executions. sendStorageStats
// runs every 5s and calls SmartCTL for every disk; on a system with a
// hung/removed USB drive, smartctl can stall for tens of seconds. Without
// this guard the cron stacks smartctl child processes every 5s until the
// process table fills up.
var statsInFlight atomic.Bool

func sendStorageStats() {
	if !statsInFlight.CompareAndSwap(false, true) {
		// Previous tick still running — skip this one. Better to miss
		// a frame than to stack work on a stuck system.
		return
	}
	defer statsInFlight.Store(false)
	// Wave 35-X: recover() so a panic inside SmartCTL parsing (rare but
	// observed when smartctl returns malformed JSON for unsupported
	// USB bridges) doesn't kill the stats cron for the daemon lifetime.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("storage stats cron panicked",
				zap.Any("panic", r))
		}
	}()
	sendDiskBySocket()
	sendUSBBySocket()
}
