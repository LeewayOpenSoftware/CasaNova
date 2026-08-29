// Container/app types and utility functions extracted from fixtures/containers.ts
// so production code can import these without pulling in fake data.

export type ContainerState = 'running' | 'stopped' | 'unhealthy' | 'updating' | 'crashed'

// The origin of an app-like workload. The UI presents all of these as a
// single unified application model; `source` only drives behaviour that
// genuinely differs (update path, lifecycle endpoints), never whether the
// app appears or opens.
//   - compose:        a CasaNova-managed compose project (incl. App Store installs)
//   - standalone:     an unmanaged single Docker container
//   - external-stack: an unmanaged compose project not installed via CasaNova
export type AppSource = 'compose' | 'standalone' | 'external-stack'

// TCP/UDP/"both" protocol selector -- CasaOS exposes a third option
// that simultaneously publishes a port over both TCP and UDP. We
// represent it in the editor state so the UI can show one row per
// logical mapping; the YAML generator expands "both" into two Docker
// port entries.
export type PortProtocol = 'tcp' | 'udp' | 'both'

export interface PortMapping {
  host: number
  container: number
  protocol: PortProtocol
}

export interface VolumeMount {
  hostPath: string
  containerPath: string
  readOnly: boolean
}

export interface EnvVar {
  key: string
  value: string
  secret?: boolean
}

export interface DeviceMapping {
  hostPath: string
  containerPath: string
}

export type RestartPolicy = 'no' | 'always' | 'unless-stopped' | 'on-failure'

/**
 * ServiceEntry -- one container in a compose project. Carries all the
 * per-container editable settings (image, ports, volumes, env, etc.)
 * plus live runtime stats (state, CPU, memory). App-level fields like
 * title, icon, description live on AppEntry, not here.
 */
export interface ServiceEntry {
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
  ports: PortMapping[]
  volumes: VolumeMount[]
  env: EnvVar[]
  devices: DeviceMapping[]
  command: string
  privileged: boolean
  restartPolicy: RestartPolicy
  networks: string[]
}

/**
 * AppEntry -- a compose project the user sees as a single tile on the
 * dashboard. Holds the user-facing identity (title, icon, description)
 * plus the list of services.
 */
export interface AppEntry {
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
  services: ServiceEntry[]
  // True (or undefined) for CasaNova-managed compose apps. False for
  // unmanaged containers (standalone Docker containers / external compose
  // stacks) that the dashboard also surfaces as tiles — the detail view
  // renders these read-only and routes lifecycle to the external-container
  // endpoints instead of the compose ones. `managed === false` iff
  // `source !== 'compose'`.
  managed?: boolean
  // Which app source this entry came from. Drives update path + lifecycle
  // endpoint selection only; every source resolves and opens identically.
  source?: AppSource
}

/** How (and whether) a given app can be updated. */
export type UpdateMethod = 'compose' | 'recreate' | 'none'

export interface UpdateEligibility {
  method: UpdateMethod
  supported: boolean
}

/**
 * Classify how an app updates, by source:
 *   - compose:        full compose update (registry-digest check + pull + recreate)
 *   - standalone:     re-pull the image reference and recreate the container,
 *                     preserving config (backend PATCH /container/:id?pull=true)
 *   - external-stack: not updatable as a unit yet — the UI shows the
 *                     `apps.update_external_stack` reason rather than hiding it.
 */
export function updateEligibility(app: Pick<AppEntry, 'source' | 'managed'>): UpdateEligibility {
  const source: AppSource =
    app.source ?? (app.managed === false ? 'standalone' : 'compose')
  switch (source) {
    case 'standalone':
      return { method: 'recreate', supported: true }
    case 'external-stack':
      return { method: 'none', supported: false }
    case 'compose':
    default:
      return { method: 'compose', supported: true }
  }
}

// TODO: Replace with real system capacity from the system store.
// Hardcoded for now so the resource-limit sliders have reasonable stops.
export const systemCapacityFixture = {
  totalMemoryMB: 32_640,
  totalCpuCores: 16,
}

/**
 * Returns true if the Docker image tag is a "rolling" tag like latest,
 * stable, nightly, etc. Used to warn users about auto-update risks.
 */
export function isRollingTag(tag: string): boolean {
  if (!tag) return false
  const normalized = tag.toLowerCase().trim()
  const rollingNames = [
    'latest', 'stable', 'main', 'master', 'release', 'edge',
    'nightly', 'develop', 'development', 'dev', 'rolling', 'beta', 'canary',
  ]
  for (const name of rollingNames) {
    if (normalized === name) return true
    if (normalized.startsWith(name + '-')) return true
  }
  return false
}

/**
 * Aggregate the most-severe state across an app's services so the
 * dashboard tile shows a single status dot that reflects the whole
 * project's health. Severity order: crashed > unhealthy > updating >
 * stopped > running — a multi-service project reports its worst
 * service, so one crashed container in a four-container stack still
 * surfaces as crashed rather than being averaged away.
 */
export function aggregateAppState(app: AppEntry): ContainerState {
  const order: ContainerState[] = ['crashed', 'unhealthy', 'updating', 'stopped', 'running']
  for (const s of order) {
    if (app.services.some((svc) => svc.state === s)) return s
  }
  // Nothing matched: either the app has no services at all, or every
  // service carries a state outside ContainerState (a backend status the
  // API layer could not resolve). This used to return 'running', which
  // meant an app whose state we genuinely did not know was painted green
  // and offered a Stop button it could not honour. Fall to the safe side
  // instead — an unresolvable state reads as "not up", never as "up".
  return 'stopped'
}
