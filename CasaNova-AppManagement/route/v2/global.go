package v2

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"

	"github.com/labstack/echo/v4"
)

// globalEnvMu serializes goroutines that restart compose apps after
// a global environment variable change, preventing concurrent
// modification races when multiple update/delete requests arrive.
var globalEnvMu sync.Mutex

func (a *AppManagement) GetGlobalSettings(ctx echo.Context) error {
	result := make([]codegen.GlobalSetting, 0)
	config.GlobalMu.RLock()
	for key, value := range config.Global {
		result = append(result, codegen.GlobalSetting{
			Key:         utils.Ptr(key),
			Value:       value,
			Description: utils.Ptr(key),
		})
	}
	config.GlobalMu.RUnlock()

	return ctx.JSON(http.StatusOK, codegen.GlobalSettingsOK{
		Data: &result,
	})
}

func (a *AppManagement) GetGlobalSetting(ctx echo.Context, key codegen.GlobalSettingKey) error {
	config.GlobalMu.RLock()
	value, ok := config.Global[string(key)]
	config.GlobalMu.RUnlock()
	if !ok {
		message := "the key is not exist"
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.GlobalSettingOK{
		Data: &codegen.GlobalSetting{
			Key:         utils.Ptr(key),
			Value:       value,
			Description: utils.Ptr(key),
		},
	})
}

func (a *AppManagement) UpdateGlobalSetting(ctx echo.Context, key codegen.GlobalSettingKey) error {
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	var action codegen.GlobalSetting
	if err := ctx.Bind(&action); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
	if err := updateGlobalEnv(ctx, key, action.Value); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.GlobalSettingOK{
		Data: &codegen.GlobalSetting{
			Key:         utils.Ptr(key),
			Value:       action.Value,
			Description: utils.Ptr(key),
		},
	})
}

func updateGlobalEnv(ctx echo.Context, key string, value string) error {
	if key == "" {
		return fmt.Errorf("openai api key is required")
	}
	if err := service.MyService.AppStoreManagement().ChangeGlobal(key, value); err != nil {
		return err
	}

	// Snapshot the query params BEFORE spawning. echo pools and reuses
	// echo.Context values across requests, so reading `ctx` from the
	// goroutine (which starts running after the handler has returned)
	// races with the next request being served on that same recycled
	// context — the event properties published for this change could be
	// another user's query string, or a torn map read. Copy out what we
	// need while we still own the context.
	props := PropertiesFromQueryParams(ctx)

	// re up all containers to apply the new env var
	go func() {
		// Wave 32: recover so a panic in UpWithCheckRequire (broken
		// compose project, missing image) does not crash the daemon.
		// We hold globalEnvMu and the HTTP handler has long since
		// returned 200 — without recover the user sees a fully
		// successful API response while the AppManagement process is
		// already on its way down.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in updateGlobalEnv goroutine",
					zap.Any("panic", r),
					zap.String("key", key))
			}
		}()
		globalEnvMu.Lock()
		defer globalEnvMu.Unlock()

		// Wave 35-X: bounded ctx. Without a deadline, one hung
		// compose-up holds globalEnvMu forever — every subsequent
		// global env API call deadlocks. 10 minutes covers a large
		// stack pull + recreate; anything longer is a real failure.
		baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer baseCancel()
		backgroundCtx := common.WithProperties(baseCtx, props)
		composeAppsWithStoreInfo, err := service.MyService.Compose().List(backgroundCtx)
		if err != nil {
			logger.Error("Failed to get composeAppsWithStoreInfo", zap.Any("error", err))
		}
		for _, project := range composeAppsWithStoreInfo {
			// ApiService() opens a NEW docker connection every call and it
			// was never closed — one leaked socket per app per global-env
			// change, which on a box with a couple dozen stacks walks the
			// daemon into its fd limit. Close it at the end of each
			// iteration (a plain `defer` here would pile every client up
			// until the whole loop finished).
			composeService, dockerClient, err := service.ApiService()
			if err != nil {
				logger.Error("Failed to get Api Service", zap.Any("error", err))
				continue
			}
			func() {
				defer dockerClient.Close()
				project.UpWithCheckRequire(backgroundCtx, composeService)
			}()
		}
	}()

	return nil
}

func deleteGlobalEnv(ctx echo.Context, key string) error {

	if err := service.MyService.AppStoreManagement().DeleteGlobal(key); err != nil {
		return err
	}

	// Snapshot before spawning — see updateGlobalEnv for why touching the
	// pooled echo.Context from the goroutine is a cross-user data race.
	props := PropertiesFromQueryParams(ctx)

	// re up all containers to apply the new env var
	go func() {
		// Wave 32: recover — same rationale as updateGlobalEnv above.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in deleteGlobalEnv goroutine",
					zap.Any("panic", r),
					zap.String("key", key))
			}
		}()
		globalEnvMu.Lock()
		defer globalEnvMu.Unlock()

		// Wave 35-X: bounded ctx — see updateGlobalEnv rationale.
		baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer baseCancel()
		backgroundCtx := common.WithProperties(baseCtx, props)
		composeAppsWithStoreInfo, err := service.MyService.Compose().List(backgroundCtx)
		if err != nil {
			logger.Error("Failed to get composeAppsWithStoreInfo", zap.Any("error", err))
		}
		for _, project := range composeAppsWithStoreInfo {
			// ApiService() opens a NEW docker connection every call and it
			// was never closed — one leaked socket per app per global-env
			// change, which on a box with a couple dozen stacks walks the
			// daemon into its fd limit. Close it at the end of each
			// iteration (a plain `defer` here would pile every client up
			// until the whole loop finished).
			composeService, dockerClient, err := service.ApiService()
			if err != nil {
				logger.Error("Failed to get Api Service", zap.Any("error", err))
				continue
			}
			func() {
				defer dockerClient.Close()
				project.UpWithCheckRequire(backgroundCtx, composeService)
			}()
		}
	}()

	return nil
}

func (a *AppManagement) DeleteGlobalSetting(ctx echo.Context, key codegen.GlobalSettingKey) error {
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	var action codegen.GlobalSetting
	if err := ctx.Bind(&action); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if err := deleteGlobalEnv(ctx, key); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ResponseOK{})
}
