package v2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var ErrComposeAppIDNotProvided = errors.New("compose AppID (compose project name) is not provided")

// composeAppConflict maps the lifecycle REJECTION sentinels to the message of a
// 409 response, and reports whether err is one of them.
//
// A rejection means the request did nothing at all: another operation owns the
// app right now, or the name is already installed. Those used to be reported as
// 500, so the first user-visible effect of holding the app lock for a whole
// update was that a double-click on "Actualizar", or a settings save during the
// nightly sweep, showed "internal server error" — indistinguishable from a real
// fault, and an invitation to keep retrying against an app that is mid-update.
// Kept as one function so the three handlers that can hit a rejection cannot
// drift apart, and so the mapping itself is testable without an HTTP stack.
func composeAppConflict(err error, appID string) (string, bool) {
	switch {
	case errors.Is(err, service.ErrComposeAppAlreadyInstalled):
		return fmt.Sprintf("compose app `%s` is already installed - update or uninstall it instead", appID), true
	case errors.Is(err, service.ErrComposeAppBusy):
		return fmt.Sprintf("compose app `%s` is busy with another operation - try again in a moment", appID), true
	}
	return "", false
}

func (a *AppManagement) MyComposeAppList(ctx echo.Context) error {
	// Use a bounded context so a slow Docker daemon or registry can't
	// stall the entire response indefinitely.
	listCtx, cancel := context.WithTimeout(ctx.Request().Context(), 15*time.Second)
	defer cancel()

	// Never check for updates during the list call — it hits Docker
	// registries synchronously per app and can take minutes when DNS
	// is slow or unavailable.
	composeAppsWithStoreInfo, err := composeAppsWithStoreInfo(listCtx, composeAppsWithStoreInfoOpts{
		checkIsUpdateAvailable: false,
	})
	if err != nil {
		message := err.Error()
		logger.Error("failed to list compose apps with store info", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppListOK{
		Data: &composeAppsWithStoreInfo,
	})
}

func (a *AppManagement) MyComposeApp(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	accept := ctx.Request().Header.Get(echo.HeaderAccept)
	if accept == common.MIMEApplicationYAML {
		// generate yaml should to replace all yaml.Marshal. But for now, we just use it Setting Page API
		yaml, err := service.GenerateYAMLFromComposeApp(*composeApp)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		return ctx.String(http.StatusOK, string(yaml))
	}

	storeInfo, err := composeApp.StoreInfo(true)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	status, err := service.MyService.Compose().Status(ctx.Request().Context(), composeApp.Name)
	if err != nil {
		status = "unknown"
		logger.Error("failed to get compose app status", zap.Error(err), zap.String("composeAppID", id))
	}

	// disable the because performance issue
	// check update is hard and cost a lot of time. specially when the tag is latest
	// such as Stable Diffusion. the check by ZimaOS GPU Application by @LinkLeong
	// Alought @LinkLeong Didn't need the field.
	// We should add a new API to get app info without update info
	// and restore the following code

	// check if updateAvailable
	// updateAvailable := service.MyService.AppStoreManagement().IsUpdateAvailable(composeApp)

	message := fmt.Sprintf("!! JSON format is for debugging purpose only - use `Accept: %s` HTTP header to get YAML instead !!", common.MIMEApplicationYAML)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppOK{
		// extension properties aren't marshalled - https://github.com/golang/go/issues/6213
		Message: &message,
		Data: &codegen.ComposeAppWithStoreInfo{
			StoreInfo: storeInfo,
			Compose:   (*types.Project)(composeApp),
			Status:    &status,

			// see above comment
			UpdateAvailable: nil,
		},
	})
}

func (a *AppManagement) IsNewComposeUncontrolled(newComposeApp *service.ComposeApp) (bool, error) {
	// to check if the new compose app is uncontrolled
	newTag, err := newComposeApp.MainTag()
	if err != nil {
		return false, err
	}

	// TODO refactor this. because if user not update. the status will be uncontrolled.
	if lo.Contains(common.NeedCheckDigestTags, newTag) {
		return false, nil
	}

	// compare store info
	StoreApp, err := service.MyService.AppStoreManagement().ComposeApp(newComposeApp.Name)
	if err != nil {
		return false, err
	}

	if StoreApp == nil {
		logger.Error("store app not found", zap.String("composeAppID", newComposeApp.Name))
		return false, nil
	}

	StableTag, err := StoreApp.MainTag()
	if err != nil {
		return false, err
	}

	return StableTag != newTag, nil
}

func (a *AppManagement) ApplyComposeAppSettings(ctx echo.Context, id codegen.ComposeAppID, params codegen.ApplyComposeAppSettingsParams) error {
	if err := requirePerm(ctx, rbac.PermAppConfigure); err != nil {
		return err
	}

	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	buf, err := YAMLfromRequest(ctx)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// Validate the new compose against the ALREADY-INSTALLED app: this is an
	// existing-app edit, not a fresh untrusted import, so pre-existing
	// dangerous settings (e.g. network_mode: host) must be preserved while
	// newly-introduced dangerous settings are still rejected.
	newComposeApp, err := service.NewComposeAppFromYAMLForUpdate(buf, composeApp)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// v0.1: tolerate IsNewComposeUncontrolled errors — same reasoning
	// as the install handler. The flag is informational; we should
	// never block the user's Apply because we couldn't compute it.
	uncontrolled, err := a.IsNewComposeUncontrolled(newComposeApp)
	if err != nil {
		logger.Info("could not determine uncontrolled flag, defaulting to false",
			zap.Error(err), zap.String("name", newComposeApp.Name))
		uncontrolled = false
	}

	_ = newComposeApp.SetUncontrolled(uncontrolled)

	// Preserve the installed app's store metadata (x-casaos) across the edit.
	// Neither the client nor the GET compose endpoint round-trips the x-casaos
	// extension, so applying the client's compose as-is DROPS it and the app
	// loses its tile title/icon (and store_app_id, main, etc.) — every tag edit
	// / settings save reduced x-casaos to a stub titled with the raw app id.
	// Updating a configured app must KEEP its data; regenerating store metadata
	// is for fresh installs only. Any display fields the user actually edited in
	// the form arrive via an x-casanova block — overlay just those onto the
	// preserved x-casaos (the extension the backend reads for the tile).
	if installedCaos, ok := composeApp.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok && len(installedCaos) > 0 {
		merged := make(map[string]interface{}, len(installedCaos))
		for k, v := range installedCaos {
			merged[k] = v
		}
		if nova, ok := newComposeApp.Extensions["x-casanova"].(map[string]interface{}); ok {
			// Overlay only GENUINE display edits. The client always sends
			// x-casanova with its current editable title/icon; on a tag change
			// those fall back to empty / the raw app id, which must NOT clobber
			// the preserved metadata (that was the whole bug).
			if icon, ok := nova["icon"].(string); ok && icon != "" {
				merged["icon"] = icon
			}
			if t, ok := nova["title"].(map[string]interface{}); ok {
				if en, ok := t["en_us"].(string); ok && en != "" && en != composeApp.Name {
					merged["title"] = t
				}
			}
			for _, k := range []string{"index", "scheme", "port_map", "hostname"} {
				if v, present := nova[k]; present && v != nil && v != "" {
					merged[k] = v
				}
			}
		}
		if newComposeApp.Extensions == nil {
			newComposeApp.Extensions = types.Extensions{}
		}
		newComposeApp.Extensions[common.ComposeExtensionNameXCasaOS] = merged
		delete(newComposeApp.Extensions, "x-casanova")
	}

	buf, err = service.GenerateYAMLFromComposeApp(*newComposeApp)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	if params.CheckPortConflict == nil || *params.CheckPortConflict {

		// validation 1 - check if there are ports in use
		validation, err := newComposeApp.GetPortsInUse()
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		if validation != nil && validation.PortsInUse != nil {
			// we want to ignore the ports being used by current compose app
			for _, service := range composeApp.Services {
				for _, portToSkip := range service.Ports {
					if validation.PortsInUse.TCP != nil {
						tcpPortsInUse := []string{}
						for _, tcpPort := range *validation.PortsInUse.TCP {
							if tcpPort != portToSkip.Published {
								tcpPortsInUse = append(tcpPortsInUse, tcpPort)
							}
						}
						validation.PortsInUse.TCP = &tcpPortsInUse
					}

					if validation.PortsInUse.UDP != nil {
						udpPortsInUse := []string{}
						for _, udpPort := range *validation.PortsInUse.UDP {
							if udpPort != portToSkip.Published {
								udpPortsInUse = append(udpPortsInUse, udpPort)
							}
						}
						validation.PortsInUse.UDP = &udpPortsInUse
					}
				}
			}

			if (validation.PortsInUse.TCP != nil && len(*validation.PortsInUse.TCP) > 0) ||
				(validation.PortsInUse.UDP != nil && len(*validation.PortsInUse.UDP) > 0) {

				validationErrors := codegen.ComposeAppValidationErrors{}
				if err := validationErrors.FromComposeAppValidationErrorsPortsInUse(*validation); err != nil {
					message := err.Error()
					return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
						Message: &message,
					})
				}

				message := "there are ports in use"
				return ctx.JSON(http.StatusBadRequest, codegen.ComposeAppBadRequest{
					Message: &message,
					Data:    &validationErrors,
				})
			}
		}
	}

	if params.DryRun != nil && *params.DryRun {
		return ctx.JSON(http.StatusOK, codegen.ComposeAppInstallOK{
			Message: lo.ToPtr("only validation has been done because `dry_run` is specified - skipping compose app installation"),
		})
	}

	// attach context key/value pairs from upstream
	//
	// Wave 35-X: 30-min bound. composeApp.Apply runs synchronously
	// inside this HTTP handler and triggers compose-up under the
	// hood. Without a deadline, a stuck pull pinned the request
	// goroutine forever; the user saw the spinner indefinitely with
	// no error ever surfacing.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer baseCancel()
	backgroundCtx := common.WithProperties(baseCtx, PropertiesFromQueryParams(ctx))

	if err := composeApp.Apply(backgroundCtx, buf); err != nil {
		message := err.Error()
		if conflict, ok := composeAppConflict(err, composeApp.Name); ok {
			return ctx.JSON(http.StatusConflict, codegen.ComposeAppBadRequest{Message: &conflict})
		}
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateSettingsOK{
		Message: utils.Ptr("compose app is being applied with changes asynchroniously"),
	})
}

func (a *AppManagement) InstallComposeApp(ctx echo.Context, params codegen.InstallComposeAppParams) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	buf, err := YAMLfromRequest(ctx)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// validate new compose yaml
	composeApp, err := service.NewComposeAppFromYAML(buf, false, true)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// v0.1: tolerate IsNewComposeUncontrolled errors. A catalog YAML
	// that's missing the `main` field in its x-casanova/x-casaos
	// extension (or whose store-side compose can't be resolved) would
	// otherwise hard-fail the install with a 500 — the user sees
	// "main service not specified" in a toast and has no way forward.
	// The uncontrolled flag is purely informational (drives the
	// "update available" badge); defaulting to false when we can't
	// compute it lets the install proceed.
	uncontrolled, err := a.IsNewComposeUncontrolled(composeApp)
	if err != nil {
		logger.Info("could not determine uncontrolled flag, defaulting to false",
			zap.Error(err), zap.String("name", composeApp.Name))
		uncontrolled = false
	}

	_ = composeApp.SetUncontrolled(uncontrolled)

	if params.CheckPortConflict == nil || *params.CheckPortConflict {
		// validation 1 - check if there are ports in use
		validation, err := composeApp.GetPortsInUse()
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		if validation != nil {
			validationErrors := codegen.ComposeAppValidationErrors{}
			if err := validationErrors.FromComposeAppValidationErrorsPortsInUse(*validation); err != nil {
				message := err.Error()
				return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
					Message: &message,
				})
			}

			message := "there are ports in use"
			return ctx.JSON(http.StatusBadRequest, codegen.ComposeAppBadRequest{
				Message: &message,
				Data:    &validationErrors,
			})
		}
	}

	if params.DryRun != nil && *params.DryRun {
		return ctx.JSON(http.StatusOK, codegen.ComposeAppInstallOK{
			Message: lo.ToPtr("only validation has been done because `dry_run` is specified - skipping compose app installation"),
		})
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := service.MyService.Compose().Install(backgroundCtx, composeApp); err != nil {
		logger.Error("failed to start compose app installation", zap.Error(err))

		message := err.Error()
		if err == service.ErrComposeAppInstalling {
			message = fmt.Sprintf("compose app `%s` is already being installed", composeApp.Name)
			return ctx.JSON(http.StatusConflict, codegen.ComposeAppBadRequest{Message: &message})
		}
		// Install rewrites the app's compose file from scratch, so it refuses
		// to run against a name that is already installed or already owned by
		// another lifecycle operation. Both are conflicts the caller resolves
		// (update/repair the existing app, or retry later) — not server faults.
		if conflict, ok := composeAppConflict(err, composeApp.Name); ok {
			return ctx.JSON(http.StatusConflict, codegen.ComposeAppBadRequest{Message: &conflict})
		}
		if err == service.ErrComposeExtensionNameXCasaOSNotFound {
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"success": 200,
		"message": "compose app is being installed asynchronously",
		"data": map[string]string{
			"id":          composeApp.Name,
			"progressUrl": fmt.Sprintf("/v2/app_management/compose/install/%s/progress", composeApp.Name),
		},
	})
}

func (a *AppManagement) UninstallComposeApp(ctx echo.Context, id codegen.ComposeAppID, params codegen.UninstallComposeAppParams) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	deleteConfigFolder := true
	if params.DeleteConfigFolder != nil {
		deleteConfigFolder = *params.DeleteConfigFolder
	}

	if err := service.MyService.Compose().Uninstall(backgroundCtx, composeApp, deleteConfigFolder); err != nil {
		logger.Error("failed to uninstall compose app", zap.Error(err), zap.String("appID", id))
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppUninstallOK{
		Message: utils.Ptr("compose app is being uninstalled asynchronously"),
	})
}

func (a *AppManagement) UpdateComposeApp(ctx echo.Context, id codegen.ComposeAppID, params codegen.UpdateComposeAppParams) error {
	if err := requirePerm(ctx, rbac.PermAppInstall); err != nil {
		return err
	}
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	if params.Force != nil && !*params.Force {
		// check if updateAvailable
		if !service.MyService.AppStoreManagement().IsUpdateAvailable(composeApp) {
			message := fmt.Sprintf("compose app `%s` is up to date", id)
			return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{Message: &message})
		}
	}

	// composeApp.Update is FIRE-AND-FORGET: it spawns a DETACHED goroutine
	// (docker pull + recreate) and returns immediately. So the ctx must
	// OUTLIVE this handler — the old `defer baseCancel()` cancelled it the
	// instant we replied 200, making every v2 update fail "context canceled".
	// We hand the detached work a self-expiring 30-min timeout ctx and do NOT
	// cancel on return; the 30-min deadline is the only (correct) cap.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	_ = baseCancel // ownership passes to the detached goroutine; timeout is the cap
	backgroundCtx := common.WithProperties(baseCtx, PropertiesFromQueryParams(ctx))

	if err := composeApp.Update(backgroundCtx); err != nil {
		logger.Error("failed to update compose app", zap.Error(err), zap.String("appID", id))
		message := err.Error()
		// A busy app is a conflict, not a fault: the update was rejected
		// because a first click on this same button, the nightly auto-update
		// sweep or a settings save already owns the app.
		if conflict, ok := composeAppConflict(err, id); ok {
			return ctx.JSON(http.StatusConflict, codegen.ComposeAppBadRequest{Message: &conflict})
		}
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	message := fmt.Sprintf("compose app `%s` is being updated asynchronously", id)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{
		Message: &message,
	})
}

func (a *AppManagement) SetComposeAppStatus(ctx echo.Context, id codegen.ComposeAppID) error {
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}

	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	var action codegen.RequestComposeAppStatus
	if err := ctx.Bind(&action); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	// Wave 35-X: 2-min bound — SetStatus does start/stop, fast unless
	// daemon is wedged.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer baseCancel()
	backgroundCtx := common.WithProperties(baseCtx, PropertiesFromQueryParams(ctx))
	if err := composeApp.SetStatus(backgroundCtx, action); err != nil {
		message := err.Error()

		if err == service.ErrInvalidComposeAppStatus {
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.RequestComposeAppStatusOK{
		Message: utils.Ptr("compose app status is being changed asynchronously"),
	})
}

// RepairComposeApp reconciles an app whose container was orphaned from its
// compose project (e.g. by an interrupted update). Such an app is absent from
// Compose().List() — which only knows projects docker already tracks — so the
// status endpoints return "not found", exactly the broken state we repair.
// This loads the app straight from its on-disk compose file (bypassing the
// project list), removes any orphan occupying its container_name slots, and
// recreates it via compose up so it comes back properly labelled and running.
func (a *AppManagement) RepairComposeApp(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermAppStart); err != nil {
		return err
	}

	id := ctx.Param("id")
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	configFile := filepath.Join(config.AppInfo.AppsPath, id, common.ComposeYAMLFileName)
	composeApp, err := service.LoadComposeAppFromConfigFile(id, configFile)
	if err != nil {
		message := fmt.Sprintf("app `%s` not found on disk: %s", id, err.Error())
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	baseCtx, baseCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	backgroundCtx := common.WithProperties(baseCtx, PropertiesFromQueryParams(ctx))
	go func() {
		defer baseCancel()
		if err := composeApp.Repair(backgroundCtx); err != nil {
			logger.Error("failed to repair compose app", zap.Error(err), zap.String("id", id))
		}
	}()

	return ctx.JSON(http.StatusOK, codegen.RequestComposeAppStatusOK{
		Message: utils.Ptr(fmt.Sprintf("repair started for `%s`", id)),
	})
}

func (a *AppManagement) ComposeAppLogs(ctx echo.Context, id codegen.ComposeAppID, params codegen.ComposeAppLogsParams) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	lines := lo.If(params.Lines == nil, 1000).Else(*params.Lines)
	logs, err := composeApp.Logs(ctx.Request().Context(), lines)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppLogsOK{Data: utils.Ptr(string(logs))})
}

func (a *AppManagement) ComposeAppContainers(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	containerLists, err := composeApp.Containers(ctx.Request().Context())
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	storeInfo, err := composeApp.StoreInfo(false)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	// Because of a stupid design by @tigerinus, the `composeApp.Containers(...)` func above was returning a map
	// of docker compose `service` to a single container:
	//
	//     `map[string]codegen.ContainerSummary`
	//
	// However, it is possible a `service` contains multiple containers. Thus as a fix, the func has now been updated
	// to return
	//
	//     `map[string][]codegen.ContainerSummary`
	//
	// Just so that @zhanghengxin does not need to change the frontend last minute before releasing v0.4.4, here is
	// an ugly workaround :(
	containersWorkaround := map[string]codegen.ContainerSummary{}
	for service, containerList := range containerLists {
		if len(containerList) > 0 {
			containersWorkaround[service] = containerList[0]
		}
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppContainersOK{
		Data: &codegen.ComposeAppContainers{
			Main:       storeInfo.Main,
			Containers: &containersWorkaround, // TODO: Remove `containersWorkaround` and use `containerLists` instead in future - requires frontend changes
		},
	})
}

func (a *AppManagement) CheckComposeAppHealthByID(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// One app, one compose file parsed. This used to call List, which
	// enumerates every stack AND parses each one's YAML off disk — 150 ms
	// to serve a single app on a box with 23 installed, indistinguishable
	// from fetching the whole collection.
	composeApp, err := service.MyService.Compose().Get(ctx.Request().Context(), id)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	if composeApp == nil {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	result, err := composeApp.HealthCheck()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{Message: &message})
	}

	if !result {
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{})
	}

	message := fmt.Sprintf("compose app `%s` passed the health check", id)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppHealthCheckOK{
		Message: &message,
	})
}

// maxComposeYAMLBytes caps any inbound compose payload at 4 MiB. The
// largest real-world compose-spec we ship in the appstore is well
// under 100 KiB; even pathological pinned-image lists with hundreds of
// services stay under 1 MiB. Without this cap, io.ReadAll on
// ctx.Request().Body let an unauthenticated multi-GB POST OOM the
// daemon — every other in-flight request fails when the runtime can't
// allocate.
const maxComposeYAMLBytes = 4 << 20

func YAMLfromRequest(ctx echo.Context) ([]byte, error) {
	var buf []byte

	switch ctx.Request().Header.Get(echo.HeaderContentType) {
	case common.MIMEApplicationYAML:

		// LimitReader caps the read at maxComposeYAMLBytes+1 so we can
		// distinguish "exactly at limit" (legal) from "exceeds limit"
		// (reject).
		_buf, err := io.ReadAll(io.LimitReader(ctx.Request().Body, maxComposeYAMLBytes+1))
		if err != nil {
			return nil, err
		}
		if len(_buf) > maxComposeYAMLBytes {
			return nil, fmt.Errorf("compose YAML exceeds %d byte limit", maxComposeYAMLBytes)
		}

		buf = _buf

	default:
		// JSON path. We intentionally do NOT echo.Bind into the strict
		// codegen.ComposeApp type here. compose-go's types.UnitBytes
		// (used by deploy.resources.{limits,reservations}.memory)
		// only JSON-unmarshals from a numeric value, so a perfectly
		// legal compose document like:
		//
		//     services:
		//       foo:
		//         deploy:
		//           resources:
		//             reservations:
		//               memory: "512M"
		//
		// round-trips through the frontend's JSON.parse(JSON.stringify(...))
		// in updateApp() as a string and fails ctx.Bind with:
		//
		//     Unmarshal type error: expected=types.UnitBytes,
		//     got=string, field=services.deploy.resources
		//       .reservations.memory
		//
		// The YAML path above works fine because compose-go's YAML
		// loader parses "512M" natively via its own UnmarshalYAML.
		// So we decode the raw JSON into a schema-free interface{}
		// and hand the resulting tree to yaml.Marshal — the same
		// downstream loader then handles human-readable byte strings,
		// duration strings, short-form port mappings, etc. without
		// us having to replicate compose-go's grammar in Go types.
		//
		// http.MaxBytesReader still caps the body size so this path
		// retains the same DoS protection as the old Bind route.
		ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, maxComposeYAMLBytes)
		raw, err := io.ReadAll(ctx.Request().Body)
		if err != nil {
			return nil, err
		}
		var anyCompose interface{}
		if err := json.Unmarshal(raw, &anyCompose); err != nil {
			return nil, err
		}
		_buf, err := yaml.Marshal(anyCompose)
		if err != nil {
			return nil, err
		}
		buf = _buf
	}

	return buf, nil
}

type composeAppsWithStoreInfoOpts struct {
	checkIsUpdateAvailable bool
	// The /web/appgrid endpoint does not require information about whether the application can be updated, so we added an option.
	// This endpoint is called as soon as CasaOS is opened, and we don't have time to cache it in advance.
	// We must ensure that this endpoint responds as quickly as possible.
}

// containersByComposeProject runs ONE container listing and buckets the result
// by compose project -> compose service. The listing used to call
// composeApp.Containers() once per app, and that helper builds a brand-new
// Docker CLI and issues its own ContainerList (plus a ContainerInspect per
// container) every time: with 23 installed apps the dashboard opened 23
// connections and ran 23 listings against dockerd on every refresh, a few
// seconds apart. A single listing feeds every app.
//
// Filter and bucketing mirror compose's own Ps(project, PsOptions{All: true})
// so each app sees exactly the containers it saw before: only containers
// carrying a config-hash label belong to a compose service (that filter is what
// keeps hand-run containers wearing a project label out), one-off containers are
// included because All is set, and the project/service labels are the same keys
// Ps groups on. Ps lowercases the project name before matching the project
// label, so the lookup below does too.
func containersByComposeProject(ctx context.Context) (map[string]map[string][]container.Summary, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		return nil, err
	}

	f := filters.NewArgs()
	f.Add("label", api.ConfigHashLabel)

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}

	byProject := map[string]map[string][]container.Summary{}
	for _, c := range containers {
		project := c.Labels[api.ProjectLabel]

		byService, ok := byProject[project]
		if !ok {
			byService = map[string][]container.Summary{}
			byProject[project] = byService
		}

		serviceName := c.Labels[api.ServiceLabel]
		byService[serviceName] = append(byService[serviceName], c)
	}

	return byProject, nil
}

// Last grouping dockerd actually answered with, reused when a later listing
// fails. Sharing one listing across the page also shares its failures: where a
// slow reply used to degrade one app, it now degrades all of them in the same
// refresh. Serving the previous snapshot rides out a hiccup (a
// `docker compose pull`, a mass start) without blanking the whole dashboard,
// and the TTL stops a genuine outage from being papered over indefinitely.
var (
	lastContainersMutex sync.Mutex
	lastContainers      map[string]map[string][]container.Summary
	lastContainersAt    time.Time
)

const lastContainersTTL = time.Minute

func rememberContainers(byProject map[string]map[string][]container.Summary) {
	lastContainersMutex.Lock()
	defer lastContainersMutex.Unlock()

	lastContainers = byProject
	lastContainersAt = time.Now()
}

// recentContainers returns the last good grouping while it is still fresh
// enough to describe the box, and nil when there is nothing usable to fall
// back to — a cold start, or dockerd unreachable for longer than the TTL. The
// map is never mutated after it is published, so it is shared, not copied.
func recentContainers() map[string]map[string][]container.Summary {
	lastContainersMutex.Lock()
	defer lastContainersMutex.Unlock()

	if lastContainers == nil || time.Since(lastContainersAt) > lastContainersTTL {
		return nil
	}

	return lastContainers
}

func composeAppsWithStoreInfo(ctx context.Context, opts composeAppsWithStoreInfoOpts) (map[string]codegen.ComposeAppWithStoreInfo, error) {
	composeApps, err := service.MyService.Compose().List(ctx)
	if err != nil {
		return nil, err
	}

	// Single container listing for the whole page, budgeted to fit inside the
	// browser's own window: the web UI's axios client gives up at 10s and the
	// app list does not raise it, so anything we spend past that is spent on a
	// response nobody is waiting for any more — the user sees the same error
	// either way, only later and with dockerd still working for it. 8s leaves
	// room for the compose listing above and the store-info pass below within
	// the caller's 15s, and absorbs the up-to-3s Ping docker.SharedClient() may
	// burn on the way in (that Ping runs on its own context, so it eats wall
	// clock here without being cut by this deadline).
	//
	// This is deliberately shorter than the room ContainerList(All) can want on
	// a busy box (a `docker compose pull` or a mass start in flight), because
	// overrunning it is no longer the disaster it was when this budget was 5s
	// and a miss meant every app fell to "unknown": a timed-out listing now
	// falls back to the last good grouping (recentContainers(), fresh within
	// its TTL) and only a cold start with no snapshot at all reaches the
	// no-status path below. Any shorter deadline the caller already set still
	// wins.
	containerCtx, containerCancel := context.WithTimeout(ctx, 8*time.Second)
	containersByProject, listErr := containersByComposeProject(containerCtx)
	containerCancel()
	if listErr != nil {
		logger.Error("failed to list containers", zap.Error(listErr))
		containersByProject = recentContainers()
	} else {
		rememberContainers(containersByProject)
	}

	// Nothing observed, and no recent snapshot to stand in for it. A failure
	// still must not sink the response — every app renders — but the apps must
	// not carry a state we never saw either; see the status handling below.
	containersUnavailable := listErr != nil && containersByProject == nil

	return lo.MapValues(composeApps, func(composeApp *service.ComposeApp, id string) codegen.ComposeAppWithStoreInfo {
		if composeApp == nil {
			return codegen.ComposeAppWithStoreInfo{}
		}

		composeAppWithStoreInfo := codegen.ComposeAppWithStoreInfo{
			Compose:         (*codegen.ComposeApp)(composeApp),
			StoreInfo:       nil,
			Status:          utils.Ptr("unknown"),
			UpdateAvailable: utils.Ptr(false),
			IsUncontrolled:  utils.Ptr(false),
		}

		// Report no status at all rather than "unknown" when the listing gave
		// us nothing. The web UI forwards an unrecognised status verbatim into
		// its state aggregation, and that aggregation falls through to
		// "running" for anything it doesn't know, so a failed listing would
		// paint every installed app green as if it were up — worse than the
		// per-app degradation this replaced. An absent status is omitted from
		// the JSON and lands on the UI's own "stopped" fallback instead. Only
		// the failure path changes: an app the listing legitimately found no
		// containers for still reports "unknown", exactly as before.
		if containersUnavailable {
			composeAppWithStoreInfo.Status = nil
		}

		storeInfo, err := composeApp.StoreInfo(true)
		if err != nil {
			logger.Error("failed to get store info", zap.Error(err), zap.String("composeAppID", id))
			return composeAppWithStoreInfo
		}

		composeAppWithStoreInfo.StoreInfo = storeInfo

		if opts.checkIsUpdateAvailable {
			// check if updateAvailable
			updateAvailable := service.MyService.AppStoreManagement().IsUpdateAvailable(composeApp)
			composeAppWithStoreInfo.UpdateAvailable = &updateAvailable
		}

		// status
		if storeInfo.Main == nil {
			logger.Error("failed to get main app", zap.String("composeAppID", id))
			return composeAppWithStoreInfo
		}

		// Index the shared listing instead of querying Docker per app. A
		// project with no containers is simply absent from the bucket map, and
		// indexing a missing project yields a nil map — same miss, same
		// fallback, as when Ps came back empty for this app.
		mainContainers, ok := containersByProject[strings.ToLower(composeApp.Name)][*storeInfo.Main]
		if !ok {
			// A failed listing was already logged once for the whole page;
			// repeating it per app just buries the actual cause under one line
			// per installed app.
			if !containersUnavailable {
				logger.Error("failed to get main app container", zap.String("composeAppID", id))
			}
			return composeAppWithStoreInfo
		}

		if extRaw, ok := composeApp.Extensions[common.ComposeExtensionNameXCasaOS]; ok {
			if extMap, ok := extRaw.(map[string]interface{}); ok {
				if val, ok := extMap[common.ComposeExtensionPropertyNameIsUncontrolled].(bool); ok {
					composeAppWithStoreInfo.IsUncontrolled = &val
				}
			}
		}

		// Because of a stupid design by @tigerinus, the per-service container lookup used to yield a map
		// of docker compose `service` to a single container:
		//
		//     `map[string]codegen.ContainerSummary`
		//
		// However, it is possible a `service` contains multiple containers. Thus as a fix, it has now been updated
		// to yield
		//
		//     `map[string][]codegen.ContainerSummary`
		//
		// Apparently, this impacts the downstream logic, like the embarrassing need to use `mainContainers[0]` below.
		//
		// In order words, the status of the compose app is determined by the status of the first main container. Silly...
		//
		// TODO: This needs a re-design in future.
		if len(mainContainers) > 0 {
			composeAppWithStoreInfo.Status = &mainContainers[0].State
		}

		return composeAppWithStoreInfo
	}), nil
}
