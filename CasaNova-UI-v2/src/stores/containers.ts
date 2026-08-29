// Containers store — single source of truth for the compose apps
// the dashboard grid, Containers settings page, and app detail view
// all consume. Wraps containersApi calls via safeCall for error
// handling. Components bind to the store and get live data from the
// backend.
//
// Mutations (install/uninstall/start/stop/restart/updateApp) hit the
// real backend. The install flow still funnels through
// installProgressStore for the progress card + toast, but the
// initiation now goes through containersApi.installManual / import.
//
// Stores like this should be created once per session and refreshed
// from a few anchor points: dashboard mount, Containers settings
// mount, app detail mount. HMR safety via Pinia's singleton.

import { defineStore } from 'pinia'
import { ref, computed, onScopeDispose } from 'vue'
import { containersApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { AppEntry, ContainerState, PortProtocol, RestartPolicy } from '@/types/containers'
import type { AppEntryApi, ComposeStack, ExternalContainer } from '@/api/containers'

// Map the API response to the internal AppEntry shape.
function apiToAppEntry(api: AppEntryApi): AppEntry {
  return {
    id: api.id,
    title: api.title,
    description: '',
    iconUrl: api.iconUrl || '',
    tileColor: '',
    autoUpdate: api.autoUpdate,
    webUIProtocol: api.webUIProtocol,
    webUIHost: api.webUIHost,
    webUIPort: api.webUIPort,
    webUIPath: api.webUIPath,
    services: api.services.map((s) => ({
      id: s.id,
      dockerImage: s.dockerImage,
      imageTag: s.imageTag,
      availableTags: s.availableTags,
      state: s.state as ContainerState,
      uptimeSeconds: s.uptimeSeconds,
      cpuPercent: s.cpuPercent,
      cpuLimitCores: s.cpuLimitCores,
      memoryMB: s.memoryMB,
      memoryLimitMB: s.memoryLimitMB,
      restartCount: s.restartCount,
      ports: s.ports.map((p) => ({ ...p, protocol: p.protocol as import('@/types/containers').PortProtocol })),
      volumes: s.volumes,
      env: s.env,
      devices: s.devices,
      command: s.command,
      privileged: s.privileged,
      restartPolicy: s.restartPolicy as import('@/types/containers').RestartPolicy,
      networks: s.networks,
    })),
    managed: true,
    source: 'compose',
  }
}

// Best-effort icon for an unmanaged container, derived from its image name
// via the same self-hosted icon CDN the dashboard already uses, so
// standalone/external apps get a real tile icon instead of a bare initial.
const EXTERNAL_ICON_CDN = 'https://cdn.jsdelivr.net/gh/selfhst/icons/png'
function iconForImage(image: string): string {
  const name = (image.split('/').pop() || '').split(':')[0]
  return name ? `${EXTERNAL_ICON_CDN}/${name}.png` : ''
}

// Map one unmanaged Docker container to a ServiceEntry. The summary
// container list doesn't expose env/volumes, so those are left empty —
// the detail view renders unmanaged apps read-only.
function externalServiceEntry(c: ExternalContainer): AppEntry['services'][number] {
  const colon = c.image.lastIndexOf(':')
  const imageName = colon > 0 ? c.image.slice(0, colon) : c.image
  const imageTag = colon > 0 ? c.image.slice(colon + 1) : 'latest'
  return {
    id: c.name,
    dockerImage: imageName,
    imageTag,
    availableTags: [],
    state: c.state === 'running' ? 'running' : 'stopped',
    uptimeSeconds: 0,
    cpuPercent: 0,
    cpuLimitCores: null,
    memoryMB: 0,
    memoryLimitMB: null,
    restartCount: 0,
    ports: c.ports.map((p) => ({
      host: p.publicPort,
      container: p.privatePort,
      protocol: (p.type === 'udp' ? 'udp' : 'tcp') as PortProtocol,
    })),
    volumes: [],
    env: [],
    devices: [],
    command: '',
    privileged: false,
    restartPolicy: 'no' as RestartPolicy,
    networks: [],
  }
}

// A standalone (unmanaged) container → a single-service read-only app, so
// it resolves and opens like any other app instead of "App not found".
function externalToAppEntry(c: ExternalContainer): AppEntry {
  const pub = c.ports.find((p) => p.publicPort > 0)
  return {
    id: c.name,
    title: c.name,
    description: '',
    iconUrl: iconForImage(c.image),
    tileColor: '',
    autoUpdate: false,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: pub ? pub.publicPort : null,
    webUIPath: '',
    managed: false,
    source: 'standalone',
    services: [externalServiceEntry(c)],
  }
}

// An external (unmanaged) compose stack → ONE multi-service read-only app,
// mirroring how a managed compose project is a single tile with per-service
// tabs. id === stack name so its detail route is stable.
function stackToAppEntry(stack: ComposeStack): AppEntry {
  const first = stack.containers[0]
  const pub = stack.containers.flatMap((c) => c.ports).find((p) => p.publicPort > 0)
  return {
    id: stack.name,
    title: stack.name,
    description: '',
    iconUrl: first ? iconForImage(first.image) : '',
    tileColor: '',
    autoUpdate: false,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: pub ? pub.publicPort : null,
    webUIPath: '',
    managed: false,
    source: 'external-stack',
    services: stack.containers.map(externalServiceEntry),
  }
}

export const useContainersStore = defineStore('containers', () => {
  // Start empty — real data loads from the backend on first refresh().
  const apps = ref<AppEntry[]>([])
  const loading = ref(false)
  const fixtureMode = ref(false)
  const lastError = ref<string | null>(null)
  const lastRefresh = ref<number>(0)

  // External (unmanaged) containers discovered from Docker
  const externalStacks = ref<ComposeStack[]>([])
  const standaloneContainers = ref<ExternalContainer[]>([])
  const totalContainers = ref(0)

  // 10 second cache window — short enough that navigating away and back
  // (dashboard → settings → dashboard) still feels live, long enough
  // that rapid component re-mounts during the same page view don't
  // spam the backend. Real-time updates come through MessageBus in
  // production; this cache is a fallback for the poll-based dev scaffold.
  const CACHE_MS = 10_000
  let refreshInFlight = false

  // Wave 33: session epoch guards every async refresh path against
  // late resolutions overwriting cleared state. _onAuthLogout bumps
  // it; refresh() captures the value before each await and bails if
  // the epoch has moved on. Without this, an in-flight listApps()
  // that started under user A could resolve into apps.value AFTER
  // user A has logged out and user B has logged in, repopulating
  // the dashboard with A's compose apps and container internals
  // until B's first refresh lands.
  let sessionEpoch = 0

  // A forced refresh means "something just changed, show me the truth" — an
  // install finished, an app was started. Dropping it because a routine poll
  // happened to be in flight left the just-installed app missing from the grid
  // until the next cache window expired. Remember it and re-run once instead.
  let pendingForce = false

  async function refresh(force = false): Promise<void> {
    if (refreshInFlight) {
      if (force) pendingForce = true
      return
    }
    if (!force && Date.now() - lastRefresh.value < CACHE_MS) return
    refreshInFlight = true
    loading.value = true
    const startedEpoch = sessionEpoch
    try {
      // Fire stats request in the background — don't block the app
      // list from rendering. The grid shows tiles immediately from the
      // compose list; CPU/RAM numbers merge in when the stats arrive.
      const statsPromise = safeCall(
        () => containersApi.getContainerStats(),
      )

      // Await the app list first — this is what the grid needs to render tiles.
      const result = await safeCall(
        () => containersApi.listApps(),
      )
      // Bail if a logout happened mid-flight: the new session must
      // not inherit the previous user's app list.
      if (startedEpoch !== sessionEpoch) return
      if (result.error) {
        lastError.value = result.error
        return
      }
      if (!result.data) return
      lastError.value = null
      fixtureMode.value = false
      apps.value = (result.data as unknown as AppEntryApi[]).map(apiToAppEntry)

      // Grid now has tiles — clear loading so the user sees them immediately.
      loading.value = false
      lastRefresh.value = Date.now()

      // Fetch external (unmanaged) containers in the background
      refreshAll()

      // Merge stats when they arrive (already in flight from above).
      {
        const statsResult = await statsPromise
        if (startedEpoch !== sessionEpoch) return
        const stats = statsResult.data
        if (stats && stats.length > 0) {
          const byName = new Map(stats.map((s: import('@/api/containers').ContainerStats) => [s.name, s]))
          apps.value = apps.value.map((app) => ({
            ...app,
            services: app.services.map((svc) => {
              const stat = byName.get(svc.id) || byName.get(app.id)
              return stat
                ? { ...svc, cpuPercent: stat.cpuPercent, memoryMB: stat.memoryMB }
                : svc
            }),
          }))
        }
      }
    } finally {
      refreshInFlight = false
      loading.value = false
      if (pendingForce) {
        pendingForce = false
        // Not after a logout — the new session must not inherit this work.
        if (startedEpoch === sessionEpoch) void refresh(true)
      }
    }
  }

  // Fetch all Docker containers and separate unmanaged ones
  async function refreshAll(): Promise<void> {
    const result = await safeCall(() => containersApi.getAllContainers())
    if (result.error || !result.data) return
    // Filter out managed stacks (already shown in main apps grid)
    externalStacks.value = (result.data.stacks ?? []).filter(s => !s.managed)
    standaloneContainers.value = (result.data.standalone ?? []).filter(c => !c.managed)
    totalContainers.value = result.data.total ?? 0
  }

  // Lifecycle helpers for external (unmanaged) containers
  async function startExternalContainer(id: string): Promise<boolean> {
    try {
      await containersApi.startContainer(id)
      await refreshAll()
      return true
    } catch { return false }
  }

  async function stopExternalContainer(id: string): Promise<boolean> {
    try {
      await containersApi.stopContainer(id)
      await refreshAll()
      return true
    } catch { return false }
  }

  async function restartExternalContainer(id: string): Promise<boolean> {
    try {
      await containersApi.restartContainer(id)
      await refreshAll()
      return true
    } catch { return false }
  }

  // Update a standalone container: re-pull its image and recreate it,
  // preserving config. The backend runs this asynchronously, so we refresh
  // shortly after to pick up the new container state.
  async function recreateExternalContainer(id: string, pull = true): Promise<boolean> {
    try {
      await containersApi.recreateContainer(id, pull)
      return true
    } catch { return false }
  }

  // ── Source-aware lifecycle dispatch ─────────────────────────────────
  // One entry point per action so every surface (dashboard card, detail
  // page) gets the right endpoint for the app's source without each
  // component re-implementing the branch:
  //   - compose:        compose lifecycle (start/stop/restartApp)
  //   - standalone:     single external container
  //   - external-stack: every container in the stack
  async function dispatchLifecycle(
    id: string,
    compose: (id: string) => Promise<boolean>,
    external: (id: string) => Promise<boolean>,
  ): Promise<boolean> {
    const app = findApp(id)
    const source = app?.source ?? 'compose'
    if (source === 'compose') return compose(id)
    if (source === 'standalone') return external(id)
    // external-stack: act on each container; success only if all succeed.
    const stack = externalStacks.value.find((s) => s.name === id)
    if (!stack || stack.containers.length === 0) return false
    const results = await Promise.all(stack.containers.map((c) => external(c.name)))
    return results.every(Boolean)
  }

  const startAppUnified = (id: string) => dispatchLifecycle(id, startApp, startExternalContainer)
  const stopAppUnified = (id: string) => dispatchLifecycle(id, stopApp, stopExternalContainer)
  const restartAppUnified = (id: string) => dispatchLifecycle(id, restartApp, restartExternalContainer)

  // ── Unified app model ───────────────────────────────────────────────
  // The single list every surface resolves against: managed compose apps +
  // standalone containers + external compose stacks, each tagged with
  // `source`. The dashboard grid (useAppOrder) and the detail-page resolver
  // (findApp) both derive from this, so a card that is visible can never
  // fail to open. Ids are deduped (a compose app wins over a same-named
  // container) so resolution is unambiguous.
  const unifiedApps = computed<AppEntry[]>(() => {
    const out: AppEntry[] = [...apps.value]
    const seen = new Set(out.map((a) => a.id))
    for (const c of standaloneContainers.value) {
      if (seen.has(c.name)) continue
      seen.add(c.name)
      out.push(externalToAppEntry(c))
    }
    for (const stack of externalStacks.value) {
      if (seen.has(stack.name)) continue
      seen.add(stack.name)
      out.push(stackToAppEntry(stack))
    }
    return out
  })

  function findApp(id: string): AppEntry | undefined {
    return unifiedApps.value.find((a) => a.id === id)
  }

  // ----- Lifecycle -----
  // Each lifecycle call hits the real API when up, no-ops locally
  // otherwise. After a successful mutation we refresh the list so
  // derived state (state badges, uptime) reflects the new reality.

  async function startApp(id: string): Promise<boolean> {
    if (fixtureMode.value) {
      mutateState(id, 'running')
      return true
    }
    const result = await safeCall(
      () => containersApi.startApp(id),
    )
    if (result.error) return false
    await refresh(true)
    return true
  }

  async function stopApp(id: string): Promise<boolean> {
    if (fixtureMode.value) {
      mutateState(id, 'stopped')
      return true
    }
    const result = await safeCall(
      () => containersApi.stopApp(id),
    )
    if (result.error) return false
    await refresh(true)
    return true
  }

  async function restartApp(id: string): Promise<boolean> {
    if (fixtureMode.value) {
      mutateState(id, 'running')
      return true
    }
    const result = await safeCall(
      () => containersApi.restartApp(id),
    )
    if (result.error) return false
    await refresh(true)
    return true
  }

  async function repairApp(id: string): Promise<boolean> {
    if (fixtureMode.value) {
      mutateState(id, 'running')
      return true
    }
    const result = await safeCall(
      () => containersApi.repairApp(id),
    )
    if (result.error) return false
    // Recreate takes a beat (pull/create/start); give it a moment then refresh.
    await new Promise((r) => setTimeout(r, 2000))
    await refresh(true)
    return true
  }

  async function uninstallApp(id: string): Promise<boolean> {
    if (fixtureMode.value) {
      apps.value = apps.value.filter((a) => a.id !== id)
      return true
    }
    const result = await safeCall(
      () => containersApi.uninstallApp(id),
    )
    if (result.error) return false
    apps.value = apps.value.filter((a) => a.id !== id)
    return true
  }

  // Local helper for fixture mode: flip every service's state in
  // the given app so the UI reflects the "virtual" lifecycle change.
  function mutateState(id: string, state: ContainerState) {
    apps.value = apps.value.map((a) => {
      if (a.id !== id) return a
      return {
        ...a,
        services: a.services.map((s) => ({ ...s, state })),
      }
    })
  }

  // ----- Live container stats (CPU/RAM) -----
  // Fetches from GET /v1/container/usage and merges into service entries.
  async function refreshStats(): Promise<void> {
    try {
      const stats = await containersApi.getContainerStats()
      if (!stats || stats.length === 0) return
      const byName = new Map(stats.map((s) => [s.name, s]))
      apps.value = apps.value.map((app) => ({
        ...app,
        services: app.services.map((svc) => {
          const stat = byName.get(svc.id) || byName.get(app.id)
          return stat
            ? { ...svc, cpuPercent: stat.cpuPercent, memoryMB: stat.memoryMB }
            : svc
        }),
      }))
    } catch { /* stats endpoint unavailable */ }
  }

  // ----- Totals for the "Resource usage by app" card -----
  const runningApps = computed(() =>
    apps.value.filter((a) =>
      a.services.some((s) => s.state === 'running' || s.state === 'unhealthy'),
    ),
  )
  const totalCpuPercent = computed(() =>
    runningApps.value.reduce(
      (sum, a) => sum + a.services.reduce((s, svc) => s + svc.cpuPercent, 0),
      0,
    ),
  )
  const totalMemoryMB = computed(() =>
    runningApps.value.reduce(
      (sum, a) => sum + a.services.reduce((s, svc) => s + svc.memoryMB, 0),
      0,
    ),
  )

  // Wave 33: clear every session-bound ref when the user logs out so
  // the next user logging in on the same browser doesn't see the
  // previous occupant's compose apps, unmanaged containers, or
  // dashboard CPU/RAM totals during the brief window before their
  // first refresh lands. Also bumps sessionEpoch so any in-flight
  // refresh() that hasn't yet hit its first await bails before
  // assigning to apps.value.
  function _onAuthLogout() {
    sessionEpoch++
    pendingForce = false
    apps.value = []
    externalStacks.value = []
    standaloneContainers.value = []
    totalContainers.value = 0
    lastError.value = null
    lastRefresh.value = 0
    loading.value = false
    fixtureMode.value = false
  }
  window.addEventListener('auth-logout', _onAuthLogout)
  onScopeDispose(() => {
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  return {
    apps,
    unifiedApps,
    loading,
    fixtureMode,
    lastError,
    runningApps,
    totalCpuPercent,
    totalMemoryMB,
    externalStacks,
    standaloneContainers,
    totalContainers,
    refresh,
    refreshAll,
    refreshStats,
    findApp,
    startApp,
    stopApp,
    restartApp,
    uninstallApp,
    repairApp,
    startExternalContainer,
    stopExternalContainer,
    restartExternalContainer,
    recreateExternalContainer,
    startAppUnified,
    stopAppUnified,
    restartAppUnified,
  }
})
