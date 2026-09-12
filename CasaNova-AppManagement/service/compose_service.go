package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/utils/envHelper"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	timeutils "github.com/LeewayOpenSoftware/CasaNova-Common/utils/time"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"go.uber.org/zap"
)

type ComposeService struct {
	installationInProgress sync.Map
}

type InstallProgress struct {
	Status    string     `json:"status"`
	Progress  int        `json:"progress"`
	Message   string     `json:"message"`
	Error     string     `json:"error,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

var ErrComposeAppInstalling = fmt.Errorf("compose app is already being installed")

// ErrComposeAppAlreadyInstalled is returned when an install targets the name of
// an app Docker already runs. It is a rejection, not a failure: nothing on disk
// was touched, and the existing app is untouched.
var ErrComposeAppAlreadyInstalled = fmt.Errorf("compose app is already installed")

func newInstallProgress(status string, progress int, message string) *InstallProgress {
	return &InstallProgress{
		Status:    status,
		Progress:  progress,
		Message:   message,
		UpdatedAt: time.Now().UTC(),
	}
}

func (p *InstallProgress) terminal() bool {
	return p != nil && (p.Status == "completed" || p.Status == "failed")
}

func (s *ComposeService) PrepareWorkingDirectory(name string) (string, error) {
	workingDirectory := filepath.Join(config.AppInfo.AppsPath, name)

	if err := file.IsNotExistMkDir(workingDirectory); err != nil {
		logger.Error("failed to create working dir", zap.Error(err), zap.String("path", workingDirectory))
		return "", err
	}

	return workingDirectory, nil
}

// listComposeApps is the app inventory Install consults before it decides that
// a name is free. It is a variable so the guard that protects installed apps
// from being overwritten can be tested without a Docker daemon — that guard
// stands between an install and a live app's directory, and shipping it
// untested is exactly how the path it protects got wide in the first place.
var listComposeApps = func(ctx context.Context, s *ComposeService) (map[string]*ComposeApp, error) {
	return s.List(ctx)
}

// isInstalled reports whether Docker already runs a compose project under this
// name. Used by Install to refuse to overwrite a live app's directory.
func (s *ComposeService) isInstalled(ctx context.Context, name string) (bool, error) {
	// Bound the question. List talks to dockerd and Install runs on a detached
	// background context, so a wedged daemon must fail this check rather than
	// pin the HTTP handler that is waiting for Install to return.
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	apps, err := listComposeApps(listCtx, s)
	if err != nil {
		return false, err
	}

	_, ok := apps[name]
	return ok, nil
}

func (s *ComposeService) IsInstalling(appName string) bool {
	value, ok := s.installationInProgress.Load(appName)
	if !ok {
		return false
	}
	progress, ok := value.(*InstallProgress)
	return !ok || !progress.terminal()
}

// TryAcquireInstall atomically checks and marks an app as installing.
// Returns true if the caller acquired the lock (proceed with install),
// false if another install is already in progress.
func (s *ComposeService) TryAcquireInstall(appName string) bool {
	next := newInstallProgress("queued", 5, "Install queued")
	value, loaded := s.installationInProgress.LoadOrStore(appName, next)
	if !loaded {
		return true
	}
	progress, ok := value.(*InstallProgress)
	if ok && progress.terminal() {
		s.installationInProgress.Store(appName, next)
		return true
	}
	return false
}

// ReleaseInstall clears the install slot for an app. Kept for callers that
// need to abort a manually claimed install; Install itself now records failed
// terminal state so SSE clients can see the real result.
func (s *ComposeService) ReleaseInstall(appName string) {
	s.installationInProgress.Delete(appName)
}

func (s *ComposeService) UpdateInstallProgress(appName, status string, progress int, message string) {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	s.installationInProgress.Store(appName, newInstallProgress(status, progress, message))
}

func (s *ComposeService) FailInstall(appName string, err error) {
	message := "Install failed"
	if err != nil {
		message = err.Error()
	}
	progress := newInstallProgress("failed", 100, message)
	progress.Error = message
	now := time.Now().UTC()
	progress.EndedAt = &now
	s.installationInProgress.Store(appName, progress)
	s.expireTerminalInstall(appName, progress)
}

func (s *ComposeService) CompleteInstall(appName string) {
	progress := newInstallProgress("completed", 100, "Installation completed")
	now := time.Now().UTC()
	progress.EndedAt = &now
	s.installationInProgress.Store(appName, progress)
	s.expireTerminalInstall(appName, progress)
}

func (s *ComposeService) InstallProgress(appName string) (InstallProgress, bool) {
	value, ok := s.installationInProgress.Load(appName)
	if !ok {
		return InstallProgress{}, false
	}
	progress, ok := value.(*InstallProgress)
	if !ok || progress == nil {
		return InstallProgress{Status: "installing", Progress: 50, Message: "Installing"}, true
	}
	return *progress, true
}

func (s *ComposeService) expireTerminalInstall(appName string, progress *InstallProgress) {
	time.AfterFunc(30*time.Minute, func() {
		value, ok := s.installationInProgress.Load(appName)
		if ok && value == progress {
			s.installationInProgress.Delete(appName)
		}
	})
}

func (s *ComposeService) Install(ctx context.Context, composeApp *ComposeApp) (err error) {
	if !s.TryAcquireInstall(composeApp.Name) {
		return ErrComposeAppInstalling
	}
	goroutineStarted := false
	defer func() {
		if !goroutineStarted && err != nil && err != ErrComposeAppInstalling {
			s.FailInstall(composeApp.Name, err)
		}
	}()

	// The app lock is taken HERE, synchronously, and not down in the goroutine
	// that pulls. Everything between this line and the goroutine is
	// destructive to whatever already lives at AppsPath/<name>: it mkdirs that
	// exact directory and overwrites its docker-compose.yml. Taking the lock
	// only at the pull meant an install aimed at the name of an app that
	// another operation was already updating had ALREADY clobbered that app's
	// compose file by the time it was told the app was busy — and then the
	// failure path below deleted the live app's working directory.
	//
	// The lock is handed to the install goroutine, which releases it when the
	// install ends; every synchronous exit below releases it instead.
	release, lockErr := MyService.AppLock().Acquire(composeApp.Name)
	if lockErr != nil {
		return fmt.Errorf("%w: %s", ErrComposeAppBusy, lockErr.Error())
	}
	defer func() {
		if !goroutineStarted {
			release()
		}
	}()

	// Refuse to install over an app Docker already knows about. Install is not
	// an update path: it rewrites the compose file from the store's YAML, so a
	// second install of an installed name silently discards the user's ports,
	// env, bind-mount paths and x-casaos metadata while its containers keep
	// running. The app lock does not cover this case — an idle installed app
	// holds no lock. Update/Apply (or Repair, for an orphaned project) are the
	// operations that are allowed to touch an app that already exists.
	installed, err := s.isInstalled(ctx, composeApp.Name)
	if err != nil {
		// Fail closed: without an answer from Docker we cannot tell an unused
		// name from a live app's name, and being wrong destroys user data.
		// Nothing has been written yet, so this costs the caller a retry.
		return fmt.Errorf("cannot verify whether app %s is already installed: %w", composeApp.Name, err)
	}
	if installed {
		return fmt.Errorf("%w: %s", ErrComposeAppAlreadyInstalled, composeApp.Name)
	}

	// set store_app_id (by convention is the same as app name at install time if it does not exist)
	_, isStoreApp := composeApp.SetStoreAppID(composeApp.Name)
	if !isStoreApp {
		logger.Info("the compose app getting installed is not a store app, skipping store app id setting.")
	}

	logger.Info("installing compose app", zap.String("name", composeApp.Name))

	composeYAMLInterpolated, err := yaml.Marshal(composeApp)
	if err != nil {
		return err
	}

	s.UpdateInstallProgress(composeApp.Name, "preparing", 10, "Preparing compose project")

	// Remember whether this directory is ours. Every cleanup path below deletes
	// AppsPath/<name> recursively, and that is only ever safe for a directory
	// this install created: a pre-existing one holds someone else's compose
	// file, config folder and — for apps that bind-mount ./data — user data.
	// Leaving a stale directory behind costs a few KB; deleting the wrong one
	// costs the app.
	createdWorkingDirectory := file.CheckNotExist(filepath.Join(config.AppInfo.AppsPath, composeApp.Name))

	workingDirectory, err := s.PrepareWorkingDirectory(composeApp.Name)
	if err != nil {
		return err
	}

	// cleanupWorkingDirectory removes the working directory only when this
	// install is the one that created it.
	cleanupWorkingDirectory := func(path string) {
		if !createdWorkingDirectory {
			logger.Error("install failed on a pre-existing app directory - KEEPING it, manual inspection required",
				zap.String("path", path), zap.String("name", composeApp.Name))
			return
		}
		cleanup(path)
	}

	yamlFilePath := filepath.Join(workingDirectory, common.ComposeYAMLFileName)

	// Wave 29: durable atomic write. The previous os.WriteFile path
	// truncated the destination first and then streamed bytes — a crash
	// or power loss between truncate and final flush would leave a
	// partial/empty compose file behind, and on next boot the daemon
	// would happily try to load it (or fail to) with no recovery path.
	// Wave 35-R: switched from the local writeFileAtomic helper to
	// commonfile.WriteFileAtomic so we also get parent-dir fsync —
	// without that, the rename can commit in a different transaction
	// from the data on btrfs/xfs/zfs and power loss between the two
	// leaves a renamed-but-empty docker-compose.yml that the next boot
	// tries to load with no recovery path.
	if err := file.WriteFileAtomic(yamlFilePath, composeYAMLInterpolated, 0o600); err != nil {
		logger.Error("failed to save compose file", zap.Error(err), zap.String("path", yamlFilePath))

		cleanupWorkingDirectory(workingDirectory)
		return err
	}

	// load project
	//
	// Wave 33: capture the app name into a local before LoadComposeAppFromConfigFile
	// can return (nil, err). The previous code did
	//   composeApp, err = LoadComposeAppFromConfigFile(composeApp.Name, yamlFilePath)
	//   if err != nil {
	//       logger.Error(..., zap.String("name", composeApp.Name))
	// which on a malformed compose file (broken env_file ref, bad
	// service interpolation, etc.) reassigned composeApp to nil and
	// then immediately dereferenced composeApp.Name in the error log
	// — nil pointer panic. The panic happens SYNCHRONOUSLY in the
	// install handler, before the goroutine spawn, so the goroutine's
	// recover() does not catch it. Echo's request-level recover then
	// caught it, but the install slot would never be released because
	// we never reached a normal error return — meaning the user could
	// not retry that app name without restarting the daemon.
	appName := composeApp.Name
	composeApp, err = LoadComposeAppFromConfigFile(appName, yamlFilePath)
	if err != nil {
		logger.Error("failed to install compose app", zap.Error(err), zap.String("name", appName))
		cleanupWorkingDirectory(workingDirectory)
		return err
	}

	// prepare for message bus events
	// Filled locally and written back as one atomic update, because the
	// context's map is shared with every goroutine publishing on this context.
	// The write-back happens EVEN IF UpdateEventPropertiesFromStoreInfo fails:
	// it sets app:icon before it can fail on a missing title, callers only log
	// and continue, so app:icon reaches the bus today on that path and must
	// keep doing so.
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: composeApp.Name}

	if err := composeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", composeApp.Name))
	}

	common.SetProperties(ctx, eventProperties)

	s.UpdateInstallProgress(composeApp.Name, "starting", 15, "Starting install")

	// Create a background context so the goroutine is not cancelled when the
	// HTTP request context ends.  Preserve event properties from the original
	// request context.
	//
	// Wave 35-X: 30-minute overall deadline. Without it, a stuck
	// docker pull (registry hang, dead mirror, slow link) leaves the
	// install in "pulling" state forever — SSE shows progress
	// indefinitely and FailInstall never fires. 30min covers a multi-GB
	// pull on slow links; anything longer is a real failure.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	bgCtx := common.WithProperties(baseCtx, common.PropertiesSnapshot(ctx))

	goroutineStarted = true
	go func(ctx context.Context) {
		defer baseCancel()
		// Ownership of the app lock passes to this goroutine: it was taken
		// before the working directory was touched and must stay held until
		// the install actually finishes, so no other writer can start on this
		// app while its containers are being created.
		defer release()
		// Wave 29: recover any panic so a crash inside compose-up wiring
		// (libcompose, docker client, file IO) does not take the whole
		// app-management daemon down with it. Without this, every
		// in-flight install/uninstall request from every other client
		// dies along with the panicking goroutine.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in compose install goroutine",
					zap.Any("panic", r),
					zap.String("name", composeApp.Name))
				go PublishEventWrapper(ctx, common.EventTypeAppInstallError, map[string]string{
					common.PropertyTypeMessage.Name: "internal error during install",
				})
			}
		}()
		go PublishEventWrapper(ctx, common.EventTypeAppInstallBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppInstallEnd, nil)

		// This goroutine already owns the app lock (taken above, released by
		// the defer at the top of the goroutine), which is pullAndInstall's
		// documented precondition.
		if err := composeApp.pullAndInstall(ctx); err != nil {
			s.FailInstall(composeApp.Name, err)
			go PublishEventWrapper(ctx, common.EventTypeAppInstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to install compose app", zap.Error(err), zap.String("name", composeApp.Name))

			// A rejection is not a failed install: nothing was created, so
			// there is nothing of ours to remove and the directory may well
			// belong to the operation that did the rejecting. Deleting it here
			// was how a lock collision turned into the loss of a live app's
			// compose file, config folder and rollback snapshot.
			if errors.Is(err, ErrComposeAppBusy) {
				logger.Error("install was rejected because the app is busy - keeping the working directory",
					zap.String("name", composeApp.Name), zap.String("path", composeApp.WorkingDir))
				return
			}

			cleanupWorkingDirectory(composeApp.WorkingDir)
			return
		}
		s.CompleteInstall(composeApp.Name)
	}(bgCtx)

	return nil
}

func (s *ComposeService) Uninstall(ctx context.Context, composeApp *ComposeApp, deleteConfigFolder bool) error {
	// prepare for message bus events
	// Filled locally and written back as one atomic update, because the
	// context's map is shared with every goroutine publishing on this context.
	// The write-back happens EVEN IF UpdateEventPropertiesFromStoreInfo fails:
	// it sets app:icon before it can fail on a missing title, callers only log
	// and continue, so app:icon reaches the bus today on that path and must
	// keep doing so.
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: composeApp.Name}

	if err := composeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", composeApp.Name))
	}

	common.SetProperties(ctx, eventProperties)

	// Create a background context so the goroutine is not cancelled when the
	// HTTP request context ends.  Preserve event properties from the original
	// request context.
	//
	// Wave 35-X: 10-minute deadline — uninstall is bounded by container
	// stop + volume cleanup, fast unless docker daemon is wedged.
	baseCtx, baseCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	bgCtx := common.WithProperties(baseCtx, common.PropertiesSnapshot(ctx))

	go func(ctx context.Context) {
		defer baseCancel()
		// Wave 29: see install goroutine — recover() so a docker client
		// crash inside Uninstall() does not take the whole daemon down.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in compose uninstall goroutine",
					zap.Any("panic", r),
					zap.String("name", composeApp.Name))
				go PublishEventWrapper(ctx, common.EventTypeAppUninstallError, map[string]string{
					common.PropertyTypeMessage.Name: "internal error during uninstall",
				})
			}
		}()

		go PublishEventWrapper(ctx, common.EventTypeAppUninstallBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppUninstallEnd, nil)

		if err := composeApp.Uninstall(ctx, deleteConfigFolder); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppUninstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to uninstall compose app", zap.Error(err), zap.String("name", composeApp.Name))
		}
	}(bgCtx)

	return nil
}

func (s *ComposeService) Status(ctx context.Context, appID string) (string, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return "", err
	}
	defer dockerClient.Close()

	stackList, err := service.List(ctx, api.ListOptions{
		All: true,
	})
	if err != nil {
		return "", err
	}

	for _, stack := range stackList {
		if stack.ID == appID {
			return stack.Status, nil
		}
	}

	return "", ErrComposeAppNotFound
}

func (s *ComposeService) List(ctx context.Context) (map[string]*ComposeApp, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	stackList, err := service.List(ctx, api.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]*ComposeApp{}

	for _, stack := range stackList {
		if composeApp := loadStack(ctx, stack.ID, stack.ConfigFiles); composeApp != nil {
			result[stack.ID] = composeApp
		}
	}

	return result, nil
}

// Get returns ONE compose app without parsing the other twenty-two.
//
// Every per-app handler used to call List and then index the result. List
// enumerates every stack AND parses each one's compose YAML off disk, so
// serving a single app did the whole collection's work: measured on the
// production box with 23 apps installed, GET /v2/app_management/compose/{id}
// took 150 ms against 156 ms for the full list — indistinguishable. Of that,
// `docker compose ls` is about 40 ms and the remaining ~110 ms is parsing 23
// YAML files (113 KB) to throw 22 of them away.
//
// The stack enumeration is still needed — it is how we learn where the app's
// compose file lives — but only the requested project is loaded. Returns
// (nil, nil) when the id is not installed, so callers keep their existing
// "not found" branch instead of inventing an error to compare against.
func (s *ComposeService) Get(ctx context.Context, id string) (*ComposeApp, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	stackList, err := service.List(ctx, api.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	for _, stack := range stackList {
		if stack.ID != id {
			continue
		}
		return loadStack(ctx, stack.ID, stack.ConfigFiles), nil
	}
	return nil, nil
}

// loadStack turns one docker-compose stack into a ComposeApp, applying the
// degraded-entry fallback. It is shared by List and Get on purpose: the
// fallback below is the difference between a user seeing a broken tile they can
// investigate and an app silently vanishing from the dashboard, and two copies
// of that rule would drift.
//
// Returns nil when even the fallback fails; the caller skips the entry.
func loadStack(ctx context.Context, stackID, configFiles string) *ComposeApp {
	{
		composeApp, err := LoadComposeAppFromConfigFile(stackID, configFiles)
		// v0.1: previously this `continue`'d on every load failure so a
		// single broken compose YAML — a missing env var, a deprecated
		// syntax compose-go v2 rejects, an env_file pointing at a
		// removed path — would make the entire app vanish from the
		// dashboard. The container kept running, Portainer kept
		// listing it, but CasaNova could never see it again and the
		// user's only signal was an empty grid + a daemon log line
		// nobody reads.
		//
		// Now: try to fall back to a degraded entry built from the
		// running container's labels so the user still sees a tile
		// (with broken metadata they can investigate) instead of
		// silent disappearance. If even the fallback fails we log
		// loudly with the stack ID — the admin gets a clear signal.
		if err != nil {
			logger.Error("failed to load compose file from disk — attempting label fallback",
				zap.Error(err),
				zap.String("name", stackID),
				zap.String("path", configFiles))
			fallback, fbErr := loadComposeAppFromContainerLabels(ctx, stackID)
			if fbErr != nil {
				logger.Error("compose app is ORPHANED — visible in Docker but not on CasaNova dashboard",
					zap.String("name", stackID),
					zap.String("path", configFiles),
					zap.Error(fbErr))
				return nil
			}
			return fallback
		}
		return composeApp
	}
}

// loadComposeAppFromContainerLabels rebuilds a minimum-viable ComposeApp
// from the running containers carrying the project's
// `com.docker.compose.project=<name>` label. Used as a fallback when
// the on-disk compose file can't be parsed — keeps the user's
// dashboard tile alive even when the project's compose is broken.
//
// The returned app has just enough structure to render: name, a
// service per container, and the image/state. No x-casanova
// extension, so StoreInfo() will return ErrXCasaNovaNotFound and the
// UI shows a generic icon. That's the intended degraded experience.
func loadComposeAppFromContainerLabels(ctx context.Context, projectName string) (*ComposeApp, error) {
	cli, err := docker.SharedClient()
	if err != nil {
		return nil, err
	}
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", "com.docker.compose.project="+projectName)
	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("no containers for project %q", projectName)
	}

	project := &types.Project{
		Name:     projectName,
		Services: types.Services{},
	}
	for _, c := range containers {
		svcName := c.Labels["com.docker.compose.service"]
		if svcName == "" {
			svcName = projectName
		}
		project.Services[svcName] = types.ServiceConfig{
			Name:  svcName,
			Image: c.Image,
		}
	}
	return (*ComposeApp)(project), nil
}

func NewComposeService() *ComposeService {
	return &ComposeService{
		installationInProgress: sync.Map{},
	}
}

func baseInterpolationMap() map[string]string {
	return map[string]string{
		"DefaultUserName": common.DefaultUserName,
		"DefaultPassword": envHelper.InstancePassword(),
		"PUID":            common.DefaultPUID,
		"PGID":            common.DefaultPGID,
		"TZ":              timeutils.GetSystemTimeZoneName(),
	}
}

func apiService() (api.Compose, client.APIClient, error) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		return nil, nil, err
	}

	if err := dockerCli.Initialize(&flags.ClientOptions{}); err != nil {
		// NewDockerCli opens the configured Docker endpoint on
		// Initialize; on failure that client is orphaned. Close it
		// so transient Docker hiccups don't leak a socket handle
		// per request.
		if apiCli := dockerCli.Client(); apiCli != nil {
			_ = apiCli.Close()
		}
		return nil, nil, err
	}

	return compose.NewComposeService(dockerCli), dockerCli.Client(), nil
}

func ApiService() (api.Compose, client.APIClient, error) {
	return apiService()
}

func cleanup(workDir string) {
	logger.Info("cleaning up working dir", zap.String("path", workDir))
	if err := file.RMDir(workDir); err != nil {
		logger.Error("failed to cleanup working dir", zap.Error(err), zap.String("path", workDir))
	}
}

// writeFileAtomic previously did its own write-tmp + fsync + rename
// dance here. It was replaced with commonfile.WriteFileAtomic (via
// the `file` import alias) so every durable writer in the codebase
// goes through the same parent-dir-fsync'd helper.
