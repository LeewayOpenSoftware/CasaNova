package v2

import (
	"fmt"

	"net/http"
	"os"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/merge"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2/fs"
	"go.uber.org/zap"

	"github.com/labstack/echo/v4"
)

var MessageMergerFSNotEnabled = "mergerfs is not enabled - either it is not enabled in configuration file; merge point is not empty before mounting; or mergerfs is not installed"

func (s *LocalStorage) GetMerges(ctx echo.Context, params codegen.GetMergesParams) error {
	// See v1/disk.go GetDiskList for why this needs PermStorageRead: the
	// Wave 26 hardening pass gated SetMerge/InitMerge below but never
	// touched these reads of the mergerfs pool config.
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	if strings.ToLower(config.ServerInfo.EnableMergerFS) != "true" {
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{Message: &MessageMergerFSNotEnabled})
	}

	merges, err := service.MyService.LocalStorage().GetMerges(params.MountPoint)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}
	data := make([]codegen.Merge, 0, len(merges))
	for _, merge := range merges {
		data = append(data, MergeAdapterOut(merge))
	}
	message := "ok"
	return ctx.JSON(http.StatusOK, codegen.GetMergesResponseOK{Data: &data, Message: &message})

}

func (s *LocalStorage) SetMerge(ctx echo.Context) error {
	// Creating/altering a mergerfs pool rewrites storage layout — admin-only.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var m codegen.Merge
	if err := ctx.Bind(&m); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	// default to mergerfs if fstype is not specified
	fstype := fs.MergerFSFullName
	if m.Fstype != nil {
		fstype = *m.Fstype
	}
	// expand source volume paths to source volumes
	var sourceVolumes []*model2.Volume
	if m.SourceVolumeUuids != nil {
		volumesFromDB, err := service.MyService.Disk().GetSerialAllFromDB()
		if err != nil {
			logger.Error("failed to get serial disks from database", zap.Error(err))
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}

		sourceVolumes = make([]*model2.Volume, 0, len(*m.SourceVolumeUuids))
		for _, volumeUUID := range *m.SourceVolumeUuids {
			volumeFound := false
			for i := range volumesFromDB {
				if volumeUUID == volumesFromDB[i].UUID {
					volumeFound = true
					sourceVolumes = append(sourceVolumes, &volumesFromDB[i])
					break
				}
			}

			if !volumeFound {
				message := "volume " + volumeUUID + " not found, or it is not a CasaOS storage. Consider adding it to CasaOS first."
				return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}
		}
	}

	merge, err := service.MyService.LocalStorage().GetFirstMergeFromDB(m.MountPoint)
	if err != nil {
		message := err.Error()
		logger.Error("failed to get merge from database", zap.Error(err), zap.String("mount point", m.MountPoint))
		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}

	if merge == nil {
		merge = &model2.Merge{
			FSType:         fstype,
			MountPoint:     m.MountPoint,
			SourceBasePath: m.SourceBasePath,
			SourceVolumes:  sourceVolumes,
		}

		if err := service.MyService.LocalStorage().CreateMerge(merge); err != nil {

			message := err.Error()
			logger.Error("failed to create merge", zap.Error(err), zap.String("mount point", m.MountPoint))
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}

		if err := service.MyService.LocalStorage().CreateMergeInDB(merge); err != nil {
			// CreateMerge already brought up a mergerfs mount on
			// disk. If we leave the DB write failure unrecovered,
			// the daemon restarts to a "ghost" merge that nobody
			// owns: it's mounted, blocking the underlying tree, but
			// no record of it exists for the user to manage. Tear
			// down the on-disk merge so the system is back to its
			// pre-call state and the user can retry cleanly.
			logger.Error("failed to create merge in database; rolling back on-disk merge", zap.Error(err), zap.String("mount point", m.MountPoint))
			if rbErr := service.MyService.LocalStorage().Umount(merge.MountPoint); rbErr != nil {
				logger.Error("merge DB rollback failed — manual umount required",
					zap.String("mount point", m.MountPoint),
					zap.NamedError("rollbackErr", rbErr),
					zap.NamedError("createErr", err))
			}
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}
	} else {
		if m.SourceBasePath != nil {
			merge.SourceBasePath = m.SourceBasePath
		}

		if m.SourceVolumeUuids != nil {
			merge.SourceVolumes = sourceVolumes // which come from m.SourceVolumeUuids
		}

		if err := service.MyService.LocalStorage().UpdateMerge(merge); err != nil {
			message := err.Error()
			logger.Error("failed to update merge", zap.Error(err), zap.String("mount point", m.MountPoint))
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}

		if err := service.MyService.LocalStorage().UpdateMergeSourcesInDB(merge); err != nil {
			message := err.Error()
			logger.Error("failed to update merge sources in database", zap.Error(err), zap.String("mount point", m.MountPoint))
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}
	}
	const messageStatus = common.ServiceName + ":merge_status"
	result := MergeAdapterOut(*merge)
	msg := make(map[string]interface{})
	msg["mount_point"] = result.MountPoint
	msg["source_base_path"] = result.SourceBasePath
	msg["source_volume_uuids"] = result.SourceVolumeUuids
	msg["fs_type"] = result.Fstype
	msg["created_at"] = result.CreatedAt
	msg["updated_at"] = result.UpdatedAt

	if err := service.MyService.Notify().SendNotify(messageStatus, msg); err != nil {
		logger.Error("error when sending notification", zap.Error(err), zap.String("message path", messageStatus), zap.Any("message", msg))
	}

	return ctx.JSON(http.StatusOK, codegen.SetMergeResponseOK{
		Data: &result,
	})
}
func (s *LocalStorage) GetMergeInitStatus(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	status := codegen.Uninitialized
	mountPoint := common.DefaultMountPoint

	existingMerges, err := service.MyService.LocalStorage().GetMergeAllFromDB(&mountPoint)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}

	// check if /DATA is already a merge point
	if len(existingMerges) > 0 {
		status = codegen.Initialized
	}
	return ctx.JSON(http.StatusOK, codegen.GetMergeInitStatusResponseOK{Data: &status})

}
func (s *LocalStorage) InitMerge(ctx echo.Context) error {
	// Initializing the merge mount rewrites storage layout — admin-only.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	var m codegen.MountPoint
	if err := ctx.Bind(&m); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if m.MountPoint == "" {
		message := "mount point is empty"
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
	if strings.ToLower(config.ServerInfo.EnableMergerFS) != "true" {
		if !file.CheckNotExist(m.MountPoint) {

			dir, _ := os.ReadDir(constants.DefaultFilePath)
			if len(dir) > 0 {
				message := "Please make sure the /var/lib/casaos/files directory is empty"
				return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}

			// Wave 35-X: SafeRename instead of os.Rename. The two
			// paths often live on different filesystems (DefaultFilePath
			// on rootfs, MountPoint on external disk) — os.Rename
			// returns EXDEV and the user's storage is DESTROYED because
			// the RMDir above already deleted /var/lib/casaos/files.
			// SafeRename falls back to copy+delete on EXDEV via the
			// atomic copy helper in Common/utils/file/safe.go.
			file.RMDir(constants.DefaultFilePath)

			err := file.SafeRename(m.MountPoint, constants.DefaultFilePath)
			if err != nil {
				fmt.Println(err)
				message := "move " + m.MountPoint + " to /var/lib/casaos/files failed"
				return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
			}
		}
		err := file.MkDir(m.MountPoint)
		if err != nil {
			fmt.Println(err)
			message := "create " + m.MountPoint + " failed"
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		if !merge.IsMergerFSInstalled() {
			config.ServerInfo.EnableMergerFS = "false"
			message := "mergerfs is not installed"
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		if !service.MyService.Disk().EnsureDefaultMergePoint() {
			config.ServerInfo.EnableMergerFS = "false"
			message := "default merge point is not empty"
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		// Surface an assembly that did not complete instead of answering OK:
		// the operator just asked to enable the pool, and reporting success
		// over a fenced or half-built /DATA is how a storage problem turns into
		// a data problem later.
		if err := service.MyService.LocalStorage().CheckMergeMount(); err != nil {
			message := "merge point could not be assembled: " + err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}

		config.Cfg.Section("server").Key("EnableMergerFS").SetValue("true")
		config.ServerInfo.EnableMergerFS = "true"

		config.Cfg.SaveTo(config.LocalStorageConfigFilePath)
	} else {
		status := codegen.Initialized
		return ctx.JSON(http.StatusOK, codegen.InitMergeResponseOK{Data: &status})
	}
	status := codegen.Initialized
	return ctx.JSON(http.StatusOK, codegen.InitMergeResponseOK{Data: &status})
}

func MergeAdapterOut(m model2.Merge) codegen.Merge {
	id := int(m.ID)

	sourceVolumeUUIDs := make([]string, 0, len(m.SourceVolumes))
	for _, volume := range m.SourceVolumes {
		sourceVolumeUUIDs = append(sourceVolumeUUIDs, volume.UUID)
	}

	return codegen.Merge{
		Id:                &id,
		Fstype:            &m.FSType,
		MountPoint:        m.MountPoint,
		SourceBasePath:    m.SourceBasePath,
		SourceVolumeUuids: &sourceVolumeUUIDs,
		CreatedAt:         &m.CreatedAt,
		UpdatedAt:         &m.UpdatedAt,
	}
}
