package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/LeewayOpenSoftware/CasaNova-AppManagement/service/v1"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/port"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/random"
	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	composeCmd "github.com/docker/compose/v2/cmd/compose"

	"github.com/docker/compose/v2/cmd/formatter"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/go-resty/resty/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type ComposeApp codegen.ComposeApp

func (a *ComposeApp) StoreInfo(includeApps bool) (*codegen.ComposeAppStoreInfo, error) {
	ex, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		return nil, ErrComposeExtensionNameXCasaOSNotFound
	}

	var storeInfo codegen.ComposeAppStoreInfo
	if err := loader.Transform(ex, &storeInfo); err != nil {
		logger.Error("Transform store info fail", zap.Error(err))
		return nil, err
	}

	// Extract IsUncontrolled flag from extension data (with safe type assertions)
	if extMap, ok := a.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
		if val, ok := extMap[common.ComposeExtensionPropertyNameIsUncontrolled]; ok {
			if isUncontrolled, ok := val.(bool); ok {
				storeInfo.IsUncontrolled = &isUncontrolled
			}
		}
	}

	// locate main app
	if storeInfo.Main == nil || *storeInfo.Main == "" {
		// if main app is not specified, use the first app
		for _, app := range a.Apps() {
			storeInfo.Main = &app.Name
			break
		}
	}

	if storeInfo.Scheme == nil || *storeInfo.Scheme == "" {
		storeInfo.Scheme = lo.ToPtr(codegen.Http)
	}

	if includeApps {
		apps := map[string]codegen.AppStoreInfo{}

		for _, app := range a.Apps() {
			appStoreInfo, err := app.StoreInfo()
			if err != nil {
				if err == ErrComposeExtensionNameXCasaOSNotFound {
					logger.Info("App does not have x-casaos extension - skipping", zap.String("app", app.Name))
					continue
				}

				return nil, err
			}
			apps[app.Name] = appStoreInfo
		}

		storeInfo.Apps = &apps
	}

	return &storeInfo, nil
}

func (a *ComposeApp) AuthorType() codegen.StoreAppAuthorType {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return codegen.Unknown
	}

	if strings.EqualFold(storeInfo.Author, storeInfo.Developer) {
		return codegen.Official
	}
	if strings.EqualFold(storeInfo.Author, common.ComposeAppAuthorCasaOSTeam) {
		return codegen.ByCasaos
	}

	return codegen.Community
}

func (a *ComposeApp) SetStoreAppID(storeAppID string) (string, bool) {
	// set store_app_id (by convention is the same as app name at install time if it does not exist)
	extension, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		logger.Info("compose app does not have x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return "", false
	}

	composeAppStoreInfo, ok := extension.(map[string]interface{})
	if !ok {
		logger.Info("compose app does not have valid x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return "", false
	}

	value, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameStoreAppID]
	if ok {
		currentStoreAppID, ok := value.(string)
		if ok {
			logger.Info("compose app already has store_app_id", zap.String("app", a.Name), zap.String("storeAppID", currentStoreAppID))
			return currentStoreAppID, true
		}
	}

	composeAppStoreInfo[common.ComposeExtensionPropertyNameStoreAppID] = storeAppID
	return storeAppID, true
}

func (a *ComposeApp) SetTitle(title, lang string) {
	if a.Extensions == nil {
		a.Extensions = make(map[string]interface{})
	}

	extension, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		extension = map[string]interface{}{}
		a.Extensions[common.ComposeExtensionNameXCasaOS] = extension
	}

	composeAppStoreInfo, ok := extension.(map[string]interface{})
	if !ok {
		logger.Info("compose app does not have valid x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return
	}

	if _, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle]; !ok {
		composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle] = map[string]string{}
	}

	titleMap, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle].(map[string]string)
	if !ok {
		logger.Info("compose app does not have valid title map in its x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return
	}

	if _, ok := titleMap[lang]; !ok {
		titleMap[lang] = title
	}
}


type operationFinisher interface {
	Complete(message string)
	Fail(err error)
}

// updateOpOutcome finishes an update operation op according to the outcome of PullAndApply.
// If err is nil, it completes op as successful.
// If err is non-nil, it fails op with the error.
func updateOpOutcome(op operationFinisher, err error) {
	if op == nil {
		return
	}
	if err != nil {
		op.Fail(err)
	} else {
		op.Complete("app updated successfully")
	}
}

func (a *ComposeApp) Update(ctx context.Context) (err error) {
	// Advisory fast-fail, NOT the mutual exclusion. The update's real work runs
	// in the detached goroutine at the bottom of this function, so a lock taken
	// here would be released by its own defer the moment Update returns — long
	// before a single byte is pulled. Holding it across the `go` statement is
	// no better: the goroutine would then race this function's release() for
	// the lock PullAndApply takes, and lose against itself. So the guarantee
	// lives in PullAndApply and this check exists purely so the common
	// double-click on "Update" gets an immediate HTTP rejection instead of a
	// rejection buried in a log line minutes later.
	if MyService.AppStoreManagement().IsUpdating(a.Name) || MyService.AppLock().IsLocked(a.Name) {
		return fmt.Errorf("app.update rejected: %w", ErrComposeAppBusy)
	}

	// Operation logging: record update start. Ownership of completing or failing
	// this operation passes to the detached goroutine below so that the oplog
	// reflects the real outcome of PullAndApply rather than claiming success
	// milliseconds after launch before an image is even pulled.
	op := StartOp("app.update", a.Name)
	goroutineStarted := false
	defer func() {
		if !goroutineStarted && op != nil {
			if err != nil {
				op.Fail(err)
			} else {
				op.Fail(fmt.Errorf("update aborted before background execution"))
			}
		}
	}()

	if len(a.ComposeFiles) <= 0 {
		return ErrComposeFileNotFound
	}

	if len(a.ComposeFiles) > 1 {
		logger.Info("warning: multiple compose files found, only the first one will be used", zap.String("compose files", strings.Join(a.ComposeFiles, ",")))
	}

	storeInfo, err := a.StoreInfo(true)
	if err != nil {
		return err
	}

	if storeInfo == nil || storeInfo.StoreAppID == nil || *storeInfo.StoreAppID == "" {
		return ErrStoreInfoNotFound
	}

	storeComposeApp, err := MyService.AppStoreManagement().ComposeApp(*storeInfo.StoreAppID)
	if err != nil {
		return err
	}

	if storeComposeApp == nil {
		return ErrNotFoundInAppStore
	}

	localComposeAppServices := lo.Keys(a.Services)
	storeComposeAppServices := lo.Keys(storeComposeApp.Services)

	localAbsentOfStore, storeAbsentOfLocal := lo.Difference(localComposeAppServices, storeComposeAppServices)
	if len(localAbsentOfStore) > 0 {
		logger.Error("local compose app has container apps that are not present in store compose app, thus update is not possible", zap.Strings("absent", localAbsentOfStore))
		return ErrComposeAppNotMatch
	}

	if len(storeAbsentOfLocal) > 0 {
		logger.Error("store compose app has container apps that are not present in local compose app, thus update is not possible", zap.Strings("absent", storeAbsentOfLocal))
		return ErrComposeAppNotMatch
	}

	for _, service := range storeComposeApp.Services {
		localSvc, ok := a.Services[service.Name]
		if !ok {
			continue
		}
		// For a digest-checked tag ("latest") keep the local image and let
		// PullAndApply's re-pull fetch the newer digest. For a concrete pinned
		// tag, adopt the store's image so a catalog version bump actually lands.
		isDigestTag := false
		for _, tag := range common.NeedCheckDigestTags {
			if strings.HasSuffix(service.Image, tag) {
				isDigestTag = true
				break
			}
		}
		if isDigestTag {
			continue
		}
		// Never downgrade: only adopt the store's pinned tag when it is a
		// genuine version upgrade over what's installed. Guards the Force path
		// too — a stale catalog (installed newer than store) must not roll the
		// app back.
		if !tagVersionNewer(tagOf(localSvc.Image), tagOf(service.Image)) {
			continue
		}
		// CRITICAL: write the new image BACK into the map. Go map values are
		// copies, so the old code's `a.App(name).Image = …` mutated a pointer
		// to a throwaway copy — the change never reached a.Services and
		// yaml.Marshal(a) kept the OLD tag, making every fixed-tag update a
		// silent no-op (bugs.md:95).
		localSvc.Image = service.Image
		a.Services[service.Name] = localSvc
	}

	// the code is need by stable diffusion.
	removeRuntime(a)

	newComposeYAML, err := yaml.Marshal(a)
	if err != nil {
		return err
	}

	// prepare for message bus events
	eventProperties := common.PropertiesFromContext(ctx)
	eventProperties[common.PropertyTypeAppName.Name] = a.Name

	if err := a.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", a.Name))
	}

	goroutineStarted = true
	go func(ctx context.Context, op operationFinisher) {
		// Detach from the HTTP handler's context: it 30-min-timeouts the ctx and
		// `defer baseCancel()`s it, so it was canceled the moment the handler
		// returned — cancelling this detached pull ("context canceled") and
		// rolling the update back. Keep the caller's values (event properties);
		// own a bounded deadline here instead.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		// Detached background work — a panic in PullAndApply (yaml
		// edge case, docker client crash, nil deref in a compose
		// helper) would otherwise tear down the entire daemon and
		// take every other in-flight app op down with it. recover()
		// here keeps the failure scoped to this single update and
		// ensures the oplog record is terminated with an error rather
		// than left in-progress forever.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in async app update",
					zap.Any("panic", r), zap.String("name", a.Name))
				if op != nil {
					op.Fail(fmt.Errorf("panic in async app update: %v", r))
				}
			}
		}()
		go PublishEventWrapper(ctx, common.EventTypeAppUpdateBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppUpdateEnd, nil)

		MyService.AppStoreManagement().StartUpgrade(a.Name)
		defer MyService.AppStoreManagement().FinishUpgrade(a.Name)

		err := a.PullAndApply(ctx, newComposeYAML)
		if err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppUpdateError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to update compose app", zap.Error(err), zap.String("name", a.Name))
		}

		updateOpOutcome(op, err)
	}(ctx, op)

	return nil
}

// TODO rename the function to service and add error return value
func (a *ComposeApp) App(name string) *App {
	if name == "" {
		return nil
	}

	if service, ok := a.Services[name]; ok {
		return (*App)(&service)
	}

	return nil
}

func (a *ComposeApp) Apps() map[string]*App {
	apps := make(map[string]*App)

	for name := range a.Services {
		service := a.Services[name]
		apps[name] = (*App)(&service)
	}

	return apps
}

func (a *ComposeApp) MainService() (*App, error) {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return nil, err
	}

	if storeInfo.Main == nil || *storeInfo.Main == "" {
		return nil, ErrMainServiceNotSpecified
	}

	return a.App(*storeInfo.Main), nil
}

func (a *ComposeApp) MainTag() (string, error) {
	mainService, err := a.MainService()
	if err != nil {
		return "", err
	}
	_, newTag := docker.ExtractImageAndTag(mainService.Image)

	return newTag, nil
}

func (a *ComposeApp) Containers(ctx context.Context) (map[string][]api.ContainerSummary, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	containers, err := service.Ps(ctx, a.Name, api.PsOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	// it is possible a `service` contains multiple containers.
	// See https://docs.docker.com/compose/compose-file/deploy/#replicas
	return lo.GroupBy(containers, func(container api.ContainerSummary) string {
		return container.Service
	}), nil
}

func (a *ComposeApp) Pull(ctx context.Context) error {
	// pull
	serviceNum := len(a.Services)

	idx := 0
	for _, app := range a.Services {
		idx++
		if err := func() error {
			go PublishEventWrapper(ctx, common.EventTypeImagePullBegin, map[string]string{
				common.PropertyTypeImageName.Name: app.Image,
			})

			defer PublishEventWrapper(ctx, common.EventTypeImagePullEnd, map[string]string{
				common.PropertyTypeImageName.Name: app.Image,
			})

			if err := docker.PullImage(ctx, app.Image, func(out io.ReadCloser) {
				pullImageProgress(ctx, out, "INSTALL", serviceNum, idx)
			}); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
					common.PropertyTypeImageName.Name: app.Image,
					common.PropertyTypeMessage.Name:   err.Error(),
				})
				return err
			}

			return nil
		}(); err != nil {
			return err
		}
	}

	return nil
}

func (a *ComposeApp) injectEnvVariableToComposeApp() {
	config.GlobalMu.RLock()
	globalCopy := make(map[string]string, len(config.Global))
	for k, v := range config.Global {
		globalCopy[k] = v
	}
	config.GlobalMu.RUnlock()

	for _, service := range a.Services {
		for k, v := range globalCopy {
			// if there is same name var declared in environment in compose yaml
			// we should not reassign a value to it.
			if service.Environment[k] == nil {
				service.Environment[k] = utils.Ptr(v)
			}
		}
	}
}

func (a *ComposeApp) Up(ctx context.Context, service api.Compose) error {
	a.injectEnvVariableToComposeApp()

	err := service.Up(ctx, (*codegen.ComposeApp)(a), api.UpOptions{
		Start: api.StartOptions{
			OnExit: api.CascadeStop,
			Wait:   true,
		},
	})
	if err == nil {
		return nil
	}

	// compose-go's Up intermittently fails with "no container found for
	// project" on projects loaded from disk (LoadComposeAppFromConfigFile),
	// leaving a mislabelled container stuck in "created" that detaches the app
	// from its compose project. This is the root cause of apps silently going
	// offline / "not found" after auto-update (PullAndApply → Up). Recover
	// with the docker compose CLI, which recreates the same file reliably —
	// clearing any stale by-name orphan first so it can't hit a name conflict.
	// Named volumes persist, so app data survives. See [[app-orphan-repair]].
	logger.Info("compose-go Up failed; recovering via docker compose CLI",
		zap.Error(err), zap.String("name", a.Name))
	if len(a.ComposeFiles) == 0 {
		return err
	}
	a.repairOrphanedContainers(ctx)
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", a.ComposeFiles[0], "up", "-d")
	if out, cliErr := cmd.CombinedOutput(); cliErr != nil {
		logger.Error("docker compose CLI recovery also failed",
			zap.Error(cliErr), zap.String("output", strings.TrimSpace(string(out))), zap.String("name", a.Name))
		return err // surface the original compose-go error
	}
	logger.Info("docker compose CLI recovery succeeded", zap.String("name", a.Name))
	return nil
}

func (a *ComposeApp) UpWithCheckRequire(ctx context.Context, service api.Compose) error {
	// prepare source path for volumes if not exist
	for i, app := range a.Services {
		for _, volume := range app.Volumes {
			if _, ok := a.Volumes[volume.Source]; ok {
				// this is a internal volume, so skip.
				continue
			}

			path := volume.Source
			if err := file.IsNotExistMkDir(path); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})
				return err
			}
		}

		// check if each required device exists
		deviceMapFiltered := []types.DeviceMapping{}
		for _, deviceMap := range app.Devices {
			if file.CheckNotExist(deviceMap.Source) {
				logger.Info("device not found", zap.String("device", deviceMap.Source))
				continue
			}
			deviceMapFiltered = append(deviceMapFiltered, deviceMap)
		}
		svc := a.Services[i]
		svc.Devices = deviceMapFiltered
		a.Services[i] = svc
	}

	if err := a.Up(ctx, service); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
		return err
	}
	return nil
}

// ErrComposeAppBusy is returned when another lifecycle operation (install,
// uninstall, manual update, settings apply, auto-update sweep) already holds
// this app's lock. It is a rejection, not a failure: the app is untouched.
var ErrComposeAppBusy = fmt.Errorf("compose app is busy with another operation")

// snapshotComposeFile copies composeFile to a fresh, per-operation backup path
// and returns it.
//
// The backup used to live at the fixed path "<compose>.bak", shared by every
// writer of the app. Two overlapping operations therefore poisoned each other's
// rollback: B snapshotted A's half-applied compose as "the original", and a
// later failure restored a configuration that had never actually run. Even with
// the per-app lock below making that overlap impossible, a fixed name is still
// wrong — a backup orphaned by a daemon kill would be silently mistaken for the
// current operation's snapshot. os.CreateTemp reserves a name that no other
// operation (or leftover) can be holding.
//
// The bytes are written THROUGH the handle CreateTemp returned, never through
// the path: file.CopySingleFile would have unlinked the placeholder and
// recreated it, so between those two syscalls the reserved name was free again
// and a concurrent CreateTemp in the same directory could hand it out twice.
// Copying into the open handle keeps the reservation true for the whole call,
// which is what the paragraph above promises.
func snapshotComposeFile(composeFile string) (string, error) {
	src, err := os.Open(composeFile)
	if err != nil {
		return "", err
	}
	defer src.Close()

	// The timestamp makes the leftovers sortable by name. Every interrupted
	// update leaves one of these behind and nothing sweeps them, so after
	// months of self-updates and killed pulls an operator can face a dozen in
	// one app directory; with a bare random suffix there is no way to tell
	// which one is the configuration that last worked without stat'ing each.
	placeholder, err := os.CreateTemp(
		filepath.Dir(composeFile),
		filepath.Base(composeFile)+".bak-"+time.Now().UTC().Format("20060102T150405Z")+"-*",
	)
	if err != nil {
		return "", err
	}
	backupComposeFile := placeholder.Name()

	if _, err := io.Copy(placeholder, src); err != nil {
		placeholder.Close()
		os.Remove(backupComposeFile)
		return "", err
	}

	// fsync the snapshot before anyone starts overwriting the original. Its
	// whole purpose is to outlive the crash that interrupted the update, and
	// an unflushed backup is no backup at all.
	if err := placeholder.Sync(); err != nil {
		placeholder.Close()
		os.Remove(backupComposeFile)
		return "", err
	}

	if err := placeholder.Close(); err != nil {
		os.Remove(backupComposeFile)
		return "", err
	}

	return backupComposeFile, nil
}

// restoreComposeFile puts a rollback snapshot back over the app's compose file
// WITHOUT ever unlinking the destination.
//
// The obvious file.CopySingleFile does os.Remove(dst) first and only then
// stages the copy, so the recovery path itself opened a window — power loss, a
// self-update stopping the daemon, ENOSPC — in which the app was left with no
// compose file at all and only a randomly named snapshot beside it. That is
// strictly worse than the failed update being recovered from. WriteFileAtomic
// gives tmp + fsync + rename + parent-dir fsync, so the destination is always
// either the failed configuration or the restored one, never absent. Compose
// files are a few KB, so buffering them in memory costs nothing.
func restoreComposeFile(backupComposeFile, composeFile string) error {
	data, err := os.ReadFile(backupComposeFile)
	if err != nil {
		return err
	}

	// A truncated snapshot would "restore" the app into a state that cannot be
	// loaded at all. Keeping the failed compose is better: it at least still
	// describes the containers that are currently running.
	if len(data) == 0 {
		return fmt.Errorf("refusing to restore empty snapshot %s over %s", backupComposeFile, composeFile)
	}

	return file.WriteFileAtomic(composeFile, data, 0o600)
}

// writeComposeFile commits the new configuration to the app's compose file. It
// is a variable so tests can suspend the operation at exactly the instant the
// write lands and check, from another goroutine, that the app lock is STILL
// held for the rest of the run. Asserting that Acquire was called proves
// nothing: the defect this seam exists to catch is a lock that is taken and
// released in the prologue, which no "second writer bounces" test can tell
// apart from a lock held for the full 30 minutes.
var writeComposeFile = func(composeYAML []byte, composeFile string) error {
	return file.WriteToFullPath(composeYAML, composeFile, 0o600)
}

func (a *ComposeApp) PullAndApply(ctx context.Context, newComposeYAML []byte) error {
	if len(a.ComposeFiles) <= 0 {
		// Update and Apply check this before marshalling, but the auto-update
		// sweep calls straight in; indexing [0] there would panic the sweep
		// goroutine on a project whose compose file went missing.
		return ErrComposeFileNotFound
	}

	// THE chokepoint. All three writers of this app's compose file — Update's
	// detached goroutine, Apply's detached goroutine, and the auto-update sweep
	// — funnel through here, and each of them rewrites compose.yml plus a
	// rollback snapshot. The lock previously lived in Update's *synchronous*
	// prologue, so its defer released it the instant Update returned, i.e.
	// before the detached work started; Apply and auto-update took no lock at
	// all. Holding it here covers the entire critical section (snapshot, write,
	// pull, up, rollback) for every caller. Acquire is TryLock-based, so a
	// second writer is rejected rather than queued behind a 30-minute pull —
	// the right answer for an operation the user can simply retry.
	release, lockErr := MyService.AppLock().Acquire(a.Name)
	if lockErr != nil {
		return fmt.Errorf("%w: %s", ErrComposeAppBusy, lockErr.Error())
	}
	defer release()

	// Open the docker client BEFORE taking the snapshot: it is the one step
	// that can still fail without having touched anything, and hoisting it here
	// means no early return can ever sit between "snapshot taken" and "cleanup
	// registered". The old order returned on an apiService() failure before any
	// cleanup existed, orphaning a backup on every such attempt.
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// backup current compose file
	currentComposeFile := a.ComposeFiles[0]

	backupComposeFile, err := snapshotComposeFile(currentComposeFile)
	if err != nil {
		return err
	}

	// composeWritten flips once the new YAML has actually landed on disk.
	// Before that the on-disk file is still the original, so "rolling back"
	// would be a pointless copy and the snapshot is pure garbage.
	composeWritten := false
	success := false

	defer func() {
		if success || !composeWritten {
			os.Remove(backupComposeFile)
			return
		}

		// Say where the known-good configuration is BEFORE touching anything.
		// The snapshot's name is random by design, so this line is the only
		// pointer an operator has to it — and if the process dies mid-rollback
		// (self-update stopping the daemon, power loss) there will be no later
		// line to print it.
		logger.Error("compose update failed - rolling back from snapshot",
			zap.String("name", a.Name), zap.String("backup", backupComposeFile), zap.String("compose", currentComposeFile))

		if err := restoreComposeFile(backupComposeFile, currentComposeFile); err != nil {
			// The snapshot is now the only copy of the configuration that was
			// known to work — keep it and say where it is, so an operator can
			// restore by hand. Deleting it here would be the data loss.
			logger.Error("failed to restore original compose file - KEEPING backup for manual recovery", zap.Error(err), zap.String("src", backupComposeFile), zap.String("dst", currentComposeFile))
			return
		}

		if err := a.Up(ctx, service); err != nil {
			logger.Error("failed to start original compose app after rollback - keeping backup", zap.Error(err), zap.String("name", a.Name), zap.String("backup", backupComposeFile))
			return
		}

		os.Remove(backupComposeFile)
	}()

	// save new compose file
	if err := writeComposeFile(newComposeYAML, currentComposeFile); err != nil {
		return err
	}
	composeWritten = true

	newComposeApp, err := LoadComposeAppFromConfigFile(a.Name, currentComposeFile)
	if err != nil {
		return err
	}

	if err := newComposeApp.Pull(ctx); err != nil {
		return err
	}

	go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, nil)

	err = newComposeApp.UpWithCheckRequire(ctx, service)
	if err == nil {
		success = true
	}

	return err
}

func (a *ComposeApp) Create(ctx context.Context, options api.CreateOptions, service api.Compose) error {
	a.injectEnvVariableToComposeApp()
	return service.Create(ctx, (*codegen.ComposeApp)(a), api.CreateOptions{})
}

// pullAndInstall performs the install: pull, create, start, verify.
//
// The caller MUST already hold this app's lock, and must have held it since
// before it touched AppsPath/<name>. The lock used to be taken here instead,
// i.e. AFTER ComposeService.Install had already created that directory and
// overwritten its docker-compose.yml — so an install aimed at the name of an
// app another operation was updating destroyed that app's configuration before
// finding out it was not allowed to run. The precondition is checked rather
// than documented because getting it wrong is silent and expensive.
func (a *ComposeApp) pullAndInstall(ctx context.Context) (err error) {
	if !MyService.AppLock().IsLocked(a.Name) {
		return fmt.Errorf("refusing to install %s: caller does not hold the app lock", a.Name)
	}

	// Operation logging: record install start, update on completion
	op := StartOp("app.install", a.Name)
	defer func() {
		if op != nil {
			if err != nil {
				op.Fail(err)
			} else {
				op.Complete("app installed successfully")
			}
		}
	}()

	// Wave 3d: reserve every published port BEFORE any Docker work begins.
	// Detecting conflicts at validation time isn't enough — two concurrent
	// installs can both pass the validation window and race to Docker. The
	// allocator gives us an atomic take-or-fail so exactly one install wins
	// per port. On install failure below, the defer releases every reservation.
	portsToReserve := make([]composeServicePorts, 0, len(a.Services))
	for _, svc := range a.Services {
		pub := make([]string, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			if p.Published != "" {
				pub = append(pub, p.Published)
			}
		}
		if len(pub) > 0 {
			portsToReserve = append(portsToReserve, composeServicePorts{Published: pub})
		}
	}
	if err := ReserveComposePorts(a.Name, portsToReserve); err != nil {
		return fmt.Errorf("port allocation: %w", err)
	}
	defer func() {
		// Release on failure so the next install attempt isn't stuck holding
		// a phantom reservation from a dead install.
		if err != nil {
			ReleaseComposePorts(a.Name)
		}
	}()

	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// Clean up orphaned containers/networks if install fails after Pull
	defer func() {
		if err != nil {
			logger.Info("install failed, cleaning up orphaned containers", zap.String("name", a.Name))
			downCtx, downCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer downCancel()
			if downErr := service.Down(downCtx, a.Name, api.DownOptions{RemoveOrphans: true}); downErr != nil {
				logger.Error("failed to clean up after install failure", zap.Error(downErr), zap.String("name", a.Name))
			}
		}
	}()

	// pull
	if MyService != nil {
		MyService.Compose().UpdateInstallProgress(a.Name, "pulling", 25, "Pulling images")
	}
	if err := a.Pull(ctx); err != nil {
		return err
	}

	// create
	if MyService != nil {
		MyService.Compose().UpdateInstallProgress(a.Name, "creating", 60, "Creating containers")
	}
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerCreateBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeContainerCreateEnd, nil)

		for i, app := range a.Services {
			// prepare source path for volumes if not exist
			for _, volume := range app.Volumes {
				if _, ok := a.Volumes[volume.Source]; ok {
					// this is a internal volume, so skip.
					continue
				}

				path := volume.Source
				if err := file.IsNotExistMkDir(path); err != nil {
					go PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
						common.PropertyTypeMessage.Name: err.Error(),
					})
					return err
				}
			}

			// check if each required device exists
			deviceMapFiltered := []types.DeviceMapping{}
			for _, deviceMap := range app.Devices {
				if file.CheckNotExist(deviceMap.Source) {
					logger.Info("device not found", zap.String("device", deviceMap.Source))
					continue
				}
				deviceMapFiltered = append(deviceMapFiltered, deviceMap)
			}
			svc := a.Services[i]
			svc.Devices = deviceMapFiltered
			a.Services[i] = svc
		}

		if err := a.Create(ctx, api.CreateOptions{}, service); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, nil)

	if MyService != nil {
		MyService.Compose().UpdateInstallProgress(a.Name, "starting", 85, "Starting containers")
	}

	startCtx, startCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer startCancel()

	if err := service.Start(startCtx, a.Name, api.StartOptions{
		OnExit: api.CascadeStop,
		Wait:   true,
	}); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
		return err
	}

	// Verify all expected containers are actually running after Start returns.
	containerSummaries, err := service.Ps(ctx, a.Name, api.PsOptions{All: true})
	if err != nil {
		return fmt.Errorf("post-start verification failed: unable to list containers: %w", err)
	}

	expectedServices := len(a.Services)
	runningCount := 0
	var notRunning []string
	for _, cs := range containerSummaries {
		if cs.State == "running" {
			runningCount++
		} else {
			notRunning = append(notRunning, fmt.Sprintf("%s(%s)", cs.Name, cs.State))
		}
	}

	if runningCount < expectedServices {
		return fmt.Errorf("partial compose-up: expected %d running services but only %d are running; not running: %v",
			expectedServices, runningCount, notRunning)
	}

	return nil
}

func (a *ComposeApp) Uninstall(ctx context.Context, deleteConfigFolder bool) (err error) {
	// Wave 2c: Acquire per-app lock
	release, lockErr := MyService.AppLock().Acquire(a.Name)
	if lockErr != nil {
		// Sentinel-wrapped for the same reason as the install path: a caller
		// that cannot recognise a rejection treats "app busy" as "uninstall
		// failed" and may go on to clean up after an operation that never ran.
		return fmt.Errorf("%w: app.uninstall rejected: %s", ErrComposeAppBusy, lockErr.Error())
	}
	defer release()

	// Operation logging
	op := StartOp("app.uninstall", a.Name)
	defer func() {
		if op != nil {
			if err != nil {
				op.Fail(err)
			} else {
				op.Complete("app uninstalled successfully")
			}
		}
	}()

	// Wave 3d: release every port held by this app so the next install can
	// reuse them. ReleaseApp is idempotent — safe even if this app never
	// reserved any ports (pre-allocator installs).
	defer ReleaseComposePorts(a.Name)

	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// stop
	func() {
		go PublishEventWrapper(ctx, common.EventTypeContainerStopBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeContainerStopEnd, nil)

		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer stopCancel()

		if err := service.Stop(stopCtx, a.Name, api.StopOptions{}); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerStopError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to stop compose app during uninstall, continuing to Down", zap.Error(err), zap.String("name", a.Name))
		}
	}()

	// remove
	go PublishEventWrapper(ctx, common.EventTypeContainerRemoveBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerRemoveEnd, nil)

	downCtx, downCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer downCancel()

	if err := service.Down(downCtx, a.Name, api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       true,
	}); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})

		return err
	}

	if err := file.RMDir(a.WorkingDir); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
		return err
	}

	if !deleteConfigFolder {
		return nil
	}

	appsPath := config.AppInfo.AppsPath
	for _, app := range a.Services {
		for _, volume := range app.Volumes {
			if strings.Contains(volume.Source, a.Name) {
				path := filepath.Clean(filepath.Join(strings.Split(volume.Source, a.Name)[0], a.Name))

				// Validate the resolved path is within the expected apps directory
				// to prevent path traversal attacks.
				if !strings.HasPrefix(path, filepath.Clean(appsPath)+string(filepath.Separator)) {
					logger.Error("refusing to delete volume path outside apps directory",
						zap.String("path", path),
						zap.String("appsPath", appsPath),
					)
					continue
				}

				if err := file.RMDir(path); err != nil {
					logger.Error("failed to remove compose app config folder", zap.Error(err), zap.String("path", path))

					go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
						common.PropertyTypeMessage.Name: err.Error(),
					})
				}
			}
		}
	}

	return nil
}

func (a *ComposeApp) Apply(ctx context.Context, newComposeYAML []byte) error {
	// compare new ComposeApp with current ComposeApp
	if getNameFrom(newComposeYAML) != a.Name {
		return ErrComposeAppNotMatch
	}

	// Advisory fast-fail, same reasoning as Update: Apply's work is detached
	// too, so without this a settings-save aimed at an app that is mid-install
	// or mid-update answers 200 and then dies as "busy" inside PullAndApply,
	// where the user never sees it. The authoritative gate is that lock.
	if MyService.AppLock().IsLocked(a.Name) || MyService.AppStoreManagement().IsUpdating(a.Name) {
		return fmt.Errorf("app.apply rejected: %w", ErrComposeAppBusy)
	}

	// Apply is the existing-app edit/update path (settings save + update).
	// Validate the new compose against THIS installed app so dangerous-but-
	// pre-existing settings (e.g. network_mode: host) are preserved while
	// newly-introduced dangerous settings are still blocked.
	newComposeApp, err := NewComposeAppFromYAMLForUpdate(newComposeYAML, a)
	if err != nil {
		return err
	}

	if len(a.ComposeFiles) <= 0 {
		return ErrComposeFileNotFound
	}

	if len(a.ComposeFiles) > 1 {
		logger.Info("warning: multiple compose files found, only the first one will be used", zap.String("compose files", strings.Join(a.ComposeFiles, ",")))
	}

	// prepare for message bus events
	eventProperties := common.PropertiesFromContext(ctx)
	eventProperties[common.PropertyTypeAppName.Name] = a.Name

	// prepare for message bus events
	if err := newComposeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", a.Name))
	}

	go func(ctx context.Context) {
		// This work OUTLIVES the HTTP handler that started it. The handler
		// creates a 30-min timeout context and `defer baseCancel()`s it, so it
		// was canceled the instant the handler returned — the async image pull
		// then died with "context canceled" and PullAndApply rolled the compose
		// back, making every tag edit / async apply silently revert. Detach
		// from the caller's cancellation (keep its values for events) and own a
		// fresh bounded deadline here.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		// Same protection as the Update goroutine above — a panic
		// inside PullAndApply (compose validation edge cases, yaml
		// driver, docker client) must not crash the daemon.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in async app apply",
					zap.Any("panic", r), zap.String("name", a.Name))
			}
		}()
		go PublishEventWrapper(ctx, common.EventTypeAppApplyChangesBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppApplyChangesEnd, nil)

		if err := a.PullAndApply(ctx, newComposeYAML); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppApplyChangesError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to apply changes to compose app", zap.Error(err), zap.String("name", a.Name))
		}
	}(ctx)

	return nil
}

// repairOrphanedContainers removes any container occupying one of this app's
// declared container_name slots that is NOT owned by this compose project
// (missing or foreign com.docker.compose.project label). Such orphans — left
// by an interrupted update, a compose-version migration, or a manual docker
// run — block both a compose start (which can't see them) and a recreate
// (docker rejects the duplicate name), so the panel shows the app "installed
// but offline / not found". Removing them lets Up() recreate a clean,
// properly-labelled container. Named volumes persist, so app data survives.
// Returns the number of orphans removed.
func (a *ComposeApp) repairOrphanedContainers(ctx context.Context) int {
	removed := 0
	for _, svc := range a.Services {
		name := svc.ContainerName
		if name == "" {
			continue // no fixed name → compose default naming is already project-scoped
		}
		info, err := docker.Container(ctx, name)
		if err != nil || info == nil {
			continue // not present — nothing to repair for this slot
		}
		project := ""
		if info.Config != nil {
			project = info.Config.Labels["com.docker.compose.project"]
		}
		if project == a.Name {
			continue // already ours — leave it (defensive; Ps should have seen it)
		}
		logger.Info("repair: removing orphaned container blocking app slot",
			zap.String("app", a.Name), zap.String("container", name), zap.String("foreign_project", project))
		if err := docker.RemoveContainer(ctx, name); err != nil {
			logger.Error("repair: failed to remove orphaned container", zap.Error(err), zap.String("container", name))
			continue
		}
		removed++
	}
	return removed
}

// Repair reconciles an app whose container was orphaned from its compose
// project: it removes the stale by-name orphan(s) and recreates the app via
// compose up so it comes back properly labelled and running. Meant to be
// called with an app loaded straight from its on-disk compose file, so it
// works even when the app is absent from the compose project list (the exact
// "installed but offline / not found" state a broken update leaves behind).
func (a *ComposeApp) Repair(ctx context.Context) error {
	removed := a.repairOrphanedContainers(ctx)
	if len(a.ComposeFiles) == 0 {
		return fmt.Errorf("repair: no compose file for app %s", a.Name)
	}
	composeFile := a.ComposeFiles[0]
	logger.Info("repair: orphans removed; recreating via docker compose up",
		zap.String("name", a.Name), zap.Int("orphans_removed", removed), zap.String("compose_file", composeFile))
	// compose-go's Up/Create (used by the normal install flow) fails with
	// "no container found for project" when run against a project loaded
	// straight from disk. The docker compose CLI recreates reliably from the
	// same file — its project name is derived from the file's dir / name:,
	// matching what List expects — so for this infrequent repair we shell out
	// to the boring, working command. Named volumes persist → app data lives.
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "up", "-d")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("repair: docker compose up failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	logger.Info("repair: docker compose up complete", zap.String("name", a.Name))
	return nil
}

// hasOrphanedContainer reports whether any of the app's declared containers is
// in a broken state: stuck in "created", or detached from its compose project
// (empty project label). These are the unambiguous signals of an app left
// offline / "not found" by an interrupted or failed recreate. A legitimately
// stopped app (state "exited", label intact) is NOT flagged.
func (a *ComposeApp) hasOrphanedContainer(ctx context.Context) bool {
	for _, svc := range a.Services {
		name := svc.ContainerName
		if name == "" {
			continue
		}
		info, err := docker.Container(ctx, name)
		if err != nil || info == nil {
			continue
		}
		project := ""
		if info.Config != nil {
			project = info.Config.Labels["com.docker.compose.project"]
		}
		state := ""
		if info.State != nil {
			state = info.State.Status
		}
		if state == "created" || project == "" {
			return true
		}
	}
	return false
}

// RepairOrphanedApps scans every on-disk app and repairs any whose container
// was orphaned from its compose project. Meant to run once at startup as a
// safety net: an interrupted update or a failed auto-update recreate can
// orphan apps in a batch (offline / "not found"), and this heals them without
// the user clicking Repair on each. Best-effort — logs and moves on.
func RepairOrphanedApps(ctx context.Context) {
	appsDir := config.AppInfo.AppsPath
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		logger.Error("startup orphan sweep: cannot read apps dir", zap.Error(err), zap.String("dir", appsDir))
		return
	}
	repaired := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		configFile := filepath.Join(appsDir, id, common.ComposeYAMLFileName)
		if !file.Exists(configFile) {
			continue
		}
		app, err := LoadComposeAppFromConfigFile(id, configFile)
		if err != nil {
			continue
		}
		if !app.hasOrphanedContainer(ctx) {
			continue
		}
		logger.Info("startup orphan sweep: repairing orphaned app", zap.String("app", id))
		if err := app.Repair(ctx); err != nil {
			logger.Error("startup orphan sweep: repair failed", zap.Error(err), zap.String("app", id))
			continue
		}
		repaired++
	}
	if repaired > 0 {
		logger.Info("startup orphan sweep complete", zap.Int("apps_repaired", repaired))
	}
}

func (a *ComposeApp) SetStatus(ctx context.Context, status codegen.RequestComposeAppStatus) error {
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	// Ownership of dockerClient is per-branch. Stop and Restart run
	// synchronously and are done with the client when this function
	// returns, so they close it here. Start hands `service` (which wraps
	// this very client) to a goroutine that OUTLIVES SetStatus — closing
	// here yanked the docker connection out from under that goroutine
	// while it was still calling Ps/Start on it. In that case the
	// goroutine owns the client and closes it itself.
	closeClientOnReturn := true
	defer func() {
		if closeClientOnReturn {
			dockerClient.Close()
		}
	}()

	eventProperties := common.PropertiesFromContext(ctx)
	eventProperties[common.PropertyTypeAppName.Name] = a.Name

	switch status {
	case codegen.RequestComposeAppStatusStart:
		closeClientOnReturn = false
		go func(ctx context.Context) {
			defer dockerClient.Close()
			// Detach from the HTTP handler's context, exactly as Update()
			// and Apply() already do. The handler builds a 2-min ctx and
			// `defer baseCancel()`s it, so this ctx was canceled the moment
			// the handler returned 200 — every Ps() below then failed
			// instantly with "context canceled", burning the whole 20s
			// stop-wait loop on 10 useless retries before a Start that was
			// doomed too. Keep the caller's values (event properties for the
			// message bus); own a bounded deadline here instead. 10 min
			// covers the 20s settle loop + 120s start, and the self-heal
			// branch below whose Up() may have to pull images.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			defer cancel()
			// Same protection as the Update/Apply goroutines: this work is
			// detached, so a panic in Ps/Up/Start would tear down the whole
			// daemon long after the handler reported success.
			defer func() {
				if r := recover(); r != nil {
					logger.Error("panic in async app start",
						zap.Any("panic", r), zap.String("name", a.Name))
				}
			}()
			go PublishEventWrapper(ctx, common.EventTypeAppStartBegin, nil)

			defer PublishEventWrapper(ctx, common.EventTypeAppStartEnd, nil)

			// Self-heal: service.Ps filters by the com.docker.compose.project
			// label, so an empty result means the project owns NO container —
			// the app is either uncreated or its container was orphaned from
			// the project (e.g. an interrupted update left a bare, unlabelled
			// container stuck in "created"). A plain start can't see such a
			// container and a recreate would hit a name conflict, so remove
			// the orphan(s) occupying our name slots and recreate via Up. This
			// turns the existing Start action into a repair for "installed but
			// offline / not found" apps.
			if summaries, psErr := service.Ps(ctx, a.Name, api.PsOptions{All: true}); psErr == nil && len(summaries) == 0 {
				removed := a.repairOrphanedContainers(ctx)
				logger.Info("start: project owns no containers; repairing+recreating",
					zap.String("name", a.Name), zap.Int("orphans_removed", removed))
				if err := a.UpWithCheckRequire(ctx, service); err != nil {
					go PublishEventWrapper(ctx, common.EventTypeAppStartError, map[string]string{
						common.PropertyTypeMessage.Name: err.Error(),
					})
					logger.Error("failed to repair+start compose app", zap.Error(err), zap.String("name", a.Name))
				}
				return
			}

			// to make sure the container is stopped
			// timeout is 20s
			for index := 0; index < 10; index++ {
				containerSummarys, err := service.Ps(ctx, a.Name, api.PsOptions{
					All: true,
				})
				if err != nil {
					logger.Error("failed to get compose app info", zap.Error(err), zap.String("name", a.Name))
					time.Sleep(2 * time.Second)
					continue
				}
				isContainerExited := true
				for _, containerSummary := range containerSummarys {
					// A container is "stopped, ready to start" in any of exited /
					// created / dead. Waiting ONLY for "exited" hangs the full 20s
					// on a container stuck in "created" (e.g. an interrupted update
					// that created but never started it) and then starts anyway.
					st := containerSummary.State
					isContainerExited = isContainerExited &&
						(st == "exited" || st == "created" || st == "dead")
				}
				if isContainerExited {
					break
				}
				time.Sleep(2 * time.Second)
			}

			startCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()

			if err := service.Start(startCtx, a.Name, api.StartOptions{
				OnExit: api.CascadeStop,
				Wait:   true,
			}); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeAppStartError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})

				logger.Error("failed to start compose app", zap.Error(err), zap.String("name", a.Name))
			}
		}(ctx)
	case codegen.RequestComposeAppStatusStop:
		go PublishEventWrapper(ctx, common.EventTypeAppStopBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppStopEnd, nil)

		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer stopCancel()

		if err := service.Stop(stopCtx, a.Name, api.StopOptions{}); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppStopError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to stop compose app", zap.Error(err), zap.String("name", a.Name))
			return err
		}
	case codegen.RequestComposeAppStatusRestart:
		go PublishEventWrapper(ctx, common.EventTypeAppRestartBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppRestartEnd, nil)

		if err := service.Restart(ctx, a.Name, api.RestartOptions{}); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppRestartError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to restart compose app", zap.Error(err), zap.String("name", a.Name))
			return err
		}
	default:
		return ErrInvalidComposeAppStatus
	}

	return nil
}

func (a *ComposeApp) Logs(ctx context.Context, lines int) ([]byte, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	var buf bytes.Buffer

	consumer := formatter.NewLogConsumer(ctx, &buf, &buf, false, true, false)

	if err := service.Logs(ctx, a.Name, consumer, api.LogOptions{
		Project:  (*codegen.ComposeApp)(a),
		Services: lo.Keys(a.Services),
		Follow:   false,
		Tail:     lo.If(lines < 0, "all").Else(strconv.Itoa(lines)),
	}); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (a *ComposeApp) GetPortsInUse() (*codegen.ComposeAppValidationErrorsPortsInUse, error) {
	tcpPorts, udpPorts, err := port.ListPortsInUse()
	if err != nil {
		return nil, err
	}

	allPortsInUse := lo.Union(tcpPorts, udpPorts)

	tcpPortInUse := []string{}
	udpPortInUse := []string{}

	for _, s := range a.Services {
		for _, p := range s.Ports {
			if lo.ContainsBy(allPortsInUse, func(portInUse int) bool { return strconv.Itoa(portInUse) == p.Published }) {
				switch strings.ToLower(p.Protocol) {
				case "tcp":
					tcpPortInUse = append(tcpPortInUse, p.Published)
				case "udp":
					udpPortInUse = append(udpPortInUse, p.Published)
				}
			}
		}
	}

	if len(tcpPortInUse) == 0 && len(udpPortInUse) == 0 {
		return nil, nil
	}

	portsInUse := struct {
		TCP *codegen.PortList "json:\"TCP,omitempty\""
		UDP *codegen.PortList "json:\"UDP,omitempty\""
	}{TCP: &tcpPortInUse, UDP: &udpPortInUse}

	return &codegen.ComposeAppValidationErrorsPortsInUse{PortsInUse: &portsInUse}, nil
}

// Try to update AppIcon and AppTitle in given event properties from store info
func (a *ComposeApp) UpdateEventPropertiesFromStoreInfo(eventProperties map[string]string) error {
	if eventProperties == nil {
		return fmt.Errorf("event properties is nil")
	}

	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return err
	}

	eventProperties[common.PropertyTypeAppIcon.Name] = storeInfo.Icon

	if storeInfo.Title == nil {
		return fmt.Errorf("compose app title not found in store info")
	}

	titles, err := json.Marshal(storeInfo.Title)
	if err != nil {
		return err
	}

	eventProperties[common.PropertyTypeAppTitle.Name] = string(titles)

	return nil
}

func (a *ComposeApp) HealthCheck() (bool, error) {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return false, err
	}

	scheme := "http"
	if storeInfo.Scheme != nil && *storeInfo.Scheme != "" {
		scheme = string(*storeInfo.Scheme)
	}

	// Wave 25: pin the hostname to the documented localhost default unless
	// the store info points at a name that resolves to *this* host (a
	// docker-compose service name on the same daemon, e.g. "db"). Anything
	// that looks like an IP literal, an FQDN we can't resolve to a private
	// service, or one of the cloud metadata addresses is rejected. The
	// previous code happily fetched whatever the YAML said, which let a
	// hostile compose file weaponize the message-bus health probe into an
	// SSRF against link-local AWS metadata or other LAN services.
	hostname := common.Localhost
	if storeInfo.Hostname != nil && *storeInfo.Hostname != "" {
		candidate := strings.TrimSpace(*storeInfo.Hostname)
		if isHealthCheckHostnameSafe(candidate) {
			hostname = candidate
		} else {
			logger.Info("ignoring unsafe healthcheck hostname, falling back to localhost",
				zap.String("name", a.Name),
				zap.String("requested", candidate),
			)
		}
	}

	url := fmt.Sprintf(
		"%s://%s:%s/%s",
		scheme,
		hostname,
		storeInfo.PortMap,
		strings.TrimLeft(storeInfo.Index, "/"),
	)

	logger.Info("checking compose app health at the specified web port...", zap.String("name", a.Name), zap.Any("url", url))

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("Accept", "text/html")
	// Health checks target local containers which may use self-signed certs.
	// Allow TLS 1.2+ but skip verify for localhost-only health probes.
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec — localhost health check only
	response, err := client.R().Get(url)
	if err != nil {
		logger.Error("failed to check container health", zap.Error(err), zap.String("name", a.Name))
		return false, err
	}
	if response.StatusCode() == http.StatusOK || response.StatusCode() == http.StatusUnauthorized {
		return true, nil
	}

	logger.Error("compose app health check failed at the specified web port", zap.Any("name", a.Name), zap.Any("url", url), zap.String("status", fmt.Sprint(response.StatusCode())))
	return false, nil
}

func LoadComposeAppFromConfigFile(appID string, configFile string) (*ComposeApp, error) {
	options := composeCmd.ProjectOptions{
		ProjectDir:  filepath.Dir(configFile),
		ProjectName: appID,
	}

	env := []string{fmt.Sprintf("%s=%s", "AppID", appID)}
	for k, v := range baseInterpolationMap() {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// load project using compose-go v2 cli
	projectOpts, err := cli.NewProjectOptions(
		nil,
		cli.WithWorkingDirectory(options.ProjectDir),
		cli.WithOsEnv,
		cli.WithDotEnv,
		cli.WithEnv(env),
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		cli.WithEnvFiles(options.EnvFiles...),
		cli.WithName(options.ProjectName),
	)
	if err != nil {
		return nil, err
	}

	// Wave 35-X: bounded ctx. Compose loader can read remote include:
	// directives and walk env/config files — a slow CIFS/NFS mount or
	// dead include URL would freeze every status_sync tick (called via
	// List()) for the daemon lifetime.
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer loadCancel()
	project, err := projectOpts.LoadProject(loadCtx)
	if err != nil {
		return nil, err
	}

	return (*ComposeApp)(project), nil
}

var (
	gpuCache     *([]external.NvidiaGPUInfo)
	gpuCacheOnce sync.Once
)

func removeRuntime(a *ComposeApp) {
	if config.RemoveRuntimeIfNoNvidiaGPUFlag {

		gpuCacheOnce.Do(func() {
			value, err := external.NvidiaGPUInfoListWithSMI()
			if err != nil {
				gpuCache = &([]external.NvidiaGPUInfo{})
			} else {
				gpuCache = &value
			}
		})

		if len(*gpuCache) == 0 {
			for i := range a.Services {
				svc := a.Services[i]
				svc.Runtime = ""
				a.Services[i] = svc
			}
		}
	}
}

// NewComposeAppFromYAML parses a compose YAML into a *ComposeApp and
// runs the Wave 25 security gate before returning. This is the path
// every INSTALL/UPDATE call goes through — if the gate rejects the
// compose, the caller gets a nil app and the rejection error.
func NewComposeAppFromYAML(yaml []byte, skipInterpolation, skipValidation bool) (*ComposeApp, error) {
	return newComposeAppFromYAML(yaml, skipInterpolation, skipValidation, false)
}

// NewComposeAppFromYAMLForUpdate is the EDIT/UPDATE-path variant. Unlike the
// strict install/import gate, it validates the new compose AGAINST the
// already-installed app (`existing`): dangerous-but-pre-existing settings
// (e.g. an existing network_mode: host) are preserved so a running app
// survives a tag/image/title edit, while any NEWLY-introduced dangerous
// setting is still rejected. `existing` may be nil, which collapses to the
// strict gate (every dangerous setting is treated as new).
func NewComposeAppFromYAMLForUpdate(yaml []byte, existing *ComposeApp) (*ComposeApp, error) {
	app, err := newComposeAppFromYAML(yaml, true, true, true) // load, skip the strict gate
	if err != nil {
		return nil, err
	}
	if err := ValidateComposeSecurityForUpdate(app, existing); err != nil {
		return nil, err
	}
	// DATA-PRESERVATION INVARIANT — an already-installed app must NEVER lose its
	// volumes (its database) or environment (its config), nor have its image
	// silently untagged, just because the incoming compose omits them. This is
	// the single chokepoint every edit/update flows through (route
	// ApplyComposeAppSettings and service Apply both call it), so guarding here
	// makes the guarantee hold "por el método que sea".
	//
	// Postmortem (2026-07-17): navidrome's compose was written back truncated to
	// a 184-byte skeleton (no volumes, no env, untagged image). On the next
	// container recreate Docker mounted empty ANONYMOUS volumes over /data and
	// /music → the app came up blank and asked for a new admin password, while
	// the real 73 MB DB sat untouched at the original bind path. Preserving
	// installed volumes/env/image here stops any client — old build, mobile,
	// raw API — from ever persisting that skeleton.
	preserveInstalledServiceData(app, existing)
	return app, nil
}

// preserveInstalledServiceData back-fills volumes, environment and a lost image
// tag from the INSTALLED app into the incoming (edited) app for every service
// they share by name. Only an OMITTED field is restored: a client that sends a
// non-empty volumes/env list is honoring an explicit user edit and wins.
//
// ponytail: "preserve when omitted" also preserves when a user genuinely wants
// to remove EVERY volume from a stateful service — the rare, almost-always-a-
// mistake case — in exchange for never silently wiping a database. If a real
// "detach all volumes" edit is ever needed, add an explicit intent flag rather
// than loosening this.
func preserveInstalledServiceData(next, installed *ComposeApp) {
	if next == nil || installed == nil {
		return
	}
	for name, svc := range next.Services {
		old, ok := installed.Services[name]
		if !ok {
			continue // genuinely new service — nothing installed to preserve
		}
		changed := false
		if len(svc.Volumes) == 0 && len(old.Volumes) > 0 { // volumes = data
			svc.Volumes = old.Volumes
			changed = true
		}
		if len(svc.Environment) == 0 && len(old.Environment) > 0 { // env = config
			svc.Environment = old.Environment
			changed = true
		}
		if tagOf(svc.Image) == "" && old.Image != "" { // bare image = tag was lost
			svc.Image = old.Image
			changed = true
		}
		if changed {
			next.Services[name] = svc
		}
	}
}

// NewComposeAppFromYAMLForCatalog is the browsing-path variant. The
// security gate is NOT run — it is an install-time policy, and
// applying it to every app in every upstream app store silently
// drops perfectly legitimate apps from the catalog UI. Examples:
//   - watchtower, portainer, dockge, diun: bind /var/run/docker.sock
//   - jellyfin/plex hardware transcoding: /dev/dri (passes through
//     the /dev/* prefix match for /dev/sd[a-d])
//   - scrutiny: /dev/sd* SMART monitoring
//
// Every one of those is a valid choice a user may want to *see* in
// the store and then decline or accept at install time. When the
// user actually clicks Install, the install path goes through
// NewComposeAppFromYAML (security gate ON) and the gate makes its
// call with full context. Catalog browsing has no such privilege
// so there is nothing for the gate to protect here.
//
// Wave 35-T: before this split, BuildCatalog's catalog load called
// NewComposeAppFromYAML directly and the gate silently dropped ~30%
// of the upstream CasaOS-AppStore catalog, producing an almost-empty
// store in the UI. The remaining apps had no common theme so it
// wasn't obvious at first glance that a security filter was at
// fault.
func NewComposeAppFromYAMLForCatalog(yaml []byte) (*ComposeApp, error) {
	return newComposeAppFromYAML(yaml, true, false, true)
}

func newComposeAppFromYAML(yaml []byte, skipInterpolation, skipValidation, skipSecurity bool) (*ComposeApp, error) {
	tmpWorkingDir, err := os.MkdirTemp("", "casaos-compose-app-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpWorkingDir)

	// the WEBUI_PORT interpolate will tiger twice. In `pulished` and `port-map`.
	// So we need to promise multiple WEBUI_PORT interpolate is a same value.
	port, _ := port.GetAvailablePort("tcp")

	// Wave 35-X: bounded ctx — see LoadProject above.
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer loadCancel()
	project, err := loader.LoadWithContext(loadCtx,
		types.ConfigDetails{
			ConfigFiles: []types.ConfigFile{
				{
					Content: []byte(yaml),
				},
			},
			Environment: map[string]string{},

			// need to set a working dir because loader/normalize.go from github.com/compose-spec/compose-go makes
			// wrong assumption that the working dir is the same as the dir where this program is launched.
			WorkingDir: tmpWorkingDir,
		},
		func(o *loader.Options) {
			o.SkipInterpolation = skipInterpolation
			o.SkipValidation = skipValidation

			o.Interpolate.LookupValue = func(key string) (string, bool) {
				switch key {
				case "WEBUI_PORT":
					fmt.Printf("WEBUI_PORT is not specified, using %d\n", port)
					return strconv.Itoa(port), true
				}

				for k := range baseInterpolationMap() {
					if k == key {
						// example:  TZ => $TZ
						// we didn't want to interpolate base interpolation value.
						// they should be interpolated in LoadComposeAppFromConfig
						return fmt.Sprintf("$%s", k), true
					}
				}
				// the function may can to replace the above code.
				value, ok := os.LookupEnv(key)
				if ok {
					return value, true
				} else {
					return fmt.Sprintf("$%s", key), true
				}
			}

			if getNameFrom(yaml) != "" {
				return
			}

			// fix compose app name
			logger.Info("compose app name is not specified, getting a name from one of our contributors :)")
			projectName := random.Name(nil)
			logger.Info("compose app name is given", zap.String("name", projectName))
			o.SetProjectName(projectName, false)
		},
	)
	if err != nil {
		return nil, err
	}

	composeApp := (*ComposeApp)(project)

	if composeApp.Extensions == nil {
		composeApp.Extensions = map[string]interface{}{}
	}

	storeInfo, err := composeApp.StoreInfo(false)

	if err != nil || storeInfo == nil || storeInfo.Title == nil {
		logger.Info("compose app does not have store info with title set, re-using app name as title", zap.String("app", composeApp.Name))
		composeApp.SetTitle(composeApp.Name, common.DefaultLanguage)
	}

	removeRuntime(composeApp)

	// pass icon information to v1 label for backward compatibility, because we are
	// still using `func getContainerStats()` from `container.go` to get container stats
	// (we are being lazy to upgrade that v1 API to v2 - please help if you can :D)
	if err == nil && storeInfo != nil && storeInfo.Icon != "" {
		for i := range composeApp.Services {
			svc := composeApp.Services[i]
			if svc.Labels == nil {
				svc.Labels = map[string]string{}
			}
			svc.Labels[v1.V1LabelIcon] = storeInfo.Icon
			composeApp.Services[i] = svc
		}
	}

	// Wave 25: run the security policy gate unless the caller is the
	// catalog browser. Every install/update path leaves skipSecurity
	// false so the gate is still the single chokepoint for hostile
	// compose files actually being scheduled onto docker — see the
	// comment on NewComposeAppFromYAMLForCatalog for why browsing is
	// exempt.
	if !skipSecurity {
		if err := ValidateComposeSecurity(composeApp); err != nil {
			return nil, err
		}
	}

	return composeApp, nil
}

func getNameFrom(composeYAML []byte) string {
	var baseStructure struct {
		Name string `yaml:"name"`
	}

	if err := yaml.Unmarshal(composeYAML, &baseStructure); err != nil {
		return ""
	}

	return baseStructure.Name
}

func (a *ComposeApp) SetUncontrolled(uncontrolled bool) error {
	xCasaos := a.Extensions[common.ComposeExtensionNameXCasaOS]
	xCasaosMap, ok := xCasaos.(map[string]interface{})

	// set to controlled app
	if !ok {
		logger.Error("failed to get map compose app extensions", zap.String("composeAppID", a.Name))
		return ErrComposeExtensionNameXCasaOSNotFound
	} else {
		xCasaosMap[common.ComposeExtensionPropertyNameIsUncontrolled] = uncontrolled
		a.Extensions[common.ComposeExtensionNameXCasaOS] = xCasaosMap
	}

	return nil
}
