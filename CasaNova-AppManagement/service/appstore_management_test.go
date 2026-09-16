package service_test

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	previousAppStorePath := config.AppInfo.AppStorePath
	t.Cleanup(func() { config.AppInfo.AppStorePath = previousAppStorePath })
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

	// THIS TEST USED TO BE UNABLE TO FAIL ON ITS OWN.
	//
	// RegisterAppStore returns nil immediately and does the work in a
	// goroutine, and that goroutine calls the callback ONLY on the success
	// path: every failure (catalog download, SaveSetup) logs and returns.
	// The callback is therefore the sole signal, and it does not exist for
	// failures — so the bare `<-ch` below waited for something that was never
	// coming, and the only thing that could end the test was Go's own
	// 10-minute timeout, taking the whole package with it.
	//
	// Measured, because the duration misled us twice: the catalog fetch fails
	// in UNDER A SECOND here ("cannot resolve github.com": DNS refused), and
	// the remaining 599 seconds are the test sitting on this channel. It is
	// not a slow download. It is a fast failure followed by an unbounded wait,
	// and the 150s/600s figures seen on different days were both this — the
	// wait's length is set by whatever eventually kills the process, never by
	// the test.
	//
	// So the fix is two things, and neither is enough alone:
	//
	//  1. NO NETWORK IS A SKIP, NOT A FAILURE. The catalog lives at a remote
	//     host; a box without DNS cannot exercise this, and reporting that as
	//     a product failure is a lie that costs somebody an afternoon. The
	//     reachability check is a Go dial rather than an assumption, because
	//     the assumption was wrong in both directions here: curl reaches
	//     github fine from this shell while the test's own resolver is
	//     refused. Only the test's own network stack can answer for the test.
	//
	//  2. WITH NETWORK, THE WAIT IS BOUNDED AND FAILS LOUD. If the host is
	//     reachable and the callback still never fires, that is a real
	//     regression in RegisterAppStore and it now says so in seconds.
	//
	// The channel is BUFFERED for a reason that only appears once the wait is
	// bounded: the callback does `ch <- metadata`, so an unbuffered channel
	// whose reader has given up blocks that goroutine forever — and
	// goleak.VerifyNone above would then report a leak instead of the timeout.
	// Bounding the wait without buffering the channel just trades one failure
	// for a more confusing one.
	if host := catalogHost(t, expectAppStoreURL); !reachable(host) {
		t.Skipf("skipping: the app store catalog host %q is not reachable from this machine, so "+
			"nothing here can be exercised. This is a missing network, not a failing product.", host)
	}

	ch := make(chan *codegen.AppStoreMetadata, 1)

	err = appStoreManagement.RegisterAppStore(ctx, expectAppStoreURL, func(appStoreMetadata *codegen.AppStoreMetadata) {
		ch <- appStoreMetadata
	})
	assert.NilError(t, err)

	var appStoreMetadata *codegen.AppStoreMetadata
	select {
	case appStoreMetadata = <-ch:
	case <-ctx.Done():
		t.Fatalf("the context was cancelled before the app store was registered: %v", ctx.Err())
	case <-time.After(appStoreRegisterTimeout):
		t.Fatalf("RegisterAppStore did not call its callback within %s, although %q is reachable. "+
			"Every failure path in that goroutine logs and returns without signalling, so the console "+
			"output above carries the reason.", appStoreRegisterTimeout, expectAppStoreURL)
	}
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

	upgradable, err := appStoreManagement.IsUpdateAvailableWith(context.Background(), localComposeApp, storeComposeApp)
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
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(context.Background(), localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, upgradable)

	// An OLDER catalog tag is not an update. This is the invariant that
	// matters: the previous `!=` comparison reported "update available"
	// for any differing tag, so a stale catalog silently downgraded an
	// app that was already newer.
	setStoreImage(storeMainAppImage + ":1.22.0")
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(context.Background(), localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, !upgradable)

	// A tag that carries no version at all is ambiguous, and guessing
	// costs a downgrade — so it is deliberately not an update either.
	// (Rolling tags like "latest" never reach here; they take the digest
	// branch above.) This case used to assert the opposite and was the
	// last remnant of the `!=` semantics.
	setStoreImage(storeMainAppImage + ":test")
	upgradable, err = appStoreManagement.IsUpdateAvailableWith(context.Background(), localComposeApp, storeComposeApp)
	assert.NilError(t, err)
	assert.Assert(t, !upgradable)
}

// appStoreRegisterTimeout bounds the wait for RegisterAppStore's callback.
//
// Generous on purpose: the catalog is a ~1.2 MB archive that is downloaded and
// unpacked, and this must not go red on a slow link. What it may not be is
// unbounded — the value only has to be smaller than `go test`'s own timeout,
// so that the failure names THIS test instead of killing the package.
const appStoreRegisterTimeout = 60 * time.Second

// catalogHost is the host the catalog is fetched from, taken from the URL the
// test already uses rather than written down twice. A second copy would be one
// more thing to keep in step, and this test is here precisely because
// something that mattered was not.
func catalogHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		t.Fatalf("the app store URL %q is not a URL this test can read: %v", rawURL, err)
	}
	return u.Hostname()
}

// reachable reports whether a TCP connection to host:443 can be made.
//
// A dial rather than a ping or an environment variable, because it exercises
// the SAME resolver and the same egress path the downloader uses. Measured
// need: on this machine `curl https://github.com/` answers 200 in half a
// second while the test binary's own lookup is refused by 127.0.0.53, so any
// check made from outside the test would have reported the opposite of the
// truth.
func reachable(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
