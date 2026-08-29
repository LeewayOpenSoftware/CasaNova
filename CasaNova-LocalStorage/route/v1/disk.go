package v1

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	model1 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

const messagePathStorageStatus = common.ServiceName + ":storage_status"

// diskMap tracks disks with in-flight operations to prevent concurrent format/mount.
// Uses sync.Map to prevent concurrent map panics from simultaneous API requests.
var diskMap sync.Map

type StorageMessage struct {
	Type   string `json:"type"`   // sata,usb
	Action string `json:"action"` // remove add
	Path   string `json:"path"`
	Volume string `json:"volume"`
	Size   uint64 `json:"size"`
}

// @Summary disk list
// @Produce  application/json
// @Accept application/json
// @Tags disk
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /disk/list [get]
func GetDiskList(ctx echo.Context) error {
	blkList := service.MyService.Disk().LSBLK(false)

	dbList, err := service.MyService.Disk().GetSerialAllFromDB()
	if err != nil {
		logger.Error("error when getting all volumes from database", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}

	part := make(map[string]int64, len(dbList))
	for _, v := range dbList {
		part[v.MountPoint] = v.CreatedAt
	}

	disks := []model1.Drive{}
	avail := []model1.Drive{}

	var systemDisk *model1.LSBLKModel

	for _, currentDisk := range blkList {
		childre := []model1.DiskChildren{}
		supported := true
		if len(currentDisk.Children) > 0 {
			for _, v := range currentDisk.Children {
				if !service.IsFormatSupported(v) {
					supported = false
				}
				t := model1.DiskChildren{
					Name:       v.Name,
					Size:       v.Size,
					Format:     v.FsType,
					UUID:       v.UUID,
					MountPoint: v.MountPoint,
					Supported:  service.IsFormatSupported(v),
				}
				// Usage comes from the service's bounded probe, which owns the
				// deadline, the caching and the breaker. The private cache that
				// used to live here fixed the polling cost but still called
				// statfs directly, so one unresponsive mount parked this
				// handler's goroutine forever and every poll added another.
				if v.MountPoint != "" {
					usage, err := service.MountUsage(v.MountPoint)
					switch {
					case err != nil:
						// A filesystem that did not answer is not an empty one.
						// Leaving the byte counts at zero without saying so is
						// how a wedged mount got rendered as a disk with
						// nothing on it.
						t.UsageUnknown = true
					default:
						t.UsedBytes = usage.UsedBytes
						t.AvailBytes = usage.AvailBytes
						t.UsageStale = usage.Stale
					}
				}
				childre = append(childre, t)
			}
		} else {
			if !service.IsFormatSupported(currentDisk) {
				supported = false
			}
		}

		// PTUUID identifies the partition table; falls back to the disk
		// FS UUID for whole-disk filesystems (no partition table). Stable
		// across reboots and USB-port shuffles — unlike /dev/sdX.
		diskUUID := currentDisk.PTUUID
		if diskUUID == "" {
			diskUUID = currentDisk.UUID
		}

		// Aggregate child-level usage so the disk row in the UI can show
		// "Used / Total" without doing a second statfs roundtrip per row.
		var totalUsed, totalAvail uint64
		for _, c := range childre {
			totalUsed += c.UsedBytes
			totalAvail += c.AvailBytes
		}

		disk := model1.Drive{
			Serial:         currentDisk.Serial,
			Name:           currentDisk.Name,
			Size:           currentDisk.Size,
			Path:           currentDisk.Path,
			Model:          currentDisk.Model,
			UUID:           diskUUID,
			UsedBytes:      totalUsed,
			AvailBytes:     totalAvail,
			ChildrenNumber: len(currentDisk.Children),
			Children:       childre,
			Supported:      supported,
		}

		if currentDisk.Rota {
			disk.DiskType = "HDD"
		} else {
			disk.DiskType = "SSD"
		}
		if currentDisk.Tran == "usb" {
			disk.DiskType = "USB"
		}

		temp := service.MyService.Disk().SmartCTL(currentDisk.Path)
		if !reflect.DeepEqual(temp, model1.SmartctlA{}) {
			disk.Temperature = temp.Temperature.Current
			disk.PowerOnHours = temp.PowerOnTime.Hours
		}

		if systemDisk == nil {
			// go 5 level deep to look for system block device by mount point being "/"
			systemDisk := service.WalkDisk(currentDisk, 5, func(blk model1.LSBLKModel) bool { return blk.MountPoint == "/" })

			if systemDisk != nil {
				disk.Model = "System"
				if strings.Contains(systemDisk.SubSystems, "mmc") {
					disk.DiskType = "MMC"
				} else if strings.Contains(systemDisk.SubSystems, "usb") {
					disk.DiskType = "USB"
				}
				disk.Health = "true"

				disks = append(disks, disk)
				continue
			}
		}

		if !service.IsDiskSupported(currentDisk) {
			continue
		}

		// `smartUnavailable` distinguishes "smartctl returned no data" from
		// "smartctl returned Passed=false". The string serialised below uses
		// "unknown" in the first case so the UI can render it as a neutral
		// state instead of a red warning for every USB drive without SMART.
		smartUnavailable := reflect.DeepEqual(temp, model1.SmartctlA{})

		isAvail := true
		if len(currentDisk.MountPoint) != 0 {
			isAvail = false
		} else {
			for _, v := range currentDisk.Children {
				if v.MountPoint != "" {
					isAvail = false
				}
			}
		}

		if isAvail {
			disk.NeedFormat = false
			avail = append(avail, disk)
		}

		if smartUnavailable {
			disk.Health = "unknown"
		} else {
			disk.Health = strconv.FormatBool(temp.SmartStatus.Passed)
		}

		disks = append(disks, disk)
	}

	data := map[string]interface{}{
		"disks": disks,
		"avail": avail,
	}

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

// @Summary disk list
// @Produce  application/json
// @Accept application/json
// @Tags disk
// @Security ApiKeyAuth
// @Success 200 {string} string "ok"
// @Router /disk/list [get]

func DeleteDisksUmount(ctx echo.Context) error {
	// Wave 26: unmounting + removing the mount dir is destructive — admin
	// only. The endpoint also nukes share entries; non-admins must not
	// be able to unshare somebody else's storage either.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	js := make(map[string]string)
	if err := ctx.Bind(&js); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: err.Error()})
	}

	// requires password from user to confirm the action
	// if claims, err := jwt.ParseToken(c.GetHeader("Authorization"), false); err != nil || encryption.GetMD5ByStr(js["password"]) != claims.Password {
	// 	return ctx.JSON(http.StatusUnauthorized, model.Result{Success: common_err.PWD_INVALID, Message: common_err.GetMsg(common_err.PWD_INVALID)})
	// 	return
	// }

	path := js["path"]

	if len(path) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}

	if err := validateDevicePath(ctx, path); err != nil {
		return err
	}

	if _, loaded := diskMap.LoadOrStore(path, struct{}{}); loaded {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.DISK_BUSYING, Message: common_err.GetMsg(common_err.DISK_BUSYING)})
	}
	defer diskMap.Delete(path)

	diskInfo := service.MyService.Disk().GetDiskInfo(path)
	if len(diskInfo.Children) == 0 && service.IsDiskSupported(diskInfo) {
		t := diskInfo
		t.Children = nil
		diskInfo.Children = append(diskInfo.Children, t)
	}
	for _, v := range diskInfo.Children {
		if err := service.MyService.Disk().UmountPointAndRemoveDir(v); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{Success: common_err.REMOVE_MOUNT_POINT_ERROR, Message: err.Error()})
		}

		// delete data
		if err := service.MyService.Disk().DeleteMountPointFromDB(v.Path, v.MountPoint); err != nil {
			logger.Error("error when deleting mount point from database", zap.Error(err), zap.String("path", v.Path), zap.String("mount point", v.MountPoint))
		}

		if err := service.MyService.Shares().DeleteShare(v.MountPoint); err != nil {
			logger.Error("error when deleting share by mount point", zap.Error(err), zap.String("mount point", v.MountPoint))
		}
	}

	service.MyService.Disk().RemoveLSBLKCache()

	// send notify to client
	go func() {
		message := map[string]interface{}{
			"data": StorageMessage{
				Action: "REMOVED",
				Path:   path,
				Volume: "",
				Size:   0,
				Type:   "",
			},
		}

		if err := service.MyService.Notify().SendNotify(messagePathStorageStatus, message); err != nil {
			logger.Error("error when sending notification", zap.Error(err), zap.String("message path", messagePathStorageStatus), zap.Any("message", message))
		}
	}()

	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: path})
}

func GetDiskSize(ctx echo.Context) error {
	path := ctx.QueryParam("path")
	if len(path) == 0 {
		return ctx.JSON(common_err.CLIENT_ERROR, model.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS)})
	}
	// Bounded probe: this endpoint takes an arbitrary path from the caller, so
	// before the deadline existed a request naming a dead network mount held
	// the handler goroutine (and the thread under it) for the life of the
	// process, and nothing stopped a client from repeating it.
	usage, err := service.MountUsage(path)
	if err != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, model.Result{Success: common_err.SERVICE_ERROR, Message: err.Error()})
	}
	data := map[string]interface{}{
		"path":  path,
		"free":  usage.AvailBytes,
		"used":  usage.UsedBytes,
		"stale": usage.Stale,
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}
