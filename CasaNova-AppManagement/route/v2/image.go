package v2

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/service/v1"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// pullImagesMaxBatch caps the number of containers a single PullImages
// request can fan out to. Without a cap, an authenticated user could
// POST `containerIds=` with thousands of comma-separated entries and
// each one spawns a goroutine that drags the docker daemon (and the
// upstream registry rate limit). 64 is well above the largest install
// any home server has and small enough that an honest mistake doesn't
// take the box down.
const pullImagesMaxBatch = 64

func (a *AppManagement) PullImages(ctx echo.Context, params codegen.PullImagesParams) error {
	// Wave 32: gate behind PermAppInstall. Pulling new images consumes
	// disk + bandwidth and is the privileged half of "install/update an
	// app" — a read-only viewer should not be able to trigger arbitrary
	// `docker pull` operations against the upstream registry.
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}

	// attach context key/value pairs from upstream.
	//
	// Wave 35-X: bound the request-scoped ctx to 5s for the read
	// (container inspect) and let each spawned pull goroutine derive
	// its own 30-min ctx. The previous unbounded backgroundCtx meant
	// the synchronous container inspect could hang the handler, and
	// pulled goroutines lived forever on a stuck registry.
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	backgroundCtx := readCtx

	if params.ContainerIds != nil {
		containerIDs := strings.Split(*params.ContainerIds, ",")
		// Wave 32: bound the fan-out so an attacker (or a buggy client
		// that hot-loops the endpoint) can't queue thousands of
		// goroutines + outbound registry connections.
		if len(containerIDs) > pullImagesMaxBatch {
			message := fmt.Sprintf("too many containerIds: max %d per request", pullImagesMaxBatch)
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}
		for _, containerID := range containerIDs {

			container, err := docker.Container(backgroundCtx, containerID)
			if err != nil {
				logger.Error("get container info failed", zap.Error(err))
				continue
			}

			imageName := docker.ImageName(container)
			if imageName == "" {
				continue
			}

			go func(containerID, imageName string) {
				// Wave 32: recover so a panic inside PullLatestImage
				// (broken docker SDK response, network race) does not
				// take the entire AppManagement daemon down. The HTTP
				// handler has already returned 200; without recover an
				// in-flight pull failure here is invisible AND fatal.
				defer func() {
					if r := recover(); r != nil {
						logger.Error("PullImages goroutine panicked",
							zap.Any("panic", r),
							zap.String("containerID", containerID),
							zap.String("image", imageName))
					}
				}()
				// Wave 35-X: derive a fresh 30-min ctx PER pull
				// goroutine. Inheriting the read-scoped ctx (5s)
				// would cancel the pull immediately when the
				// request handler returns. context.Background gives
				// the goroutine its own root, then WithTimeout
				// guarantees it dies if the registry hangs.
				pullCtx, pullCancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer pullCancel()
				pullCtx = common.WithProperties(pullCtx, PropertiesFromQueryParams(ctx))

				eventProperties := common.PropertiesFromContext(pullCtx)
				eventProperties[common.PropertyTypeAppName.Name] = v1.AppName(container)
				eventProperties[common.PropertyTypeAppIcon.Name] = v1.AppIcon(container)

				_, err := service.MyService.Docker().PullLatestImage(pullCtx, imageName)
				if err != nil {
					logger.Error("pull new image failed", zap.Error(err), zap.String("image", imageName))
				}
			}(containerID, imageName)
		}
	}

	return ctx.JSON(http.StatusOK, codegen.PullImagesOK{
		Message: utils.Ptr("Images are being pulled asynchronously"),
	})
}
