<script setup lang="ts">
// AppDetailView — per-app settings page (CasaOS-parity rewrite).
//
// Reads an AppEntry by route param. Single-container apps render one
// tab matching the app name; multi-container apps (Immich, Nextcloud
// with a DB, etc.) render a tab per service and let the user configure
// each one independently. App-level metadata (title, icon, autoUpdate,
// danger zone) lives above the tab strip because it applies to the
// whole project.
//
// Every editable field is held in a local ref cloned from the fixture.
// Mutations stay in-component until the user clicks Save in the
// sticky bottom bar; Save is a no-op in fixture mode but the intent
// is wired so swapping to the real PUT endpoint is a single handler.
//
// Sections per service tab:
//   1. Core identity — image, tag, title, icon URL + preview, Web UI
//      composite (protocol/host/port/path), primary network
//   2. Ports — editable rows with TCP/UDP/Both dropdown
//   3. Volumes — editable rows with mini folder-picker on the host side
//   4. Environment variables — editable rows with secret mask toggle
//   5. Devices — editable host:container pairs
//   6. Container command — textarea override
//   7. Resource limits — slider presets for CPU cores + memory MB
//   8. Runtime — privileged + restart policy
//
// App-level (outside the tab switcher):
//   - Header card: icon, title, aggregate state, Open, Start/Stop/Restart, compose YAML button
//   - Auto-update toggle
//   - Danger zone: Uninstall confirm dialog
//   - Sticky Save bar at the bottom
//   - Compose YAML dialog (textarea + download + copy)

import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import duration from 'dayjs/plugin/duration'
dayjs.extend(relativeTime)
dayjs.extend(duration)
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import Slider from 'primevue/slider'
import ToggleSwitch from 'primevue/toggleswitch'
import Select from 'primevue/select'
import Dialog from 'primevue/dialog'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import FolderPickerDialog from '@/components/apps/FolderPickerDialog.vue'
import ContainerTerminalDialog from '@/components/containers/ContainerTerminalDialog.vue'
import {
  aggregateAppState,
  isRollingTag,
  updateEligibility,
  type AppEntry,
  type ServiceEntry,
  type ContainerState,
  type PortProtocol,
  type RestartPolicy,
} from '@/types/containers'
import { useContainersStore } from '@/stores/containers'
import { useInstallProgressStore } from '@/stores/installProgress'
import { useSystemStore } from '@/stores/system'
import { hardwareApi } from '@/api'
import { getImageTags } from '@/api/containers'
import { buildComposeYaml, downloadYaml } from '@/composables/useComposeYaml'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()

// Resolve the :id route param to a live AppEntry. A changing param
// (navigating between two app details without unmounting this view)
// re-seeds the editor state via the watcher below.
const containersStore = useContainersStore()
const installs = useInstallProgressStore()
const systemStore = useSystemStore()
const appId = computed(() =>
  Array.isArray(route.params.id) ? route.params.id[0] : route.params.id,
)
const app = computed<AppEntry | undefined>(() => containersStore.findApp(appId.value))

// Compose-managed apps expose the full editor (compose YAML, update,
// env/volume/port editing). Unmanaged containers (standalone Docker
// containers / external stacks) resolve read-only: lifecycle routes to
// the external-container endpoints and the compose-only controls hide.
const isManaged = computed(() => app.value?.managed !== false)

// Whether this view has completed at least one load attempt. Until it
// has, an absent `app` means "still loading", NOT "app does not exist".
// Rendering the terminal not-found state during the initial async
// refresh is exactly what made valid, dashboard-visible installed apps
// show "App not found" on direct navigation / hard refresh / deep link.
const ready = ref(false)
// True while the first load (or any in-flight store refresh) is pending.
const isLoading = computed(() => !ready.value || containersStore.loading)

// Refresh the containers list on mount so the detail view has the
// freshest data, flipping `ready` only once the attempt resolves. The
// store caches for 10s, so navigating in from the dashboard (where the
// list is already loaded) resolves immediately with no spinner.
onMounted(async () => {
  // Await BOTH the compose list and the external/standalone container list
  // before opening the gate: an unmanaged app resolves from
  // standaloneContainers (loaded by refreshAll), so gating on refresh()
  // alone would briefly show "App not found" for a standalone container.
  await Promise.all([containersStore.refresh(), containersStore.refreshAll()])
  ready.value = true
})

// Retry handler for the load-error state (a failed list fetch must not
// be presented to the user as "app not found").
async function reloadApp(): Promise<void> {
  ready.value = false
  await Promise.all([containersStore.refresh(true), containersStore.refreshAll()])
  ready.value = true
}

// System capacity for resource-limit sliders. RAM comes from the live
// system store; CPU cores are fetched once from the hardware endpoint.
const systemCpuCores = ref(16) // sensible fallback until hardware API responds
hardwareApi.getSystemHardware().then((hw) => {
  if (hw.cpu.cores > 0) systemCpuCores.value = hw.cpu.cores
}).catch(() => { /* keep fallback */ })
const systemCapacity = computed(() => ({
  totalMemoryMB: Math.round(systemStore.ramTotalGB * 1024) || 32_640,
  totalCpuCores: systemCpuCores.value,
}))

// ----- UI state (declared up front so the app watcher can reset it) -----
const revealedSecrets = ref<Set<string>>(new Set())
const uninstallDialogVisible = ref(false)
const yamlDialogVisible = ref(false)
const folderPickerVisible = ref(false)
const folderPickerTargetIndex = ref<number | null>(null)
const copyFeedback = ref(false)
const containerTerminalVisible = ref(false)
const containerTerminalMode = ref<'terminal' | 'logs'>('terminal')

// ----- Editable app-level state -----
const editableTitle = ref('')
const editableIconUrl = ref('')
const editableWebProtocol = ref<'http' | 'https'>('http')
const editableWebHost = ref('')
const editableWebPort = ref<number | null>(null)
const editableWebPath = ref('/')
const editableAutoUpdate = ref(false)

// ----- Editable per-service state -----
// One array of service snapshots; the active tab index selects which
// one the form cards bind to. Using a single array (rather than a
// Map) keeps reactivity straightforward and preserves service order.
const editableServices = ref<ServiceEntry[]>([])
const activeTabIndex = ref(0)
const activeService = computed<ServiceEntry | null>(
  () => editableServices.value[activeTabIndex.value] ?? null,
)

// ----- Dirty tracking -----
// Declared up here because the seeding watcher below has to consult it:
// a background refresh must never re-seed a form the user is typing in.
//
// Dirtiness is a comparison against the loaded state, not a "something
// changed" flag. A flag was wrong twice over: the registry tag fetch
// writes `availableTags` on the seeded services a second after mount, so
// every visit raised the Save bar on its own, and undoing an edit by hand
// left the form pinned as dirty forever. The snapshot deliberately leaves
// out the runtime fields (state, uptime, live CPU/RAM) and the tag list —
// those only ever arrive from the server, so they cannot be a user edit.
function formSnapshot(): string {
  return JSON.stringify({
    title: editableTitle.value,
    iconUrl: editableIconUrl.value,
    webUIProtocol: editableWebProtocol.value,
    webUIHost: editableWebHost.value,
    webUIPort: editableWebPort.value,
    webUIPath: editableWebPath.value,
    autoUpdate: editableAutoUpdate.value,
    services: editableServices.value.map((s) => ({
      id: s.id,
      dockerImage: s.dockerImage,
      imageTag: s.imageTag,
      cpuLimitCores: s.cpuLimitCores,
      memoryLimitMB: s.memoryLimitMB,
      ports: s.ports,
      volumes: s.volumes,
      env: s.env,
      devices: s.devices,
      command: s.command,
      privileged: s.privileged,
      restartPolicy: s.restartPolicy,
      networks: s.networks,
    })),
  })
}
// The form as it was last seeded from (or saved to) the server. null until
// the first seed lands, so a still-loading view is never "dirty".
const seededSnapshot = ref<string | null>(null)
const isDirty = computed(
  () => seededSnapshot.value !== null && formSnapshot() !== seededSnapshot.value,
)
// Which app the form was seeded from. The store replaces its AppEntry
// objects wholesale on every refresh, so `app` changing identity says
// nothing about whether the user actually navigated somewhere else.
const seededAppId = ref<string | null>(null)
// Set when a refresh landed while the form was dirty: newer server data
// exists that we deliberately did not apply over the user's edits.
const serverChangedWhileDirty = ref(false)

// Copy only the read-only runtime fields of a freshly-fetched AppEntry onto
// the editable snapshots, matched by service id, so the status block keeps
// ticking while the form the user is typing in stays exactly as they left it.
function syncRuntimeFields(a: AppEntry): void {
  for (const svc of editableServices.value) {
    const fresh = a.services.find((s) => s.id === svc.id)
    if (!fresh) continue
    svc.state = fresh.state
    svc.uptimeSeconds = fresh.uptimeSeconds
    svc.cpuPercent = fresh.cpuPercent
    svc.memoryMB = fresh.memoryMB
    svc.restartCount = fresh.restartCount
  }
}

// ----- Slider preset arrays -----
// Index-bound: slider value is the position in the stops array, the
// displayed label maps through to the actual GB/cores. Stop 0 is
// always "Unlimited" so the user can snap back to that intent.
const memoryStops = computed(() => {
  const base = [0, 256, 512, 1024, 2048, 4096, 8192, 16384]
  const max = systemCapacity.value.totalMemoryMB
  if (max > base[base.length - 1]) base.push(max)
  return base
})
const cpuStops = computed(() => {
  const base = [0, 0.5, 1, 2, 4, 8]
  const max = systemCapacity.value.totalCpuCores
  if (max > base[base.length - 1]) base.push(max)
  return base
})
const memoryIndex = ref(0)
const cpuIndex = ref(0)
const memoryValue = computed(() => memoryStops.value[memoryIndex.value] ?? 0)
const cpuValue = computed(() => cpuStops.value[cpuIndex.value] ?? 0)

// The limits each service was last loaded (or saved) with, keyed by service id.
// snapIndexToStop() is lossy — a stored 1500 MB and the 1024 MB stop both live
// at slider position 3 — so blindly writing the stop value back on every index
// change rewrote the limit to something the user never asked for. Worse, it
// made the position non-idempotent: dragging the slider away and back to the
// same stop mutated memoryLimitMB, the form stopped matching its baseline, and
// the "unsaved changes" bar stayed pinned open with nothing visibly changed —
// so hitting Save to dismiss it pushed a memory-limit change out of nowhere.
// Remembering the loaded value lets the write-back restore it verbatim while
// the slider sits on the stop that value snapped to.
const seededLimits = new Map<
  string,
  { memoryLimitMB: number | null; cpuLimitCores: number | null }
>()
// Re-anchor the loaded limits to whatever the form currently holds. Called at
// every point the dirty baseline is (re)taken — seed and save — so the two
// always describe the same state.
function captureSeededLimits(): void {
  seededLimits.clear()
  for (const svc of editableServices.value) {
    seededLimits.set(svc.id, {
      memoryLimitMB: svc.memoryLimitMB,
      cpuLimitCores: svc.cpuLimitCores,
    })
  }
}

function snapIndexToStop(value: number | null, stops: number[]): number {
  if (value === null || value <= 0) return 0
  let best = 0
  let bestDelta = Infinity
  for (let i = 1; i < stops.length; i++) {
    const d = Math.abs(stops[i] - value)
    if (d < bestDelta) {
      best = i
      bestDelta = d
    }
  }
  return best
}

// Reseed everything from the fixture whenever the app id changes.
// The service-index resync + slider resync happens inside a nested
// watcher so swapping tabs reloads slider positions for the new svc.
watch(
  app,
  (a) => {
    if (!a) return

    // This watcher also fires for reasons the user never asked for: the
    // store hands out fresh AppEntry objects after every refresh, and a
    // refresh is triggered by a lifecycle action, an install event or the
    // deferred re-read after Save. Re-seeding on one of those threw away
    // whatever was half-typed into the env / port / volume editors, with no
    // warning and no way back. Keep the form when it has unsaved work on
    // the app we already seeded: only the read-only runtime fields follow
    // the server, and a marker in the save bar says newer data is waiting.
    if (seededAppId.value === a.id && isDirty.value) {
      syncRuntimeFields(a)
      serverChangedWhileDirty.value = true
      return
    }

    seededAppId.value = a.id
    serverChangedWhileDirty.value = false
    editableTitle.value = a.title
    editableIconUrl.value = a.iconUrl ?? ''
    editableWebProtocol.value = a.webUIProtocol
    editableWebHost.value = a.webUIHost
    editableWebPort.value = a.webUIPort
    editableWebPath.value = a.webUIPath
    editableAutoUpdate.value = a.autoUpdate
    editableServices.value = a.services.map((svc) => ({
      ...svc,
      ports: svc.ports.map((p) => ({ ...p })),
      volumes: svc.volumes.map((v) => ({ ...v })),
      env: svc.env.map((e) => ({ ...e })),
      devices: svc.devices.map((d) => ({ ...d })),
      networks: [...svc.networks],
    }))
    // Anchor the slider write-back to the limits as the server has them, before
    // the slider watchers below flush and start writing through the indices.
    captureSeededLimits()
    activeTabIndex.value = 0
    revealedSecrets.value = new Set()

    // Fetch available tags for each service from the Docker registry.
    // Runs in the background — the Select dropdown populates once the
    // fetch resolves, so the user sees the current tag immediately and
    // the rest appear within a second or two.
    //
    // Wave 34-Y: previously the .then/.catch captured `svc` from the
    // for-loop. If the user clicked a different app while one of
    // those fetches was still in flight, the watcher re-ran with the
    // new app id, editableServices was replaced with fresh copies,
    // and the stale promise would eventually write
    // `svc.availableTags = tags` on a service object that was no
    // longer part of the reactive array. For the first app it looked
    // harmless; for rapid back-and-forth navigation between two apps
    // it clobbered the new app's populated tag list with the old
    // app's results, because two getImageTags calls raced and
    // whichever settled last won. Guard every write by checking that
    // the captured svc reference is still the live entry at the
    // expected index of editableServices before mutating.
    const currentRun = editableServices.value
    currentRun.forEach((svc, idx) => {
      if (!svc.dockerImage) return
      // Pass image name without tag — the registry API expects just the image name
      getImageTags(svc.dockerImage)
        .then((tags) => {
          if (editableServices.value !== currentRun) return
          if (editableServices.value[idx] !== svc) return
          // Ensure current tag is always in the list.
          if (svc.imageTag && !tags.includes(svc.imageTag)) {
            tags.unshift(svc.imageTag)
          }
          svc.availableTags = tags
        })
        .catch(() => {
          if (editableServices.value !== currentRun) return
          if (editableServices.value[idx] !== svc) return
          // Fallback: at least show current tag.
          svc.availableTags = svc.imageTag ? [svc.imageTag, 'latest'] : ['latest']
        })
    })

    // Take the dirty baseline one microtask later, once the watchers below
    // have flushed: they resync the slider indices to the active service's
    // limits and write back through those indices. That write-back is part of
    // loading the app, not a user edit, so it has to land before the baseline.
    Promise.resolve().then(() => {
      seededSnapshot.value = formSnapshot()
    })
  },
  { immediate: true },
)

// Keep slider indices in sync with the active service's stored limits.
// Fires on mount via the app watcher (via activeService) and on every
// tab switch.
watch(
  activeService,
  (svc) => {
    if (!svc) return
    memoryIndex.value = snapIndexToStop(svc.memoryLimitMB, memoryStops.value)
    cpuIndex.value = snapIndexToStop(svc.cpuLimitCores, cpuStops.value)
  },
  { immediate: true },
)

// Push slider changes back onto the active service's editable state
// so Save picks them up. Also re-coerces 0 to null (the "unlimited"
// sentinel matches how the backend stores "no limit").
//
// While the slider sits on the stop the loaded limit snapped to, the loaded
// value is restored instead of the stop value: the user has not moved off the
// position the app arrived on, so the limit must come out of here untouched.
// Without that, seeding the form or switching service tabs was enough to
// rewrite a limit like 1500 MB down to the 1024 MB stop underneath it.
watch(memoryIndex, (idx) => {
  const svc = activeService.value
  if (!svc) return
  const loaded = seededLimits.get(svc.id)
  if (loaded && snapIndexToStop(loaded.memoryLimitMB, memoryStops.value) === idx) {
    svc.memoryLimitMB = loaded.memoryLimitMB
    return
  }
  const v = memoryStops.value[idx] ?? 0
  svc.memoryLimitMB = v === 0 ? null : v
})
watch(cpuIndex, (idx) => {
  const svc = activeService.value
  if (!svc) return
  const loaded = seededLimits.get(svc.id)
  if (loaded && snapIndexToStop(loaded.cpuLimitCores, cpuStops.value) === idx) {
    svc.cpuLimitCores = loaded.cpuLimitCores
    return
  }
  const v = cpuStops.value[idx] ?? 0
  svc.cpuLimitCores = v === 0 ? null : v
})

// ----- Secret-masking helpers -----
function secretKey(svcIdx: number, envIdx: number): string {
  return `${svcIdx}:${envIdx}`
}
function toggleSecret(envIdx: number) {
  const k = secretKey(activeTabIndex.value, envIdx)
  const next = new Set(revealedSecrets.value)
  if (next.has(k)) next.delete(k)
  else next.add(k)
  revealedSecrets.value = next
}

// ----- Display helpers -----
const stateVisuals: Record<ContainerState, { icon: string; classes: string }> = {
  running: { icon: 'CircleCheck', classes: 'text-emerald-500' },
  stopped: { icon: 'Square', classes: 'text-slate-400' },
  unhealthy: { icon: 'CircleAlert', classes: 'text-amber-500' },
  updating: { icon: 'Loader', classes: 'text-blue-500' },
  crashed: { icon: 'CircleX', classes: 'text-red-500' },
}

function formatMemory(mb: number): string {
  if (mb === 0) return t('apps.unlimited')
  if (mb >= 1024) return (mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1) + ' GB'
  return mb + ' MB'
}
function formatCpu(cores: number): string {
  if (cores === 0) return t('apps.unlimited')
  return cores + ' ' + (cores === 1 ? t('apps.core') : t('apps.cores'))
}

const aggregateState = computed<ContainerState>(() =>
  app.value ? aggregateAppState(app.value) : 'stopped',
)

// ----- Protocol options (TCP / UDP / TCP+UDP) -----
const protocolOptions = computed(() => [
  { value: 'tcp' as PortProtocol, label: 'TCP' },
  { value: 'udp' as PortProtocol, label: 'UDP' },
  { value: 'both' as PortProtocol, label: t('apps.tcp_udp') },
])

const restartPolicyOptions = computed(() => [
  { value: 'no' as RestartPolicy, label: t('apps.restart.no') },
  { value: 'always' as RestartPolicy, label: t('apps.restart.always') },
  { value: 'unless-stopped' as RestartPolicy, label: t('apps.restart.unless_stopped') },
  { value: 'on-failure' as RestartPolicy, label: t('apps.restart.on_failure') },
])

// Primary network selector — for the Core card's single-value picker.
// The full networks array stays in the deploy section; this is a
// convenience for the common case of one-network apps.
const primaryNetwork = computed({
  get: () => activeService.value?.networks[0] ?? 'bridge',
  set: (v: string) => {
    if (!activeService.value) return
    activeService.value.networks = [v, ...activeService.value.networks.slice(1)]
  },
})

// ----- Update tracking -----
// Split the services into two buckets: those tracking a rolling tag
// (latest/stable/main/…) and those pinned to a specific version. This
// drives the auto-update card's warning state: if every service is
// pinned, auto-update can't do anything useful and the toggle is
// presented as ineffective.
const servicesWithRollingTag = computed(() =>
  editableServices.value.filter((s) => isRollingTag(s.imageTag)),
)
const autoUpdateIsEffective = computed(
  () => servicesWithRollingTag.value.length > 0,
)

// Manual update check state. The real backend call hits the registry
// manifest endpoint for each service; the fixture just sets a
// timestamp after a fake delay so the UI pattern is visible.
const checkingUpdates = ref(false)
const lastCheckedAt = ref<string | null>(null)
const updatesAvailable = ref<string[]>([]) // service ids with a newer digest
const checkedOnce = ref(false) // true after a check completes (to show "up to date")
const updating = ref(false)

// hasUpdates drives the result banner + the "Update now" button. The
// backend reports availability per app (the store is the unit of
// versioning), so any service flagged means the app has an update.
const hasUpdates = computed(() => updatesAvailable.value.length > 0)

async function checkForUpdatesNow() {
  if (!app.value) return
  checkingUpdates.value = true
  updatesAvailable.value = []
  try {
    const { containersApi } = await import('@/api')
    const { safeCall } = await import('@/api/safeCall')
    const result = await safeCall(
      () => containersApi.checkForUpdates(app.value!.id),
    )
    if (result.data) {
      updatesAvailable.value = result.data.services
        .filter((s) => s.hasUpdate)
        .map((s) => s.serviceId)
    }
    lastCheckedAt.value = new Date().toISOString()
    checkedOnce.value = true
  } catch {
    installs.pushToast('error', t('apps.update_check_failed'))
  } finally {
    checkingUpdates.value = false
  }
}

// performUpdate re-pulls the app's images from the store and recreates the
// containers (PATCH /compose/:id). The pull runs server-side asynchronously,
// so we toast immediately and refresh the app shortly after; volumes and env
// are preserved by the compose update path. `force` re-pulls even when the
// store reports up-to-date (used by the "re-pull anyway" affordance).
async function performUpdate(force = false) {
  if (!app.value || updating.value) return
  updating.value = true
  try {
    const { containersApi } = await import('@/api')
    const { safeCall } = await import('@/api/safeCall')
    const res = await safeCall(() => containersApi.pullAppUpdate(app.value!.id, force))
    if (res.error) {
      installs.pushToast('error', t('apps.update_failed', { error: res.error }))
      return
    }
    installs.pushToast('success', t('apps.update_started'))
    updatesAvailable.value = []
    checkedOnce.value = false
    // The pull is async on the backend; give it a moment then refresh the
    // app list so the app reflects the new image/state instead of looking
    // unchanged (and re-appears on the dashboard rather than vanishing).
    setTimeout(() => { void containersStore.refresh() }, 4000)
  } finally {
    updating.value = false
  }
}

// How this app updates, by source: compose → full compose update;
// standalone → re-pull + recreate; external-stack → not as a unit (reason).
const updateInfo = computed(() => updateEligibility(app.value ?? { source: undefined, managed: undefined }))

// Standalone update: re-pull the image reference and recreate the container,
// preserving config. Backend runs it asynchronously (PATCH /container/:id).
async function performStandaloneUpdate() {
  if (!app.value || updating.value) return
  updating.value = true
  try {
    const ok = await containersStore.recreateExternalContainer(app.value.id, true)
    if (ok) {
      installs.pushToast('success', t('apps.update_started'))
      setTimeout(() => { void containersStore.refreshAll() }, 4000)
    } else {
      installs.pushToast('error', t('apps.update_failed', { error: '' }))
    }
  } finally {
    updating.value = false
  }
}

function formatRelative(iso: string | null): string {
  if (!iso) return t('apps.update_never_checked')
  return dayjs(iso).fromNow()
}

// ----- Lifecycle stubs -----
// Capture title/id before the await so the toast shows the correct
// app name even if the user navigates away mid-flight.
async function onStart() {
  if (!app.value) return
  const { id, title } = app.value
  const ok = await containersStore.startAppUnified(id)
  if (ok) installs.pushToast('success', `${title} started`)
  else installs.pushToast('error', `Failed to start ${title}`)
}
async function onStop() {
  if (!app.value) return
  const { id, title } = app.value
  const ok = await containersStore.stopAppUnified(id)
  if (ok) installs.pushToast('info', `${title} stopped`)
  else installs.pushToast('error', `Failed to stop ${title}`)
}
async function onRestart() {
  if (!app.value) return
  const { id, title } = app.value
  const ok = await containersStore.restartAppUnified(id)
  if (ok) installs.pushToast('success', `${title} restarted`)
  else installs.pushToast('error', `Failed to restart ${title}`)
}
async function onKill() {
  if (!app.value) return
  const { id, title } = app.value
  // Kill = force stop. Use the stop endpoint — the backend sends SIGKILL
  // after the grace period. A double-stop achieves the same as Portainer's
  // Kill button for a stuck container.
  const ok = await containersStore.stopAppUnified(id)
  if (ok) installs.pushToast('info', `${title} killed`)
  else installs.pushToast('error', `Failed to kill ${title}`)
}

function onOpen() {
  if (!app.value?.webUIPort) return
  const host = app.value.webUIHost || window.location.hostname
  const path = app.value.webUIPath || '/'
  const url = `${app.value.webUIProtocol}://${host}:${app.value.webUIPort}${path}`
  window.open(url, '_blank', 'noopener,noreferrer')
}

// ----- Port editor -----
function addPort() {
  if (!activeService.value) return
  activeService.value.ports = [
    ...activeService.value.ports,
    { host: 0, container: 0, protocol: 'tcp' as PortProtocol },
  ]
}
function removePort(index: number) {
  if (!activeService.value) return
  activeService.value.ports = activeService.value.ports.filter((_, i) => i !== index)
}

// ----- Volume editor -----
function addVolume() {
  if (!activeService.value) return
  activeService.value.volumes = [
    ...activeService.value.volumes,
    { hostPath: '', containerPath: '', readOnly: false },
  ]
}
function removeVolume(index: number) {
  if (!activeService.value) return
  activeService.value.volumes = activeService.value.volumes.filter((_, i) => i !== index)
}
function openFolderPicker(index: number) {
  folderPickerTargetIndex.value = index
  folderPickerVisible.value = true
}
function onFolderSelected(path: string) {
  if (folderPickerTargetIndex.value === null || !activeService.value) return
  const target = activeService.value.volumes[folderPickerTargetIndex.value]
  if (target) target.hostPath = path
  folderPickerTargetIndex.value = null
}

// ----- Env editor -----
function addEnvVar() {
  if (!activeService.value) return
  activeService.value.env = [...activeService.value.env, { key: '', value: '' }]
}
function removeEnvVar(index: number) {
  if (!activeService.value) return
  activeService.value.env = activeService.value.env.filter((_, i) => i !== index)
}

// ----- Device editor -----
function addDevice() {
  if (!activeService.value) return
  activeService.value.devices = [
    ...activeService.value.devices,
    { hostPath: '', containerPath: '' },
  ]
}
function removeDevice(index: number) {
  if (!activeService.value) return
  activeService.value.devices = activeService.value.devices.filter((_, i) => i !== index)
}

// ----- YAML viewer -----
const composeYaml = computed(() => {
  if (!app.value) return ''
  // Build from the editable state so the dialog shows unsaved changes.
  const snap: AppEntry = {
    ...app.value,
    title: editableTitle.value,
    iconUrl: editableIconUrl.value || undefined,
    webUIProtocol: editableWebProtocol.value,
    webUIHost: editableWebHost.value,
    webUIPort: editableWebPort.value,
    webUIPath: editableWebPath.value,
    autoUpdate: editableAutoUpdate.value,
    services: editableServices.value,
  }
  return buildComposeYaml(snap, editableServices.value)
})

async function copyYaml() {
  try {
    await navigator.clipboard.writeText(composeYaml.value)
    copyFeedback.value = true
    setTimeout(() => (copyFeedback.value = false), 1500)
  } catch {
    /* clipboard blocked */
  }
}
function doDownload() {
  downloadYaml(`${app.value?.id ?? 'app'}-compose.yml`, composeYaml.value)
}

// ----- Save / uninstall -----
const saving = ref(false)

async function onSave() {
  if (!app.value || saving.value) return
  saving.value = true
  try {
    // Build the body from the editable state + push to the backend.
    const body = {
      title: editableTitle.value,
      iconUrl: editableIconUrl.value || undefined,
      webUIProtocol: editableWebProtocol.value,
      webUIHost: editableWebHost.value,
      webUIPort: editableWebPort.value,
      webUIPath: editableWebPath.value,
      autoUpdate: editableAutoUpdate.value,
      services: editableServices.value,
    }
    const { containersApi } = await import('@/api')
    const { safeCall } = await import('@/api/safeCall')
    const result = await safeCall(
      () => containersApi.updateApp(app.value!.id, body as never),
      () => undefined,
    )
    if (result.error && !result.fixture) {
      installs.pushToast('error', result.error)
      return
    }
    // What we just pushed is the new baseline: the form is clean again and
    // there is nothing being held back from the server any more. The slider
    // anchors move with it, so a limit we just saved is the one a return to
    // its stop restores.
    seededSnapshot.value = formSnapshot()
    captureSeededLimits()
    serverChangedWhileDirty.value = false
    installs.pushToast('success', t('apps.save_success', { name: app.value.title }))
    // The backend applies the new compose (pull + recreate) ASYNCHRONOUSLY and
    // returns before the new YAML is written to disk. Refreshing immediately
    // re-reads the OLD compose and snaps the tag Select back (the "change to
    // latest doesn't stick" bug). Defer: 4s to pick up the freshly-written
    // compose, and again at 15s to reflect the final state in case the async
    // pull/recreate failed and the backend rolled the tag back.
    setTimeout(() => containersStore.refresh(true), 4000)
    setTimeout(() => containersStore.refresh(true), 15000)
  } finally {
    saving.value = false
  }
}

async function confirmUninstall() {
  if (!app.value) return
  const { id, title } = app.value
  uninstallDialogVisible.value = false
  const ok = await containersStore.uninstallApp(id)
  if (ok) {
    installs.pushToast('success', `${title} uninstalled`)
    router.push('/app')
  } else {
    installs.pushToast('error', `Failed to uninstall ${title}`)
  }
}

function goBack() {
  router.back()
}
</script>

<template>
  <!-- App-missing states. Distinguish "still loading" and "load failed"
       from a genuinely-absent app so a valid installed app never shows
       the terminal not-found page during or after its list fetch. -->
  <div v-if="!app" class="flex flex-col items-center justify-center py-16 text-center">
    <!-- Loading: the list hasn't resolved yet -->
    <template v-if="isLoading">
      <div class="mb-4 flex h-16 w-16 items-center justify-center rounded-2xl bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]">
        <AppIcon name="RefreshCw" :size="28" class="animate-spin" />
      </div>
      <p class="text-sm text-[var(--cn-text-muted)]">{{ t('common.loading') }}</p>
    </template>
    <!-- Load failed: the fetch errored — do NOT claim the app is missing -->
    <template v-else-if="containersStore.lastError">
      <div class="mb-4 flex h-16 w-16 items-center justify-center rounded-2xl bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]">
        <AppIcon name="AlertTriangle" :size="32" />
      </div>
      <h2 class="text-lg font-semibold text-[var(--cn-text)]">{{ t('apps.load_error') }}</h2>
      <p class="mt-1 max-w-sm text-sm text-[var(--cn-text-muted)]">{{ containersStore.lastError }}</p>
      <div class="mt-4 flex gap-2">
        <Button :label="t('apps.retry')" @click="reloadApp" />
        <Button severity="secondary" :label="t('apps.back_to_dashboard')" @click="router.push('/app')" />
      </div>
    </template>
    <!-- Genuinely not found: the list loaded successfully but has no app
         with this id (e.g. it was uninstalled). -->
    <template v-else>
      <div class="mb-4 flex h-16 w-16 items-center justify-center rounded-2xl bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]">
        <AppIcon name="Container" :size="32" />
      </div>
      <h2 class="text-lg font-semibold text-[var(--cn-text)]">{{ t('apps.not_found') }}</h2>
      <p class="mt-1 max-w-sm text-sm text-[var(--cn-text-muted)]">{{ t('apps.not_found_desc') }}</p>
      <Button class="mt-4" :label="t('apps.back_to_dashboard')" @click="router.push('/app')" />
    </template>
  </div>

  <div v-else class="relative">
    <!-- Edge-aligned nav buttons on wide screens. On screens wider than
         the content column (≥1200px), the Back link and Close X float
         to the far viewport edges so the content card can fill the
         full-width reading area. On narrower screens they fall inline
         above the card, keeping the legacy stacked layout. The pivot
         is Tailwind's xl: breakpoint (1280px). -->
    <button
      type="button"
      class="absolute left-4 top-6 z-10 hidden items-center gap-1 text-xs text-[var(--cn-text-muted)] hover:text-[var(--cn-text)] xl:inline-flex"
      @click="goBack"
    >
      <AppIcon name="ArrowLeft" :size="13" />
      {{ t('apps.back') }}
    </button>
    <button
      type="button"
      class="absolute right-4 top-5 z-10 hidden h-8 w-8 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] xl:flex"
      :aria-label="t('common.close')"
      @click="router.push('/app')"
    >
      <AppIcon name="X" :size="16" />
    </button>

    <div class="mx-auto flex max-w-4xl flex-col gap-6 p-6 pb-28">
      <!-- Narrow-screen inline fallback — only shown below the xl
           breakpoint. The wide-screen version lives as absolutely-
           positioned buttons on the outer wrapper above. -->
      <div class="flex items-center justify-between xl:hidden">
        <button
          type="button"
          class="inline-flex w-fit items-center gap-1 text-xs text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
          @click="goBack"
        >
          <AppIcon name="ArrowLeft" :size="13" />
          {{ t('apps.back') }}
        </button>
        <button
          type="button"
          class="flex h-8 w-8 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :aria-label="t('common.close')"
          @click="router.push('/app')"
        >
          <AppIcon name="X" :size="16" />
        </button>
      </div>

    <!-- Header card: app identity + primary actions + YAML button -->
    <div class="flex items-start justify-between gap-4 rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-5">
      <div class="flex min-w-0 flex-1 items-start gap-4">
        <div
          class="flex h-16 w-16 shrink-0 items-center justify-center overflow-hidden rounded-2xl text-2xl font-semibold text-white shadow-md"
          :style="{ backgroundColor: app.tileColor ?? '#475569' }"
        >
          <img
            v-if="editableIconUrl"
            :src="editableIconUrl"
            :alt="editableTitle"
            class="h-12 w-12 object-contain"
          />
          <span v-else>{{ editableTitle.charAt(0) }}</span>
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <h1 class="text-xl font-semibold text-[var(--cn-text)]">{{ editableTitle || app.title }}</h1>
            <span
              class="inline-flex items-center gap-1.5 text-xs"
              :class="stateVisuals[aggregateState].classes"
            >
              <AppIcon :name="stateVisuals[aggregateState].icon" :size="12" />
              {{ t(`settings.containers.state.${aggregateState}`) }}
            </span>
          </div>
          <p v-if="app.description" class="mt-1 text-sm text-[var(--cn-text-muted)]">
            {{ app.description }}
          </p>
          <div class="mt-2 text-xs text-[var(--cn-text-muted)]">
            {{ app.services.length }}
            {{ app.services.length === 1 ? t('apps.service_single') : t('apps.service_plural') }}
          </div>
        </div>
      </div>

      <div class="flex shrink-0 flex-col items-end gap-2">
        <div class="flex gap-1">
          <button
            v-if="isManaged"
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :title="t('apps.view_compose')"
            @click="yamlDialogVisible = true"
          >
            <AppIcon name="Code2" :size="14" />
          </button>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :title="t('apps.open_terminal')"
            @click="containerTerminalMode = 'terminal'; containerTerminalVisible = true"
          >
            <AppIcon name="Terminal" :size="14" />
          </button>
        </div>
        <Button
          v-if="app.webUIPort"
          :label="t('apps.open')"
          size="small"
          @click="onOpen"
        >
          <template #icon><AppIcon name="ExternalLink" :size="13" /></template>
        </Button>
      </div>
    </div>

    <!-- Container actions bar — Portainer-style lifecycle controls -->
    <div class="rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-4">
      <div class="mb-3 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
        <AppIcon name="Container" :size="14" />
        {{ t('apps.actions') }}
      </div>
      <div class="flex flex-wrap gap-2">
        <Button
          size="small"
          :severity="aggregateState !== 'running' ? undefined : 'secondary'"
          :disabled="aggregateState === 'running'"
          @click="onStart"
        >
          <template #icon><AppIcon name="Play" :size="13" /></template>
          {{ t('settings.containers.start') }}
        </Button>
        <Button
          size="small"
          severity="secondary"
          :disabled="aggregateState === 'stopped'"
          @click="onStop"
        >
          <template #icon><AppIcon name="Square" :size="13" /></template>
          {{ t('settings.containers.stop') }}
        </Button>
        <Button
          size="small"
          severity="secondary"
          :disabled="aggregateState === 'stopped'"
          @click="onRestart"
        >
          <template #icon><AppIcon name="RotateCw" :size="13" /></template>
          {{ t('settings.containers.restart') }}
        </Button>
        <Button
          size="small"
          severity="warning"
          :disabled="aggregateState === 'stopped'"
          @click="onKill"
        >
          <template #icon><AppIcon name="AlertTriangle" :size="13" /></template>
          {{ t('apps.kill') }}
        </Button>
        <div class="flex-1" />
        <Button
          v-if="isManaged"
          size="small"
          severity="danger"
          @click="uninstallDialogVisible = true"
        >
          <template #icon><AppIcon name="Trash2" :size="13" /></template>
          {{ t('apps.uninstall') }}
        </Button>
      </div>

      <!-- Container status info -->
      <div v-if="activeService" class="mt-4 grid grid-cols-1 gap-x-6 gap-y-2 border-t border-[var(--cn-border)] pt-3 text-xs sm:grid-cols-2">
        <div class="flex items-center justify-between gap-2">
          <span class="text-[var(--cn-text-muted)]">{{ t('apps.container_id') }}</span>
          <span class="font-mono text-[var(--cn-text)]">{{ activeService.id }}</span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-[var(--cn-text-muted)]">{{ t('apps.status') }}</span>
          <span class="flex items-center gap-1.5" :class="stateVisuals[activeService.state]?.classes ?? 'text-slate-400'">
            <AppIcon :name="stateVisuals[activeService.state]?.icon ?? 'Square'" :size="11" />
            {{ t(`settings.containers.state.${activeService.state}`) }}
            <span v-if="activeService.state === 'running' && activeService.uptimeSeconds > 0" class="text-[var(--cn-text-muted)]">
              ({{ dayjs.duration(activeService.uptimeSeconds, 'seconds').humanize() }})
            </span>
          </span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-[var(--cn-text-muted)]">{{ t('apps.image') }}</span>
          <span class="font-mono text-[var(--cn-text)]">{{ activeService.dockerImage }}:{{ activeService.imageTag || 'latest' }}</span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-[var(--cn-text-muted)]">{{ t('apps.restart_policy') }}</span>
          <span class="text-[var(--cn-text)]">{{ activeService.restartPolicy }}</span>
        </div>
        <div v-if="activeService.restartCount > 0" class="flex items-center justify-between gap-2">
          <span class="text-[var(--cn-text-muted)]">{{ t('apps.restart_count') }}</span>
          <span class="text-amber-500">{{ activeService.restartCount }}</span>
        </div>
      </div>
    </div>

    <!-- Service tab strip — hidden when the app has only one service. -->
    <div
      v-if="editableServices.length > 1"
      class="flex gap-1 overflow-x-auto border-b border-[var(--cn-border)]"
    >
      <button
        v-for="(svc, i) in editableServices"
        :key="svc.id"
        type="button"
        :class="[
          'shrink-0 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
          activeTabIndex === i
            ? 'border-blue-500 text-blue-500'
            : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
        ]"
        @click="activeTabIndex = i"
      >
        {{ svc.id }}
      </button>
    </div>

    <!-- ============================================================
         Per-service editable cards. Everything from here binds to
         activeService (v-model depth: activeService.xxx). Wrapped
         in a v-if so switching to a tab with no service doesn't
         render broken bindings.
         ============================================================ -->
    <template v-if="activeService">
      <!-- 1. Core identity -->
      <SettingsCard :title="t('apps.core_settings')" :description="t('apps.core_settings_desc')">
        <div class="flex flex-col gap-4">
          <!-- Docker image + tag row -->
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-[2fr_1fr]">
            <div>
              <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
                {{ t('apps.docker_image') }} *
              </label>
              <div class="relative">
                <InputText
                  v-model="activeService.dockerImage"
                  class="w-full font-mono text-xs"
                  :placeholder="t('apps.docker_image_placeholder')"
                />
                <AppIcon
                  v-if="activeService.dockerImage"
                  name="Check"
                  :size="15"
                  class="absolute right-3 top-1/2 -translate-y-1/2 text-emerald-500"
                />
              </div>
            </div>
            <div>
              <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
                {{ t('apps.tag') }}
              </label>
              <!-- Editable Select: dropdown seeded from availableTags so
                   users see sensible options (latest, stable, pinned
                   versions) but can still type a custom tag. The small
                   "rolling" / "pinned" tag below the select tells the
                   user why auto-update may or may not apply. -->
              <Select
                v-model="activeService.imageTag"
                editable
                :options="activeService.availableTags"
                class="w-full font-mono text-xs"
              />
              <div class="mt-1 text-[10px]">
                <span
                  v-if="isRollingTag(activeService.imageTag)"
                  class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 px-2 py-0.5 text-emerald-500"
                >
                  {{ t('apps.tag_rolling') }}
                </span>
                <span
                  v-else
                  class="inline-flex items-center gap-1 rounded-full bg-[var(--cn-surface-hover)] px-2 py-0.5 text-[var(--cn-text-muted)]"
                >
                  {{ t('apps.tag_pinned') }}
                </span>
              </div>
            </div>
          </div>

          <!-- Title (app-level, editable only when active tab is the first service) -->
          <div v-if="activeTabIndex === 0">
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('apps.title_label') }} *
            </label>
            <InputText
              v-model="editableTitle"
              class="w-full"
              :placeholder="t('apps.title_placeholder')"
            />
          </div>

          <!-- Icon URL with live preview -->
          <div v-if="activeTabIndex === 0">
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('apps.icon_url') }}
            </label>
            <div class="flex items-center gap-2">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)]"
              >
                <img
                  v-if="editableIconUrl"
                  :src="editableIconUrl"
                  :alt="editableTitle"
                  class="h-full w-full object-contain"
                />
                <AppIcon v-else name="Container" :size="18" class="text-[var(--cn-text-muted)]" />
              </div>
              <InputText
                v-model="editableIconUrl"
                class="flex-1 font-mono text-xs"
                :placeholder="t('apps.icon_url_placeholder')"
              />
            </div>
          </div>

          <!-- Web UI composite -->
          <div v-if="activeTabIndex === 0">
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('apps.web_ui') }}
            </label>
            <div class="grid grid-cols-[auto_1fr_auto_1fr] items-center gap-2">
              <Select
                v-model="editableWebProtocol"
                :options="[
                  { value: 'http', label: 'http://' },
                  { value: 'https', label: 'https://' },
                ]"
                option-label="label"
                option-value="value"
                class="w-28"
              />
              <InputText
                v-model="editableWebHost"
                :placeholder="t('apps.web_ui_host')"
                class="w-full"
              />
              <div class="flex items-center gap-1">
                <span class="text-[var(--cn-text-muted)]">:</span>
                <InputText
                  :model-value="editableWebPort != null ? String(editableWebPort) : ''"
                  type="number"
                  class="w-20"
                  @update:model-value="editableWebPort = $event ? Number($event) : null"
                />
              </div>
              <InputText
                v-model="editableWebPath"
                class="w-full"
                placeholder="/"
              />
            </div>
          </div>

          <!-- Primary network selector -->
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('apps.network') }}
            </label>
            <Select
              v-model="primaryNetwork"
              editable
              :options="activeService.networks.length > 0 ? activeService.networks : ['bridge', 'host']"
              class="w-full sm:w-72"
            />
          </div>
        </div>
      </SettingsCard>

      <!-- 2. Ports -->
      <SettingsCard :title="t('apps.ports')" :description="t('apps.ports_desc')">
        <div class="flex flex-col gap-2">
          <div
            v-if="activeService.ports.length > 0"
            class="grid grid-cols-[1fr_1fr_auto_auto] gap-2 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]"
          >
            <span>{{ t('apps.host_port') }}</span>
            <span>{{ t('apps.container_port') }}</span>
            <span class="w-28">{{ t('apps.protocol') }}</span>
            <span class="w-7" />
          </div>
          <div
            v-for="(p, i) in activeService.ports"
            :key="i"
            class="grid grid-cols-[1fr_1fr_auto_auto] items-center gap-2"
          >
            <InputText
              :model-value="String(p.host)"
              type="number"
              class="w-full"
              @update:model-value="p.host = Number($event)"
            />
            <InputText
              :model-value="String(p.container)"
              type="number"
              class="w-full"
              @update:model-value="p.container = Number($event)"
            />
            <Select
              v-model="p.protocol"
              :options="protocolOptions"
              option-label="label"
              option-value="value"
              class="w-28"
            />
            <button
              type="button"
              class="rounded-md p-1.5 text-[var(--cn-text-muted)] hover:bg-red-500/10 hover:text-red-500"
              @click="removePort(i)"
            >
              <AppIcon name="Trash2" :size="14" />
            </button>
          </div>
          <div
            v-if="activeService.ports.length === 0"
            class="py-2 text-center text-xs text-[var(--cn-text-muted)]"
          >
            {{ t('apps.no_ports_click_add') }}
          </div>
          <button
            type="button"
            class="mt-1 inline-flex w-fit items-center gap-1 text-xs text-blue-500 hover:underline"
            @click="addPort"
          >
            <AppIcon name="Plug" :size="12" /> {{ t('apps.add_port') }}
          </button>
        </div>
      </SettingsCard>

      <!-- 3. Volumes (with mini folder picker) -->
      <SettingsCard :title="t('apps.volumes')" :description="t('apps.volumes_desc')">
        <div class="flex flex-col gap-2">
          <div
            v-if="activeService.volumes.length > 0"
            class="grid grid-cols-[1fr_1fr_auto_auto] gap-2 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]"
          >
            <span>{{ t('apps.volume_host') }}</span>
            <span>{{ t('apps.volume_container') }}</span>
            <span class="w-20">{{ t('apps.read_only') }}</span>
            <span class="w-7" />
          </div>
          <div
            v-for="(v, i) in activeService.volumes"
            :key="i"
            class="grid grid-cols-[1fr_1fr_auto_auto] items-center gap-2"
          >
            <div class="relative flex items-center">
              <InputText
                v-model="v.hostPath"
                class="w-full font-mono text-xs pr-9"
                placeholder="/DATA/AppData/..."
              />
              <button
                type="button"
                class="absolute right-1 flex h-7 w-7 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-blue-500"
                :title="t('apps.browse')"
                @click="openFolderPicker(i)"
              >
                <AppIcon name="FolderSearch" :size="14" />
              </button>
            </div>
            <InputText
              v-model="v.containerPath"
              class="w-full font-mono text-xs"
              placeholder="/container/path"
            />
            <div class="flex w-20 justify-center">
              <ToggleSwitch v-model="v.readOnly" />
            </div>
            <button
              type="button"
              class="rounded-md p-1.5 text-[var(--cn-text-muted)] hover:bg-red-500/10 hover:text-red-500"
              @click="removeVolume(i)"
            >
              <AppIcon name="Trash2" :size="14" />
            </button>
          </div>
          <button
            type="button"
            class="mt-1 inline-flex w-fit items-center gap-1 text-xs text-blue-500 hover:underline"
            @click="addVolume"
          >
            <AppIcon name="FolderTree" :size="12" /> {{ t('apps.add_volume') }}
          </button>
        </div>
      </SettingsCard>

      <!-- 4. Environment variables -->
      <SettingsCard :title="t('apps.env_vars')" :description="t('apps.env_vars_desc')">
        <div class="flex flex-col gap-2">
          <div
            v-for="(e, i) in activeService.env"
            :key="i"
            class="flex items-center gap-2"
          >
            <InputText
              v-model="e.key"
              :placeholder="t('apps.env_key')"
              class="w-44 font-mono text-xs"
            />
            <div class="relative flex-1">
              <InputText
                v-model="e.value"
                :type="e.secret && !revealedSecrets.has(secretKey(activeTabIndex, i)) ? 'password' : 'text'"
                :placeholder="t('apps.env_value')"
                class="w-full font-mono text-xs"
              />
              <button
                v-if="e.secret"
                type="button"
                class="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
                @click="toggleSecret(i)"
              >
                <AppIcon
                  :name="revealedSecrets.has(secretKey(activeTabIndex, i)) ? 'EyeOff' : 'Eye'"
                  :size="14"
                />
              </button>
            </div>
            <button
              type="button"
              class="rounded-md p-1.5 text-[var(--cn-text-muted)] hover:bg-red-500/10 hover:text-red-500"
              @click="removeEnvVar(i)"
            >
              <AppIcon name="Trash2" :size="14" />
            </button>
          </div>
          <button
            type="button"
            class="mt-1 inline-flex w-fit items-center gap-1 text-xs text-blue-500 hover:underline"
            @click="addEnvVar"
          >
            <AppIcon name="KeyRound" :size="12" /> {{ t('apps.add_env') }}
          </button>
        </div>
      </SettingsCard>

      <!-- 5. Devices -->
      <SettingsCard :title="t('apps.devices')" :description="t('apps.devices_desc')">
        <div class="flex flex-col gap-2">
          <div
            v-for="(d, i) in activeService.devices"
            :key="i"
            class="grid grid-cols-[1fr_1fr_auto] items-center gap-2"
          >
            <InputText v-model="d.hostPath" placeholder="/dev/dri" class="w-full font-mono text-xs" />
            <InputText v-model="d.containerPath" placeholder="/dev/dri" class="w-full font-mono text-xs" />
            <button
              type="button"
              class="rounded-md p-1.5 text-[var(--cn-text-muted)] hover:bg-red-500/10 hover:text-red-500"
              @click="removeDevice(i)"
            >
              <AppIcon name="Trash2" :size="14" />
            </button>
          </div>
          <div
            v-if="activeService.devices.length === 0"
            class="py-2 text-center text-xs text-[var(--cn-text-muted)]"
          >
            {{ t('apps.no_devices') }}
          </div>
          <button
            type="button"
            class="mt-1 inline-flex w-fit items-center gap-1 text-xs text-blue-500 hover:underline"
            @click="addDevice"
          >
            + {{ t('apps.add_device') }}
          </button>
        </div>
      </SettingsCard>

      <!-- 6. Container command -->
      <SettingsCard :title="t('apps.command')" :description="t('apps.command_desc')">
        <Textarea
          v-model="activeService.command"
          rows="2"
          :placeholder="t('apps.command_placeholder')"
          class="w-full font-mono text-xs"
        />
      </SettingsCard>

      <!-- 7. Resource limit sliders -->
      <SettingsCard :title="t('apps.resource_limits')" :description="t('apps.resource_limits_desc')">
        <div class="mb-6">
          <div class="mb-2 flex items-center justify-between">
            <label class="flex items-center gap-1.5 text-xs font-medium text-[var(--cn-text-muted)]">
              <AppIcon name="MemoryStick" :size="13" /> {{ t('apps.memory_limit') }}
            </label>
            <span class="rounded-md bg-blue-500/15 px-2 py-0.5 text-xs font-semibold text-blue-500">
              {{ formatMemory(memoryValue) }}
            </span>
          </div>
          <Slider v-model="memoryIndex" :min="0" :max="memoryStops.length - 1" :step="1" class="w-full" />
          <div class="mt-2 flex justify-between px-1 text-[10px] text-[var(--cn-text-muted)]">
            <span
              v-for="(stop, i) in memoryStops"
              :key="i"
              :class="memoryIndex === i ? 'font-semibold text-[var(--cn-text)]' : ''"
            >
              {{ stop === 0 ? '∞' : stop >= 1024 ? (stop / 1024).toFixed(stop % 1024 === 0 ? 0 : 1) + 'G' : stop }}
            </span>
          </div>
        </div>
        <div>
          <div class="mb-2 flex items-center justify-between">
            <label class="flex items-center gap-1.5 text-xs font-medium text-[var(--cn-text-muted)]">
              <AppIcon name="Cpu" :size="13" /> {{ t('apps.cpu_limit') }}
            </label>
            <span class="rounded-md bg-blue-500/15 px-2 py-0.5 text-xs font-semibold text-blue-500">
              {{ formatCpu(cpuValue) }}
            </span>
          </div>
          <Slider v-model="cpuIndex" :min="0" :max="cpuStops.length - 1" :step="1" class="w-full" />
          <div class="mt-2 flex justify-between px-1 text-[10px] text-[var(--cn-text-muted)]">
            <span
              v-for="(stop, i) in cpuStops"
              :key="i"
              :class="cpuIndex === i ? 'font-semibold text-[var(--cn-text)]' : ''"
            >
              {{ stop === 0 ? '∞' : stop }}
            </span>
          </div>
        </div>
      </SettingsCard>

      <!-- 8. Runtime -->
      <SettingsCard :title="t('apps.runtime')" :description="t('apps.runtime_desc')">
        <div class="flex flex-col gap-5">
          <div class="flex items-center justify-between">
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('apps.privileged') }}</div>
              <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
                {{ activeService.privileged ? t('apps.privileged_warning') : t('apps.privileged_desc') }}
              </div>
            </div>
            <ToggleSwitch v-model="activeService.privileged" />
          </div>
          <div class="flex items-center justify-between border-t border-[var(--cn-border)] pt-5">
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('apps.restart_policy') }}</div>
              <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
                {{ t('apps.restart_policy_desc') }}
              </div>
            </div>
            <Select
              v-model="activeService.restartPolicy"
              :options="restartPolicyOptions"
              option-label="label"
              option-value="value"
              class="w-48"
            />
          </div>
        </div>
      </SettingsCard>
    </template>

    <!-- Auto-update (app-level) — rolling-tag aware. Auto-update only
         does something useful for services tracking a rolling tag
         (latest / stable / main / release); pinned semver tags never
         change upstream, so the toggle is presented as ineffective
         and the card warns the user in that case. -->
    <SettingsCard v-if="isManaged" :title="t('apps.auto_update')" :description="t('apps.auto_update_desc')">
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div
            class="flex h-10 w-10 items-center justify-center rounded-lg"
            :class="autoUpdateIsEffective
              ? 'bg-[var(--cn-surface-hover)] text-blue-500'
              : 'bg-amber-500/10 text-amber-500'"
          >
            <AppIcon name="RefreshCw" :size="18" />
          </div>
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('apps.auto_update_label') }}
            </div>
            <div class="text-xs text-[var(--cn-text-muted)]">
              {{ !autoUpdateIsEffective
                  ? t('apps.auto_update_all_pinned')
                  : editableAutoUpdate
                    ? t('apps.auto_update_on')
                    : t('apps.auto_update_off') }}
            </div>
          </div>
        </div>
        <ToggleSwitch v-model="editableAutoUpdate" :disabled="!autoUpdateIsEffective" />
      </div>

      <!-- Per-service tag breakdown — makes the rolling vs pinned
           distinction concrete. Users can see at a glance which
           services will actually receive updates. -->
      <div
        v-if="editableServices.length > 0"
        class="mt-4 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3"
      >
        <div class="mb-2 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]">
          {{ t('apps.tag_tracking') }}
        </div>
        <div class="flex flex-col gap-1.5">
          <div
            v-for="svc in editableServices"
            :key="svc.id"
            class="flex items-center justify-between gap-2 text-xs"
          >
            <span class="truncate text-[var(--cn-text)]">{{ svc.id }}</span>
            <span class="flex items-center gap-2">
              <code class="text-[var(--cn-text-muted)]">{{ svc.imageTag }}</code>
              <span
                v-if="isRollingTag(svc.imageTag)"
                class="rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] text-emerald-500"
              >
                {{ t('apps.tag_rolling') }}
              </span>
              <span
                v-else
                class="rounded-full bg-[var(--cn-border)] px-2 py-0.5 text-[10px] text-[var(--cn-text-muted)]"
              >
                {{ t('apps.tag_pinned') }}
              </span>
            </span>
          </div>
        </div>
      </div>

      <!-- Result banner — shown after a check completes so the user
           actually sees the outcome instead of nothing. -->
      <div
        v-if="checkedOnce && hasUpdates"
        class="mt-4 flex items-center gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-xs text-amber-600 dark:text-amber-400"
      >
        <AppIcon name="ArrowUpCircle" :size="15" />
        <span>{{ t('apps.update_available_n', { n: updatesAvailable.length }, updatesAvailable.length) }}</span>
      </div>
      <div
        v-else-if="checkedOnce && !hasUpdates"
        class="mt-4 flex items-center gap-2 rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-3 text-xs text-emerald-600 dark:text-emerald-400"
      >
        <AppIcon name="CheckCircle2" :size="15" />
        <span>{{ t('apps.update_up_to_date') }}</span>
      </div>

      <!-- Check + perform-update actions + last-checked timestamp. The
           "Update now" button only acts when there's an update; a small
           "re-pull anyway" link forces a recreate even when up to date. -->
      <div class="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-[var(--cn-border)] pt-4">
        <div class="text-xs text-[var(--cn-text-muted)]">
          <span class="font-medium text-[var(--cn-text)]">{{ t('apps.update_last_checked') }}:</span>
          {{ formatRelative(lastCheckedAt) }}
        </div>
        <div class="flex items-center gap-2">
          <button
            v-if="checkedOnce && !hasUpdates && !updating"
            type="button"
            class="text-[11px] text-[var(--cn-text-muted)] underline-offset-2 hover:underline"
            @click="performUpdate(true)"
          >
            {{ t('apps.update_repull') }}
          </button>
          <Button
            :label="checkingUpdates ? t('apps.update_checking') : t('apps.update_check_now')"
            size="small"
            severity="secondary"
            :disabled="checkingUpdates || updating"
            @click="checkForUpdatesNow"
          >
            <template #icon>
              <AppIcon name="RefreshCw" :size="13" :class="checkingUpdates ? 'animate-spin' : ''" />
            </template>
          </Button>
          <Button
            v-if="hasUpdates"
            :label="updating ? t('apps.update_updating') : t('apps.update_now')"
            size="small"
            :disabled="updating"
            @click="performUpdate(false)"
          >
            <template #icon>
              <AppIcon name="ArrowUpCircle" :size="13" :class="updating ? 'animate-spin' : ''" />
            </template>
          </Button>
        </div>
      </div>
    </SettingsCard>

    <!-- Updates for unmanaged apps. Never silently hidden: standalone
         containers get a real re-pull+recreate action; external stacks get
         a clear reason why they can't be updated as a unit. -->
    <SettingsCard v-if="!isManaged" :title="t('apps.update_title')" :description="t('apps.update_desc')">
      <div v-if="updateInfo.method === 'recreate'" class="flex flex-wrap items-center justify-between gap-3">
        <p class="max-w-md text-xs text-[var(--cn-text-muted)]">{{ t('apps.update_recreate_desc') }}</p>
        <Button
          :label="updating ? t('apps.update_updating') : t('apps.update_recreate')"
          size="small"
          :disabled="updating"
          @click="performStandaloneUpdate"
        >
          <template #icon>
            <AppIcon name="ArrowUpCircle" :size="13" :class="updating ? 'animate-spin' : ''" />
          </template>
        </Button>
      </div>
      <div
        v-else
        class="flex items-start gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3 text-xs text-[var(--cn-text-muted)]"
      >
        <AppIcon name="Info" :size="15" class="mt-0.5 shrink-0" />
        <span>{{ t('apps.update_external_stack') }}</span>
      </div>
    </SettingsCard>

    <!-- Danger zone removed — Uninstall button is now in the Actions bar above -->

    <!-- Sticky save bar — fades in when the form is dirty. -->
    <Transition
      enter-from-class="opacity-0 translate-y-4"
      enter-to-class="opacity-100 translate-y-0"
      leave-from-class="opacity-100 translate-y-0"
      leave-to-class="opacity-0 translate-y-4"
      enter-active-class="transition-all duration-200"
      leave-active-class="transition-all duration-200"
    >
      <div
        v-if="isDirty && isManaged"
        class="fixed bottom-4 left-1/2 z-30 flex -translate-x-1/2 items-center gap-3 rounded-full border border-[var(--cn-border)] bg-[var(--cn-surface)] px-5 py-2 shadow-lg"
      >
        <span class="text-xs text-[var(--cn-text-muted)]">
          {{ t('apps.unsaved_changes') }}
        </span>
        <!-- A refresh landed while the form was dirty and we kept the form.
             Say so, rather than letting the read-only numbers move while the
             fields silently disagree with the server. -->
        <span
          v-if="serverChangedWhileDirty"
          class="flex items-center gap-1 text-[11px] text-amber-500"
          :title="t('apps.server_changed_hint')"
        >
          <AppIcon name="Info" :size="12" />
          {{ t('apps.server_changed') }}
        </span>
        <Button :label="saving ? t('common.saving') : t('common.save')" size="small" :disabled="saving" :loading="saving" @click="onSave">
          <template #icon><AppIcon name="Save" :size="13" /></template>
        </Button>
      </div>
    </Transition>

    <!-- Folder picker dialog -->
    <FolderPickerDialog
      v-model:visible="folderPickerVisible"
      start-path="/DATA"
      @select="onFolderSelected"
    />

    <!-- Compose YAML dialog -->
    <Dialog
      v-model:visible="yamlDialogVisible"
      modal
      :style="{ width: '42rem' }"
      :header="t('apps.compose_title')"
      :dismissable-mask="true"
    >
      <pre class="max-h-[60vh] overflow-auto rounded-lg bg-[var(--cn-bg)] p-4 font-mono text-xs text-[var(--cn-text)]">{{ composeYaml }}</pre>
      <template #footer>
        <Button :label="t('common.close')" severity="secondary" @click="yamlDialogVisible = false" />
        <Button
          :label="copyFeedback ? t('apps.copied') : t('apps.copy')"
          severity="secondary"
          @click="copyYaml"
        >
          <template #icon><AppIcon name="Copy" :size="13" /></template>
        </Button>
        <Button :label="t('apps.download_yaml')" @click="doDownload">
          <template #icon><AppIcon name="Download" :size="13" /></template>
        </Button>
      </template>
    </Dialog>

    <!-- Uninstall confirm -->
    <Dialog
      v-model:visible="uninstallDialogVisible"
      modal
      :style="{ width: '26rem' }"
      :header="t('apps.uninstall_title')"
      :dismissable-mask="true"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('apps.uninstall_warning', { name: app.title }) }}
      </p>
      <template #footer>
        <Button :label="t('common.cancel')" severity="secondary" @click="uninstallDialogVisible = false" />
        <Button :label="t('apps.uninstall')" severity="danger" @click="confirmUninstall" />
      </template>
    </Dialog>
    <!-- Per-container terminal / logs dialog -->
    <ContainerTerminalDialog
      v-model:visible="containerTerminalVisible"
      :container-id="app.services[0]?.id ?? app.id"
      :container-name="app.title"
      :mode="containerTerminalMode"
    />
    </div>
  </div>
</template>
