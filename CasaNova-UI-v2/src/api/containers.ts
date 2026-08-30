// Containers API — CasaNova-AppManagement service. Wraps the compose
// lifecycle endpoints the dashboard + Containers settings + per-app
// detail page consume.
//
// Route shapes match CasaNova-AppManagement/route/v1/docker.go and
// compose.go. The compose app structure maps 1:1 to the AppEntry
// type in src/fixtures/containers.ts so swapping the fixture for this
// API layer is mechanical.

import client from './client'

export type ContainerState = 'running' | 'stopped' | 'unhealthy' | 'updating' | 'crashed'

export interface ServiceApi {
  id: string
  dockerImage: string
  imageTag: string
  availableTags: string[]
  state: ContainerState
  uptimeSeconds: number
  cpuPercent: number
  cpuLimitCores: number | null
  memoryMB: number
  memoryLimitMB: number | null
  restartCount: number
  ports: Array<{ host: number; container: number; protocol: string }>
  volumes: Array<{ hostPath: string; containerPath: string; readOnly: boolean }>
  env: Array<{ key: string; value: string; secret?: boolean }>
  devices: Array<{ hostPath: string; containerPath: string }>
  command: string
  privileged: boolean
  restartPolicy: string
  networks: string[]
}

export interface AppEntryApi {
  id: string
  title: string
  description?: string
  iconUrl?: string
  tileColor?: string
  autoUpdate: boolean
  webUIProtocol: 'http' | 'https'
  webUIHost: string
  webUIPort: number | null
  webUIPath: string
  services: ServiceApi[]
}

// ----- List / get -----

// The backend returns a map of compose projects keyed by name, not
// the normalized AppEntryApi[]. We transform it here so the rest of
// the frontend works with a clean, flat list.

// Labels in compose JSON can come back as either a map (the canonical
// compose-go `Labels` type) or, for files authored by hand, a string
// slice like ["foo=bar", "baz=qux"]. We normalise both to a plain map
// on read.
type ComposeLabels = Record<string, string> | string[] | undefined

interface BackendComposeResponse {
  data: Record<string, {
    compose: {
      name: string
      services: Record<string, {
        image?: string
        ports?: Array<{ target: number; published: string; protocol: string }>
        volumes?: Array<{ source: string; target: string; type: string }>
        environment?: Record<string, string>
        container_name?: string
        restart?: string
        labels?: ComposeLabels
      }>
    }
    // Optional: the backend omits `status` entirely when the Docker listing
    // it derives states from failed, precisely so the UI does not render a
    // state nobody observed. Absent lands on normalizeContainerState's
    // 'stopped' fallback.
    status?: string
    store_info?: {
      title?: Record<string, string>
      icon?: string
      scheme?: string
      port_map?: string
      main?: string
    }
  }>
}

// The compose listing reports an app's status as the raw Docker state of its
// main container ("created", "exited", "paused", …), the literal "unknown"
// when it could not resolve one, or nothing at all when the Docker listing
// itself failed. None of those except "running" are members of ContainerState,
// and the old `app.status as ContainerState` cast forwarded them verbatim:
// "unknown" and "exited" are truthy, so they slipped past the `|| 'stopped'`
// fallback and reached aggregateAppState(), which used to answer 'running' for
// anything it did not recognise. The result was stopped (and unknown-state)
// apps painted green with a Stop button that had nothing to stop. Translate
// every state we actually know and send the rest to 'stopped', so a state the
// backend could not determine reads as "not up" rather than "up".
const CONTAINER_STATE_ALIASES: Record<string, ContainerState> = {
  running: 'running',
  unhealthy: 'unhealthy',
  updating: 'updating',
  crashed: 'crashed',
  stopped: 'stopped',
  // Docker's own container states. Everything other than "running" means the
  // workload is not serving, so they all collapse onto 'stopped' — including
  // "dead", which we deliberately do not promote to 'crashed' here because
  // that would newly paint containers red on a path nobody has reviewed.
  created: 'stopped',
  restarting: 'stopped',
  removing: 'stopped',
  paused: 'stopped',
  exited: 'stopped',
  dead: 'stopped',
}

function normalizeContainerState(raw: string | undefined | null): ContainerState {
  if (!raw) return 'stopped'
  // `docker compose ls` style statuses carry a container count ("running(2)");
  // the leading token is the state itself.
  const token = raw.trim().toLowerCase().split('(')[0].trim()
  return CONTAINER_STATE_ALIASES[token] ?? 'stopped'
}

// AutoUpdateLabel must stay in sync with CasaNova-AppManagement
// service/auto_update.go — that backend goroutine filters running
// containers with `label=casanova-auto-update=true` and pulls fresh
// digests for them every 24h. The UI writes this label through the
// compose spec so the change survives recreate + restart.
const AUTO_UPDATE_LABEL = 'casanova-auto-update'

// normalizeLabels converts compose's two accepted label shapes into a
// flat map so downstream code can just do `labels[KEY]` without
// branching.
function normalizeLabels(raw: ComposeLabels): Record<string, string> {
  if (!raw) return {}
  if (Array.isArray(raw)) {
    const out: Record<string, string> = {}
    for (const entry of raw) {
      const eq = entry.indexOf('=')
      if (eq < 0) out[entry] = ''
      else out[entry.slice(0, eq)] = entry.slice(eq + 1)
    }
    return out
  }
  return raw
}

// Resolve an icon URL for a compose app. Priority:
//   1. store_info.icon from the backend (x-casaos metadata)
//   2. CDN lookup by known app name (selfhst/icons)
//   3. CDN lookup by Docker image name
//   4. empty string (the tile falls back to the initial letter)
const ICON_CDN = 'https://cdn.jsdelivr.net/gh/selfhst/icons/png'
const KNOWN_ICONS: Record<string, string> = {
  // Map compose project names and image prefixes to icon filenames.
  // This covers the most common self-hosted apps.
  'photoprism': 'photoprism',
  'filebrowser': 'filebrowser',
  'heimdall': 'heimdall',
  'uptime-kuma': 'uptime-kuma',
  'uptimekuma': 'uptime-kuma',
  'jellyfin': 'jellyfin',
  'nextcloud': 'nextcloud',
  'immich': 'immich',
  'home-assistant': 'home-assistant',
  'homeassistant': 'home-assistant',
  'pihole': 'pi-hole',
  'pi-hole': 'pi-hole',
  'gitea': 'gitea',
  'portainer': 'portainer',
  'plex': 'plex',
  'sonarr': 'sonarr',
  'radarr': 'radarr',
  'prowlarr': 'prowlarr',
  'bazarr': 'bazarr',
  'vaultwarden': 'vaultwarden',
  'adguard': 'adguard-home',
  'adguardhome': 'adguard-home',
  'grafana': 'grafana',
  'prometheus': 'prometheus',
  'traefik': 'traefik',
  'nginx': 'nginx-proxy-manager',
  'caddy': 'caddy',
  'wireguard': 'wireguard',
  'syncthing': 'syncthing',
  'navidrome': 'navidrome',
  'calibre-web': 'calibre-web',
  'freshrss': 'freshrss',
  'audiobookshelf': 'audiobookshelf',
  'emby': 'emby',
  'code-server': 'code-server',
  'duplicati': 'duplicati',
  'overseerr': 'overseerr',
  'tautulli': 'tautulli',
  'watchtower': 'watchtower',
  'dashy': 'dashy',
  'homarr': 'homarr',
  'homepage': 'homepage',
  'transmission': 'transmission',
  'qbittorrent': 'qbittorrent',
  'deluge': 'deluge',
  'minecraft': 'minecraft',
  'ghost': 'ghost',
  'wordpress': 'wordpress',
  'mariadb': 'mariadb',
  'postgres': 'postgresql',
  'postgresql': 'postgresql',
  'redis': 'redis',
  'mongo': 'mongodb',
  'mongodb': 'mongodb',
  'minio': 'minio',
  'paperless': 'paperless-ngx',
  'paperless-ngx': 'paperless-ngx',
}

function resolveIcon(name: string, image: string, storeIcon: string): string {
  if (storeIcon) return storeIcon

  // Try project name first
  const nameKey = name.toLowerCase().replace(/[^a-z0-9-]/g, '')
  if (KNOWN_ICONS[nameKey]) return `${ICON_CDN}/${KNOWN_ICONS[nameKey]}.png`

  // Try Docker image name (strip registry and tag)
  const imageName = (image.split('/').pop() || '').split(':')[0].toLowerCase()
  if (KNOWN_ICONS[imageName]) return `${ICON_CDN}/${KNOWN_ICONS[imageName]}.png`

  // Try matching partial image path
  const imageBase = image.split(':')[0].toLowerCase()
  for (const [key, icon] of Object.entries(KNOWN_ICONS)) {
    if (imageBase.includes(key)) return `${ICON_CDN}/${icon}.png`
  }

  // Last-resort heuristic — the selfhst/icons CDN covers hundreds of
  // common self-hosted images beyond our hand-curated KNOWN_ICONS
  // table. Build a candidate URL from the normalized image name and
  // let the <img onerror> in AppCard fall back to the initial-letter
  // tile when the CDN 404s. Previously this returned '' for every
  // image we didn't hard-code, which meant that a server with 27
  // containers (plex, jellyfin, tautulli, sonarr, radarr, bazarr,
  // overseerr — half of which ARE in the table, but the other half
  // aren't, and servers imported from CasaOS never had store_info
  // metadata to begin with) rendered as plain letter tiles. The
  // @error handler ensures nothing is visually broken if the
  // heuristic misses.
  const candidate = (imageName || nameKey).replace(/[^a-z0-9-]/g, '')
  if (candidate) return `${ICON_CDN}/${candidate}.png`

  return ''
}

export async function listApps(): Promise<AppEntryApi[]> {
  const res = await client.get<BackendComposeResponse>(
    '/v2/app_management/compose',
  )
  const raw = res.data.data ?? {}
  return Object.entries(raw).map(([name, app]) => {
    const mainSvc = app.store_info?.main || Object.keys(app.compose.services)[0] || name
    const svcData = app.compose.services[mainSvc] || Object.values(app.compose.services)[0]
    const ports = (svcData?.ports || []).map(p => ({
      host: parseInt(String(p.published), 10) || 0,
      container: p.target,
      protocol: p.protocol || 'tcp',
    }))
    // Web-UI port: prefer the app author's declared port (x-casaos port_map)
    // over the first published port. Portainer publishes 8000/9000/9443 but its
    // web UI is on 9000 — ports[0]=8000 opened the wrong port ("not found").
    // port_map may name the host port directly or the web service's container
    // port; resolve to the matching host port when possible.
    const declaredWeb = app.store_info?.port_map ? parseInt(app.store_info.port_map, 10) : null
    let webPort = ports[0]?.host || null
    if (declaredWeb) {
      const byHost = ports.find(p => p.host === declaredWeb)
      const byContainer = ports.find(p => Number(p.container) === declaredWeb)
      webPort = byHost?.host ?? byContainer?.host ?? declaredWeb
    }
    const scheme = (app.store_info?.scheme || 'http') as 'http' | 'https'
    const title = app.store_info?.title?.en_us || app.store_info?.title?.en_US || name
    const mainImage = svcData?.image || ''
    const icon = resolveIcon(name, mainImage, app.store_info?.icon || '')

    // Auto-update reflects the opt-in label on at least one service.
    // Previously this was hard-coded to `false`, which meant the
    // toggle in Containers settings and the per-app detail card
    // always rendered as "off" after a refresh — even for apps the
    // user had just enabled. Because the toggle used that same stale
    // value as its source of truth for the next click, the
    // optimistic flip in `toggleAutoUpdate` would immediately flip
    // back on the next list refresh, making the feature appear
    // broken and silently dropping the user's preference.
    const autoUpdateOn = Object.values(app.compose.services).some((svc) => {
      const labels = normalizeLabels(svc?.labels)
      return labels[AUTO_UPDATE_LABEL] === 'true'
    })
    return {
      id: name,
      title,
      iconUrl: icon || undefined,
      tileColor: undefined,
      autoUpdate: autoUpdateOn,
      webUIProtocol: scheme,
      webUIHost: '',
      webUIPort: webPort,
      webUIPath: '',
      services: Object.entries(app.compose.services).map(([svcName, svc]) => {
        const [img, tag] = (svc?.image || '').split(':')
        return {
          id: svc?.container_name || svcName,
          dockerImage: img || '',
          imageTag: tag || 'latest',
          availableTags: [],
          state: normalizeContainerState(app.status),
          uptimeSeconds: 0,
          cpuPercent: 0,
          cpuLimitCores: null,
          memoryMB: 0,
          memoryLimitMB: null,
          restartCount: 0,
          ports: (svc?.ports || []).map(p => ({
            host: parseInt(String(p.published), 10) || 0,
            container: p.target,
            protocol: p.protocol || 'tcp',
          })),
          volumes: (svc?.volumes || []).map(v => ({
            hostPath: v.source || '',
            containerPath: v.target || '',
            readOnly: false,
          })),
          env: Object.entries(svc?.environment || {}).map(([key, value]) => ({
            key,
            value: String(value),
          })),
          devices: [],
          command: '',
          privileged: false,
          restartPolicy: svc?.restart || 'unless-stopped',
          networks: [],
        } satisfies ServiceApi
      }),
    } satisfies AppEntryApi
  })
}

export async function getApp(id: string): Promise<AppEntryApi> {
  const res = await client.get<{ success: number; data: AppEntryApi }>(
    `/v2/app_management/compose/${encodeURIComponent(id)}`,
  )
  return res.data.data
}

// ----- Install / update / uninstall -----

export interface InstallManualRequest {
  image: string
  tag: string
  title: string
  iconUrl?: string
  webUI?: { protocol: 'http' | 'https'; host: string; port: number; path: string }
  network?: string
  ports?: Array<{ host: number; container: number; protocol: string }>
  volumes?: Array<{ hostPath: string; containerPath: string; readOnly: boolean }>
  env?: Array<{ key: string; value: string }>
}

export async function installManual(
  body: InstallManualRequest,
): Promise<{ id: string; progressUrl: string }> {
  const res = await client.post<{
    success: number
    data: { id: string; progressUrl: string }
  }>('/v2/app_management/compose', body)
  return res.data.data
}

/**
 * installFromCompose — takes a raw docker-compose.yml string and
 * creates a new compose project from it. Used by the Import tab of
 * the install dialog.
 */
export async function installFromCompose(
  yaml: string,
): Promise<{ id: string; progressUrl: string }> {
  const res = await client.post<{
    success: number
    data: { id: string; progressUrl: string }
  }>('/v2/app_management/compose/import', { yaml })
  return res.data.data
}

export async function installFromCli(
  command: string,
): Promise<{ id: string; progressUrl: string }> {
  const res = await client.post<{
    success: number
    data: { id: string; progressUrl: string }
  }>('/v2/app_management/compose/import-cli', { command })
  return res.data.data
}

export async function updateApp(
  id: string,
  body: Partial<AppEntryApi>,
): Promise<void> {
  // Fetch the current compose project so we only patch changed fields
  // rather than reconstructing (and losing networks, labels, depends_on, etc.).
  let existing: Record<string, unknown> | null = null
  try {
    const res = await client.get<{ data: Record<string, unknown> }>(
      `/v2/app_management/compose/${encodeURIComponent(id)}`,
    )
    existing = (res.data.data as Record<string, unknown>) ?? null
  } catch {
    // Fall through — reconstruct from scratch as a last resort.
  }

  // If we got the existing compose project, patch only what changed.
  // Otherwise fall back to full reconstruction.
  const composeBody: Record<string, unknown> = { name: id }

  if (existing && typeof existing === 'object' && 'compose' in existing) {
    // Clone the existing compose block and patch services into it.
    const compose = JSON.parse(JSON.stringify((existing as { compose: unknown }).compose)) as Record<string, unknown>
    composeBody.name = (compose.name as string) || id

    if (body.services && compose.services && typeof compose.services === 'object') {
      const existingSvcs = compose.services as Record<string, Record<string, unknown>>
      for (const svc of body.services) {
        // Match by container_name or service key
        const svcKey = Object.keys(existingSvcs).find(
          k => existingSvcs[k]?.container_name === svc.id || k === svc.id,
        ) || svc.id || Object.keys(existingSvcs)[0]

        if (existingSvcs[svcKey]) {
          // Patch the fields the user can edit in the UI. Each of the
          // collection fields (ports, volumes, devices) is the full
          // value the user sees in the editor — an empty array means
          // "the user removed everything", NOT "the UI didn't touch
          // this field". Previously we guarded with `length > 0`
          // which silently preserved the old list, so deleting the
          // last port in the dialog and hitting Save was a no-op.
          const tag = svc.imageTag || 'latest'
          existingSvcs[svcKey].image = `${svc.dockerImage}:${tag}`
          existingSvcs[svcKey].restart = svc.restartPolicy || 'unless-stopped'

          existingSvcs[svcKey].ports = svc.ports
            .filter(p => p.container > 0)
            .map(p => ({
              target: p.container,
              published: String(p.host || p.container),
              protocol: p.protocol || 'tcp',
            }))
          existingSvcs[svcKey].volumes = svc.volumes
            .filter(v => v.containerPath)
            .map(v => ({
              type: 'bind',
              source: v.hostPath,
              target: v.containerPath,
              read_only: v.readOnly || undefined,
            }))
          existingSvcs[svcKey].environment = Object.fromEntries(
            svc.env.filter(e => e.key).map(e => [e.key, e.value]),
          )
          if (svc.command) existingSvcs[svcKey].command = svc.command
          else delete existingSvcs[svcKey].command
          if (svc.privileged) existingSvcs[svcKey].privileged = true
          else delete existingSvcs[svcKey].privileged
          existingSvcs[svcKey].devices = svc.devices?.length
            ? svc.devices.map(d => `${d.hostPath}:${d.containerPath}`)
            : []
        }
      }
      composeBody.services = existingSvcs
    } else {
      composeBody.services = compose.services
    }

    // Auto-update is a per-service Docker label. When the caller
    // set body.autoUpdate (e.g. the toggle in Containers settings
    // or the AppDetail save path) we apply or strip the opt-in
    // label on EVERY service in the compose project so the daily
    // sweep in CasaNova-AppManagement's StartAutoUpdate goroutine
    // sees a consistent state. Writing the label at the compose
    // level (rather than `docker label`) is what makes the change
    // survive the recreate-on-save path — otherwise the label
    // would be rebuilt from the stored compose file on the next
    // restart and silently dropped.
    if (body.autoUpdate !== undefined && composeBody.services && typeof composeBody.services === 'object') {
      const svcs = composeBody.services as Record<string, Record<string, unknown>>
      for (const key of Object.keys(svcs)) {
        const svc = svcs[key]
        const labels = normalizeLabels(svc.labels as ComposeLabels)
        if (body.autoUpdate) {
          labels[AUTO_UPDATE_LABEL] = 'true'
        } else {
          delete labels[AUTO_UPDATE_LABEL]
        }
        if (Object.keys(labels).length > 0) {
          svc.labels = labels
        } else {
          delete svc.labels
        }
      }
    }
  } else if (body.services) {
    // Fallback: reconstruct from scratch (may lose some compose fields).
    const svcMap: Record<string, Record<string, unknown>> = {}
    for (const svc of body.services) {
      const name = svc.id || 'app'
      const tag = svc.imageTag || 'latest'
      svcMap[name] = {
        image: `${svc.dockerImage}:${tag}`,
        container_name: svc.id || undefined,
        restart: svc.restartPolicy || 'unless-stopped',
        privileged: svc.privileged || undefined,
        command: svc.command || undefined,
        ports: svc.ports
          .filter(p => p.container > 0)
          .map(p => ({
            target: p.container,
            published: String(p.host || p.container),
            protocol: p.protocol || 'tcp',
          })),
        volumes: svc.volumes
          .filter(v => v.containerPath)
          .map(v => ({
            type: 'bind',
            source: v.hostPath,
            target: v.containerPath,
            read_only: v.readOnly || undefined,
          })),
        environment: Object.fromEntries(
          svc.env.filter(e => e.key).map(e => [e.key, e.value]),
        ),
        devices: svc.devices.length > 0
          ? svc.devices.map(d => `${d.hostPath}:${d.containerPath}`)
          : undefined,
        networks: svc.networks.length > 0 ? svc.networks : undefined,
      }
    }
    composeBody.services = svcMap
  }

  // Include store metadata so the backend preserves title/icon/etc.
  // v0.1: canonical key is `x-casanova`. The backend's
  // common.ComposeExtension reads `x-casanova` first and falls back to
  // `x-casaos` for compose files imported from upstream CasaOS app
  // stores — so existing app definitions keep working without
  // modification.
  if (body.title || body.iconUrl || body.webUIPort != null) {
    composeBody['x-casanova'] = {
      title: { en_us: body.title || id },
      icon: body.iconUrl || '',
      scheme: body.webUIProtocol || 'http',
      hostname: body.webUIHost || '',
      port_map: body.webUIPort ? String(body.webUIPort) : '',
      index: body.webUIPath || '/',
    }
  }
  await client.put(
    `/v2/app_management/compose/${encodeURIComponent(id)}`,
    composeBody,
    { headers: { 'Content-Type': 'application/json' } },
  )
}

export async function uninstallApp(id: string): Promise<void> {
  await client.delete(`/v2/app_management/compose/${encodeURIComponent(id)}`)
}

// ----- Lifecycle -----

export async function startApp(id: string): Promise<void> {
  await client.put(`/v2/app_management/compose/${encodeURIComponent(id)}/status`, { status: 'running' })
}

export async function stopApp(id: string): Promise<void> {
  await client.put(`/v2/app_management/compose/${encodeURIComponent(id)}/status`, { status: 'stopped' })
}

export async function restartApp(id: string): Promise<void> {
  await client.put(`/v2/app_management/compose/${encodeURIComponent(id)}/status`, { status: 'restarting' })
}

// Repair reconciles an app whose container was orphaned from its compose
// project (shows "installed but offline / not found", can't be started): it
// loads the app from its on-disk compose file, removes the stale orphan, and
// recreates it properly labelled. No body — the app id in the path is enough.
export async function repairApp(id: string): Promise<void> {
  await client.post(`/v2/app_management/compose/${encodeURIComponent(id)}/repair`)
}

// ----- Container stats (CPU/RAM per container) -----

export interface ContainerStats {
  name: string
  cpuPercent: number
  memoryMB: number
}

export async function getContainerStats(): Promise<ContainerStats[]> {
  // The backend returns FLAT, already-computed per-container stats keyed by
  // container name: { id, name, cpuPercent, memoryMB, memoryLimit, netRx/Tx }.
  // (The old nested {title, data:{cpu_stats,...}} shape this used to parse no
  // longer exists — reading c.data.* threw and silently zeroed every app's
  // CPU/RAM in the dashboard breakdown.)
  const res = await client.get<{
    success: number
    data: Array<{ id?: string; name?: string; cpuPercent?: number; memoryMB?: number }>
  }>('/v1/container/stats')
  return (res.data.data ?? []).map((c) => ({
    name: c.name ?? '',
    cpuPercent: typeof c.cpuPercent === 'number' ? Math.round(c.cpuPercent * 100) / 100 : 0,
    memoryMB: typeof c.memoryMB === 'number' ? Math.round(c.memoryMB * 100) / 100 : 0,
  }))
}

// ----- Update check -----

export async function checkForUpdates(id: string): Promise<{
  services: Array<{ serviceId: string; hasUpdate: boolean; newTag?: string }>
}> {
  const res = await client.post<{
    success: number
    data: {
      services: Array<{ serviceId: string; hasUpdate: boolean; newTag?: string }>
    }
  }>(`/v2/app_management/compose/${encodeURIComponent(id)}/check_updates`)
  return res.data.data
}

/**
 * pullAppUpdate — actually performs the update: PATCH /compose/:id maps to
 * the backend's UpdateComposeApp, which re-pulls the images from the store
 * YAML and recreates the containers, preserving volumes and env. The pull
 * runs asynchronously server-side (the call returns as soon as it's queued),
 * so callers should refresh the app list shortly after.
 *
 * `force` skips the "is an update available?" gate — used for "re-pull and
 * recreate anyway" even when the store reports up-to-date.
 */
export async function pullAppUpdate(id: string, force = false): Promise<string> {
  const res = await client.patch<{ success: number; message?: string }>(
    `/v2/app_management/compose/${encodeURIComponent(id)}`,
    undefined,
    { params: { force } },
  )
  return res.data.message ?? ''
}

// ----- Docker image tag lookup -----

/**
 * Fetch available tags for a Docker image from the registry.
 * Returns prioritized list: latest/stable first, then version tags newest-first.
 */
export async function getImageTags(image: string): Promise<string[]> {
  const res = await client.get<{ success: number; data: string[] }>(
    '/v1/container/image/tags',
    { params: { image } },
  )
  return res.data.data ?? ['latest']
}

// ----- Per-container WebSocket URLs (terminal + logs) -----

export function containerTerminalWsUrl(containerId: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const token = localStorage.getItem('casanova.access_token') ?? ''
  const params = token ? `?token=${encodeURIComponent(token)}` : ''
  return `${proto}//${window.location.host}/v1/container/${encodeURIComponent(containerId)}/terminal${params}`
}

// --- External/unmanaged container discovery ---

export interface ExternalContainer {
  id: string
  name: string
  image: string
  state: string    // running, exited, paused
  status: string   // "Up 3 days"
  created: number  // unix timestamp
  ports: Array<{ privatePort: number; publicPort: number; type: string; ip?: string }>
  labels: Record<string, string>
  managed: boolean
}

export interface ComposeStack {
  name: string
  containers: ExternalContainer[]
  status: string   // running, partial, stopped
  serviceCount: number
  managed: boolean
}

export interface AllContainersResponse {
  stacks: ComposeStack[]
  standalone: ExternalContainer[]
  total: number
}

export async function getAllContainers(): Promise<AllContainersResponse> {
  const res = await client.get<{ success: number; data: AllContainersResponse }>(
    '/v1/container/all',
  )
  return res.data.data
}

export async function startContainer(id: string): Promise<void> {
  await client.post(`/v1/container/${encodeURIComponent(id)}/start`)
}

export async function stopContainer(id: string): Promise<void> {
  await client.post(`/v1/container/${encodeURIComponent(id)}/stop`)
}

export async function restartContainer(id: string): Promise<void> {
  await client.post(`/v1/container/${encodeURIComponent(id)}/restart`)
}

/**
 * Update a standalone (unmanaged) container: re-pull its image reference
 * and recreate the container, preserving config (env, volumes, ports,
 * labels, restart policy). Backend does a Portainer-style clone+recreate
 * asynchronously. `pull=true` fetches a newer image; `force` recreates even
 * when the image is unchanged. Returns the backend's status message.
 */
export async function recreateContainer(id: string, pull = true, force = false): Promise<string> {
  const res = await client.patch<{ success: number; message?: string }>(
    `/v2/app_management/container/${encodeURIComponent(id)}`,
    undefined,
    { params: { pull, force } },
  )
  return res.data.message ?? ''
}

export function containerLogsWsUrl(containerId: string, tail = 500): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const token = localStorage.getItem('casanova.access_token') ?? ''
  const base = `${proto}//${window.location.host}/v1/container/${encodeURIComponent(containerId)}/logs`
  const params = new URLSearchParams({ tail: String(tail) })
  if (token) params.set('token', token)
  return `${base}?${params.toString()}`
}

// ----- Install progress SSE -----

/**
 * subscribeProgress — opens an EventSource to the progress stream
 * for an ongoing install/update. The backend emits JSON payloads
 * like `{ progress: 0..100, status, log_line }` which the
 * installProgress store consumes to advance its progress bar.
 *
 * Returns the EventSource so the caller can .close() when done.
 * The source auto-closes on terminal status (completed/failed) to
 * prevent the browser's built-in reconnect loop.
 *
 * Auth: the native EventSource API cannot send custom headers, so
 * the token is appended as a query parameter. The backend must
 * accept `?token=<jwt>` on SSE endpoints.
 */
export function subscribeProgress(
  installId: string,
  onMessage: (msg: { progress: number; status: string; log_line?: string }) => void,
  onError?: (err: Event) => void,
): EventSource {
  const base = `/v2/app_management/compose/install/${encodeURIComponent(installId)}/progress`
  // Append auth token as query param since EventSource can't send headers.
  // Must match the key used in stores/auth.ts (STORAGE_ACCESS).
  const token = localStorage.getItem('casanova.access_token') ?? ''
  const url = token ? `${base}?token=${encodeURIComponent(token)}` : base

  const es = new EventSource(url)
  es.onmessage = (e) => {
    try {
      const msg = JSON.parse(e.data) as { progress: number; status: string; log_line?: string }
      onMessage(msg)
      // Auto-close on terminal status to prevent infinite reconnect.
      if (msg.status === 'completed' || msg.status === 'failed') {
        es.close()
      }
    } catch {
      /* ignore malformed frames */
    }
  }
  es.onerror = (err) => {
    // If the server closed the stream, readyState becomes CLOSED.
    // Don't let EventSource's auto-reconnect loop hammer a dead endpoint.
    if (es.readyState === EventSource.CLOSED) {
      es.close()
    }
    onError?.(err)
  }
  return es
}

// ----- Docker Hub search (feeds the install wizard) -----

export interface DockerHubResult {
  name: string        // pullable image name (library/<x> for official images)
  description: string
  stars: number
  /** Human-formatted by Docker Hub itself: "1B+", "50K+". */
  pulls: string
  official: boolean
}

export async function searchDockerHub(q: string): Promise<DockerHubResult[]> {
  const res = await client.get<DockerHubResult[]>(
    `/v2/app_management/dockerhub/search?q=${encodeURIComponent(q)}`,
  )
  return res.data
}
