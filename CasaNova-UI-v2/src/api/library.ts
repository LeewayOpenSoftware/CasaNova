// App Library API — wraps the catalog + store management endpoints.
//
// The library is the app discovery surface — browsing, searching,
// filtering by category, and one-click install from a curated catalog.
// Store management (adding/removing sources) lives alongside the
// catalog since both hit /v2/app_management/*.
//
// IMPORTANT: Endpoint paths match the OpenAPI codegen routes in
// CasaNova-AppManagement/route/v2/appstore.go:
//   GET  /v2/app_management/apps                  → ComposeAppStoreInfoList
//   GET  /v2/app_management/apps/:id              → ComposeAppStoreInfo
//   GET  /v2/app_management/apps/:id/compose      → ComposeApp (YAML)
//   GET  /v2/app_management/categories            → CategoryList
//   GET  /v2/app_management/apps/upgradable       → UpgradableAppList
//   POST /v2/app_management/compose               → InstallComposeApp
//   GET  /v2/app_management/stores                → StoreList (custom route)

import client from './client'

// ---- Catalog types ----

export interface CatalogApp {
  // The backend returns ComposeAppStoreInfo via codegen — these
  // fields map to its JSON representation.
  name: string
  title: Record<string, string>
  tagline: Record<string, string>
  description: Record<string, string>
  icon: string
  thumbnail?: string
  screenshot_link?: string[]
  category: string
  author: string
  developer: string
  architectures: string[]
  main?: string
  port_map?: string
  index?: string
  store_app_id?: string
  version?: string
  min_memory?: number
  tips?: { before_install?: Record<string, string> }
}

export interface CategoryInfo {
  id?: number
  name: string
  font: string
  description: string
  count?: number
}

export interface UpgradableApp {
  title: string
  version: string
  store_app_id?: string
  status: string
  icon?: string
}

export interface StoreEntry {
  id: string
  name: string
  type: 'local' | 'remote'
  url?: string
  path: string
  enabled: boolean
  priority: number
  sync_interval_hours?: number
  last_synced: string | null
}

// ---- Catalog endpoints ----

/**
 * getCatalog — fetches the full merged catalog from all enabled stores.
 * Returns a map keyed by store_app_id → ComposeAppStoreInfo, plus a
 * list of installed app IDs so the UI can badge them.
 *
 * v0.1: the backend's ComposeAppStoreInfo schema has no `name` field
 * — the map key IS the name. The previous client trusted the type
 * declaration and left `name` undefined, which caused the panel to
 * empty out the moment the user typed in the search box (the
 * `app.name.includes(q)` filter threw on every row). Populate `name`
 * AND `store_app_id` from the key here so every downstream consumer
 * sees a usable identifier on each app object.
 */
export async function getCatalog(params?: {
  category?: string
  author_type?: string
}): Promise<{ list: Record<string, CatalogApp>; installed: string[] }> {
  const res = await client.get<{
    data: {
      list?: Record<string, CatalogApp>
      installed?: string[]
    }
  }>('/v2/app_management/apps', { params })
  const rawList = res.data.data?.list ?? {}
  const list: Record<string, CatalogApp> = {}
  for (const [key, app] of Object.entries(rawList)) {
    list[key] = {
      ...app,
      name: app.name ?? key,
      store_app_id: app.store_app_id ?? key,
    }
  }
  return {
    list,
    installed: res.data.data?.installed ?? [],
  }
}

/**
 * getCategories — category sidebar data. Backend prepends an "All"
 * entry with the total count.
 */
export async function getCategories(): Promise<CategoryInfo[]> {
  const res = await client.get<{ data: CategoryInfo[] }>(
    '/v2/app_management/categories',
  )
  return res.data.data ?? []
}

/**
 * getAppDetail — full detail for a single catalog app by store_app_id.
 */
export async function getAppDetail(appId: string): Promise<CatalogApp | null> {
  const res = await client.get<{ data?: CatalogApp }>(
    `/v2/app_management/apps/${encodeURIComponent(appId)}`,
  )
  return res.data.data ?? null
}

/**
 * getAppComposeYaml — fetches the raw docker-compose.yml for a catalog
 * app. The backend's ComposeApp handler returns YAML when the Accept
 * header is set to application/yaml.
 */
export async function getAppComposeYaml(appId: string): Promise<string> {
  const res = await client.get(
    `/v2/app_management/apps/${encodeURIComponent(appId)}/compose`,
    {
      headers: { Accept: 'application/yaml' },
      responseType: 'text',
    },
  )
  return typeof res.data === 'string' ? res.data : String(res.data)
}

/**
 * installFromLibrary — sends a compose YAML to the backend's install
 * endpoint. The backend parses the YAML, pulls images, and creates
 * containers. Content-Type must be application/yaml.
 */
export async function installFromLibrary(composeYaml: string): Promise<string> {
  const res = await client.post<{ data?: { id?: string } }>(
    '/v2/app_management/compose',
    composeYaml,
    { headers: { 'Content-Type': 'application/yaml' } },
  )
  return res.data?.data?.id ?? ''
}

/**
 * getUpgradableApps — returns apps that have updates available.
 */
export async function getUpgradableApps(): Promise<UpgradableApp[]> {
  const res = await client.get<{ data: UpgradableApp[] }>(
    '/v2/app_management/apps/upgradable',
  )
  return res.data.data ?? []
}

// ---- Store management endpoints ----

export async function listStores(): Promise<StoreEntry[]> {
  const res = await client.get<{ data: StoreEntry[] }>(
    '/v2/app_management/stores',
  )
  return res.data.data ?? []
}

export async function registerStore(entry: Partial<StoreEntry>): Promise<StoreEntry> {
  const res = await client.post<{ data: StoreEntry }>(
    '/v2/app_management/stores',
    entry,
  )
  return res.data.data
}

export async function updateStore(id: string, update: Partial<StoreEntry>): Promise<StoreEntry> {
  const res = await client.put<{ data: StoreEntry }>(
    `/v2/app_management/stores/${encodeURIComponent(id)}`,
    update,
  )
  return res.data.data
}

export async function unregisterStore(id: string): Promise<void> {
  await client.delete(`/v2/app_management/stores/${encodeURIComponent(id)}`)
}

/** StoreSync calls SyncRemoteStore inline — no goroutine — and the download it
 *  drives is bounded at 5 minutes (downloadHelper/getter.go:68). At the shared
 *  10s default the button reported failure while the catalog downloaded and
 *  installed anyway. */
const STORE_SYNC_TIMEOUT_MS = 6 * 60_000

export async function syncStore(id: string): Promise<void> {
  await client.post(`/v2/app_management/stores/${encodeURIComponent(id)}/sync`, undefined, {
    timeout: STORE_SYNC_TIMEOUT_MS,
  })
}
