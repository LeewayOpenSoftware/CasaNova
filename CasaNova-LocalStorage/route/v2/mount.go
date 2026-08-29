package v2

import (
	"errors"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	v2 "github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/v2"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MountError interface {
	Error() string
	Cause() error
	Unwrap() error
}

func (s *LocalStorage) GetMounts(ctx echo.Context, params codegen.GetMountsParams) error {
	mounts, err := service.MyService.LocalStorage().GetMounts(params)
	if err != nil {
		message := err.Error()
		response := codegen.BaseResponse{
			Message: &message,
		}
		return ctx.JSON(http.StatusInternalServerError, response)
	}

	return ctx.JSON(http.StatusOK, codegen.GetMountsResponseOK{
		Data: &mounts,
	})
}

func (s *LocalStorage) Mount(ctx echo.Context) error {
	// Wave 26: mounting block devices is admin-only and confined to a
	// fixed list of safe roots. Without these guards a non-admin could
	// shadow /etc with a writable tmpfs and own the host.
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}

	var request codegen.Mount
	if err := ctx.Bind(&request); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if err := validateMountPoint(request.MountPoint); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	mount, err := service.MyService.LocalStorage().Mount(request)
	if err != nil {
		message := err.Error()

		if errors.Is(err, v2.ErrAlreadyMounted) || errors.Is(err, v2.ErrMountPointIsNotEmpty) {
			return ctx.JSON(http.StatusConflict, codegen.ResponseConflict{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.AddMountResponseOK{Data: mount})
}

func (s *LocalStorage) Umount(ctx echo.Context, params codegen.UmountParams) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}
	// Wave 26: also confine the unmount target. Otherwise a non-admin
	// path is gone (above) but a malicious admin compromise could still
	// hand `/` or `/proc` to umount2(2) and brick the box.
	if err := validateMountPoint(params.MountPoint); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
	if err := service.MyService.LocalStorage().Umount(params.MountPoint); err != nil {
		message := err.Error()

		if errors.Is(err, v2.ErrNotMounted) {
			return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.UmountResponseOK{})
}

func (s *LocalStorage) UpdateMount(ctx echo.Context, params codegen.UpdateMountParams) error {
	if err := requirePerm(ctx, rbac.PermStorageWrite); err != nil {
		return err
	}

	var request codegen.Mount
	if err := ctx.Bind(&request); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	// Wave 26: confine both the old (umount) and the new (mount) paths.
	if err := validateMountPoint(params.MountPoint); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
	if err := validateMountPoint(request.MountPoint); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	// Snapshot the existing mount BEFORE unmounting. If the new Mount
	// call fails, we can roll back and re-attach the original so the
	// user doesn't silently lose access to their data. Without this
	// snapshot, a failed UpdateMount left the volume detached with
	// no way to recover short of a manual remount.
	var previousMount *codegen.Mount
	existing, err := service.MyService.LocalStorage().GetMounts(codegen.GetMountsParams{
		MountPoint: &params.MountPoint,
	})
	if err == nil && len(existing) > 0 {
		snap := existing[0]
		previousMount = &snap
	}

	if err := service.MyService.LocalStorage().Umount(params.MountPoint); err != nil {
		message := err.Error()

		if !errors.Is(err, v2.ErrNotMounted) {
			return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
		}
	}

	mount, err := service.MyService.LocalStorage().Mount(request)
	if err != nil {
		message := err.Error()

		// Attempt to roll back to the previous mount so the user
		// isn't left with a detached volume. If rollback also fails
		// we log both failures but still surface the original error
		// to the caller — they need to know the update didn't stick.
		if previousMount != nil {
			if _, rbErr := service.MyService.LocalStorage().Mount(*previousMount); rbErr != nil &&
				!errors.Is(rbErr, v2.ErrAlreadyMounted) {
				logger.Error("UpdateMount rollback failed — previous mount is NOT reattached",
					zap.String("mountpoint", params.MountPoint),
					zap.Error(rbErr),
					zap.NamedError("updateErr", err),
				)
			} else {
				logger.Info("UpdateMount rolled back to previous mount after failure",
					zap.String("mountpoint", params.MountPoint),
					zap.NamedError("updateErr", err),
				)
			}
		}

		if errors.Is(err, v2.ErrAlreadyMounted) || errors.Is(err, v2.ErrMountPointIsNotEmpty) {
			return ctx.JSON(http.StatusConflict, codegen.ResponseConflict{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.BaseResponse{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.AddMountResponseOK{Data: mount})
}
