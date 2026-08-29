// useAppOrder — persistent custom ordering + folder grouping for the
// dashboard app grid.
//
// Wired to the containers Pinia store which fetches real data from the
// CasaNova-AppManagement backend. The store's AppEntry[] is mapped to
// the AppFixture shape the grid components expect. User drag-order and
// folder groups are saved in localStorage so layouts survive reloads.
//
// Data model:
//   orderIds:   string[]       — flat ordered list of IDs (app or group)
//   groups:     AppGroup[]     — folder definitions (id, name, color, collapsed)
//   membership: Record<groupId, appId[]> — which apps belong to each group
//
// The grid renders items in orderIds sequence. When an ID matches a
// group, that group's member apps are rendered inline (expanded) or as
// a single folder tile (collapsed). Apps not in any group appear as
// standalone tiles at their orderIds position.

import { ref, computed, watch } from 'vue'
import { useContainersStore } from '@/stores/containers'
import { useDevMode } from './useDevMode'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'
import { getUtilization } from '@/api/system'
import type { AppFixture, AppGroup } from '@/types/dashboard'
import { aggregateAppState } from '@/types/containers'

const ORDER_KEY = 'casanova.apps.order'
const GROUPS_KEY = 'casanova.apps.groups'
const MEMBERSHIP_KEY = 'casanova.apps.membership'

// ── App web-UI host resolution ──────────────────────────────────────
// App tiles open http://<host>:<port>. When CasaNova is loaded via a private
// LAN IP (or localhost) the apps are reachable at that same host. But when
// it's loaded via a DOMAIN (a reverse proxy / cloudflared tunnel that only
// forwards the CasaNova port), raw app ports are NOT reachable through it — a
// tile would open <domain>:<appPort> and the proxy 404s ("not found"). In that
// case we point the tile at the box's own LAN IP instead, fetched once from
// the utilization endpoint, so apps open for anyone on the same LAN.
const boxLanIp = ref<string | null>(null)

function isPrivateHost(h: string): boolean {
  return (
    h === 'localhost' ||
    /^127\./.test(h) ||
    /^10\./.test(h) ||
    /^192\.168\./.test(h) ||
    /^172\.(1[6-9]|2\d|3[01])\./.test(h)
  )
}

let lanIpResolving = false
export async function resolveBoxLanIp(): Promise<void> {
  if (boxLanIp.value || lanIpResolving) return
  lanIpResolving = true
  try {
    const u = await getUtilization()
    const nic = u.net.find((n) => n.ipv4 && isPrivateHost(n.ipv4))
    if (nic?.ipv4) boxLanIp.value = nic.ipv4
  } catch {
    /* leave null — tiles fall back to the browser host */
  } finally {
    lanIpResolving = false
  }
}

// The host a tile should use to reach its app's raw port.
export function appHost(): string {
  const h = window.location.hostname
  if (isPrivateHost(h)) return h
  return boxLanIp.value || h
}

// ── syncedPrefs helpers (Wave 29) ───────────────────────────────────
// Reads/writes go through the central per-user pref cache so the
// dashboard tile order, folder definitions, and folder membership
// follow the user across browsers and devices instead of being
// locked to one localStorage origin.

function loadJson<T>(key: string, fallback: T): T {
  try {
    const raw = getPref(key)
    if (!raw) return fallback
    return JSON.parse(raw) as T
  } catch {
    return fallback
  }
}

function saveJson(key: string, value: unknown) {
  try {
    setPref(key, JSON.stringify(value))
  } catch {
    /* sandboxed — ignore */
  }
}

// ── Persistent state (module-level so all consumers share one copy) ─

const orderIds = ref<string[]>(loadJson<string[]>(ORDER_KEY, []))
const groups = ref<AppGroup[]>(loadJson<AppGroup[]>(GROUPS_KEY, []))
const membership = ref<Record<string, string[]>>(
  loadJson<Record<string, string[]>>(MEMBERSHIP_KEY, {}),
)

// Suppress one persist tick after a backend hydrate so we don't echo
// freshly downloaded values straight back to the server.
let suppressNextPersist = false

watch(
  orderIds,
  (v) => {
    if (suppressNextPersist) return
    saveJson(ORDER_KEY, v)
  },
  { deep: true },
)
watch(
  groups,
  (v) => {
    if (suppressNextPersist) return
    saveJson(GROUPS_KEY, v)
  },
  { deep: true },
)
watch(
  membership,
  (v) => {
    if (suppressNextPersist) return
    saveJson(MEMBERSHIP_KEY, v)
  },
  { deep: true },
)

// Wave 29: re-read all three pieces of state when syncedPrefs.hydrate
// finishes so a fresh login on a new device picks up the user's saved
// dashboard layout. The suppress flag covers the resulting writes so
// they don't echo back to the backend.
const gen = prefsGeneration()
watch(gen, () => {
  suppressNextPersist = true
  orderIds.value = loadJson<string[]>(ORDER_KEY, [])
  groups.value = loadJson<AppGroup[]>(GROUPS_KEY, [])
  membership.value = loadJson<Record<string, string[]>>(MEMBERSHIP_KEY, {})
  queueMicrotask(() => {
    suppressNextPersist = false
  })
})

// ── System tiles (dev mode only) ───────────────────────────────────

const VM_SYSTEM_TILE: AppFixture = {
  id: 'vms',
  name: 'Virtual Machines',
  initial: 'V',
  tileColor: '#475569',
  status: 'stopped',
  internalRoute: '/app/vms',
}
const SYSTEM_TILE_IDS = new Set(['vms'])

// ── Helpers ─────────────────────────────────────────────────────────

function mapStatus(state: string): AppFixture['status'] {
  switch (state) {
    case 'running': return 'running'
    case 'stopped': case 'exited': case 'created': return 'stopped'
    case 'unhealthy': return 'unhealthy'
    case 'updating': return 'updating'
    default: return 'stopped'
  }
}

function hashColor(name: string): string {
  const colors = [
    '#7b4397', '#4a5bf7', '#0082c9', '#18bcf2', '#609926',
    '#f60d1a', '#e67e22', '#1abc9c', '#9b59b6', '#2c3e50',
  ]
  let hash = 0
  for (let i = 0; i < name.length; i++) hash = (hash * 31 + name.charCodeAt(i)) | 0
  return colors[Math.abs(hash) % colors.length]
}

let nextGroupId = Date.now()
function genGroupId(): string {
  return `group-${nextGroupId++}`
}

// ── Composable ──────────────────────────────────────────────────────

export function useAppOrder() {
  const { devMode } = useDevMode()
  const store = useContainersStore()

  // Trigger a backend refresh on first use (idempotent).
  store.refresh()
  // Resolve the box's LAN IP once so app tiles opened while browsing via a
  // domain/tunnel still point at a reachable host (see boxLanIp above).
  resolveBoxLanIp()

  // Convert the unified app list to AppFixture shape for the grid. ONE
  // source of truth (store.unifiedApps) so every workload — compose apps,
  // standalone containers, and external compose stacks — appears in the
  // same grid as a first-class tile, never a separate "external" bucket.
  const allApps = computed<AppFixture[]>(() => {
    const host = appHost()
    return store.unifiedApps.map((app) => {
      const mainService = app.services?.[0]
      const port = mainService?.ports?.[0]?.host
      const webPort = app.webUIPort || port
      const url = webPort ? `${app.webUIProtocol || 'http'}://${host}:${webPort}` : undefined

      // Determine if the app belongs to a group.
      let groupId: string | undefined
      for (const [gId, members] of Object.entries(membership.value)) {
        if (members.includes(app.id)) {
          groupId = gId
          break
        }
      }

      return {
        id: app.id,
        name: app.title || app.id,
        initial: (app.title || app.id).charAt(0).toUpperCase(),
        iconUrl: app.iconUrl || undefined,
        tileColor: app.tileColor || hashColor(app.id),
        status: mapStatus(aggregateAppState(app)),
        url,
        groupId,
        source: app.source,
      }
    })
  })

  // ── Build the ordered grid structure ────────────────────────────

  /** A standalone app or a group header in the flat rendering order. */
  type GridItem =
    | { kind: 'app'; app: AppFixture }
    | { kind: 'group'; group: AppGroup; apps: AppFixture[] }

  const groupMap = computed(() => new Map(groups.value.map((g) => [g.id, g])))

  const gridItems = computed<GridItem[]>(() => {
    const appById = new Map(
      allApps.value.filter((a) => !SYSTEM_TILE_IDS.has(a.id)).map((a) => [a.id, a]),
    )

    // Collect grouped app IDs for quick lookup.
    const groupedAppIds = new Set<string>()
    for (const members of Object.values(membership.value)) {
      for (const id of members) groupedAppIds.add(id)
    }

    const seen = new Set<string>()
    const items: GridItem[] = []

    // Walk saved order first.
    for (const id of orderIds.value) {
      if (seen.has(id)) continue
      seen.add(id)

      const group = groupMap.value.get(id)
      if (group) {
        const memberIds = membership.value[id] ?? []
        const memberApps = memberIds
          .map((mid) => appById.get(mid))
          .filter((a): a is AppFixture => !!a)
        items.push({ kind: 'group', group, apps: memberApps })
        for (const m of memberApps) seen.add(m.id)
        continue
      }

      const app = appById.get(id)
      if (app && !groupedAppIds.has(id)) {
        items.push({ kind: 'app', app })
      }
    }

    // Append newly discovered ungrouped apps.
    for (const app of appById.values()) {
      if (!seen.has(app.id) && !groupedAppIds.has(app.id)) {
        items.push({ kind: 'app', app })
      }
    }

    // Prepend system tiles in dev mode.
    if (devMode.value) {
      items.unshift({ kind: 'app', app: VM_SYSTEM_TILE })
    }

    return items
  })

  // Legacy flat list — still used by the "N installed" counter and the
  // InstallAppDialog empty-state check.
  const orderedApps = computed<AppFixture[]>(() => {
    const result: AppFixture[] = []
    for (const item of gridItems.value) {
      if (item.kind === 'app') result.push(item.app)
      else result.push(...item.apps)
    }
    return result
  })

  // ── Mutations ───────────────────────────────────────────────────

  /** Reorder a top-level item (app or group) by dragging onto another. */
  function moveApp(sourceId: string, targetId: string) {
    if (sourceId === targetId) return
    const current = _topLevelIds()
    const from = current.indexOf(sourceId)
    const to = current.indexOf(targetId)
    if (from === -1 || to === -1) return
    const [moved] = current.splice(from, 1)
    current.splice(to, 0, moved)
    orderIds.value = current
  }

  function _topLevelIds(): string[] {
    return gridItems.value.map((item) =>
      item.kind === 'app' ? item.app.id : item.group.id,
    )
  }

  function resetOrder() {
    orderIds.value = []
    groups.value = []
    membership.value = {}
  }

  // ── Group CRUD ──────────────────────────────────────────────────

  /** Create a new empty folder and insert it at the end of the grid. */
  function createGroup(name: string, color?: string): string {
    const id = genGroupId()
    groups.value = [...groups.value, { id, name, color: color ?? '#475569', collapsed: true }]
    membership.value = { ...membership.value, [id]: [] }
    orderIds.value = [...orderIds.value, id]
    return id
  }

  /** Rename an existing folder. */
  function renameGroup(id: string, name: string) {
    groups.value = groups.value.map((g) => (g.id === id ? { ...g, name } : g))
  }

  /** Change the accent color of a folder. */
  function setGroupColor(id: string, color: string) {
    groups.value = groups.value.map((g) => (g.id === id ? { ...g, color } : g))
  }

  /** Toggle collapsed / expanded state. */
  function toggleGroup(id: string) {
    groups.value = groups.value.map((g) =>
      g.id === id ? { ...g, collapsed: !g.collapsed } : g,
    )
  }

  /** Delete a folder. Member apps become ungrouped (inserted at the
   *  folder's position in the grid so order is preserved). */
  function deleteGroup(id: string) {
    const memberIds = membership.value[id] ?? []
    // Insert member apps at the folder's position in orderIds.
    const idx = orderIds.value.indexOf(id)
    const newOrder = [...orderIds.value]
    if (idx !== -1) {
      newOrder.splice(idx, 1, ...memberIds)
    } else {
      newOrder.push(...memberIds)
    }
    orderIds.value = newOrder

    groups.value = groups.value.filter((g) => g.id !== id)
    const next = { ...membership.value }
    delete next[id]
    membership.value = next
  }

  /** Move an app into a folder. Removes from previous group if any. */
  function addToGroup(appId: string, groupId: string) {
    const next = { ...membership.value }
    // Remove from current group (if any).
    for (const [gId, members] of Object.entries(next)) {
      if (members.includes(appId)) {
        next[gId] = members.filter((m) => m !== appId)
      }
    }
    // Add to target group.
    next[groupId] = [...(next[groupId] ?? []), appId]
    membership.value = next

    // Remove the app from top-level orderIds (it's now inside a group).
    orderIds.value = orderIds.value.filter((id) => id !== appId)
  }

  /** Remove an app from its folder (make it ungrouped). Places it
   *  right after the group in the top-level order. */
  function removeFromGroup(appId: string) {
    let groupId: string | null = null
    const next = { ...membership.value }
    for (const [gId, members] of Object.entries(next)) {
      if (members.includes(appId)) {
        groupId = gId
        next[gId] = members.filter((m) => m !== appId)
        break
      }
    }
    membership.value = next

    if (groupId) {
      const idx = orderIds.value.indexOf(groupId)
      const newOrder = [...orderIds.value]
      if (idx !== -1) {
        newOrder.splice(idx + 1, 0, appId)
      } else {
        newOrder.push(appId)
      }
      orderIds.value = newOrder
    }
  }

  /** Reorder apps within a group. */
  function moveInGroup(groupId: string, sourceAppId: string, targetAppId: string) {
    if (sourceAppId === targetAppId) return
    const members = [...(membership.value[groupId] ?? [])]
    const from = members.indexOf(sourceAppId)
    const to = members.indexOf(targetAppId)
    if (from === -1 || to === -1) return
    const [moved] = members.splice(from, 1)
    members.splice(to, 0, moved)
    membership.value = { ...membership.value, [groupId]: members }
  }

  /** Create a new folder from two apps (drag one onto another). Both
   *  apps are removed from the top-level and placed in the new group.
   *  The folder takes the position of the target app. */
  function createGroupFromApps(appId1: string, appId2: string, name?: string): string {
    const id = genGroupId()
    const groupName = name ?? 'New Folder'
    groups.value = [...groups.value, { id, name: groupName, color: '#475569', collapsed: true }]
    membership.value = { ...membership.value, [id]: [appId1, appId2] }

    // Replace the target app's position with the new group; remove the
    // source app from the order. If neither is in the order yet, append.
    const newOrder = [...orderIds.value].filter((oid) => oid !== appId1 && oid !== appId2)
    const targetIdx = orderIds.value.indexOf(appId2)
    if (targetIdx !== -1) {
      // Find where target was among the items that survived the filter.
      // Since we removed both, we need to find the right spot.
      const before = orderIds.value.slice(0, targetIdx).filter((oid) => oid !== appId1)
      newOrder.splice(0, newOrder.length, ...before, id, ...newOrder.slice(before.length))
    } else {
      newOrder.push(id)
    }
    orderIds.value = newOrder
    return id
  }

  return {
    // Data
    orderedApps,
    gridItems,
    groups: computed(() => groups.value),

    // Top-level reorder
    moveApp,
    resetOrder,

    // Group CRUD
    createGroup,
    renameGroup,
    setGroupColor,
    toggleGroup,
    deleteGroup,

    // Group membership
    addToGroup,
    removeFromGroup,
    moveInGroup,
    createGroupFromApps,
  }
}
