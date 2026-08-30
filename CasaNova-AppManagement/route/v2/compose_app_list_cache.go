package v2

// A short-lived cache for the compose app LISTING, held as the finished
// response value rather than as parsed objects.
//
// The cost being avoided: ComposeService.List re-parses every installed app's
// compose YAML on every call — compose-go's LoadProject with env
// interpolation, env_file reads and validation, measured at ~110 ms for the 23
// apps on the production box — and the dashboard polls this endpoint from
// every open tab.
//
// WHY THE RESPONSE AND NOT THE PARSED APPS. Caching *ComposeApp objects was
// the obvious design and is the wrong one here. An audit of every List/Get
// caller found the mutation surface is diffuse and mostly not in anything
// named like a mutator: GenerateYAMLFromComposeApp rewrites env values through
// shared inner maps despite taking the app by value, preserveInstalledServiceData
// aliases one app's Volumes/Environment into another, and five paths (Update,
// Apply, the start self-heal, both global-env re-ups, the nightly sweep) mutate
// from goroutines that run minutes after the request returned. Today every
// List re-parses, so those mutations die harmlessly; sharing pointers would
// make them persist — and would let a detached mutator write a map while this
// handler marshals it, which is a runtime panic, not a stale read.
//
// The response value shares nothing mutable: it is built, marshalled and
// handed out. Internal callers of List() keep their private parse, so no
// behaviour changes for them at all — only the polling endpoint stops paying.

import (
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

// listCacheTTL is deliberately shorter than the UI's poll interval so a user
// action still shows up on the next refresh, while a burst of tabs polling
// together collapses onto one parse.
const listCacheTTL = 3 * time.Second

var (
	listCacheMu      sync.Mutex
	listCacheValue   map[string]codegen.ComposeAppWithStoreInfo
	listCacheStamped time.Time
)

// invalidateComposeAppListCache drops the cached listing. Called wherever an
// app is installed, updated, uninstalled or changes state, so the very next
// list reflects it instead of waiting out the TTL — a user who clicks "stop"
// must not see the app still running for three seconds.
func invalidateComposeAppListCache() {
	listCacheMu.Lock()
	listCacheValue = nil
	listCacheMu.Unlock()
}

// cachedComposeAppList returns the cached listing when it is fresh AND no app
// has changed since it was built. The AppChange flag is the same signal the
// stats collector uses to drop its own cache; consulting it here means docker
// events invalidate the listing even when nothing went through this process.
func cachedComposeAppList() (map[string]codegen.ComposeAppWithStoreInfo, bool) {
	if config.CasaOSGlobalVariables.AppChange.Load() {
		return nil, false
	}
	listCacheMu.Lock()
	defer listCacheMu.Unlock()
	if listCacheValue == nil || time.Since(listCacheStamped) >= listCacheTTL {
		return nil, false
	}
	return listCacheValue, true
}

func storeComposeAppList(v map[string]codegen.ComposeAppWithStoreInfo) {
	listCacheMu.Lock()
	listCacheValue = v
	listCacheStamped = time.Now()
	listCacheMu.Unlock()
}
