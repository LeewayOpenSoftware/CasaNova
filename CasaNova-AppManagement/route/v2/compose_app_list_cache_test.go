package v2

// The listing cache is only acceptable if it can never show the user a world
// that no longer exists. These tests pin the invalidation paths, not the
// speed: a cache that is merely fast and occasionally wrong is worse than no
// cache, because the dashboard is how the operator checks whether their last
// action worked.

import (
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

func resetListCache(t *testing.T) {
	t.Helper()
	invalidateComposeAppListCache()
	config.CasaOSGlobalVariables.AppChange.Store(false)
	t.Cleanup(func() {
		invalidateComposeAppListCache()
		config.CasaOSGlobalVariables.AppChange.Store(false)
	})
}

func sample() map[string]codegen.ComposeAppWithStoreInfo {
	return map[string]codegen.ComposeAppWithStoreInfo{"jellyfin": {}}
}

func TestListCacheServesAFreshEntry(t *testing.T) {
	resetListCache(t)
	if _, ok := cachedComposeAppList(); ok {
		t.Fatal("an empty cache reported a hit")
	}
	storeComposeAppList(sample())
	got, ok := cachedComposeAppList()
	if !ok || len(got) != 1 {
		t.Fatalf("fresh entry not served: ok=%v got=%v", ok, got)
	}
}

func TestListCacheExpires(t *testing.T) {
	resetListCache(t)
	storeComposeAppList(sample())
	listCacheMu.Lock()
	listCacheStamped = time.Now().Add(-listCacheTTL - time.Millisecond)
	listCacheMu.Unlock()
	if _, ok := cachedComposeAppList(); ok {
		t.Fatalf("an entry older than the %s TTL was served", listCacheTTL)
	}
}

func TestListCacheDropsOnExplicitInvalidation(t *testing.T) {
	// The path every mutating handler defers into. Without it, stopping an
	// app leaves it displayed as running until the TTL runs out.
	resetListCache(t)
	storeComposeAppList(sample())
	invalidateComposeAppListCache()
	if _, ok := cachedComposeAppList(); ok {
		t.Fatal("invalidation did not drop the entry: the UI would keep showing pre-action state")
	}
}

func TestListCacheYieldsToTheAppChangeFlag(t *testing.T) {
	// AppChange is set by the docker event watcher, so a container that dies
	// or starts OUTSIDE this process (portainer, docker CLI, a crash) also
	// invalidates the listing. Without this the dashboard could show a
	// crashed app as healthy for the whole TTL.
	resetListCache(t)
	storeComposeAppList(sample())
	config.CasaOSGlobalVariables.AppChange.Store(true)
	if _, ok := cachedComposeAppList(); ok {
		t.Fatal("a pending app change did not suppress the cache")
	}
}
