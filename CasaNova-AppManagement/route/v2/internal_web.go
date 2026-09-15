package v2

import (
	"fmt"
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/model"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

func (a *AppManagement) GetAppGrid(ctx echo.Context) error {
	// Wave 34 (ported from 27.04, docs/design/per-app-grants.md §7): this
	// handler had NO authorization gate at all — not the wrong bit, none.
	// The router's JWT middleware still refused an anonymous caller, so it
	// was AUTHENTICATED but UNAUTHORIZED: every logged-in account reached
	// it, and so did any context that resolved to guest without a principal
	// behind it. PermAppView is the bit the whole family gets — guest
	// already holds it, so no role loses anything it has today.
	if err := requirePerm(ctx, rbac.PermAppView); err != nil {
		return err
	}

	// v2 Apps
	composeAppsWithStoreInfo, err := composeAppsWithStoreInfo(ctx.Request().Context(), composeAppsWithStoreInfoOpts{
		checkIsUpdateAvailable: false,
	})
	if err != nil {
		message := err.Error()
		logger.Error("failed to list compose apps with store info", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	v2AppGridItems := lo.FilterMap(lo.Values(composeAppsWithStoreInfo), func(app codegen.ComposeAppWithStoreInfo, i int) (codegen.WebAppGridItem, bool) {
		item, err := WebAppGridItemAdapterV2(&app)
		if err != nil {
			logger.Error("failed to adapt web app grid item", zap.Error(err), zap.String("app", app.Compose.Name))
			return codegen.WebAppGridItem{}, false
		}

		return *item, true
	})

	// v1 Apps
	casaOSApps, containers := service.MyService.Docker().GetContainerAppList(nil, nil, nil)

	v1AppGridItems := lo.Map(*casaOSApps, func(app model.MyAppList, i int) codegen.WebAppGridItem {
		item, err := WebAppGridItemAdapterV1(&app)
		if err != nil {
			logger.Error("failed to adapt web app grid item", zap.Error(err), zap.String("app", app.Name))
			return codegen.WebAppGridItem{}
		}
		return *item
	})

	// containers from compose apps
	composeAppContainers := []codegen.ContainerSummary{}
	for _, app := range composeAppsWithStoreInfo {
		composeApp := (service.ComposeApp)(*app.Compose)
		containerLists, err := composeApp.Containers(ctx.Request().Context())
		if err != nil {
			logger.Error("failed to get containers for compose app", zap.Error(err), zap.String("app", composeApp.Name))
			return nil
		}

		for _, containcontainerList := range containerLists {
			composeAppContainers = append(composeAppContainers, containcontainerList...)
		}
	}

	containerAppGridItems := lo.FilterMap(*containers, func(app model.MyAppList, i int) (codegen.WebAppGridItem, bool) {
		if lo.ContainsBy(composeAppContainers, func(container codegen.ContainerSummary) bool { return container.ID == app.ID }) {
			// already exists as compose app, skipping...
			return codegen.WebAppGridItem{}, false
		}

		// check if this is a replacement container for a compose app when applying new settings or updating.
		//
		// we need this logic so that user does not see the temporary replacement container in the UI.
		{
			container, err := service.MyService.Docker().GetContainerByName(app.Name)
			if err != nil {
				logger.Error("failed to get container by name", zap.Error(err), zap.String("container", app.Name))
				return codegen.WebAppGridItem{}, false
			}

			// see recreateContainer() func from https://github.com/docker/compose/blob/v2/pkg/compose/convergence.go
			if replaceLabel, ok := container.Labels[api.ContainerReplaceLabel]; ok {
				if lo.ContainsBy(
					composeAppContainers,
					func(container codegen.ContainerSummary) bool {
						return container.ID == replaceLabel
					},
				) {
					// this is a replacement container for a compose app, skipping...
					return codegen.WebAppGridItem{}, false
				}
			}
		}

		item, err := WebAppGridItemAdapterContainer(&app)
		if err != nil {
			logger.Error("failed to adapt web app grid item", zap.Error(err), zap.String("app", app.Name))
			return codegen.WebAppGridItem{}, false
		}
		return *item, true
	})

	// merge v1 and v2 apps
	var appGridItems []codegen.WebAppGridItem
	appGridItems = append(appGridItems, v2AppGridItems...)
	appGridItems = append(appGridItems, v1AppGridItems...)
	appGridItems = append(appGridItems, containerAppGridItems...)

	return ctx.JSON(http.StatusOK, codegen.GetWebAppGridOK{
		Message: utils.Ptr("This data is for internal use ONLY - will not be supported for public use."),
		Data:    &appGridItems,
	})
}

func WebAppGridItemAdapterV2(composeAppWithStoreInfo *codegen.ComposeAppWithStoreInfo) (*codegen.WebAppGridItem, error) {
	if composeAppWithStoreInfo == nil {
		return nil, fmt.Errorf("v2 compose app is nil")
	}

	// validation
	composeApp := (*service.ComposeApp)(composeAppWithStoreInfo.Compose)
	if composeApp == nil {
		return nil, fmt.Errorf("failed to get compose app")
	}

	item := &codegen.WebAppGridItem{
		AppType: codegen.V2app,
		Name:    &composeApp.Name,
		Title: lo.ToPtr(map[string]string{
			common.DefaultLanguage: composeApp.Name,
		}),
		IsUncontrolled: utils.Ptr(false),
	}

	composeAppStoreInfo := composeAppWithStoreInfo.StoreInfo
	if composeAppStoreInfo != nil {

		// item properties from store info
		item.Hostname = composeAppStoreInfo.Hostname
		item.Icon = &composeAppStoreInfo.Icon
		item.Index = &composeAppStoreInfo.Index
		item.Port = &composeAppStoreInfo.PortMap
		item.Scheme = composeAppStoreInfo.Scheme
		item.Status = composeAppWithStoreInfo.Status
		item.StoreAppID = composeAppStoreInfo.StoreAppID
		item.Title = &composeAppStoreInfo.Title
		item.IsUncontrolled = composeAppStoreInfo.IsUncontrolled

		var mainApp *types.ServiceConfig
		if svc, ok := composeApp.Services[*composeAppStoreInfo.Main]; ok {
			mainApp = &svc
			item.Image = &mainApp.Image // Hengxin needs this image property for some reason...
		}
	}

	// item type
	itemAuthorType := composeApp.AuthorType()
	item.AuthorType = &itemAuthorType
	if composeAppWithStoreInfo.IsUncontrolled == nil {
		item.IsUncontrolled = utils.Ptr(false)
	} else {
		item.IsUncontrolled = composeAppWithStoreInfo.IsUncontrolled
	}

	return item, nil
}

func WebAppGridItemAdapterV1(app *model.MyAppList) (*codegen.WebAppGridItem, error) {
	if app == nil {
		return nil, fmt.Errorf("v1 app is nil")
	}

	item := &codegen.WebAppGridItem{
		AppType:  codegen.V1app,
		Name:     &app.ID,
		Status:   &app.State,
		Image:    &app.Image,
		Hostname: &app.Host,
		Icon:     &app.Icon,
		Index:    &app.Index,
		Port:     &app.Port,
		Scheme:   (*codegen.Scheme)(&app.Protocol),
		Title: &map[string]string{
			common.DefaultLanguage: app.Name,
		},
		IsUncontrolled: &app.IsUncontrolled,
	}

	return item, nil
}

func WebAppGridItemAdapterContainer(container *model.MyAppList) (*codegen.WebAppGridItem, error) {
	if container == nil {
		return nil, fmt.Errorf("container is nil")
	}

	item := &codegen.WebAppGridItem{
		AppType: codegen.Container,
		Name:    &container.ID,
		Status:  &container.State,
		Image:   &container.Image,
		Title: &map[string]string{
			common.DefaultLanguage: container.Name,
		},
		IsUncontrolled: &container.IsUncontrolled,
	}

	return item, nil
}
