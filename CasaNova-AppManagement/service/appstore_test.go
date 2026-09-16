package service_test

import (
	_ "embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/docker"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/samber/lo"
	"go.uber.org/goleak"
	"gotest.tools/v3/assert"
)

func TestGetComposeApp(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191
	logger.LogInitConsoleOnly()

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	appStorePath, err := os.MkdirTemp("", "appstore")
	assert.NilError(t, err)

	previousAppStorePath := config.AppInfo.AppStorePath
	t.Cleanup(func() { config.AppInfo.AppStorePath = previousAppStorePath })
	defer os.RemoveAll(appStorePath)

	config.AppInfo.AppStorePath = appStorePath

	appStore, err := service.AppStoreByURL("https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip")
	assert.NilError(t, err)

	err = appStore.UpdateCatalog()
	assert.NilError(t, err)

	catalog, err := appStore.Catalog()
	assert.NilError(t, err)

	for name, composeApp := range catalog {
		assert.Equal(t, name, composeApp.Name)
	}
}

func TestGetApp(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	logger.LogInitConsoleOnly()

	appStorePath, err := os.MkdirTemp("", "appstore")
	assert.NilError(t, err)

	previousAppStorePath := config.AppInfo.AppStorePath
	t.Cleanup(func() { config.AppInfo.AppStorePath = previousAppStorePath })
	defer os.RemoveAll(appStorePath)

	config.AppInfo.AppStorePath = appStorePath

	appStore, err := service.AppStoreByURL("https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip")
	assert.NilError(t, err)

	err = appStore.UpdateCatalog()
	assert.NilError(t, err)

	catalog, err := appStore.Catalog()
	assert.NilError(t, err)

	for _, composeApp := range catalog {
		for _, service := range composeApp.Services {
			app := composeApp.App(service.Name)
			assert.Equal(t, app.Name, service.Name)
		}
	}
}

// Note: the test need root permission
// THIS TEST USED TO WRITE INTO THE REAL APP STORE.
//
// Every other test in this package redirects config.AppInfo.AppStorePath at a
// temporary directory first. This one did not — and not because it inherited a
// sibling's setup: run on its own it still resolved to
// /var/lib/casaos/appstore, because that is the package default. On a machine
// where the suite runs as the service user — which is how the product is
// deployed, as root — `go test` would have unpacked a catalogue into the LIVE
// app store.
//
// What hid it was a guard narrower than its own message. It skipped on
// os.IsPermission only, while saying "is not writable"; EACCES is one way for a
// path not to be writable and the one that happens to occur on a developer
// box, where /var/lib/casaos is root-owned. A read-only filesystem gives EROFS,
// which is not a permission error, so the guard did not fire and the write was
// attempted. That the write then failed was luck, not design.
//
//	A guard that enumerates ways to fail protects against the ones somebody
//	thought of. Removing the reason to fail protects against the rest.
//
// So the fix is not a wider errno list: it is to point the path somewhere that
// belongs to this test, which makes the whole question moot and matches what
// every neighbour already does.
func TestSkipUpdateCatalog(t *testing.T) {
	logger.LogInitConsoleOnly()

	previousAppStorePath := config.AppInfo.AppStorePath
	t.Cleanup(func() { config.AppInfo.AppStorePath = previousAppStorePath })
	sandbox := t.TempDir()
	config.AppInfo.AppStorePath = sandbox

	appStoreURL := []string{
		"https://casaos.app/store/main.zip",
		"https://casaos.oss-cn-shanghai.aliyuncs.com/store/main.zip",
	}

	for _, url := range appStoreURL {
		appStore, err := service.AppStoreByURL(url)
		assert.NilError(t, err)
		workdir, err := appStore.WorkDir()
		assert.NilError(t, err)

		// CONTAINMENT IS CHECKED BEFORE ANYTHING IS CREATED, and that ordering
		// is the point rather than a detail. It is what lets the redirection
		// above be verified by deleting it: without it this test targets
		// /var/lib/casaos again, and the check below says so and stops — no
		// directory is attempted, nothing real is touched, and the failure is
		// legible. A check placed after file.MkDir could only report a write
		// that had already been tried.
		if !strings.HasPrefix(workdir, sandbox+string(os.PathSeparator)) {
			t.Fatalf("this test would operate on %q, which is outside its own temporary directory %q. "+
				"config.AppInfo.AppStorePath is not pointing where this test believes it is, and on a box "+
				"running as the service user that path is the LIVE app store.", workdir, sandbox)
		}

		// The catalogue is fetched over the network. Without one there is
		// nothing here to exercise, and reporting a missing network as a
		// failing product costs somebody an afternoon — the same reasoning,
		// and the same two helpers, as TestAppStoreList next door.
		//
		// This precondition only became reachable by fixing the path: while
		// the test skipped on EACCES it never got far enough to need a
		// network. Uncovering it is not breaking it.
		if host := catalogHost(t, url); !reachable(host) {
			t.Skipf("skipping: the app store catalog host %q is not reachable from this machine, so "+
				"nothing here can be exercised. This is a missing network, not a failing product.", host)
		}

		// mkdir workdir for first
		//
		// The os.IsPermission skip that used to stand here is deliberately
		// gone. Inside a directory this test just created, a mkdir that fails
		// is a real failure, and keeping the skip would turn it into a silent
		// pass — a stale excuse for a condition that can no longer arise.
		err = file.MkDir(workdir)
		assert.NilError(t, err)

		appStoreStat, err := os.Stat(workdir)
		assert.NilError(t, err)

		err = appStore.UpdateCatalog()
		assert.NilError(t, err)

		// get create and change time of appstore
		appStoreStatFirst, err := os.Stat(workdir)
		assert.NilError(t, err)

		assert.Equal(t, false, appStoreStatFirst.ModTime().Equal(appStoreStat.ModTime()))

		err = appStore.UpdateCatalog()
		assert.NilError(t, err)

		// get create and change time of appstore
		appStoreStatSecond, err := os.Stat(workdir)
		assert.NilError(t, err)

		assert.Equal(t, appStoreStatFirst.ModTime(), appStoreStatSecond.ModTime())
	}
}

func TestWorkDir(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	// test for http
	hostport := "localhost:8080"
	appStore, err := service.AppStoreByURL("http://" + hostport)
	assert.NilError(t, err)

	workdir, err := appStore.WorkDir()
	assert.NilError(t, err)
	assert.Equal(t, workdir, filepath.Join(config.AppInfo.AppStorePath, hostport, "d41d8cd98f00b204e9800998ecf8427e"))

	// test for https
	appStore, err = service.AppStoreByURL("https://" + hostport)
	assert.NilError(t, err)

	workdir, err = appStore.WorkDir()
	assert.NilError(t, err)
	assert.Equal(t, workdir, filepath.Join(config.AppInfo.AppStorePath, hostport, "d41d8cd98f00b204e9800998ecf8427e"))

	// test for github
	hostname := "github.com"
	path := "/IceWhaleTech/CasaOS-AppStore/archive/refs/heads/main.zip"
	appStore, err = service.AppStoreByURL("https://" + hostname + path)
	assert.NilError(t, err)

	workdir, err = appStore.WorkDir()
	assert.NilError(t, err)
	assert.Equal(t, workdir, filepath.Join(config.AppInfo.AppStorePath, hostname, "8b0968a7d7cda3f813d05736a89d0c92"))
}

func TestStoreRoot(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	workdir := t.TempDir()

	expectedStoreRoot := filepath.Join(workdir, "github.com", "IceWhaleTech", "CasaOS-AppStore", "main")
	err := file.MkDir(filepath.Join(expectedStoreRoot, common.AppsDirectoryName))
	assert.NilError(t, err)

	actualStoreRoot, err := service.StoreRoot(workdir)
	assert.NilError(t, err)

	assert.Equal(t, actualStoreRoot, expectedStoreRoot)
}

func TestLoadCategoryList(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	logger.LogInitConsoleOnly()

	storeRoot := t.TempDir()

	categoryListFilePath := filepath.Join(storeRoot, common.CategoryListFileName)

	err := file.WriteToFullPath([]byte(common.SampleCategoryListJSON), categoryListFilePath, 0o644)
	assert.NilError(t, err)

	dummyList := []interface{}{}
	buf := file.ReadFullFile(categoryListFilePath)
	err = json.Unmarshal(buf, &dummyList)
	assert.NilError(t, err)

	actualCategoryMap := service.LoadCategoryMap(storeRoot)
	assert.Assert(t, actualCategoryMap != nil)
	assert.Equal(t, len(actualCategoryMap), len(dummyList))

	for name, category := range actualCategoryMap {
		assert.Assert(t, category.Name != nil)
		assert.Assert(t, *category.Name == name)

		assert.Assert(t, category.Font != nil)
		assert.Assert(t, *category.Font != "")

		assert.Assert(t, category.Description != nil)
	}
}

func TestLoadRecommend(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	logger.LogInitConsoleOnly()

	storeRoot := t.TempDir()

	recommendListFilePath := filepath.Join(storeRoot, common.RecommendListFileName)

	type recommendListItem struct {
		AppID string `json:"appid"`
	}

	expectedRecommendList := []recommendListItem{
		{AppID: "app1"},
		{AppID: "app2"},
		{AppID: "app3"},
	}
	buf, err := json.Marshal(expectedRecommendList)
	assert.NilError(t, err)

	err = file.WriteToFullPath(buf, recommendListFilePath, 0o644)
	assert.NilError(t, err)

	actualRecommendList := service.LoadRecommend(storeRoot)
	assert.DeepEqual(t, actualRecommendList, lo.Map(expectedRecommendList, func(item recommendListItem, i int) string {
		return item.AppID
	}))
}

func TestBuildCatalog(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(topFunc1), goleak.IgnoreTopFunction(pollFunc1), goleak.IgnoreTopFunction(httpFunc1)) // https://github.com/census-instrumentation/opencensus-go/issues/1191

	defer func() {
		// workaround due to https://github.com/patrickmn/go-cache/issues/166
		docker.Cache = nil
		runtime.GC()
	}()

	storeRoot := t.TempDir()

	// test for invalid storeRoot
	_, err := service.BuildCatalog(storeRoot)
	assert.ErrorType(t, err, new(fs.PathError))

	appsPath := filepath.Join(storeRoot, common.AppsDirectoryName)
	err = file.MkDir(appsPath)
	assert.NilError(t, err)

	// test for empty catalog
	catalog, err := service.BuildCatalog(storeRoot)
	assert.NilError(t, err)
	assert.Equal(t, len(catalog), 0)

	// build test catalog
	err = file.MkDir(filepath.Join(appsPath, "test1"))
	assert.NilError(t, err)

	err = file.WriteToFullPath([]byte(common.SampleComposeAppYAML), filepath.Join(appsPath, "test1", common.ComposeYAMLFileName), 0o644)
	assert.NilError(t, err)
	catalog, err = service.BuildCatalog(storeRoot)
	assert.NilError(t, err)
	assert.Equal(t, len(catalog), 1)
}
