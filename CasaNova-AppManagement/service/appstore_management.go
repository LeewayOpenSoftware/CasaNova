package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/bluele/gcache"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

var (
	ErrAppStoreSourceExists = fmt.Errorf("appstore source already exists")
	appStoreListMu          sync.RWMutex
)

type AppStoreManagement struct {
	isAppUpgradable      gcache.Cache
	defaultAppStore      AppStore
	isAppUpgrading       sync.Map
	onAppStoreRegister   []func(string) error
	onAppStoreUnregister []func(string) error
}

func (a *AppStoreManagement) AppStoreList() []codegen.AppStoreMetadata {
	appStoreListMu.RLock()
	listCopy := make([]string, len(config.ServerInfo.AppStoreList))
	copy(listCopy, config.ServerInfo.AppStoreList)
	appStoreListMu.RUnlock()

	return lo.Map(listCopy, func(appStoreURL string, id int) codegen.AppStoreMetadata {
		appStore, err := AppStoreByURL(appStoreURL)
		if err != nil {
			logger.Error("failed to construct appstore", zap.Error(err), zap.String("appstoreURL", appStoreURL))
			return codegen.AppStoreMetadata{}
		}

		workDir, err := appStore.WorkDir()
		if err != nil {
			logger.Error("failed to get appstore workdir", zap.Error(err), zap.String("appstoreURL", appStoreURL))
			return codegen.AppStoreMetadata{}
		}

		storeRoot, err := StoreRoot(workDir)
		if err != nil {
			logger.Error("failed to get appstore storeRoot", zap.Error(err), zap.String("appstoreURL", appStoreURL))
			storeRoot = "internal error - store root not found"
		}

		return codegen.AppStoreMetadata{
			ID:        &id,
			URL:       &appStoreURL,
			StoreRoot: &storeRoot,
		}
	})
}

func (a *AppStoreManagement) OnAppStoreRegister(fn func(string) error) {
	a.onAppStoreRegister = append(a.onAppStoreRegister, fn)
}

func (a *AppStoreManagement) OnAppStoreUnregister(fn func(string) error) {
	a.onAppStoreUnregister = append(a.onAppStoreUnregister, fn)
}

// ChangeGlobal sets a global setting and PERSISTS IT BEFORE RETURNING.
//
// It used to mutate the map and hand the write to a detached goroutine whose
// only failure channel was a log line — while returning nil to a handler that
// had therefore already answered 200. The setting was live in memory, so the
// UI re-read it and showed it applied; the next restart reloaded from the file
// that was never written and the setting was gone. That is the "my setting
// reverts by itself" report, and nothing in the product could see it: the API
// said success, the screen agreed, and only the daemon log knew.
//
// The write is a few hundred bytes to a local file, so doing it on the request
// path costs a millisecond and buys the caller a true answer. The handler in
// route/v2/global.go already has `if err := ...ChangeGlobal(...); err != nil`
// — it was simply never given a non-nil error to act on.
//
// On failure the in-memory value is PUT BACK. Otherwise memory and disk
// disagree in exactly the way the bug was about, only now with an error
// message on top of it.
func (a *AppStoreManagement) ChangeGlobal(key string, value string) error {
	config.GlobalMu.Lock()
	previous, existed := config.Global[key]
	config.Global[key] = value
	config.GlobalMu.Unlock()

	if err := config.SaveGlobal(); err != nil {
		config.GlobalMu.Lock()
		if existed {
			config.Global[key] = previous
		} else {
			delete(config.Global, key)
		}
		config.GlobalMu.Unlock()
		// Do NOT log the value — it may contain secrets (API keys, passwords).
		logger.Error("failed to save global env", zap.Error(err), zap.String("key", key))
		return fmt.Errorf("global setting %q was not saved: %w", key, err)
	}

	return nil
}

// DeleteGlobal removes a global setting and persists before returning.
// Same reasoning as ChangeGlobal above, including putting the value back when
// the write fails — a setting that is gone from memory and still on disk comes
// back at the next restart, which is the same lie pointing the other way.
func (a *AppStoreManagement) DeleteGlobal(key string) error {
	config.GlobalMu.Lock()
	previous, existed := config.Global[key]
	delete(config.Global, key)
	config.GlobalMu.Unlock()

	if err := config.SaveGlobal(); err != nil {
		if existed {
			config.GlobalMu.Lock()
			config.Global[key] = previous
			config.GlobalMu.Unlock()
		}
		logger.Error("failed to delete global env", zap.Error(err), zap.String("key", key))
		return fmt.Errorf("global setting %q was not deleted: %w", key, err)
	}

	return nil
}

func (a *AppStoreManagement) RegisterAppStore(ctx context.Context, appstoreURL string, callbacks ...func(*codegen.AppStoreMetadata)) error {
	// check if appstore already exists
	appStoreListMu.RLock()
	for _, url := range config.ServerInfo.AppStoreList {
		if strings.EqualFold(url, appstoreURL) {
			appStoreListMu.RUnlock()
			return ErrAppStoreSourceExists
		}
	}
	appStoreListMu.RUnlock()

	appstore, err := AppStoreByURL(appstoreURL)
	if err != nil {
		return err
	}

	go func() {
		// Wave 31: recover() so a panic inside the long-running
		// catalog fetch / save / callback chain does not silently
		// take down the AppManagement daemon. RegisterAppStore
		// already returned nil to its caller before this goroutine
		// runs, so without recovery any panic here is invisible to
		// the user *and* fatal to every other in-flight operation.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("RegisterAppStore goroutine panicked",
					zap.Any("panic", r), zap.String("appstoreURL", appstoreURL))
			}
		}()
		go PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterEnd, nil)

		var err error

		defer func() {
			if err == nil {
				return
			}

			PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
		}()

		if err = appstore.UpdateCatalog(); err != nil {
			logger.Error("failed to update appstore catalog", zap.Error(err), zap.String("appstoreURL", appstoreURL))

			return
		}

		// if everything is good, add to the list
		appStoreListMu.Lock()
		config.ServerInfo.AppStoreList = append(config.ServerInfo.AppStoreList, appstoreURL)
		appStoreListMu.Unlock()

		if err = config.SaveSetup(); err != nil {
			logger.Error("failed to save appstore list", zap.Error(err), zap.String("appstoreURL", appstoreURL))
			return
		}

		for _, fn := range a.onAppStoreRegister {
			if err := fn(appstoreURL); err != nil {
				logger.Error("failed to run onAppStoreRegister", zap.Error(err), zap.String("appstoreURL", appstoreURL))
			}
		}

		appStoreListMu.RLock()
		appStoreMetadata := &codegen.AppStoreMetadata{
			ID:  utils.Ptr(len(config.ServerInfo.AppStoreList) - 1),
			URL: &appstoreURL,
		}
		appStoreListMu.RUnlock()

		for _, callback := range callbacks {
			callback(appStoreMetadata)
		}
	}()

	return nil
}

// TODO: refactor the function and above function
func (a *AppStoreManagement) RegisterAppStoreSync(ctx context.Context, appstoreURL string, callbacks ...func(*codegen.AppStoreMetadata)) error {
	// check if appstore already exists
	appStoreListMu.RLock()
	for _, url := range config.ServerInfo.AppStoreList {
		if strings.EqualFold(url, appstoreURL) {
			appStoreListMu.RUnlock()
			return ErrAppStoreSourceExists
		}
	}
	appStoreListMu.RUnlock()

	appstore, err := AppStoreByURL(appstoreURL)
	if err != nil {
		return err
	}

	go PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterEnd, nil)

	defer func() {
		if err == nil {
			return
		}

		PublishEventWrapper(ctx, common.EventTypeAppStoreRegisterError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
	}()

	if err = appstore.UpdateCatalog(); err != nil {
		logger.Error("failed to update appstore catalog", zap.Error(err), zap.String("appstoreURL", appstoreURL))

		return err
	}

	// if everything is good, add to the list
	appStoreListMu.Lock()
	config.ServerInfo.AppStoreList = append(config.ServerInfo.AppStoreList, appstoreURL)
	appStoreListMu.Unlock()

	if err = config.SaveSetup(); err != nil {
		logger.Error("failed to save appstore list", zap.Error(err), zap.String("appstoreURL", appstoreURL))
		return err
	}

	for _, fn := range a.onAppStoreRegister {
		if err := fn(appstoreURL); err != nil {
			logger.Error("failed to run onAppStoreRegister", zap.Error(err), zap.String("appstoreURL", appstoreURL))
		}
	}

	appStoreListMu.RLock()
	appStoreMetadata := &codegen.AppStoreMetadata{
		ID:  utils.Ptr(len(config.ServerInfo.AppStoreList) - 1),
		URL: &appstoreURL,
	}
	appStoreListMu.RUnlock()

	for _, callback := range callbacks {
		callback(appStoreMetadata)
	}

	return nil
}

func (a *AppStoreManagement) UnregisterAppStore(appStoreID uint) error {
	appStoreListMu.Lock()
	if appStoreID >= uint(len(config.ServerInfo.AppStoreList)) {
		appStoreListMu.Unlock()
		return fmt.Errorf("appstore id %d out of range", appStoreID)
	}

	appStoreURL := config.ServerInfo.AppStoreList[appStoreID]

	// remove appstore from list
	{
		config.ServerInfo.AppStoreList = append(config.ServerInfo.AppStoreList[:appStoreID], config.ServerInfo.AppStoreList[appStoreID+1:]...)
	}
	appStoreListMu.Unlock()

	{
		if err := config.SaveSetup(); err != nil {
			return err
		}
	}

	// remove appstore workdir
	{
		appStore, err := AppStoreByURL(appStoreURL)
		if err != nil {
			return err
		}

		workdir, err := appStore.WorkDir()
		if err != nil {
			logger.Error("error while getting appstore workdir", zap.Error(err), zap.String("url", appStoreURL))
		}

		if len(workdir) != 0 {
			if err := file.RMDir(workdir); err != nil {
				logger.Error("error while removing appstore workdir", zap.Error(err), zap.String("workdir", workdir))
			}
		}
	}

	for _, fn := range a.onAppStoreUnregister {
		if err := fn(appStoreURL); err != nil {
			return err
		}
	}
	return nil
}

func (a *AppStoreManagement) AppStoreMap() (map[string]AppStore, error) {
	appStoreListMu.RLock()
	listCopy := make([]string, len(config.ServerInfo.AppStoreList))
	copy(listCopy, config.ServerInfo.AppStoreList)
	appStoreListMu.RUnlock()

	appStoreMap := lo.SliceToMap(listCopy, func(appStoreURL string) (string, AppStore) {
		appStore, err := AppStoreByURL(appStoreURL)
		if err != nil {
			return "", nil
		}
		return appStoreURL, appStore
	})

	delete(appStoreMap, "")

	return appStoreMap, nil
}

// AppStore interface
func (a *AppStoreManagement) CategoryMap() (map[string]codegen.CategoryInfo, error) {
	registry := LoadStoreRegistry()
	registryCatalog, err := registry.MergedCatalog()
	if err == nil && len(registryCatalog) > 0 {
		categoryMap := registry.MergedCategoryMap()
		for name, category := range categoryMap {
			category.Count = utils.Ptr(0)
			categoryMap[name] = category
		}
		for _, app := range registryCatalog {
			storeInfo, err := app.StoreInfo(false)
			if err != nil {
				continue
			}
			category, ok := categoryMap[storeInfo.Category]
			if !ok {
				continue
			}
			category.Count = lo.ToPtr(*category.Count + 1)
			categoryMap[storeInfo.Category] = category
		}
		return categoryMap, nil
	}
	if err != nil {
		logger.Error("error while loading registry category map", zap.Error(err))
	}

	appStoreMap, err := a.AppStoreMap()
	if err != nil {
		return nil, err
	}

	allFailed := true

	categoryMap := map[string]codegen.CategoryInfo{}
	for _, appStore := range appStoreMap {
		c, err := appStore.CategoryMap()
		if err != nil {
			logger.Error("error while loading category map", zap.Error(err))
			continue
		}

		allFailed = false

		for name, category := range c {
			categoryMap[name] = category
		}
	}

	if allFailed {
		logger.Info("all appstores failed to load category map, using default")

		if a.defaultAppStore == nil {
			logger.Info("no default appstore — returning empty category map")
			return map[string]codegen.CategoryInfo{}, nil
		}

		categoryMap, err = a.defaultAppStore.CategoryMap()
		if err != nil {
			return nil, err
		}
	}

	for name, category := range categoryMap {
		category.Count = utils.Ptr(0)
		categoryMap[name] = category
	}

	catalog, err := a.Catalog()
	if err != nil {
		return nil, err
	}

	for _, app := range catalog {
		storeInfo, err := app.StoreInfo(false)
		if err != nil {
			continue
		}

		category, ok := categoryMap[storeInfo.Category]
		if !ok {
			continue
		}

		category.Count = lo.ToPtr(*category.Count + 1)

		categoryMap[storeInfo.Category] = category
	}

	return categoryMap, nil
}

func (a *AppStoreManagement) Recommend() ([]string, error) {
	registry := LoadStoreRegistry()
	registryCatalog, err := registry.MergedCatalog()
	if err == nil && len(registryCatalog) > 0 {
		return registry.MergedRecommend(), nil
	}
	if err != nil {
		logger.Error("error while loading registry recommend list", zap.Error(err))
	}

	appStoreMap, err := a.AppStoreMap()
	if err != nil {
		logger.Error("error while loading appstore map", zap.Error(err))
		return nil, err
	}

	allFailed := true

	recommend := []string{}
	for _, appStore := range appStoreMap {
		r, err := appStore.Recommend()
		if err != nil {
			logger.Error("error while getting appstore recommend", zap.Error(err))
			continue
		}

		allFailed = false
		recommend = lo.Union(recommend, r)
	}

	if !allFailed {
		return recommend, nil
	}

	logger.Info("No appstore registered")
	if a.defaultAppStore == nil {
		logger.Info("WARNING - no default appstore")
		return nil, nil
	}

	logger.Info("Using default appstore")
	recommend, err = a.defaultAppStore.Recommend()
	if err != nil {
		logger.Error("error while getting default appstore recommend list", zap.Error(err))
		return nil, err
	}

	return recommend, nil
}

func (a *AppStoreManagement) Catalog() (map[string]*ComposeApp, error) {
	registryCatalog, registryErr := LoadStoreRegistry().MergedCatalog()
	if registryErr == nil && len(registryCatalog) > 0 {
		return registryCatalog, nil
	}
	if registryErr != nil {
		logger.Error("error while getting registry appstore catalog", zap.Error(registryErr))
	}

	catalog := map[string]*ComposeApp{}

	appStoreMap, err := a.AppStoreMap()
	if err != nil {
		return nil, err
	}

	allFailed := true

	for _, appStore := range appStoreMap {

		c, err := appStore.Catalog()
		if err != nil {
			logger.Error("error while getting appstore catalog", zap.Error(err))
			continue
		}

		allFailed = false
		for storeAppID, composeApp := range c {
			catalog[storeAppID] = composeApp
		}
	}

	if !allFailed {
		return catalog, nil
	}

	logger.Info("No appstore registered or all stores failed — trying default")
	if a.defaultAppStore == nil {
		logger.Info("no default appstore available — catalog will be empty")
		return map[string]*ComposeApp{}, fmt.Errorf("all app stores failed to load and no default store is available")
	}

	logger.Info("Using default appstore")
	catalog, err = a.defaultAppStore.Catalog()
	if err != nil {
		return map[string]*ComposeApp{}, err
	}

	return catalog, nil
}

func (a *AppStoreManagement) UpdateCatalog() error {
	defer LoadStoreRegistry().invalidateCatalog()
	// reload config.
	// the appstore may be change in runtime.
	config.ReloadConfig()

	registry := LoadStoreRegistry()
	for _, store := range registry.EnabledStores() {
		if store.Type != "remote" {
			continue
		}
		if err := SyncRemoteStore(store); err != nil {
			logger.Error("error while syncing registered app store", zap.Error(err), zap.String("store", store.ID), zap.String("url", store.URL))
			continue
		}
		registry.MarkSynced(store.ID)
	}

	appStoreMap, err := a.AppStoreMap()
	if err != nil {
		return err
	}

	for url, appStore := range appStoreMap {
		if err := appStore.UpdateCatalog(); err != nil {
			logger.Error("error while updating catalog for app store", zap.Error(err), zap.String("url", url))
		}
	}

	// clean cache
	a.isAppUpgradable.Purge()

	return nil
}

func (a *AppStoreManagement) ComposeApp(id string) (*ComposeApp, error) {
	registryCatalog, registryErr := LoadStoreRegistry().MergedCatalog()
	if registryErr == nil {
		if composeApp, ok := registryCatalog[id]; ok {
			return composeApp, nil
		}
	} else {
		logger.Error("error while getting registry appstore compose app", zap.Error(registryErr), zap.String("id", id))
	}

	appStoreMap, err := a.AppStoreMap()
	if err != nil {
		return nil, err
	}

	for _, appStore := range appStoreMap {
		composeApp, appErr := appStore.ComposeApp(id)
		if appErr != nil {
			logger.Error("error while getting appstore compose app", zap.Error(appErr))
			continue
		}

		if composeApp != nil {
			return composeApp, nil
		}
	}

	logger.Info("app not found in any appstore", zap.String("id", id))

	if a.defaultAppStore == nil {
		logger.Info("WARNING - no default appstore")
		return nil, nil
	}

	logger.Info("Using default appstore")

	composeApp, err := a.defaultAppStore.ComposeApp(id)
	if err != nil {
		return nil, err
	}

	return composeApp, nil
}

func (a *AppStoreManagement) WorkDir() (string, error) {
	return "", fmt.Errorf("not implemented: AppStoreManagement is a virtual appstore")
}

// IsUpdateAvailable answers from a one-hour cache when it can. On a miss it
// talks to the image's registry, which is why it takes a context: the caller
// decides how long the whole sweep may run, and a request that has already
// given up should not leave registry calls running behind it.
func (a *AppStoreManagement) IsUpdateAvailable(ctx context.Context, composeApp *ComposeApp) bool {
	storeID := composeApp.Name
	if value, err := a.isAppUpgradable.Get(storeID); err == nil {
		switch value := value.(type) {
		case bool:
			return value
		default:
			logger.Error("invalid type in cache", zap.String("storeID", storeID), zap.Any("value", value))
			return false
		}
	}

	isUpdate, err := a.isUpdateAvailable(ctx, composeApp)
	if err != nil {
		logger.Error("failed to check if update is available", zap.Error(err))
		return false
	}
	_ = a.isAppUpgradable.Set(storeID, isUpdate)
	return isUpdate
}

func (a *AppStoreManagement) isUpdateAvailable(ctx context.Context, composeApp *ComposeApp) (bool, error) {
	// handle no tag logic and for easy to test
	storeInfo, err := composeApp.StoreInfo(false)
	if err != nil {
		// A FAILURE TO CHECK IS NOT "UP TO DATE".
		//
		// This returned (false, nil), and the caller only declines to cache
		// when the error is non-nil — so a transient failure here was written
		// into isAppUpgradable as a measured `false` and stayed there for the
		// cache's whole lifetime. The app reported "up to date" for something
		// nobody had managed to check, and the only way out was a restart or
		// the cache expiring. Propagating the error costs the caller one
		// uncached retry and costs the user nothing.
		logger.Error("failed to get store info of compose app, cannot tell whether an update exists", zap.Error(err))
		return false, err
	}

	// Nil check BEFORE the first dereference. It used to sit below
	// storeInfo.IsUncontrolled, where it could only ever run after a nil
	// storeInfo had already panicked — a guard placed where it cannot guard.
	if storeInfo == nil {
		return false, ErrStoreInfoNotFound
	}

	// if app is uncontrolled, no update available
	if storeInfo.IsUncontrolled != nil && *storeInfo.IsUncontrolled {
		return false, nil
	}

	if storeInfo.StoreAppID == nil || *storeInfo.StoreAppID == "" {
		// Genuinely "no update", not a failure: an app with no store app ID
		// has no upstream to compare against, so there is nothing to check and
		// caching the answer is correct. Written as an explicit nil because it
		// used to `return false, err` with err already known to be nil, which
		// reads like a propagated failure and is not one.
		return false, nil
	}

	storeComposeApp, err := a.ComposeApp(*storeInfo.StoreAppID)
	if err != nil {
		logger.Error("failed to get store compose app, thus no update available", zap.Error(err))
		return false, err
	}

	if storeComposeApp == nil {
		logger.Error("store compose app not found, thus no update available", zap.String("storeAppID", *storeInfo.StoreAppID))
		return false, nil
	}

	return a.IsUpdateAvailableWith(ctx, composeApp, storeComposeApp)
}

// the patch is have no choice
// the digest compare is not work for these images
// I don't know why, but I have to do this
// I will remove the patch after I rewrite the digest compare
var NoUpdateBlacklist = []string{
	"johnguan/stable-diffusion-webui:latest",
}

func (a *AppStoreManagement) IsUpdateAvailableWith(ctx context.Context, composeApp *ComposeApp, storeComposeApp *ComposeApp) (bool, error) {
	currentTag, err := composeApp.MainTag()
	if err != nil {
		logger.Error("failed to get current tag", zap.Error(err))
		return false, err
	}
	mainService, err := composeApp.MainService()
	if err != nil {
		logger.Error("failed to get main service", zap.Error(err))
		return false, err
	}
	if lo.Contains(common.NeedCheckDigestTags, currentTag) {
		// Wave 35-X: use SharedClient + bounded ctx instead of a fresh
		// client + context.Background(). The previous code created a
		// new Docker client on every update-availability check (called
		// from catalog refresh, the daily auto-update sweep, and the
		// UI's "check for updates" button), each one leaking a TCP
		// half-open if the daemon was wedged. The unbounded ctx meant
		// any single hung ImageInspect froze the calling goroutine
		// until daemon restart.
		// Derived from the caller's context, not from Background: a request
		// that has already spent its budget must not start another 15 seconds
		// of work. Whichever deadline is nearer wins.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cli, clientErr := docker.SharedClient()
		if clientErr != nil {
			logger.Error("failed to get shared docker client", zap.Error(clientErr))
			return false, clientErr
		}

		if lo.Contains(NoUpdateBlacklist, mainService.Image) {
			return false, nil
		}

		image, _ := docker.ExtractImageAndTag(mainService.Image)

		imageInfo, _, clientErr := cli.ImageInspectWithRaw(ctx, image)
		if clientErr != nil {
			logger.Error("failed to inspect image", zap.Error(clientErr))
			return false, clientErr
		}

		match, clientErr := docker.CompareDigest(ctx, mainService.Image, imageInfo.RepoDigests)
		if clientErr != nil {
			logger.Error("failed to compare digest", zap.Error(clientErr))
			return false, clientErr
		}
		// match means no update available
		return !match, nil
	}
	storeTag, err := storeComposeApp.MainTag()
	if err != nil {
		return false, err
	}
	// An update is available only when the catalog tag is a genuine UPGRADE.
	// Comparing with `!=` (the old behaviour) flagged an update whenever the
	// installed tag merely DIFFERED — including when the installed version was
	// NEWER than a stale catalog — and applying it would silently downgrade.
	return tagVersionNewer(currentTag, storeTag), nil
}

func (a *AppStoreManagement) IsUpdating(appID string) bool {
	_, ok := a.isAppUpgrading.Load(appID)
	return ok
}

func (a *AppStoreManagement) StartUpgrade(appID string) {
	a.isAppUpgrading.Store(appID, struct{}{})
}

func (a *AppStoreManagement) FinishUpgrade(appID string) {
	a.isAppUpgrading.Delete(appID)
	a.isAppUpgradable.Remove(appID)
}

func NewAppStoreManagement() *AppStoreManagement {
	defaultAppStore, err := NewDefaultAppStore()
	if err != nil {
		logger.Error("failed to load default appstore — catalog fallback will be unavailable",
			zap.Error(err))
	}

	appStoreManagement := &AppStoreManagement{
		defaultAppStore: defaultAppStore,
		isAppUpgradable: gcache.New(100).LRU().Expiration(1 * time.Hour).Build(),
		isAppUpgrading:  sync.Map{},
	}

	return appStoreManagement
}
