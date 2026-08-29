package service_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/goleak"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v3"
	"gotest.tools/v3/assert"
)

func TestAppStoreList(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	logger.LogInitConsoleOnly()

	file, err := os.CreateTemp("", "app-management.conf")
	assert.NilError(t, err)

	defer os.Remove(file.Name())

	config.InitSetup(file.Name(), "")
	config.AppInfo.AppStorePath = t.TempDir()

	appStoreManagement := service.NewAppStoreManagement()

	appStoreList := appStoreManagement.AppStoreList()
	assert.Equal(t, len(appStoreList), 0)

	registeredAppStoreList := []string{}
	appStoreManagement.OnAppStoreRegister(func(appStoreURL string) error {
		registeredAppStoreList = append(registeredAppStoreList, appStoreURL)
		return nil
	})

	unregisteredAppStoreList := []string{}
	appStoreManagement.OnAppStoreUnregister(func(appStoreURL string) error {
		unregisteredAppStoreList = append(unregisteredAppStoreList, appStoreURL)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = common.WithProperties(ctx, map[string]string{})

	expectAppStoreURL := strings.ToLower("https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip")

	ch := make(chan *codegen.AppStoreMetadata)

	err = appStoreManagement.RegisterAppStore(ctx, expectAppStoreURL, func(appStoreMetadata *codegen.AppStoreMetadata) {
		ch <- appStoreMetadata
	})
	assert.NilError(t, err)

	appStoreMetadata := <-ch
	assert.Equal(t, *appStoreMetadata.URL, expectAppStoreURL)
	assert.Assert(t, len(registeredAppStoreList) == 1)

	appStoreList = appStoreManagement.AppStoreList()
	assert.Equal(t, len(appStoreList), 1)

	actualAppStoreURL := *appStoreList[0].URL
	assert.Equal(t, actualAppStoreURL, expectAppStoreURL)

	err = appStoreManagement.UnregisterAppStore(0)
	assert.NilError(t, err)
	assert.Assert(t, len(unregisteredAppStoreList) == 1)

	assert.DeepEqual(t, registeredAppStoreList, unregisteredAppStoreList)
}

func TestIsUpgradable(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	logger.LogInitConsoleOnly()

	appStoreManagement := service.NewAppStoreManagement()

	// mock store compose app
	storeComposeApp, err := service.NewComposeAppFromYAML([]byte(common.SampleComposeAppYAML), true, false)
	assert.NilError(t, err)

	storeComposeApp.SetStoreAppID("test")

	// compose-go v2: Services is map[string]ServiceConfig keyed by
	// service name. Resolve the main service through the public helper
	// instead of [0], which only worked when v1 returned a slice.
	storeMainService, err := storeComposeApp.MainService()
	assert.NilError(t, err)
	storeMainAppImage, _ := docker.ExtractImageAndTag(storeMainService.Image)

	storeComposeAppStoreInfo, err := storeComposeApp.StoreInfo(false)
	assert.NilError(t, err)

	// mock local compose app
	appsPath := t.TempDir()

	composeFilePath := filepath.Join(appsPath, common.ComposeYAMLFileName)

	buf, err := yaml.Marshal(storeComposeApp)
	assert.NilError(t, err)

	err = file.WriteToFullPath(buf, composeFilePath, 0o644)
	assert.NilError(t, err)

	localComposeApp, err := service.LoadComposeAppFromConfigFile(*storeComposeAppStoreInfo.StoreAppID, composeFilePath)
	assert.NilError(t, err)

	upgradable, err := appStoreManagement.IsUpdateAvailableWith(localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, !upgradable)

	// compose-go v2: mutate via map key. Going through MainService()
	// would return a value copy, so re-key the underlying service
	// directly instead.
	storeMainName := storeMainService.Name
	setStoreImage := func(image string) {
		mutated := storeComposeApp.Services[storeMainName]
		mutated.Image = image
		storeComposeApp.Services[storeMainName] = mutated
	}

	// A genuinely newer catalog version IS an update. The fixture ships
	// 1.23.1, so 1.24.0 is the real upgrade case.
	setStoreImage(storeMainAppImage + ":1.24.0")
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, upgradable)

	// An OLDER catalog tag is not an update. This is the invariant that
	// matters: the previous `!=` comparison reported "update available"
	// for any differing tag, so a stale catalog silently downgraded an
	// app that was already newer.
	setStoreImage(storeMainAppImage + ":1.22.0")
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, !upgradable)

	// A tag that carries no version at all is ambiguous, and guessing
	// costs a downgrade — so it is deliberately not an update either.
	// (Rolling tags like "latest" never reach here; they take the digest
	// branch above.) This case used to assert the opposite and was the
	// last remnant of the `!=` semantics.
	setStoreImage(storeMainAppImage + ":test")
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, !upgradable)
}
