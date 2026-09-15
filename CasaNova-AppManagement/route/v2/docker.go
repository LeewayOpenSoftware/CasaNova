package v2

import (
	"context"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/service/v1"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/docker/docker/errdefs"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (a *AppManagement) CheckContainerHealthByID(ctx echo.Context, id codegen.ContainerID) error {
	// Wave 34 (ported from 27.04, docs/design/per-app-grants.md §7): this
	// handler had NO authorization gate at all — not the wrong bit, none.
	// The router's JWT middleware still refused an anonymous caller, so it
	// was AUTHENTICATED but UNAUTHORIZED: every logged-in account reached
	// it, and so did any context that resolved to guest without a principal
	// behind it. PermAppView is the bit the whole family gets — guest
	// already holds it, so no role loses anything it has today. (27.04 also
	// drops this handler's per-call HTTP timeout from 30s to 5s as a
	// separate, non-authorization change — not ported here: out of scope
	// for this security-only pass.)
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	result, err := service.MyService.Docker().CheckContainerHealth(id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{Message: &message})
	}

	if !result {
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{})
	}

	return ctx.JSON(http.StatusOK, codegen.ContainerHealthCheckOK{})
}

func (a *AppManagement) RecreateContainerByID(ctx echo.Context, id codegen.ContainerID, params codegen.RecreateContainerByIDParams) error {
	// Wave 32: gate behind PermAppInstall. RecreateContainer is the
	// container-update path (it can pull a new image, swap volumes,
	// and rebuild from scratch) — that is firmly in "install/update an
	// app" territory and a read-only viewer must not be able to trigger
	// it. Without this gate any authenticated session could rebuild
	// arbitrary containers and cause downtime.
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	container, err := service.MyService.Docker().DescribeContainer(backgroundCtx, id)
	if err != nil {
		message := err.Error()
		if _, ok := err.(errdefs.ErrNotFound); ok {
			return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
		}
		// Wave 27: was returning 500 with codegen.ResponseNotFound — a
		// type/code mismatch that confused both clients and our own
		// monitoring (which alerted on 5xx for what was usually just
		// docker socket churn). Use the InternalServerError shape so
		// the body matches the status line.
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	pullLatestImage := false
	if params.Pull != nil {
		pullLatestImage = *params.Pull
	}

	force := false
	if params.Force != nil {
		force = *params.Force
	}

	common.SetProperties(backgroundCtx, map[string]string{
		common.PropertyTypeAppName.Name: v1.AppName(container),
		common.PropertyTypeAppIcon.Name: v1.AppIcon(container),
	})

	go func() {
		// Detached fire-and-forget worker. Without recover() a panic
		// in RecreateContainer (nil docker client, broken inspect
		// response) propagates straight to the runtime and crashes
		// the daemon — every other in-flight HTTP request fails.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in async container recreate",
					zap.Any("panic", r),
					zap.String("containerID", string(id)))
			}
		}()
		go service.PublishEventWrapper(backgroundCtx, common.EventTypeAppUpdateBegin, nil)

		defer service.PublishEventWrapper(backgroundCtx, common.EventTypeAppUpdateEnd, nil)

		if err := service.MyService.Docker().RecreateContainer(backgroundCtx, id, pullLatestImage, force); err != nil {
			go service.PublishEventWrapper(backgroundCtx, common.EventTypeAppUpdateError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			logger.Error("error when trying to recreate container", zap.Error(err), zap.String("containerID", string(id)), zap.Bool("pull", pullLatestImage))
		}
	}()

	return ctx.JSON(http.StatusOK, codegen.ContainerRecreateOK{
		Message: utils.Ptr("Container is being recreated asynchronously"),
	})
}
