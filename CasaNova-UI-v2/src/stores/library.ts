// App Library store — catalog browsing, search, categories, install.
//
// Fetches the merged catalog from /v2/app_management/apps (which the
// backend assembles from all enabled stores).
//
// Install flow:
//   1. Fetch compose YAML via getAppComposeYaml(appId)
//   2. POST YAML to /v2/app_management/compose (installFromLibrary)
//   3. Subscribe to SSE progress via containersApi.subscribeProgress
//   4. On completion, refresh containersStore so app appears on dashboard

import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { libraryApi, containersApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { CatalogApp, CategoryInfo, StoreEntry, UpgradableApp } from '@/api/library'
import { useInstallProgressStore } from './installProgress'
import { useContainersStore } from './containers'

export const useLibraryStore = defineStore('library', () => {
  const catalog = ref<Record<string, CatalogApp>>({})
  const installedIds = ref<string[]>([])
  const categories = ref<CategoryInfo[]>([])
  const upgradable = ref<UpgradableApp[]>([])
  const stores = ref<StoreEntry[]>([])
  const loading = ref(false)
  const fixtureMode = ref(false)
  const lastError = ref<string | null>(null)

  // Wave 30: track every in-flight install's EventSource and fallback
  // setTimeout so they can be torn down on logout. Without this, if a
  // user logs out mid-install the EventSource keeps polling with a
  // stale token (the backend logs auth errors forever) and the timer
  // eventually fires a refresh() with the same stale token. Both
  // surfaces leak resources and pollute logs.
  const liveSources = new Set<EventSource>()
  const liveTimers = new Set<ReturnType<typeof setTimeout>>()
  function trackSource(es: EventSource) { liveSources.add(es) }
  function untrackSource(es: EventSource) { liveSources.delete(es); try { es.close() } catch { /* already closed */ } }
  function trackTimer(t: ReturnType<typeof setTimeout>) { liveTimers.add(t) }
  function untrackTimer(t: ReturnType<typeof setTimeout>) { liveTimers.delete(t); clearTimeout(t) }
  function abortAllInstalls() {
    for (const es of liveSources) {
      try { es.close() } catch { /* already closed */ }
    }
    liveSources.clear()
    for (const t of liveTimers) clearTimeout(t)
    liveTimers.clear()
    // Wave 35-V: also slam the panel closed on logout. Without this,
    // the panel's visibility bit persists in the store across the
    // logout → /login → re-login bounce, so when the user comes back
    // they briefly see the library panel backdrop flash on top of
    // the dashboard before realizing it's stale state. The panel
    // should always open fresh via an explicit click, not via
    // leftover state from a previous session.
    panelOpen.value = false
    // Drop the cached catalog too so a re-open re-fetches under the
    // new session's credentials instead of showing stale data
    // belonging to whoever was logged in before.
    catalog.value = {}
    installedIds.value = []
    categories.value = []
    upgradable.value = []
    lastError.value = null
  }
  // Listen once per store instance for the global logout signal that
  // auth.ts dispatches in logout(). The listener is never removed
  // because Pinia stores live for the lifetime of the page; on full
  // page reload the store is re-created and a fresh listener is
  // installed.
  if (typeof window !== 'undefined') {
    window.addEventListener('auth-logout', abortAllInstalls)
  }

  // Panel open state — shared across TopBar, AppGrid, and the panel
  const panelOpen = ref(false)
  const searchQuery = ref('')
  const selectedCategory = ref<string | null>(null)

  // Set of installed store_app_ids for quick lookup
  const installedSet = computed(() => new Set(installedIds.value))

  // Cross-reference with containers store for live status.
  // Lazy access inside computed avoids Pinia init-order issues.
  const installedAppNames = computed(() =>
    new Set(useContainersStore().apps.map((a) => a.id)),
  )

  // Derived: flat sorted array for rendering
  const catalogList = computed(() => {
    const all = Object.values(catalog.value)
    const q = searchQuery.value.toLowerCase().trim()
    const cat = selectedCategory.value

    return all
      .filter((app) => {
        // Skip the "All" pseudo-category
        if (cat && cat !== 'All' && app.category !== cat) return false
        if (q) {
          const title = app.title?.en_US?.toLowerCase() ?? ''
          const tagline = app.tagline?.en_US?.toLowerCase() ?? ''
          const dev = app.developer?.toLowerCase() ?? ''
          return title.includes(q) || tagline.includes(q) || dev.includes(q) || app.name.includes(q)
        }
        return true
      })
      .sort((a, b) => (a.title?.en_US ?? a.name).localeCompare(b.title?.en_US ?? b.name))
  })

  // Backend returns categories as an array (with "All" prepended)
  const categoryList = computed(() => {
    // Filter out "All" from the sidebar — it's represented by the null
    // selectedCategory state (the "All Apps" button).
    return categories.value.filter((c) => c.name !== 'All')
  })

  const totalAppCount = computed(() => {
    const all = categories.value.find((c) => c.name === 'All')
    return all?.count ?? Object.keys(catalog.value).length
  })

  // Recommended: first 6 apps by priority (or whatever the backend returns)
  const recommendedApps = computed(() =>
    Object.values(catalog.value).slice(0, 6),
  )

  // Upgradable app IDs for badge display
  const upgradableSet = computed(() =>
    new Set(upgradable.value.map((u) => u.store_app_id).filter(Boolean) as string[]),
  )

  /**
   * getAppStatus — returns the status of an app in the library grid.
   * Used by AppLibraryPanel to show badges on cards.
   */
  function getAppStatus(app: CatalogApp): 'not_installed' | 'running' | 'stopped' | 'update' {
    const storeId = app.store_app_id ?? app.name
    if (upgradableSet.value.has(storeId)) return 'update'
    if (installedSet.value.has(storeId) || installedAppNames.value.has(app.name)) {
      // Check if any service is running
      const installed = useContainersStore().apps.find((a) => a.id === app.name)
      if (installed && installed.services.some((s) => s.state === 'running')) return 'running'
      return 'stopped'
    }
    return 'not_installed'
  }

  async function refresh(): Promise<void> {
    loading.value = true
    lastError.value = null

    const [catResult, categoriesResult] = await Promise.all([
      safeCall(() => libraryApi.getCatalog()),
      safeCall(() => libraryApi.getCategories()),
    ])

    loading.value = false

    if (catResult.error) {
      lastError.value = catResult.error
      return
    }

    fixtureMode.value = false

    // Backend may return null/undefined data on transient failures (all
    // stores down, empty catalog rebuild, etc.).  Treat as empty rather
    // than silently leaving stale state — the empty-state UI will show
    // and the user can retry.
    const data = catResult.data as { list: Record<string, CatalogApp>; installed: string[] } | null
    catalog.value = data?.list ?? {}
    installedIds.value = data?.installed ?? []

    categories.value = (categoriesResult.data as CategoryInfo[]) ?? []

    // Upgradable — best effort, don't block
    safeCall(() => libraryApi.getUpgradableApps())
      .then((r) => { upgradable.value = r.data ?? [] })
      .catch(() => { /* upgradable check failed — non-critical */ })
  }

  async function refreshStores(): Promise<void> {
    const result = await safeCall(
      () => libraryApi.listStores(),
    )
    stores.value = result.data ?? []
  }

  // ----- Store (app source) management -----
  // Each action wraps the api-client, refreshes the local `stores`
  // list from the backend on success (so priority ordering / last_synced
  // stay authoritative), and returns an error string | null so the
  // AppStores settings page can surface a toast. The backend owns SSRF
  // validation, default-store protection, and merge ordering.

  async function addStore(entry: Partial<StoreEntry>): Promise<string | null> {
    const res = await safeCall(() => libraryApi.registerStore(entry))
    if (res.error) return res.error
    await refreshStores()
    return null
  }

  async function removeStore(id: string): Promise<string | null> {
    const res = await safeCall(async () => {
      await libraryApi.unregisterStore(id)
      return true
    })
    if (res.error) return res.error
    await refreshStores()
    return null
  }

  async function toggleStore(id: string, enabled: boolean): Promise<string | null> {
    const res = await safeCall(() => libraryApi.updateStore(id, { enabled }))
    if (res.error) return res.error
    await refreshStores()
    return null
  }

  async function reprioritize(id: string, priority: number): Promise<string | null> {
    const res = await safeCall(() => libraryApi.updateStore(id, { priority }))
    if (res.error) return res.error
    await refreshStores()
    return null
  }

  async function sync(id: string): Promise<string | null> {
    const res = await safeCall(async () => {
      await libraryApi.syncStore(id)
      return true
    })
    if (res.error) return res.error
    await refreshStores()
    // A sync changes the merged catalog — refresh it too, best effort.
    refresh()
    return null
  }

  /**
   * installApp — the real install flow:
   *   1. Fetch compose YAML from catalog
   *   2. POST to install endpoint
   *   3. Subscribe to SSE for progress
   *   4. Refresh containers on completion
   */
  async function installApp(appId: string): Promise<boolean> {
    const installs = useInstallProgressStore()
    const app = catalog.value[appId]
    const name = app?.title?.en_US ?? appId
    const icon = app?.icon

    // Step 1: Fetch the compose YAML
    const yamlResult = await safeCall(
      () => libraryApi.getAppComposeYaml(appId),
    )
    if (yamlResult.error) {
      installs.pushToast('error', `Failed to fetch ${name}: ${yamlResult.error}`)
      return false
    }
    if (!yamlResult.data) {
      installs.pushToast('error', `No compose file found for ${name}`)
      return false
    }

    // Step 2: Start the install
    const installId = installs.beginInstall(name, icon)

    const installResult = await safeCall(
      () => libraryApi.installFromLibrary(yamlResult.data),
    )
    if (installResult.error) {
      installs.pushToast('error', `Failed to install ${name}: ${installResult.error}`)
      installs.finishInstall(installId)
      return false
    }

    // Step 3: Subscribe to SSE progress if we got an install ID back
    const backendId = installResult.data
    if (backendId) {
      try {
        // Captured-in-closure ref so we can untrack on terminal status
        // OR have the logout handler close it from outside.
        let es: EventSource
        // Safety net: if SSE never reports a terminal status (token
        // expires mid-install, backend crashes, network drops and the
        // browser gives up after its retry budget), the install would
        // otherwise sit in the progress panel forever. Force a refresh
        // after a generous timeout so the panel self-heals. The timer
        // is cleared on the happy path below.
        let safetyTimer: ReturnType<typeof setTimeout> | null = null
        const armSafety = () => {
          if (safetyTimer) untrackTimer(safetyTimer)
          safetyTimer = setTimeout(() => {
            safetyTimer = null
            try { es?.close() } catch { /* already closed */ }
            untrackSource(es)
            installs.finishInstall(installId)
            useContainersStore().refresh(true)
            refresh()
          }, 5 * 60 * 1000) // 5 min — longer than any sane pull
          trackTimer(safetyTimer)
        }
        armSafety()
        es = containersApi.subscribeProgress(
          backendId,
          (msg) => {
            // Update the install progress store entry
            const idx = installs.active.findIndex((i) => i.id === installId)
            if (idx !== -1) {
              const next = [...installs.active]
              next[idx] = { ...next[idx], progress: msg.progress }
              installs.active = next
            }
            // Any progress frame rearms the safety timer so a healthy
            // long pull (large images, slow mirrors) isn't cut off.
            armSafety()
            if (msg.status === 'completed') {
              if (safetyTimer) { untrackTimer(safetyTimer); safetyTimer = null }
              untrackSource(es)
              installs.finishInstall(installId)
              installs.pushToast('success', `${name} installed successfully`)
              useContainersStore().refresh(true)
              // Refresh the catalog to update installed status
              refresh()
            } else if (msg.status === 'failed') {
              if (safetyTimer) { untrackTimer(safetyTimer); safetyTimer = null }
              untrackSource(es)
              installs.finishInstall(installId)
              installs.pushToast('error', msg.log_line || `${name} install failed`)
            }
          },
          () => {
            // SSE died (auth expired, backend restart, etc). EventSource
            // errors are async so the outer try/catch never sees them.
            // If the connection is now CLOSED, tear down the in-flight
            // state and schedule a final refresh. A transient blip with
            // readyState=CONNECTING is left alone so the browser's
            // built-in reconnect gets a chance first.
            if (es.readyState !== EventSource.CLOSED) return
            if (safetyTimer) { untrackTimer(safetyTimer); safetyTimer = null }
            untrackSource(es)
            installs.finishInstall(installId)
            const t = setTimeout(() => {
              untrackTimer(t)
              useContainersStore().refresh(true)
              refresh()
            }, 2000)
            trackTimer(t)
          },
        )
        trackSource(es)
      } catch {
        // subscribeProgress threw synchronously (EventSource constructor
        // rejected the URL, etc). Fall back to timer-based refresh.
        const t = setTimeout(() => {
          untrackTimer(t)
          installs.finishInstall(installId)
          useContainersStore().refresh(true)
          refresh()
        }, 10000)
        trackTimer(t)
      }
    } else {
      // No install ID — refresh after a delay
      const t = setTimeout(() => {
        untrackTimer(t)
        useContainersStore().refresh(true)
        refresh()
      }, 6000)
      trackTimer(t)
    }

    return true
  }

  /**
   * getComposeYaml — fetches the raw YAML for "Custom Install" flow.
   * Returns the YAML string to pre-fill into InstallAppDialog.
   */
  async function getComposeYaml(appId: string): Promise<string | null> {
    const result = await safeCall(
      () => libraryApi.getAppComposeYaml(appId),
    )
    if (result.error) return null
    return result.data || null
  }

  function setSearch(q: string) { searchQuery.value = q }
  function setCategory(c: string | null) { selectedCategory.value = c }

  function openPanel() { panelOpen.value = true }
  function closePanel() { panelOpen.value = false }
  function togglePanel() { panelOpen.value = !panelOpen.value }

  return {
    panelOpen,
    catalog,
    installedIds,
    categories,
    upgradable,
    stores,
    loading,
    fixtureMode,
    lastError,
    searchQuery,
    selectedCategory,
    catalogList,
    categoryList,
    totalAppCount,
    recommendedApps,
    installedSet,
    upgradableSet,
    getAppStatus,
    refresh,
    refreshStores,
    addStore,
    removeStore,
    toggleStore,
    reprioritize,
    sync,
    installApp,
    getComposeYaml,
    setSearch,
    setCategory,
    openPanel,
    closePanel,
    togglePanel,
  }
})
