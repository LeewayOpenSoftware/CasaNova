<script setup lang="ts">
// Storage settings — Stage 5.5 landing.
//
// Lists physical disks with SMART health, mount state, and the UUID
// the real v0.9 backend uses as the mount key. Actions surface as
// per-row buttons: mount / unmount / format. Format is wired to a
// confirmation dialog because it's destructive.

import { ref, computed, onMounted } from 'vue'
import { useDevMode } from '@/composables/useDevMode'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import Menu from 'primevue/menu'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import ToggleSwitch from 'primevue/toggleswitch'
import type { MenuItem } from 'primevue/menuitem'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import DriveSetupWizard from '@/components/storage/DriveSetupWizard.vue'
import type { DiskEntry, SmartHealth } from '@/types/storage'
import { formatBytes } from '@/stores/files'
import { storageApi } from '@/api'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()
const { devMode, advancedMode } = useDevMode()

// Drive Setup Wizard visibility — controlled by the launcher card.
// Triggers a disk list refresh when the user closes after completing
// so the table reflects the new mount state immediately.
const wizardVisible = ref(false)

// Live disk list from backend.
const disks = ref<DiskEntry[]>([])
const fixtureMode = ref(false)

// Reusable disk-list refresher — used by onMounted, mount/unmount/
// format actions, and the wizard's `completed` event.
async function refreshDisks(): Promise<void> {
  const result = await safeCall(() => storageApi.listDisks())
  fixtureMode.value = result.fixture
  if (result.data) {
    disks.value = result.data as unknown as DiskEntry[]
  }
}

onMounted(() => {
  void refreshDisks()
})

const healthVisuals: Record<SmartHealth, { icon: string; classes: string }> = {
  good: { icon: 'CircleCheck', classes: 'text-emerald-500' },
  warning: { icon: 'AlertTriangle', classes: 'text-amber-500' },
  critical: { icon: 'AlertTriangle', classes: 'text-red-500' },
  unknown: { icon: 'CircleHelp', classes: 'text-slate-400' },
}

// ----- Disk standby (HDD spin-down) -----
// Inspired by OpenMediaVault + ZimaOS: rotational disks idle most of
// the day in a home server, so letting them spin down saves ~5 W per
// drive and extends bearing life. Only shown when the host has at
// least one rotational disk — pure-SSD systems hide the setting.
// The timeout is persisted to localStorage for now; real backend call
// is `hdparm -S <value>` on each rotational device.
const hasRotational = computed(() => disks.value.some((d) => d.rotational))
const STANDBY_KEY = 'casanova.storage.standby'
const standbyMinutes = ref<number>(0)
const standbyOptions = computed(() => [
  { value: 0, label: t('settings.storage.standby_never') },
  { value: 10, label: t('settings.storage.standby_minutes', { n: 10 }) },
  { value: 20, label: t('settings.storage.standby_minutes', { n: 20 }) },
  { value: 30, label: t('settings.storage.standby_minutes', { n: 30 }) },
  { value: 60, label: t('settings.storage.standby_hour') },
])
// Standby is server-authoritative — fetch on mount, fall back to the
// localStorage cache only when the backend is unreachable (offline /
// fixture mode). Saving writes both: the backend is the truth, the LS
// cache survives a flaky reconnect.
onMounted(async () => {
  const remote = await safeCall(() => storageApi.getStandbySettings())
  if (remote.data && typeof remote.data.minutes === 'number') {
    standbyMinutes.value = remote.data.minutes
    localStorage.setItem(STANDBY_KEY, String(remote.data.minutes))
    return
  }
  const raw = localStorage.getItem(STANDBY_KEY)
  if (raw) standbyMinutes.value = Number(raw) || 0
})
async function onStandbyChange(v: number) {
  const prev = standbyMinutes.value
  standbyMinutes.value = v
  // Persist to backend first; only update the LS cache if the backend
  // accepted the change. That way we never strand the UI showing a
  // value the server rejected (e.g. permission denied).
  const res = await safeCall(() => storageApi.setStandbySettings(v))
  if (res.error) {
    standbyMinutes.value = prev
    return
  }
  localStorage.setItem(STANDBY_KEY, String(v))
}

// Aggregate summary card — total size, mounted size, free space across
// all mounted disks. Uses a fake "used" ratio of 42% so the progress
// bar has meaningful paint without building a full per-disk usage API.
const totals = computed(() => {
  const totalBytes = disks.value.reduce((acc, d) => acc + d.sizeBytes, 0)
  const mounted = disks.value.filter((d) => d.mountPoint)
  const mountedBytes = mounted.reduce((acc, d) => acc + d.sizeBytes, 0)
  const usedBytes = disks.value.reduce((acc, d) => acc + (d.usedBytes ?? 0), 0)
  return {
    totalBytes,
    mountedBytes,
    diskCount: disks.value.length,
    mountedCount: mounted.length,
    usedRatio: totalBytes > 0 ? usedBytes / totalBytes : 0,
  }
})

// ----- Mount / Unmount -----
const mountLoading = ref<string | null>(null) // device path being mounted/unmounted

async function handleMount(disk: DiskEntry) {
  mountLoading.value = disk.device
  const mountResult = await safeCall(() => storageApi.mountDisk(disk.device))
  if (mountResult.error) {
    mountLoading.value = null
    try {
      const { useInstallProgressStore } = await import('@/stores/installProgress')
      useInstallProgressStore().pushToast('error', `Mount failed: ${mountResult.error}`)
    } catch (err) {
      console.error('StorageSettings: failed to load install-progress store', err)
    }
    return
  }
  const result = await safeCall(() => storageApi.listDisks())
  if (result.data) disks.value = result.data as unknown as DiskEntry[]
  mountLoading.value = null
}

async function handleUnmount(disk: DiskEntry) {
  mountLoading.value = disk.device
  const unmountResult = await safeCall(() => storageApi.unmountDisk(disk.device))
  if (unmountResult.error) {
    mountLoading.value = null
    try {
      const { useInstallProgressStore } = await import('@/stores/installProgress')
      useInstallProgressStore().pushToast('error', `Unmount failed: ${unmountResult.error}`)
    } catch (err) {
      console.error('StorageSettings: failed to load install-progress store', err)
    }
    return
  }
  const result = await safeCall(() => storageApi.listDisks())
  if (result.data) disks.value = result.data as unknown as DiskEntry[]
  mountLoading.value = null
}

// ----- Format confirmation -----
const formatTarget = ref<DiskEntry | null>(null)
const formatDialogVisible = ref(false)
// Filesystem choice for the format dialog. Matches the FormatFsType the
// API client accepts; the backend dispatches to mkfs.<fs> when present.
const formatFs = ref<storageApi.FormatFsType>('ext4')
const formatFsOptions: { value: storageApi.FormatFsType; label: string; hint: string }[] = [
  { value: 'ext4', label: 'ext4', hint: 'Default Linux — broadest tool support' },
  { value: 'btrfs', label: 'btrfs', hint: 'Snapshots, checksumming, compression' },
  { value: 'xfs', label: 'xfs', hint: 'High throughput, large files' },
  { value: 'f2fs', label: 'f2fs', hint: 'Optimized for SSD / flash' },
  { value: 'exfat', label: 'exFAT', hint: 'Cross-platform (Win / macOS / Linux)' },
  { value: 'vfat', label: 'FAT32', hint: 'Legacy compatibility' },
  { value: 'ntfs', label: 'NTFS', hint: 'Windows-native (read/write via ntfs-3g)' },
]
function requestFormat(disk: DiskEntry) {
  formatTarget.value = disk
  formatDialogVisible.value = true
}

// ----- Resize -----
// Online grow only — confirm-then-run. The backend rejects unsupported
// filesystems and unmounted partitions, but we gate the menu item ahead
// of time so the user doesn't see a "not supported" error for ext-on-a-
// USB-stick.
const resizeTarget = ref<DiskEntry | null>(null)
const resizeDialogVisible = ref(false)
const resizeBusy = ref(false)
const resizeResult = ref<storageApi.ResizeResult | null>(null)
const resizeError = ref('')
function requestResize(disk: DiskEntry) {
  resizeTarget.value = disk
  resizeResult.value = null
  resizeError.value = ''
  resizeDialogVisible.value = true
}
async function confirmResize() {
  if (!resizeTarget.value || resizeBusy.value) return
  resizeBusy.value = true
  resizeError.value = ''
  const res = await safeCall(() => storageApi.resizeDisk(resizeTarget.value!.device))
  resizeBusy.value = false
  if (res.error) {
    resizeError.value = res.error
    return
  }
  resizeResult.value = res.data ?? null
  // Refresh the disk list so the new size lands in the table.
  const list = await safeCall(() => storageApi.listDisks())
  if (list.data) disks.value = list.data as unknown as DiskEntry[]
}

// ----- Benchmark -----
const benchTarget = ref<DiskEntry | null>(null)
const benchDialogVisible = ref(false)
const benchMode = ref<'quick' | 'deep'>('quick')
const benchBusy = ref(false)
const benchResult = ref<storageApi.BenchmarkResult | null>(null)
const benchError = ref('')
function requestBenchmark(disk: DiskEntry) {
  benchTarget.value = disk
  benchResult.value = null
  benchError.value = ''
  benchMode.value = 'quick'
  benchDialogVisible.value = true
}
async function runBenchmark() {
  if (!benchTarget.value || benchBusy.value) return
  benchBusy.value = true
  benchError.value = ''
  const res = await safeCall(() =>
    storageApi.benchmarkDisk(benchTarget.value!.device, benchMode.value),
  )
  benchBusy.value = false
  if (res.error) {
    benchError.value = res.error
    return
  }
  benchResult.value = res.data ?? null
}

// ----- Merge into pool -----
// The wizard for first-time pool creation lives further down; this menu
// action targets the simple "add this disk into an existing pool" case.
// If no pools exist we redirect the user to the create-pool wizard.
const mergeTarget = ref<DiskEntry | null>(null)
const mergeDialogVisible = ref(false)
function requestMerge(disk: DiskEntry) {
  mergeTarget.value = disk
  mergeDialogVisible.value = true
}
async function confirmFormat() {
  if (!formatTarget.value) return
  const device = formatTarget.value.device
  // Keep dialog open until format succeeds — closing early hides errors
  // from a destructive operation.
  const formatResult = await safeCall(
    () => storageApi.formatDisk(device, formatFs.value),
  )
  if (formatResult.error) {
    try {
      const { useInstallProgressStore } = await import('@/stores/installProgress')
      useInstallProgressStore().pushToast('error', `Format failed: ${formatResult.error}`)
    } catch (err) {
      console.error('StorageSettings: failed to load install-progress store', err)
    }
    return // Keep dialog open so user can retry or cancel
  }
  formatDialogVisible.value = false
  formatTarget.value = null
  // Refresh the disk list after format completes.
  const result = await safeCall(() => storageApi.listDisks())
  if (result.data) disks.value = result.data as unknown as DiskEntry[]
}

// ----- Per-disk action submenu -----
// PrimeVue Menu in popup mode. One Menu instance per disk; each disk
// row owns its own ref so clicking the kebab on one disk doesn't
// dismiss the menu on another. The menu groups destructive + heavy
// operations (Format, Resize, Merge, Benchmark) so the happy-path
// Mount/Unmount button stays prominent and unambiguous.
const menuRefs = ref<Record<string, InstanceType<typeof Menu> | null>>({})

function setMenuRef(device: string, el: unknown) {
  menuRefs.value[device] = el as InstanceType<typeof Menu> | null
}

function openMenu(disk: DiskEntry, event: MouseEvent) {
  menuRefs.value[disk.device]?.toggle(event)
}

function diskMenuItems(disk: DiskEntry): MenuItem[] {
  return [
    {
      label: t('settings.storage.menu_benchmark'),
      icon: 'cn-bench',
      disabled: !disk.mountPoint,
      command: () => requestBenchmark(disk),
    },
    {
      label: t('settings.storage.menu_resize'),
      icon: 'cn-resize',
      disabled: !disk.mountPoint,
      command: () => requestResize(disk),
    },
    {
      label: t('settings.storage.menu_merge'),
      icon: 'cn-merge',
      disabled: !disk.mountPoint,
      command: () => requestMerge(disk),
    },
    { separator: true },
    {
      label: t('settings.storage.format'),
      icon: 'cn-format',
      class: 'cn-menu-danger',
      command: () => requestFormat(disk),
    },
  ]
}

// ----- SMART Disk Health Monitoring -----
import type { SmartDiskInfo } from '@/api/storage'

const smartDisks = ref<SmartDiskInfo[]>([])
const smartLoading = ref(false)
const smartNoSmartctl = ref(false)
const smartExpanded = ref<Record<string, boolean>>({})
const smartTestLoading = ref<string | null>(null)

async function loadSmartData() {
  smartLoading.value = true
  try {
    const result = await safeCall(() => storageApi.getSmartSummary())
    if (result.data) {
      const resp = result.data as { success: number; message: string; data: SmartDiskInfo[] }
      smartDisks.value = resp.data ?? []
      smartNoSmartctl.value = resp.message === 'smartctl not found'
    }
  } catch {
    smartDisks.value = []
  } finally {
    smartLoading.value = false
  }
}

function toggleSmartExpand(device: string) {
  smartExpanded.value[device] = !smartExpanded.value[device]
}

function formatPowerOnHours(hours: number): string {
  if (hours <= 0) return '0h'
  const days = Math.floor(hours / 24)
  if (days < 365) {
    return t('settings.storage.smart_days', { n: days })
  }
  const years = Math.floor(days / 365)
  const remainDays = days % 365
  return t('settings.storage.smart_years', { y: years, d: remainDays })
}

function tempColorClass(temp: number | null): string {
  if (temp === null) return 'text-[var(--cn-text-muted)]'
  if (temp < 45) return 'text-emerald-500'
  if (temp <= 55) return 'text-amber-500'
  return 'text-red-500'
}

async function runTest(device: string, type: 'short' | 'long') {
  smartTestLoading.value = device
  try {
    await safeCall(() => storageApi.runSmartTest(device, type))
  } finally {
    smartTestLoading.value = null
  }
}

onMounted(() => {
  loadSmartData()
  loadSmartSchedules()
  loadSmartResults()
})

// ----- SMART Test Scheduling -----
import type { SmartScheduleEntry, SmartDeviceResults } from '@/api/storage'

const smartSchedules = ref<SmartScheduleEntry[]>([])
const smartScheduleLoading = ref(false)
const smartScheduleDialogVisible = ref(false)
const smartScheduleSaving = ref(false)

const scheduleForm = ref({
  device: '',
  test_type: 'short' as 'short' | 'long',
  interval: 'weekly' as 'daily' | 'weekly' | 'monthly',
  day_of_week: 0,
  hour: 3,
  enabled: true,
})

const testTypeOptions = computed(() => [
  { value: 'short', label: t('settings.storage.smart_run_short') },
  { value: 'long', label: t('settings.storage.smart_run_long') },
])

const intervalOptions = computed(() => [
  { value: 'daily', label: t('settings.storage.smart_schedule_daily') },
  { value: 'weekly', label: t('settings.storage.smart_schedule_weekly') },
  { value: 'monthly', label: t('settings.storage.smart_schedule_monthly') },
])

const dayOfWeekLabels = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

const hourOptions = Array.from({ length: 24 }, (_, i) => ({
  value: i,
  label: `${String(i).padStart(2, '0')}:00`,
}))

const smartDeviceOptions = computed(() =>
  smartDisks.value
    .filter((d) => d.smart_available)
    .map((d) => ({ value: d.device, label: `${d.model || d.device} (${d.device})` })),
)

async function loadSmartSchedules() {
  smartScheduleLoading.value = true
  try {
    const result = await safeCall(() => storageApi.getSmartSchedules())
    if (result.data) {
      const resp = result.data as { success: number; data: SmartScheduleEntry[] }
      smartSchedules.value = resp.data ?? []
    }
  } catch {
    smartSchedules.value = []
  } finally {
    smartScheduleLoading.value = false
  }
}

function openScheduleDialog() {
  scheduleForm.value = {
    device: smartDeviceOptions.value.length > 0 ? smartDeviceOptions.value[0].value : '',
    test_type: 'short',
    interval: 'weekly',
    day_of_week: 0,
    hour: 3,
    enabled: true,
  }
  smartScheduleDialogVisible.value = true
}

function editSchedule(s: SmartScheduleEntry) {
  scheduleForm.value = {
    device: s.device,
    test_type: s.test_type,
    interval: s.interval,
    day_of_week: s.day_of_week,
    hour: s.hour,
    enabled: s.enabled,
  }
  smartScheduleDialogVisible.value = true
}

async function saveSchedule() {
  smartScheduleSaving.value = true
  try {
    await safeCall(() => storageApi.postSmartSchedule(scheduleForm.value))
    smartScheduleDialogVisible.value = false
    await loadSmartSchedules()
  } finally {
    smartScheduleSaving.value = false
  }
}

async function toggleScheduleEnabled(s: SmartScheduleEntry) {
  await safeCall(() =>
    storageApi.postSmartSchedule({
      device: s.device,
      test_type: s.test_type,
      interval: s.interval,
      day_of_week: s.day_of_week,
      hour: s.hour,
      enabled: !s.enabled,
    }),
  )
  await loadSmartSchedules()
}

async function deleteSchedule(device: string) {
  await safeCall(() => storageApi.deleteSmartSchedule(device))
  await loadSmartSchedules()
}

function formatNextRun(dateStr: string): string {
  if (!dateStr) return t('settings.storage.smart_schedule_never')
  try {
    const d = new Date(dateStr)
    return d.toLocaleString()
  } catch {
    return dateStr
  }
}

// ----- SMART Test Results -----
const smartResults = ref<SmartDeviceResults[]>([])
const smartResultsLoading = ref(false)
const smartResultsExpanded = ref<Record<string, boolean>>({})

async function loadSmartResults() {
  smartResultsLoading.value = true
  try {
    const result = await safeCall(() => storageApi.getSmartResults())
    if (result.data) {
      const resp = result.data as { success: number; data: SmartDeviceResults[] }
      smartResults.value = (resp.data ?? []).filter((d) => d.tests && d.tests.length > 0)
    }
  } catch {
    smartResults.value = []
  } finally {
    smartResultsLoading.value = false
  }
}

function toggleResultsExpand(device: string) {
  smartResultsExpanded.value[device] = !smartResultsExpanded.value[device]
}

function resultStatusClass(passed: boolean, status: string): string {
  if (status.toLowerCase().includes('progress')) return 'text-blue-500'
  return passed ? 'text-emerald-500' : 'text-red-500'
}

// ----- ZFS Pool Management (Developer Mode) -----
// This is a visual/frontend-only implementation for now.
// Real ZFS commands will be wired when the backend endpoint ships.
const zfsPools = ref<Array<{
  name: string
  status: 'ONLINE' | 'DEGRADED' | 'FAULTED' | 'OFFLINE'
  raidLevel: string
  totalBytes: number
  usedBytes: number
  freeBytes: number
  fragmentation: number
  dedup: number
  compression: string
  disks: Array<{ device: string; state: string; errors: number }>
  lastScrub: string | null
}>>([])
const zfsLoading = ref(false)
const zfsSupported = ref(false)
const showCreatePool = ref(false)
const newPool = ref({
  name: 'tank',
  raidLevel: 'mirror',
  selectedDisks: [] as string[],
  compression: 'lz4',
  ashift: 12,
})

const raidLevels = [
  { value: 'stripe', label: 'Stripe (RAID 0)', description: 'Maximum performance, no redundancy. If one disk fails, all data is lost.', minDisks: 2 },
  { value: 'mirror', label: 'Mirror (RAID 1)', description: 'Full redundancy. Data is mirrored across all disks. Can survive N-1 disk failures.', minDisks: 2 },
  { value: 'raidz1', label: 'RAIDZ1 (RAID 5)', description: 'Single parity. Can survive 1 disk failure. Good balance of space and safety.', minDisks: 3 },
  { value: 'raidz2', label: 'RAIDZ2 (RAID 6)', description: 'Double parity. Can survive 2 disk failures. Recommended for large arrays.', minDisks: 4 },
  { value: 'raidz3', label: 'RAIDZ3', description: 'Triple parity. Can survive 3 disk failures. Maximum safety for critical data.', minDisks: 5 },
]

const compressionOptions = [
  { value: 'off', label: 'Off' },
  { value: 'lz4', label: 'LZ4 (recommended)' },
  { value: 'zstd', label: 'Zstandard' },
  { value: 'gzip', label: 'Gzip' },
]

const unmountedDisks = computed(() => disks.value.filter((d) => !d.mountPoint))

const selectedRaidInfo = computed(() => raidLevels.find((r) => r.value === newPool.value.raidLevel))

const canCreatePool = computed(() => {
  const minDisks = selectedRaidInfo.value?.minDisks ?? 2
  return newPool.value.name.trim().length > 0 && newPool.value.selectedDisks.length >= minDisks
})

// Live ZFS detection + inventory. The Wave 3 backend lights up only
// when zpool/zfs are present on the host (detect-only — we don't
// bundle the userland). zfsStatus carries `available`, version, count;
// zfsRealPools holds the inventory rows the UI renders.
const zfsStatus = ref<storageApi.ZFSStatus | null>(null)
const zfsRealPools = ref<storageApi.ZFSPool[]>([])
const scrubBusy = ref<string | null>(null)

async function checkZfsSupport() {
  zfsLoading.value = true
  try {
    const stRes = await safeCall(() => storageApi.getZFSStatus())
    if (stRes.data) {
      zfsStatus.value = stRes.data
      zfsSupported.value = stRes.data.available
    } else {
      zfsSupported.value = false
    }
    if (zfsSupported.value) {
      const poolsRes = await safeCall(() => storageApi.getZFSPools())
      if (poolsRes.data) zfsRealPools.value = poolsRes.data
    } else {
      zfsRealPools.value = []
    }
  } finally {
    zfsLoading.value = false
  }
}

function fmtBytes(n: number): string {
  if (!n) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}

async function startScrub(pool: string) {
  scrubBusy.value = pool
  const res = await safeCall(() => storageApi.startZFSScrub(pool))
  scrubBusy.value = null
  if (!res.error) await checkZfsSupport()
}
async function stopScrub(pool: string) {
  scrubBusy.value = pool
  const res = await safeCall(() => storageApi.stopZFSScrub(pool))
  scrubBusy.value = null
  if (!res.error) await checkZfsSupport()
}
async function exportPool(pool: string) {
  if (!window.confirm(t('settings.storage.zfs_export_confirm', { pool }))) return
  const res = await safeCall(() => storageApi.exportZFSPool(pool, false))
  if (!res.error) await checkZfsSupport()
}

// ----- ZFS Tier 3: create / destroy / replace (advanced mode only) -----
// All three live behind useDevMode().advancedMode. Backend still
// enforces PermStorageWrite + handler-level safety (system disk,
// confirmName match, topology min-devices) — the UI gate is the
// "I read the docs" affordance, not the only check.
const createPoolVisible = ref(false)
const createPoolBusy = ref(false)
const createPoolError = ref('')
const createPoolDryRunOutput = ref('')
const createPoolForm = ref<{
  name: string
  topology: storageApi.ZFSTopology
  devices: string[]
  mountpoint: string
  force: boolean
}>({
  name: '',
  topology: 'mirror',
  devices: [],
  mountpoint: '',
  force: false,
})
const topologyOptions: { value: storageApi.ZFSTopology; label: string; min: number; hint: string }[] = [
  { value: 'stripe', label: 'Stripe (RAID 0)', min: 1, hint: 'No redundancy. Sum of sizes. One disk lost = pool lost.' },
  { value: 'mirror', label: 'Mirror (RAID 1)', min: 2, hint: 'Every disk has a full copy. Survives N-1 failures.' },
  { value: 'raidz1', label: 'RAID-Z1', min: 3, hint: 'One parity drive. Survives 1 failure.' },
  { value: 'raidz2', label: 'RAID-Z2', min: 4, hint: 'Two parity drives. Survives 2 failures.' },
  { value: 'raidz3', label: 'RAID-Z3', min: 5, hint: 'Three parity drives. Survives 3 failures.' },
]
function openCreatePool() {
  createPoolForm.value = {
    name: '', topology: 'mirror', devices: [],
    mountpoint: '', force: false,
  }
  createPoolError.value = ''
  createPoolDryRunOutput.value = ''
  createPoolVisible.value = true
}
function toggleCreateDevice(device: string) {
  const idx = createPoolForm.value.devices.indexOf(device)
  if (idx >= 0) createPoolForm.value.devices.splice(idx, 1)
  else createPoolForm.value.devices.push(device)
}
const currentTopology = computed(() =>
  topologyOptions.find((o) => o.value === createPoolForm.value.topology),
)
const createPoolReady = computed(() => {
  if (!createPoolForm.value.name) return false
  if (!currentTopology.value) return false
  return createPoolForm.value.devices.length >= currentTopology.value.min
})
async function previewCreatePool() {
  createPoolBusy.value = true
  createPoolError.value = ''
  createPoolDryRunOutput.value = ''
  const res = await safeCall(() =>
    storageApi.createZFSPool({
      ...createPoolForm.value,
      mountpoint: createPoolForm.value.mountpoint || undefined,
      dryRun: true,
    }),
  )
  createPoolBusy.value = false
  if (res.error) {
    createPoolError.value = res.error
    return
  }
  createPoolDryRunOutput.value = res.data?.output ?? '(zpool returned no output for the dry-run)'
}
async function submitCreatePool() {
  createPoolBusy.value = true
  createPoolError.value = ''
  const res = await safeCall(() =>
    storageApi.createZFSPool({
      ...createPoolForm.value,
      mountpoint: createPoolForm.value.mountpoint || undefined,
      dryRun: false,
    }),
  )
  createPoolBusy.value = false
  if (res.error) {
    createPoolError.value = res.error
    return
  }
  createPoolVisible.value = false
  await checkZfsSupport()
}

// ----- Destroy pool (advanced) -----
const destroyPoolVisible = ref(false)
const destroyPoolBusy = ref(false)
const destroyPoolError = ref('')
const destroyPoolTarget = ref<string>('')
const destroyPoolConfirm = ref('')
const destroyPoolForce = ref(false)
function openDestroyPool(pool: string) {
  destroyPoolTarget.value = pool
  destroyPoolConfirm.value = ''
  destroyPoolForce.value = false
  destroyPoolError.value = ''
  destroyPoolVisible.value = true
}
async function submitDestroyPool() {
  if (destroyPoolBusy.value) return
  if (destroyPoolConfirm.value !== destroyPoolTarget.value) {
    destroyPoolError.value = t('settings.storage.zfs_destroy_mismatch')
    return
  }
  destroyPoolBusy.value = true
  const res = await safeCall(() =>
    storageApi.destroyZFSPool(
      destroyPoolTarget.value,
      destroyPoolConfirm.value,
      destroyPoolForce.value,
    ),
  )
  destroyPoolBusy.value = false
  if (res.error) {
    destroyPoolError.value = res.error
    return
  }
  destroyPoolVisible.value = false
  await checkZfsSupport()
}

// ----- Replace vdev (advanced) -----
const replaceVdevVisible = ref(false)
const replaceVdevBusy = ref(false)
const replaceVdevError = ref('')
const replaceVdevForm = ref({
  pool: '',
  oldDevice: '',
  newDevice: '',
  force: false,
})
function openReplaceVdev(pool: string, oldDevice: string) {
  replaceVdevForm.value = { pool, oldDevice, newDevice: '', force: false }
  replaceVdevError.value = ''
  replaceVdevVisible.value = true
}
async function submitReplaceVdev() {
  if (replaceVdevBusy.value) return
  replaceVdevBusy.value = true
  replaceVdevError.value = ''
  const res = await safeCall(() =>
    storageApi.replaceZFSVdev(
      replaceVdevForm.value.pool,
      replaceVdevForm.value.oldDevice,
      replaceVdevForm.value.newDevice,
      replaceVdevForm.value.force,
    ),
  )
  replaceVdevBusy.value = false
  if (res.error) {
    replaceVdevError.value = res.error
    return
  }
  replaceVdevVisible.value = false
  await checkZfsSupport()
}

function toggleDiskSelection(device: string) {
  const idx = newPool.value.selectedDisks.indexOf(device)
  if (idx >= 0) newPool.value.selectedDisks.splice(idx, 1)
  else newPool.value.selectedDisks.push(device)
}

async function createPool() {
  // Will call backend: POST /v2/local_storage/zfs/pool
  // For now just log the intent
  console.log('[ZFS] Create pool:', newPool.value)
  showCreatePool.value = false
}

async function scrubPool(poolName: string) {
  console.log('[ZFS] Scrub pool:', poolName)
}

function poolStatusColor(status: string): string {
  if (status === 'ONLINE') return 'bg-emerald-500/15 text-emerald-500'
  if (status === 'DEGRADED') return 'bg-amber-500/15 text-amber-500'
  return 'bg-red-500/15 text-red-500'
}

function poolStatusBg(status: string): string {
  if (status === 'ONLINE') return 'bg-emerald-500/15'
  if (status === 'DEGRADED') return 'bg-amber-500/15'
  return 'bg-red-500/15'
}

onMounted(() => {
  // Detection runs for every admin so the card can honestly say
  // "install zfsutils-linux" instead of staying invisible. Snapshot
  // listing stays dev-mode because the UI for it isn't ready yet.
  checkZfsSupport()
  if (devMode.value) {
    loadZfsSnapshots()
  }
})

// ----- ZFS Snapshot Management (Developer Mode) -----
import type { ZFSSnapshot, ZFSDataset } from '@/api/storage'

const zfsSnapshots = ref<ZFSSnapshot[]>([])
const zfsDatasets = ref<ZFSDataset[]>([])
const zfsSnapshotLoading = ref(false)
const zfsNoZfs = ref(false)
const zfsSelectedDataset = ref('')
const zfsNewSnapshotName = ref('')
const zfsCreateLoading = ref(false)

// Rollback dialog
const zfsRollbackTarget = ref<ZFSSnapshot | null>(null)
const zfsRollbackDialogVisible = ref(false)
const zfsRollbackConfirmText = ref('')

// Delete dialog
const zfsDeleteTarget = ref<ZFSSnapshot | null>(null)
const zfsDeleteDialogVisible = ref(false)

// Auto-suggest snapshot name: manual-YYYY-MM-DD-HHmm
function suggestSnapshotName(): string {
  const now = new Date()
  const pad = (n: number) => String(n).padStart(2, '0')
  return `manual-${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}`
}

// Snapshots filtered by selected dataset
const filteredSnapshots = computed(() => {
  if (!zfsSelectedDataset.value) return zfsSnapshots.value
  return zfsSnapshots.value.filter((s) => s.dataset === zfsSelectedDataset.value)
})

// Sorted newest first
const sortedSnapshots = computed(() => {
  return [...filteredSnapshots.value].reverse()
})

async function loadZfsSnapshots() {
  zfsSnapshotLoading.value = true
  try {
    const [snapResult, dsResult] = await Promise.all([
      safeCall(() => storageApi.getZFSSnapshots()),
      safeCall(() => storageApi.getZFSDatasets()),
    ])
    if (snapResult.data) {
      const resp = snapResult.data as { success: number; message: string; data: ZFSSnapshot[] }
      zfsSnapshots.value = resp.data ?? []
      zfsNoZfs.value = resp.message === 'ZFS not available'
    }
    if (dsResult.data) {
      const resp = dsResult.data as { success: number; message: string; data: ZFSDataset[] }
      zfsDatasets.value = resp.data ?? []
      if (resp.data?.length > 0 && !zfsSelectedDataset.value) {
        zfsSelectedDataset.value = resp.data[0].name
      }
    }
  } catch {
    zfsSnapshots.value = []
    zfsDatasets.value = []
  } finally {
    zfsSnapshotLoading.value = false
  }
  // Suggest name on load
  if (!zfsNewSnapshotName.value) {
    zfsNewSnapshotName.value = suggestSnapshotName()
  }
}

async function createSnapshot() {
  if (!zfsSelectedDataset.value || !zfsNewSnapshotName.value) return
  zfsCreateLoading.value = true
  try {
    await safeCall(() => storageApi.createZFSSnapshot(zfsSelectedDataset.value, zfsNewSnapshotName.value))
    zfsNewSnapshotName.value = suggestSnapshotName()
    await loadZfsSnapshots()
  } finally {
    zfsCreateLoading.value = false
  }
}

function requestRollback(snap: ZFSSnapshot) {
  zfsRollbackTarget.value = snap
  zfsRollbackConfirmText.value = ''
  zfsRollbackDialogVisible.value = true
}

async function confirmRollback() {
  if (!zfsRollbackTarget.value) return
  const name = zfsRollbackTarget.value.name
  zfsRollbackDialogVisible.value = false
  zfsRollbackTarget.value = null
  await safeCall(() => storageApi.rollbackZFSSnapshot(name))
  await loadZfsSnapshots()
}

function requestDelete(snap: ZFSSnapshot) {
  zfsDeleteTarget.value = snap
  zfsDeleteDialogVisible.value = true
}

async function confirmDeleteSnapshot() {
  if (!zfsDeleteTarget.value) return
  const name = zfsDeleteTarget.value.name
  zfsDeleteDialogVisible.value = false
  zfsDeleteTarget.value = null
  await safeCall(() => storageApi.deleteZFSSnapshot(name))
  await loadZfsSnapshots()
}
</script>

<template>
  <SettingsPage
    icon="HardDrive"
    :title="t('settings.storage.title')"
    :description="t('settings.storage.description')"
  >
    <!-- Drive Setup Wizard launcher — guided flow for users who don't want to
         think about format / mount / pool individually. It lives in the page
         header alongside the title, like every other page-level action; a
         three-line card at the top of the page for one button pushed the
         actual disk list below the fold. The raw per-disk controls stay
         as-is for power users. -->
    <template #actions>
      <Button
        :label="t('settings.storage.wizard.launch')"
        :title="t('settings.storage.wizard.launcher_desc')"
        severity="primary"
        @click="wizardVisible = true"
      />
    </template>

    <!-- Disk standby — HDD spin-down to save power. Hidden on SSD-only
         systems because the setting has no meaning there. -->
    <SettingsCard
      v-if="hasRotational"
      :title="t('settings.storage.standby_title')"
      :description="t('settings.storage.standby_desc')"
    >
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-[var(--cn-surface-hover)] text-blue-500">
            <AppIcon name="Moon" :size="18" />
          </div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ standbyMinutes === 0
                ? t('settings.storage.standby_off_hint')
                : t('settings.storage.standby_on_hint', { n: standbyMinutes }) }}
          </div>
        </div>
        <Select
          :model-value="standbyMinutes"
          :options="standbyOptions"
          option-label="label"
          option-value="value"
          class="w-48"
          @update:model-value="onStandbyChange"
        />
      </div>
    </SettingsCard>

    <!-- Summary card -->
    <SettingsCard :title="t('settings.storage.summary')">
      <div class="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.storage.total_disks') }}
          </div>
          <div class="mt-1 text-2xl font-semibold text-[var(--cn-text)]">
            {{ totals.diskCount }}
          </div>
        </div>
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.storage.mounted_disks') }}
          </div>
          <div class="mt-1 text-2xl font-semibold text-[var(--cn-text)]">
            {{ totals.mountedCount }}
          </div>
        </div>
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.storage.total_capacity') }}
          </div>
          <div class="mt-1 text-2xl font-semibold text-[var(--cn-text)]">
            {{ formatBytes(totals.totalBytes) }}
          </div>
        </div>
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.storage.usage') }}
          </div>
          <div class="mt-1 text-2xl font-semibold text-[var(--cn-text)]">
            {{ Math.round(totals.usedRatio * 100) }}%
          </div>
        </div>
      </div>
      <div class="mt-4 h-2 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
        <div
          class="h-full bg-blue-500 transition-all"
          :style="{ width: (totals.usedRatio * 100) + '%' }"
        />
      </div>
    </SettingsCard>

    <!-- Disk list -->
    <SettingsCard :title="t('settings.storage.disks')">
      <div class="flex flex-col gap-3">
        <div
          v-for="disk in disks"
          :key="disk.device"
          class="rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex items-start justify-between gap-4">
            <div class="flex min-w-0 flex-1 items-start gap-3">
              <div
                class="flex h-11 w-11 shrink-0 items-center justify-center rounded-lg"
                :class="disk.isUSB ? 'bg-amber-500/10 text-amber-500' : 'bg-blue-500/10 text-blue-500'"
              >
                <AppIcon :name="disk.isUSB ? 'Usb' : 'HardDrive'" :size="22" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="truncate font-medium text-[var(--cn-text)]">{{ disk.model }}</span>
                  <span
                    class="inline-flex items-center gap-1 rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]"
                  >
                    {{ disk.rotational ? t('settings.storage.hdd') : t('settings.storage.ssd') }}
                  </span>
                  <span
                    class="inline-flex items-center gap-1.5 text-xs"
                    :class="healthVisuals[disk.smartHealth].classes"
                  >
                    <AppIcon :name="healthVisuals[disk.smartHealth].icon" :size="13" />
                    {{ t(`settings.storage.health.${disk.smartHealth}`) }}
                  </span>
                </div>
                <div class="mt-1 font-mono text-xs text-[var(--cn-text-muted)]">
                  {{ disk.device }} · {{ formatBytes(disk.sizeBytes) }}
                </div>
                <div
                  v-if="disk.mountPoint"
                  class="mt-1 text-xs text-[var(--cn-text-muted)]"
                >
                  {{ t('settings.storage.mounted_at') }}
                  <code class="text-[var(--cn-text)]">{{ disk.mountPoint }}</code>
                  ({{ disk.filesystem }})
                </div>
                <div v-else class="mt-1 text-xs text-amber-500">
                  {{ t('settings.storage.not_mounted') }}
                </div>
              </div>
            </div>

            <div class="flex shrink-0 items-center gap-2">
              <Button
                v-if="!disk.mountPoint"
                :label="t('settings.storage.mount')"
                size="small"
                :loading="mountLoading === disk.device"
                @click="handleMount(disk)"
              />
              <Button
                v-else
                :label="disk.isUSB ? t('settings.storage.eject') : t('settings.storage.unmount')"
                severity="secondary"
                size="small"
                :loading="mountLoading === disk.device"
                @click="handleUnmount(disk)"
              />

              <!-- Actions submenu — destructive + heavy operations live
                   here so the primary button (mount/unmount) is never
                   adjacent to a one-click Format. -->
              <button
                type="button"
                class="flex h-8 w-8 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
                :aria-label="t('settings.storage.more_actions')"
                @click="openMenu(disk, $event)"
              >
                <AppIcon name="MoreVertical" :size="15" />
              </button>
              <Menu
                :ref="(el) => setMenuRef(disk.device, el)"
                :model="diskMenuItems(disk)"
                :popup="true"
              >
                <template #item="{ item, props }">
                  <a
                    v-bind="props.action"
                    :class="[
                      'flex cursor-pointer items-center gap-2 px-3 py-2 text-sm',
                      item.class === 'cn-menu-danger'
                        ? 'text-red-500 hover:bg-red-500/10'
                        : 'text-[var(--cn-text)]',
                      item.disabled ? 'cursor-not-allowed opacity-40' : '',
                    ]"
                  >
                    <AppIcon v-if="item.icon === 'cn-bench'" name="Gauge" :size="14" />
                    <AppIcon v-else-if="item.icon === 'cn-resize'" name="Maximize2" :size="14" />
                    <AppIcon v-else-if="item.icon === 'cn-merge'" name="Layers" :size="14" />
                    <AppIcon v-else-if="item.icon === 'cn-format'" name="Trash2" :size="14" />
                    <span>{{ item.label }}</span>
                  </a>
                </template>
              </Menu>
            </div>
          </div>

          <!-- SMART details row (only if SMART data exists) -->
          <div
            v-if="disk.temperature !== null || disk.powerOnHours > 0"
            class="mt-3 grid grid-cols-2 gap-3 border-t border-[var(--cn-border)] pt-3 text-xs sm:grid-cols-4"
          >
            <div v-if="disk.temperature !== null">
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Thermometer" :size="11" /> {{ t('settings.storage.temperature') }}
              </div>
              <div class="mt-0.5 font-medium text-[var(--cn-text)]">
                {{ disk.temperature }}°C
              </div>
            </div>
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Clock" :size="11" /> {{ t('settings.storage.power_on_hours') }}
              </div>
              <div class="mt-0.5 font-medium text-[var(--cn-text)]">
                {{ disk.powerOnHours.toLocaleString() }} h
              </div>
            </div>
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Activity" :size="11" /> {{ t('settings.storage.reallocated') }}
              </div>
              <div class="mt-0.5 font-medium text-[var(--cn-text)]">
                {{ disk.reallocatedSectors }}
              </div>
            </div>
            <div>
              <div class="text-[var(--cn-text-muted)]">
                {{ t('settings.storage.uuid') }}
              </div>
              <div class="mt-0.5 truncate font-mono text-[10px] text-[var(--cn-text)]">
                {{ disk.fsUUID }}
              </div>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- SMART Disk Health -->
    <SettingsCard
      :title="t('settings.storage.smart_title')"
      :description="t('settings.storage.smart_desc')"
    >
      <!-- smartctl not installed -->
      <div v-if="smartNoSmartctl && !smartLoading" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="AlertTriangle" :size="28" class="mx-auto mb-2 text-amber-500" />
        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.storage.smart_no_smartctl') }}
        </div>
      </div>

      <!-- Loading -->
      <div v-else-if="smartLoading" class="flex items-center justify-center py-8">
        <AppIcon name="RefreshCw" :size="18" class="animate-spin text-[var(--cn-text-muted)]" />
      </div>

      <!-- No disks found -->
      <div v-else-if="smartDisks.length === 0" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="HardDrive" :size="28" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.storage.smart_not_available') }}
        </div>
      </div>

      <!-- Disk list -->
      <div v-else class="flex flex-col gap-3">
        <div
          v-for="sd in smartDisks"
          :key="sd.device"
          class="rounded-lg border border-[var(--cn-border)] p-4"
        >
          <!-- Header row -->
          <div class="flex items-start justify-between gap-4">
            <div class="flex min-w-0 flex-1 items-start gap-3">
              <div
                class="flex h-11 w-11 shrink-0 items-center justify-center rounded-lg"
                :class="sd.smart_healthy ? 'bg-emerald-500/10 text-emerald-500' : 'bg-red-500/10 text-red-500'"
              >
                <AppIcon v-if="sd.smart_healthy" name="CircleCheck" :size="22" />
                <AppIcon v-else name="AlertTriangle" :size="22" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="truncate font-medium text-[var(--cn-text)]">{{ sd.model || sd.device }}</span>
                  <span
                    class="inline-flex items-center gap-1 rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]"
                  >
                    {{ sd.type }}
                  </span>
                  <span v-if="sd.transport" class="text-[10px] text-[var(--cn-text-muted)]">
                    {{ sd.transport }}
                  </span>
                  <span
                    v-if="sd.smart_available"
                    class="inline-flex items-center gap-1 text-xs"
                    :class="sd.smart_healthy ? 'text-emerald-500' : 'text-red-500'"
                  >
                    {{ sd.smart_healthy ? t('settings.storage.smart_healthy') : t('settings.storage.smart_warning') }}
                  </span>
                  <span v-else class="text-xs text-[var(--cn-text-muted)]">
                    {{ t('settings.storage.smart_not_available') }}
                  </span>
                </div>
                <div class="mt-1 font-mono text-xs text-[var(--cn-text-muted)]">
                  {{ sd.device }} · {{ sd.size }}
                  <span v-if="sd.serial"> · {{ sd.serial }}</span>
                </div>
              </div>
            </div>

            <div class="flex shrink-0 items-center gap-2">
              <Button
                :label="t('settings.storage.smart_run_short')"
                size="small"
                severity="secondary"
                :loading="smartTestLoading === sd.device"
                @click="runTest(sd.device, 'short')"
              />
            </div>
          </div>

          <!-- Stats row -->
          <div
            v-if="sd.smart_available"
            class="mt-3 grid grid-cols-2 gap-3 border-t border-[var(--cn-border)] pt-3 text-xs sm:grid-cols-4"
          >
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Thermometer" :size="11" /> {{ t('settings.storage.smart_temperature') }}
              </div>
              <div class="mt-0.5 font-medium" :class="tempColorClass(sd.temperature_c)">
                {{ sd.temperature_c !== null ? sd.temperature_c + '°C' : '—' }}
              </div>
            </div>
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Clock" :size="11" /> {{ t('settings.storage.smart_power_hours') }}
              </div>
              <div class="mt-0.5 font-medium text-[var(--cn-text)]">
                {{ formatPowerOnHours(sd.power_on_hours) }}
              </div>
            </div>
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="Activity" :size="11" /> {{ t('settings.storage.smart_reallocated') }}
              </div>
              <div class="mt-0.5 font-medium" :class="sd.reallocated_sectors > 0 ? 'text-red-500' : 'text-[var(--cn-text)]'">
                {{ sd.reallocated_sectors }}
              </div>
            </div>
            <div>
              <div class="flex items-center gap-1 text-[var(--cn-text-muted)]">
                <AppIcon name="AlertCircle" :size="11" /> {{ t('settings.storage.smart_pending') }}
              </div>
              <div class="mt-0.5 font-medium" :class="sd.pending_sectors > 0 ? 'text-red-500' : 'text-[var(--cn-text)]'">
                {{ sd.pending_sectors }}
              </div>
            </div>
          </div>

          <!-- Expandable SMART attributes -->
          <div v-if="sd.smart_available && sd.attributes.length > 0" class="mt-3 border-t border-[var(--cn-border)] pt-3">
            <button
              type="button"
              class="flex items-center gap-1 text-xs text-[var(--cn-text-muted)] transition-colors hover:text-[var(--cn-text)]"
              @click="toggleSmartExpand(sd.device)"
            >
              <AppIcon :name="smartExpanded[sd.device] ? 'ChevronDown' : 'ChevronRight'" :size="13" />
              {{ t('settings.storage.smart_attributes') }} ({{ sd.attributes.length }})
            </button>
            <div v-if="smartExpanded[sd.device]" class="mt-2 overflow-x-auto">
              <table class="w-full text-xs">
                <thead>
                  <tr class="border-b border-[var(--cn-border)] text-left text-[var(--cn-text-muted)]">
                    <th class="pb-1 pr-3">ID</th>
                    <th class="pb-1 pr-3">Name</th>
                    <th class="pb-1 pr-3">Value</th>
                    <th class="pb-1 pr-3">Worst</th>
                    <th class="pb-1 pr-3">Thresh</th>
                    <th class="pb-1">Raw</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="attr in sd.attributes"
                    :key="attr.id"
                    class="border-b border-[var(--cn-border)]"
                    :class="attr.value <= attr.thresh && attr.thresh > 0 ? 'text-red-500' : 'text-[var(--cn-text)]'"
                  >
                    <td class="py-1 pr-3 font-mono">{{ attr.id }}</td>
                    <td class="py-1 pr-3">{{ attr.name }}</td>
                    <td class="py-1 pr-3 font-mono">{{ attr.value }}</td>
                    <td class="py-1 pr-3 font-mono">{{ attr.worst }}</td>
                    <td class="py-1 pr-3 font-mono">{{ attr.thresh }}</td>
                    <td class="py-1 font-mono">{{ attr.raw }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- SMART Scheduled Tests -->
    <SettingsCard
      :title="t('settings.storage.smart_schedule_title')"
      :description="t('settings.storage.smart_schedule_desc')"
    >
      <div class="mb-3 flex items-center justify-between">
        <div class="flex items-center gap-2">
          <AppIcon name="Calendar" :size="18" class="text-blue-500" />
          <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.storage.smart_schedule_title') }}</span>
        </div>
        <Button
          :label="t('settings.storage.smart_schedule_add')"
          size="small"
          @click="openScheduleDialog"
          :disabled="smartDeviceOptions.length === 0"
        >
          <template #icon><AppIcon name="Plus" :size="13" /></template>
        </Button>
      </div>

      <!-- No schedules -->
      <div v-if="smartSchedules.length === 0 && !smartScheduleLoading" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="Calendar" :size="28" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.storage.smart_schedule_desc') }}
        </div>
      </div>

      <!-- Loading -->
      <div v-else-if="smartScheduleLoading" class="flex items-center justify-center py-8">
        <AppIcon name="RefreshCw" :size="18" class="animate-spin text-[var(--cn-text-muted)]" />
      </div>

      <!-- Schedule list -->
      <div v-else class="flex flex-col gap-3">
        <div
          v-for="sched in smartSchedules"
          :key="sched.device"
          class="rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex items-center justify-between gap-4">
            <div class="flex min-w-0 flex-1 items-center gap-3">
              <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg" :class="sched.enabled ? 'bg-blue-500/10 text-blue-500' : 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]'">
                <AppIcon name="Calendar" :size="18" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="truncate font-medium text-[var(--cn-text)]">{{ sched.device }}</span>
                  <span class="inline-flex items-center gap-1 rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]">
                    {{ sched.test_type }}
                  </span>
                  <span class="inline-flex items-center gap-1 rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]">
                    {{ sched.interval }}
                  </span>
                </div>
                <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
                  {{ t('settings.storage.smart_schedule_next_run') }}: {{ formatNextRun(sched.next_run) }}
                  <span v-if="sched.last_run"> · {{ t('settings.storage.smart_schedule_last_run') }}: {{ formatNextRun(sched.last_run) }}</span>
                </div>
              </div>
            </div>
            <div class="flex shrink-0 items-center gap-2">
              <ToggleSwitch :model-value="sched.enabled" @update:model-value="toggleScheduleEnabled(sched)" />
              <button
                type="button"
                class="flex h-8 w-8 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
                @click="editSchedule(sched)"
              >
                <AppIcon name="Play" :size="13" />
              </button>
              <button
                type="button"
                class="flex h-8 w-8 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] transition-colors hover:bg-red-500/10 hover:text-red-500"
                @click="deleteSchedule(sched.device)"
              >
                <AppIcon name="Trash2" :size="13" />
              </button>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- SMART Test Results -->
    <SettingsCard
      :title="t('settings.storage.smart_results_title')"
    >
      <!-- Loading -->
      <div v-if="smartResultsLoading" class="flex items-center justify-center py-8">
        <AppIcon name="RefreshCw" :size="18" class="animate-spin text-[var(--cn-text-muted)]" />
      </div>

      <!-- No results -->
      <div v-else-if="smartResults.length === 0" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="Activity" :size="28" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.storage.smart_results_none') }}
        </div>
      </div>

      <!-- Results per device -->
      <div v-else class="flex flex-col gap-3">
        <div
          v-for="dr in smartResults"
          :key="dr.device"
          class="rounded-lg border border-[var(--cn-border)] p-4"
        >
          <button
            type="button"
            class="flex w-full items-center gap-2 text-left text-sm font-medium text-[var(--cn-text)] transition-colors hover:text-blue-500"
            @click="toggleResultsExpand(dr.device)"
          >
            <AppIcon :name="smartResultsExpanded[dr.device] ? 'ChevronDown' : 'ChevronRight'" :size="14" />
            <AppIcon name="HardDrive" :size="14" />
            {{ dr.device }}
            <span class="ml-1 text-xs text-[var(--cn-text-muted)]">({{ dr.tests.length }} {{ dr.tests.length === 1 ? 'test' : 'tests' }})</span>
          </button>

          <div v-if="smartResultsExpanded[dr.device]" class="mt-3 overflow-x-auto">
            <table class="w-full text-xs">
              <thead>
                <tr class="border-b border-[var(--cn-border)] text-left text-[var(--cn-text-muted)]">
                  <th class="pb-1 pr-3">{{ t('settings.storage.smart_results_type') }}</th>
                  <th class="pb-1 pr-3">{{ t('settings.storage.smart_results_status') }}</th>
                  <th class="pb-1 pr-3">{{ t('settings.storage.smart_results_remaining') }}</th>
                  <th class="pb-1">{{ t('settings.storage.smart_results_hours') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="(test, idx) in dr.tests"
                  :key="idx"
                  class="border-b border-[var(--cn-border)]"
                >
                  <td class="py-1 pr-3 text-[var(--cn-text)]">{{ test.type }}</td>
                  <td class="py-1 pr-3 font-medium" :class="resultStatusClass(test.passed, test.status)">
                    {{ test.status }}
                  </td>
                  <td class="py-1 pr-3 font-mono text-[var(--cn-text)]">{{ test.remaining }}</td>
                  <td class="py-1 font-mono text-[var(--cn-text)]">{{ test.lifetime_hours.toLocaleString() }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- Schedule dialog -->
    <Dialog
      v-model:visible="smartScheduleDialogVisible"
      modal
      :style="{ width: '28rem' }"
      :header="t('settings.storage.smart_schedule_add')"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-4">
        <!-- Device -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.smart_schedule_device') }}</label>
          <Select
            v-model="scheduleForm.device"
            :options="smartDeviceOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>

        <!-- Test type -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.smart_schedule_type') }}</label>
          <Select
            v-model="scheduleForm.test_type"
            :options="testTypeOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>

        <!-- Interval -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.smart_schedule_interval') }}</label>
          <Select
            v-model="scheduleForm.interval"
            :options="intervalOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>

        <!-- Day of week (only for weekly) -->
        <div v-if="scheduleForm.interval === 'weekly'">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.smart_schedule_day') }}</label>
          <div class="flex gap-1">
            <button
              v-for="(label, idx) in dayOfWeekLabels"
              :key="idx"
              type="button"
              class="flex h-8 w-10 items-center justify-center rounded-lg border text-xs transition-colors"
              :class="scheduleForm.day_of_week === idx
                ? 'border-blue-500 bg-blue-500/10 text-blue-500 font-medium'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)]'"
              @click="scheduleForm.day_of_week = idx"
            >
              {{ label }}
            </button>
          </div>
        </div>

        <!-- Hour -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.smart_schedule_hour') }}</label>
          <Select
            v-model="scheduleForm.hour"
            :options="hourOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>

        <!-- Enabled -->
        <div class="flex items-center justify-between">
          <span class="text-sm text-[var(--cn-text)]">{{ t('settings.storage.smart_schedule_enabled') }}</span>
          <ToggleSwitch v-model="scheduleForm.enabled" />
        </div>
      </div>

      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="smartScheduleDialogVisible = false"
        />
        <Button
          :label="t('common.save')"
          :loading="smartScheduleSaving"
          :disabled="!scheduleForm.device"
          @click="saveSchedule"
        />
      </template>
    </Dialog>

    <!-- Resize dialog. Online grow of the partition's filesystem to the
         underlying block-device size — useful after extending a virtual
         disk or replacing a smaller partition image. -->
    <Dialog
      v-model:visible="resizeDialogVisible"
      modal
      :style="{ width: '28rem' }"
      :header="t('settings.storage.resize_title')"
      :dismissable-mask="!resizeBusy"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.resize_description', { device: resizeTarget?.device ?? '' }) }}
      </p>
      <div class="mt-3 rounded-lg bg-[var(--cn-surface-hover)] p-3 text-xs">
        <div>
          <span class="text-[var(--cn-text-muted)]">{{ t('settings.storage.filesystem') }}:</span>
          <span class="ml-2 font-mono text-[var(--cn-text)]">{{ resizeTarget?.filesystem ?? '—' }}</span>
        </div>
        <div class="mt-1">
          <span class="text-[var(--cn-text-muted)]">{{ t('settings.storage.mount_point') }}:</span>
          <span class="ml-2 font-mono text-[var(--cn-text)]">{{ resizeTarget?.mountPoint ?? '—' }}</span>
        </div>
      </div>
      <div v-if="resizeResult" class="mt-3 rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-3 text-xs">
        <div class="font-medium text-emerald-500">{{ t('settings.storage.resize_done') }}</div>
        <pre class="mt-1 whitespace-pre-wrap font-mono text-[var(--cn-text-muted)]">{{ resizeResult.output }}</pre>
      </div>
      <div v-if="resizeError" class="mt-3 rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-xs text-red-500">
        {{ resizeError }}
      </div>
      <template #footer>
        <Button
          :label="resizeResult ? t('common.close') : t('common.cancel')"
          severity="secondary"
          :disabled="resizeBusy"
          @click="resizeDialogVisible = false"
        />
        <Button
          v-if="!resizeResult"
          :label="t('settings.storage.resize_action')"
          :loading="resizeBusy"
          :disabled="resizeBusy"
          @click="confirmResize"
        />
      </template>
    </Dialog>

    <!-- Benchmark dialog. fio when present (richer numbers), dd fallback.
         The disk row stays usable during the run so the user can still
         navigate. -->
    <Dialog
      v-model:visible="benchDialogVisible"
      modal
      :style="{ width: '32rem' }"
      :header="t('settings.storage.benchmark_title')"
      :dismissable-mask="!benchBusy"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.benchmark_description', { device: benchTarget?.device ?? '' }) }}
      </p>
      <div class="mt-3 flex items-center gap-2">
        <label class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.storage.benchmark_mode') }}:
        </label>
        <Select
          v-model="benchMode"
          :options="[
            { value: 'quick', label: t('settings.storage.benchmark_quick') },
            { value: 'deep', label: t('settings.storage.benchmark_deep') },
          ]"
          option-label="label"
          option-value="value"
          :disabled="benchBusy"
          class="w-40"
        />
      </div>
      <div
        v-if="benchResult"
        class="mt-3 grid grid-cols-2 gap-2 rounded-lg bg-[var(--cn-surface-hover)] p-3 text-xs"
      >
        <div>
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.storage.seq_read') }}</div>
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ benchResult.seqReadMBps.toFixed(1) }} MB/s
          </div>
        </div>
        <div>
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.storage.seq_write') }}</div>
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ benchResult.seqWriteMBps.toFixed(1) }} MB/s
          </div>
        </div>
        <div v-if="benchResult.randReadIOPS > 0">
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.storage.rand_read') }}</div>
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ benchResult.randReadIOPS.toFixed(0) }} IOPS
          </div>
        </div>
        <div v-if="benchResult.randWriteIOPS > 0">
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.storage.rand_write') }}</div>
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ benchResult.randWriteIOPS.toFixed(0) }} IOPS
          </div>
        </div>
        <div class="col-span-2 mt-1 text-[var(--cn-text-muted)]">
          {{ t('settings.storage.benchmark_engine') }}:
          <span class="font-mono">{{ benchResult.engine }}</span>
          — {{ benchResult.durationMs }} ms
        </div>
        <div v-if="benchResult.notes" class="col-span-2 text-[var(--cn-text-muted)]">
          {{ benchResult.notes }}
        </div>
      </div>
      <div v-if="benchError" class="mt-3 rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-xs text-red-500">
        {{ benchError }}
      </div>
      <template #footer>
        <Button
          :label="t('common.close')"
          severity="secondary"
          :disabled="benchBusy"
          @click="benchDialogVisible = false"
        />
        <Button
          :label="benchResult ? t('settings.storage.benchmark_rerun') : t('settings.storage.benchmark_run')"
          :loading="benchBusy"
          :disabled="benchBusy"
          @click="runBenchmark"
        />
      </template>
    </Dialog>

    <!-- Merge into existing pool. For now the action redirects to the
         create-pool wizard if no pool exists; future iteration will list
         existing mergerfs pools and offer "add to <pool name>". -->
    <Dialog
      v-model:visible="mergeDialogVisible"
      modal
      :style="{ width: '28rem' }"
      :header="t('settings.storage.merge_title')"
      :dismissable-mask="true"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.merge_description', { device: mergeTarget?.device ?? '' }) }}
      </p>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="mergeDialogVisible = false"
        />
        <Button
          :label="t('settings.storage.merge_open_wizard')"
          @click="() => { mergeDialogVisible = false; showCreatePool = true }"
        />
      </template>
    </Dialog>

    <!-- Format confirmation -->
    <Dialog
      v-model:visible="formatDialogVisible"
      modal
      :style="{ width: '26rem' }"
      :header="t('settings.storage.format_title')"
      :dismissable-mask="true"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.format_warning', { device: formatTarget?.device ?? '' }) }}
      </p>
      <div class="mt-4 flex flex-col gap-2">
        <label class="text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('settings.storage.format_fs_label') }}
        </label>
        <Select
          v-model="formatFs"
          :options="formatFsOptions"
          option-label="label"
          option-value="value"
          class="w-full"
        >
          <template #option="slot">
            <div class="flex flex-col">
              <span class="font-mono text-sm">{{ slot.option.label }}</span>
              <span class="text-xs text-[var(--cn-text-muted)]">{{ slot.option.hint }}</span>
            </div>
          </template>
        </Select>
      </div>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="formatDialogVisible = false"
        />
        <Button
          :label="t('settings.storage.format')"
          severity="danger"
          @click="confirmFormat"
        />
      </template>
    </Dialog>

    <!-- Create ZFS Pool Dialog -->
    <Dialog
      v-model:visible="showCreatePool"
      modal
      :style="{ width: '34rem' }"
      header="Create ZFS Pool"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-4">
        <!-- Pool name -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">Pool name</label>
          <InputText v-model="newPool.name" class="w-full" placeholder="tank" />
        </div>

        <!-- RAID level -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">RAID level</label>
          <Select
            v-model="newPool.raidLevel"
            :options="raidLevels"
            option-label="label"
            option-value="value"
            class="w-full"
          />
          <div v-if="selectedRaidInfo" class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ selectedRaidInfo.description }}
          </div>
        </div>

        <!-- Compression -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">Compression</label>
          <Select
            v-model="newPool.compression"
            :options="compressionOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>

        <!-- Disk selection -->
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            Select disks ({{ newPool.selectedDisks.length }}/{{ unmountedDisks.length }} available)
          </label>
          <div v-if="unmountedDisks.length === 0" class="rounded-lg border border-dashed border-[var(--cn-border)] p-4 text-center text-xs text-[var(--cn-text-muted)]">
            No unmounted disks available. Unmount disks first.
          </div>
          <div v-else class="flex flex-col gap-2">
            <button
              v-for="disk in unmountedDisks"
              :key="disk.device"
              type="button"
              class="flex items-center gap-3 rounded-lg border px-3 py-2 text-left transition-colors"
              :class="newPool.selectedDisks.includes(disk.device)
                ? 'border-blue-500 bg-blue-500/10'
                : 'border-[var(--cn-border)] hover:bg-[var(--cn-surface-hover)]'"
              @click="toggleDiskSelection(disk.device)"
            >
              <div
                class="flex h-5 w-5 items-center justify-center rounded border"
                :class="newPool.selectedDisks.includes(disk.device) ? 'border-blue-500 bg-blue-500 text-white' : 'border-[var(--cn-border)]'"
              >
                <AppIcon v-if="newPool.selectedDisks.includes(disk.device)" name="CheckCircle2" :size="12" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="text-sm text-[var(--cn-text)]">{{ disk.model }}</div>
                <div class="text-xs text-[var(--cn-text-muted)]">{{ disk.device }} · {{ formatBytes(disk.sizeBytes) }}</div>
              </div>
            </button>
          </div>
          <div v-if="selectedRaidInfo && newPool.selectedDisks.length < selectedRaidInfo.minDisks" class="mt-1 text-xs text-amber-500">
            {{ selectedRaidInfo.label }} requires at least {{ selectedRaidInfo.minDisks }} disks.
          </div>
        </div>
      </div>

      <template #footer>
        <Button label="Cancel" severity="secondary" @click="showCreatePool = false" />
        <Button label="Create Pool" :disabled="!canCreatePool" @click="createPool" />
      </template>
    </Dialog>

    <!-- ZFS Pool Management. Tier 1+2 surface — read-only inventory
         plus safe ops (scrub, export). Pool create is gated behind
         dev mode until the wizard arc lands; running pools are
         visible to every admin so they can monitor health. -->
    <SettingsCard
      :title="t('settings.storage.zfs_title')"
      :description="t('settings.storage.zfs_desc')"
    >
      <!-- Detection guard. The card is always rendered (so users know
           where the feature lives), but the body changes depending on
           whether the host has zfsutils-linux installed. We render the
           "unavailable" branch when status loaded and reported false
           OR when the status request itself failed — both are honest
           signals that ZFS isn't usable from here right now. -->
      <div v-if="!zfsLoading && !zfsSupported"
           class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="Database" :size="28" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
        <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.storage.zfs_unavailable') }}</div>
        <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.storage.zfs_install_hint') }}
        </div>
      </div>

      <template v-else>
        <div class="mb-4 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <AppIcon name="Database" :size="18" class="text-purple-500" />
            <span class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.storage.zfs_pools') }}
            </span>
            <span v-if="zfsStatus?.version" class="text-[10px] text-[var(--cn-text-muted)]">
              {{ zfsStatus.version }}
            </span>
          </div>
          <div class="flex items-center gap-2">
            <Button
              :label="t('common.refresh')"
              size="small"
              severity="secondary"
              :loading="zfsLoading"
              @click="checkZfsSupport"
            >
              <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
            </Button>
            <Button
              v-if="advancedMode"
              :label="t('settings.storage.zfs_create_pool')"
              size="small"
              :disabled="unmountedDisks.length < 1"
              @click="openCreatePool"
            >
              <template #icon><AppIcon name="Plus" :size="13" /></template>
            </Button>
          </div>
        </div>

        <!-- No pools yet -->
        <div v-if="zfsRealPools.length === 0 && !zfsLoading"
             class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-8 text-center">
          <AppIcon name="Database" :size="32" class="mx-auto mb-3 text-[var(--cn-text-muted)]" />
          <div class="text-sm font-medium text-[var(--cn-text)]">
            {{ t('settings.storage.zfs_no_pools') }}
          </div>
          <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_no_pools_hint') }}
          </div>
        </div>

        <!-- Live pool rows -->
        <div
          v-for="pool in zfsRealPools"
          :key="pool.name"
          class="mb-3 rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-mono text-sm font-medium text-[var(--cn-text)]">{{ pool.name }}</span>
                <span
                  class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium"
                  :class="poolStatusColor(pool.health)"
                >
                  {{ pool.health }}
                </span>
                <span
                  v-if="pool.scrub.state === 'scrubbing'"
                  class="inline-flex items-center gap-1 rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] font-medium text-blue-500"
                >
                  <AppIcon name="Loader2" :size="10" class="animate-spin" />
                  {{ pool.scrub.percentComplete || t('settings.storage.zfs_scrubbing') }}
                </span>
              </div>
              <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
                {{ fmtBytes(pool.allocBytes) }} / {{ fmtBytes(pool.sizeBytes) }}
                · {{ t('settings.storage.zfs_frag') }}: {{ pool.frag || '0%' }}
                <span v-if="pool.dedup && pool.dedup !== '1.00x'">
                  · {{ t('settings.storage.zfs_dedup') }}: {{ pool.dedup }}
                </span>
              </div>
              <div class="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
                <div
                  class="h-full bg-purple-500"
                  :style="{ width: ((pool.allocBytes / Math.max(pool.sizeBytes, 1)) * 100).toFixed(1) + '%' }"
                />
              </div>
            </div>
            <div class="flex shrink-0 items-center gap-1">
              <Button
                v-if="pool.scrub.state !== 'scrubbing'"
                :label="t('settings.storage.zfs_scrub')"
                size="small"
                severity="secondary"
                :loading="scrubBusy === pool.name"
                @click="startScrub(pool.name)"
              >
                <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
              </Button>
              <Button
                v-else
                :label="t('settings.storage.zfs_scrub_stop')"
                size="small"
                severity="secondary"
                :loading="scrubBusy === pool.name"
                @click="stopScrub(pool.name)"
              >
                <template #icon><AppIcon name="Square" :size="13" /></template>
              </Button>
              <Button
                :label="t('settings.storage.zfs_export')"
                size="small"
                severity="secondary"
                @click="exportPool(pool.name)"
              >
                <template #icon><AppIcon name="LogOut" :size="13" /></template>
              </Button>
              <Button
                v-if="advancedMode"
                :label="t('settings.storage.zfs_destroy')"
                size="small"
                severity="danger"
                @click="openDestroyPool(pool.name)"
              >
                <template #icon><AppIcon name="Trash2" :size="13" /></template>
              </Button>
            </div>
          </div>

          <!-- Vdev tree -->
          <div v-if="pool.vdevs.length" class="mt-3 border-t border-[var(--cn-border)] pt-3">
            <div class="text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.storage.zfs_vdevs') }}
            </div>
            <div class="mt-2 flex flex-col gap-1">
              <div
                v-for="v in pool.vdevs"
                :key="v.name"
                class="flex items-center justify-between rounded px-2 py-1 text-xs"
                :class="v.state === 'ONLINE' ? '' : 'bg-red-500/5'"
              >
                <div class="flex items-center gap-2">
                  <AppIcon
                    :name="v.type === 'disk' ? 'HardDrive' : 'Layers'"
                    :size="12"
                    :class="v.state === 'ONLINE' ? 'text-emerald-500' : 'text-red-500'"
                  />
                  <span class="font-mono text-[var(--cn-text)]">{{ v.name }}</span>
                  <span v-if="v.type !== 'disk'" class="text-[10px] text-[var(--cn-text-muted)]">{{ v.type }}</span>
                </div>
                <div class="flex items-center gap-3">
                  <span :class="v.state === 'ONLINE' ? 'text-emerald-500' : 'text-red-500'">{{ v.state }}</span>
                  <span v-if="v.read || v.write || v.cksum" class="text-red-500">
                    {{ v.read }}/{{ v.write }}/{{ v.cksum }}
                  </span>
                  <button
                    v-if="advancedMode && v.type === 'disk'"
                    class="text-[10px] text-blue-500 hover:underline"
                    @click="openReplaceVdev(pool.name, v.name)"
                  >
                    {{ t('settings.storage.zfs_replace') }}
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </template>
    </SettingsCard>

    <!-- Developer-only legacy fixture panel + RAID guide. Kept behind
         devMode while the create-pool wizard is unfinished. -->
    <template v-if="devMode && false">
      <SettingsCard
        :title="'ZFS Pool Management (legacy fixture)'"
        :description="'Old hardcoded preview — replaced by the live panel above.'"
      >
        <div class="mb-4 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <AppIcon name="Database" :size="18" class="text-purple-500" />
            <span class="text-sm font-medium text-[var(--cn-text)]">Storage Pools</span>
            <span class="rounded-full bg-purple-500/15 px-2 py-0.5 text-[10px] font-medium text-purple-500">
              DEV
            </span>
          </div>
          <div class="flex items-center gap-2">
            <Button
              label="Refresh"
              size="small"
              severity="secondary"
              @click="checkZfsSupport"
              :loading="zfsLoading"
            >
              <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
            </Button>
            <Button
              label="Create Pool"
              size="small"
              @click="showCreatePool = true"
              :disabled="unmountedDisks.length < 2"
            >
              <template #icon><AppIcon name="Plus" :size="13" /></template>
            </Button>
          </div>
        </div>

        <!-- No pools yet -->
        <div v-if="zfsPools.length === 0 && !zfsLoading" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-8 text-center">
          <AppIcon name="Database" :size="32" class="mx-auto mb-3 text-[var(--cn-text-muted)]" />
          <div class="text-sm font-medium text-[var(--cn-text)]">No ZFS pools found</div>
          <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
            Create a pool to get started with ZFS RAID storage.
            <br />You need at least 2 unmounted disks.
          </div>
        </div>

        <!-- Existing pools -->
        <div v-for="pool in zfsPools" :key="pool.name" class="mb-3 rounded-lg border border-[var(--cn-border)] p-4">
          <div class="flex items-start justify-between">
            <div>
              <div class="flex items-center gap-2">
                <span class="font-medium text-[var(--cn-text)]">{{ pool.name }}</span>
                <span
                  class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium"
                  :class="[poolStatusColor(pool.status), poolStatusBg(pool.status)]"
                >
                  <AppIcon v-if="pool.status === 'ONLINE'" name="CheckCircle2" :size="10" />
                  <AppIcon v-else name="AlertCircle" :size="10" />
                  {{ pool.status }}
                </span>
                <span class="rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] text-[var(--cn-text-muted)]">
                  {{ pool.raidLevel }}
                </span>
              </div>
              <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
                {{ formatBytes(pool.usedBytes) }} used of {{ formatBytes(pool.totalBytes) }}
                · {{ Math.round(pool.fragmentation) }}% fragmented
                · Compression: {{ pool.compression }}
              </div>
            </div>
            <Button
              label="Scrub"
              size="small"
              severity="secondary"
              @click="scrubPool(pool.name)"
            >
              <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
            </Button>
          </div>

          <!-- Pool disk members -->
          <div class="mt-3 border-t border-[var(--cn-border)] pt-3">
            <div class="text-xs font-medium text-[var(--cn-text-muted)]">Pool members</div>
            <div class="mt-2 flex flex-col gap-1">
              <div
                v-for="d in pool.disks"
                :key="d.device"
                class="flex items-center justify-between rounded px-2 py-1 text-xs"
                :class="d.state === 'ONLINE' ? '' : 'bg-red-500/5'"
              >
                <div class="flex items-center gap-2">
                  <AppIcon name="HardDrive" :size="12" :class="d.state === 'ONLINE' ? 'text-emerald-500' : 'text-red-500'" />
                  <span class="font-mono text-[var(--cn-text)]">{{ d.device }}</span>
                </div>
                <div class="flex items-center gap-3">
                  <span :class="d.state === 'ONLINE' ? 'text-emerald-500' : 'text-red-500'">{{ d.state }}</span>
                  <span v-if="d.errors > 0" class="text-red-500">{{ d.errors }} errors</span>
                </div>
              </div>
            </div>
          </div>

          <div v-if="pool.lastScrub" class="mt-2 text-[10px] text-[var(--cn-text-muted)]">
            Last scrub: {{ pool.lastScrub }}
          </div>
        </div>
      </SettingsCard>

      <!-- RAID level guide -->
      <SettingsCard
        :title="'RAID Level Guide'"
        :description="'Understanding ZFS redundancy options for your storage array.'"
      >
        <div class="flex flex-col gap-3">
          <div
            v-for="level in raidLevels"
            :key="level.value"
            class="rounded-lg border border-[var(--cn-border)] p-3"
          >
            <div class="flex items-center justify-between">
              <div class="flex items-center gap-2">
                <AppIcon v-if="level.value === 'stripe'" name="GitMerge" :size="14" class="text-blue-500" />
                <AppIcon v-else-if="level.value === 'mirror'" name="Copy" :size="14" class="text-emerald-500" />
                <AppIcon v-else name="Shield" :size="14" class="text-purple-500" />
                <span class="text-sm font-medium text-[var(--cn-text)]">{{ level.label }}</span>
              </div>
              <span class="text-[10px] text-[var(--cn-text-muted)]">Min. {{ level.minDisks }} disks</span>
            </div>
            <div class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ level.description }}</div>
          </div>
        </div>
      </SettingsCard>

      <!-- ZFS Snapshot Management -->
      <SettingsCard
        :title="t('settings.storage.zfs_snapshots')"
        :description="t('settings.storage.zfs_snapshots_desc')"
      >
        <div class="mb-4 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <AppIcon name="Camera" :size="18" class="text-purple-500" />
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.storage.zfs_snapshots') }}</span>
            <span class="rounded-full bg-purple-500/15 px-2 py-0.5 text-[10px] font-medium text-purple-500">
              DEV
            </span>
          </div>
          <Button
            :label="'Refresh'"
            size="small"
            severity="secondary"
            @click="loadZfsSnapshots"
            :loading="zfsSnapshotLoading"
          >
            <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
          </Button>
        </div>

        <!-- ZFS not available -->
        <div v-if="zfsNoZfs && !zfsSnapshotLoading" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
          <AppIcon name="AlertTriangle" :size="28" class="mx-auto mb-2 text-amber-500" />
          <div class="text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_snapshot_no_zfs') }}
          </div>
        </div>

        <!-- Loading -->
        <div v-else-if="zfsSnapshotLoading" class="flex items-center justify-center py-8">
          <AppIcon name="RefreshCw" :size="18" class="animate-spin text-[var(--cn-text-muted)]" />
        </div>

        <template v-else>
          <!-- Create snapshot form -->
          <div class="mb-4 rounded-lg border border-[var(--cn-border)] p-4">
            <div class="mb-3 text-xs font-medium text-[var(--cn-text-muted)]">{{ t('settings.storage.zfs_snapshot_create') }}</div>
            <div class="flex flex-col gap-3 sm:flex-row sm:items-end">
              <div class="flex-1">
                <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">{{ t('settings.storage.zfs_dataset_select') }}</label>
                <Select
                  v-model="zfsSelectedDataset"
                  :options="zfsDatasets.map((d) => ({ value: d.name, label: d.name + ' (' + d.available + ' free)' }))"
                  option-label="label"
                  option-value="value"
                  class="w-full"
                  :placeholder="t('settings.storage.zfs_dataset_select')"
                />
              </div>
              <div class="flex-1">
                <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">{{ t('settings.storage.zfs_snapshot_name') }}</label>
                <InputText v-model="zfsNewSnapshotName" class="w-full" :placeholder="'manual-2026-04-07-1530'" />
              </div>
              <Button
                :label="t('settings.storage.zfs_snapshot_create')"
                size="small"
                :loading="zfsCreateLoading"
                :disabled="!zfsSelectedDataset || !zfsNewSnapshotName"
                @click="createSnapshot"
              >
                <template #icon><AppIcon name="Camera" :size="13" /></template>
              </Button>
            </div>
          </div>

          <!-- Dataset filter -->
          <div v-if="zfsDatasets.length > 1" class="mb-3 flex items-center gap-2">
            <label class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.storage.zfs_datasets') }}:</label>
            <button
              type="button"
              class="rounded-full px-2 py-0.5 text-[10px] font-medium transition-colors"
              :class="!zfsSelectedDataset ? 'bg-blue-500/15 text-blue-500' : 'text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]'"
              @click="zfsSelectedDataset = ''"
            >
              All
            </button>
            <button
              v-for="ds in zfsDatasets"
              :key="ds.name"
              type="button"
              class="rounded-full px-2 py-0.5 text-[10px] font-medium transition-colors"
              :class="zfsSelectedDataset === ds.name ? 'bg-blue-500/15 text-blue-500' : 'text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]'"
              @click="zfsSelectedDataset = ds.name"
            >
              {{ ds.name }}
            </button>
          </div>

          <!-- No snapshots -->
          <div v-if="sortedSnapshots.length === 0" class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-8 text-center">
            <AppIcon name="Camera" :size="32" class="mx-auto mb-3 text-[var(--cn-text-muted)]" />
            <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.storage.zfs_snapshot_no_snapshots') }}</div>
          </div>

          <!-- Snapshot table -->
          <div v-else class="overflow-x-auto">
            <table class="w-full text-xs">
              <thead>
                <tr class="border-b border-[var(--cn-border)] text-left text-[var(--cn-text-muted)]">
                  <th class="pb-2 pr-3">{{ t('settings.storage.zfs_snapshot_name') }}</th>
                  <th class="pb-2 pr-3">{{ t('settings.storage.zfs_snapshot_created') }}</th>
                  <th class="pb-2 pr-3">{{ t('settings.storage.zfs_snapshot_used') }}</th>
                  <th class="pb-2 pr-3">{{ t('settings.storage.zfs_snapshot_referenced') }}</th>
                  <th class="pb-2 text-right"></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="snap in sortedSnapshots"
                  :key="snap.name"
                  class="border-b border-[var(--cn-border)]"
                >
                  <td class="py-2 pr-3">
                    <div class="flex items-center gap-2">
                      <AppIcon name="Camera" :size="12" class="shrink-0 text-purple-500" />
                      <div>
                        <div class="font-medium text-[var(--cn-text)]">{{ snap.snapshot }}</div>
                        <div class="font-mono text-[10px] text-[var(--cn-text-muted)]">{{ snap.dataset }}</div>
                      </div>
                    </div>
                  </td>
                  <td class="py-2 pr-3 text-[var(--cn-text)]">
                    <div class="flex items-center gap-1">
                      <AppIcon name="Clock" :size="11" class="text-[var(--cn-text-muted)]" />
                      {{ snap.creation }}
                    </div>
                  </td>
                  <td class="py-2 pr-3 font-mono text-[var(--cn-text)]">{{ snap.used }}</td>
                  <td class="py-2 pr-3 font-mono text-[var(--cn-text)]">{{ snap.referenced }}</td>
                  <td class="py-2 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <button
                        type="button"
                        class="flex h-7 items-center gap-1 rounded-md border border-[var(--cn-border)] px-2 text-[10px] text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
                        :title="t('settings.storage.zfs_snapshot_rollback')"
                        @click="requestRollback(snap)"
                      >
                        <AppIcon name="RotateCcw" :size="11" />
                        {{ t('settings.storage.zfs_snapshot_rollback') }}
                      </button>
                      <button
                        type="button"
                        class="flex h-7 items-center gap-1 rounded-md border border-[var(--cn-border)] px-2 text-[10px] text-red-500 transition-colors hover:bg-red-500/10"
                        :title="t('settings.storage.zfs_snapshot_delete')"
                        @click="requestDelete(snap)"
                      >
                        <AppIcon name="Trash2" :size="11" />
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </SettingsCard>
    </template>

    <!-- ZFS Rollback confirmation dialog -->
    <Dialog
      v-model:visible="zfsRollbackDialogVisible"
      modal
      :style="{ width: '30rem' }"
      :header="t('settings.storage.zfs_snapshot_confirm_rollback')"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-3">
        <div class="flex items-center gap-2 rounded-lg bg-amber-500/10 p-3 text-xs text-amber-600">
          <AppIcon name="AlertTriangle" :size="16" class="shrink-0" />
          <span>{{ t('settings.storage.zfs_snapshot_rollback_warning') }}</span>
        </div>
        <div class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3">
          <div class="font-mono text-sm text-[var(--cn-text)]">{{ zfsRollbackTarget?.name }}</div>
        </div>
        <InputText
          v-model="zfsRollbackConfirmText"
          class="w-full"
          :placeholder="zfsRollbackTarget?.name ?? ''"
        />
      </div>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="zfsRollbackDialogVisible = false"
        />
        <Button
          :label="t('settings.storage.zfs_snapshot_rollback')"
          severity="danger"
          :disabled="zfsRollbackConfirmText !== zfsRollbackTarget?.name"
          @click="confirmRollback"
        />
      </template>
    </Dialog>

    <!-- ZFS Delete confirmation dialog -->
    <Dialog
      v-model:visible="zfsDeleteDialogVisible"
      modal
      :style="{ width: '26rem' }"
      :header="t('settings.storage.zfs_snapshot_confirm_delete')"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-3">
        <p class="text-sm text-[var(--cn-text)]">
          {{ t('settings.storage.zfs_snapshot_delete_warning') }}
        </p>
        <div class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3">
          <div class="font-mono text-sm text-[var(--cn-text)]">{{ zfsDeleteTarget?.name }}</div>
        </div>
      </div>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="zfsDeleteDialogVisible = false"
        />
        <Button
          :label="t('settings.storage.zfs_snapshot_delete')"
          severity="danger"
          @click="confirmDeleteSnapshot"
        />
      </template>
    </Dialog>

    <!-- Drive Setup Wizard — guided flow over the same mount/format
         endpoints exposed by the per-disk row actions above. -->
    <DriveSetupWizard
      v-model:visible="wizardVisible"
      @completed="refreshDisks"
    />

    <!-- ─── ZFS Tier 3 dialogs (advanced mode only) ─────────────── -->

    <!-- Create pool wizard. Two-stage flow: pick devices + topology,
         then preview with --dry-run before committing. dry-run output
         is real certbot-style transcript from the backend so the user
         sees exactly what zpool will execute. -->
    <Dialog
      v-model:visible="createPoolVisible"
      modal
      :style="{ width: '36rem' }"
      :header="t('settings.storage.zfs_create_pool_title')"
      :dismissable-mask="!createPoolBusy"
    >
      <div class="flex flex-col gap-4">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_pool_name') }}
          </label>
          <InputText
            v-model="createPoolForm.name"
            class="w-full font-mono text-sm"
            placeholder="tank"
          />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_topology') }}
          </label>
          <Select
            v-model="createPoolForm.topology"
            :options="topologyOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          >
            <template #option="slot">
              <div class="flex flex-col">
                <span class="font-mono text-sm">{{ slot.option.label }}</span>
                <span class="text-xs text-[var(--cn-text-muted)]">{{ slot.option.hint }}</span>
              </div>
            </template>
          </Select>
          <p v-if="currentTopology" class="mt-1 text-[10px] text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_min_devices', { n: currentTopology.min }) }}
          </p>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_devices') }}
          </label>
          <div
            v-if="unmountedDisks.length === 0"
            class="rounded border border-[var(--cn-border)] p-3 text-xs text-[var(--cn-text-muted)]"
          >
            {{ t('settings.storage.zfs_no_unmounted') }}
          </div>
          <div v-else class="flex max-h-40 flex-col gap-1 overflow-auto rounded border border-[var(--cn-border)] p-2">
            <label
              v-for="disk in unmountedDisks"
              :key="disk.device"
              class="flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-xs hover:bg-[var(--cn-surface-hover)]"
            >
              <input
                type="checkbox"
                :checked="createPoolForm.devices.includes(disk.device)"
                @change="toggleCreateDevice(disk.device)"
              />
              <span class="font-mono text-[var(--cn-text)]">{{ disk.device }}</span>
              <span class="text-[var(--cn-text-muted)]">{{ disk.model }}</span>
              <span class="ml-auto text-[var(--cn-text-muted)]">{{ fmtBytes(disk.sizeBytes) }}</span>
            </label>
          </div>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.storage.zfs_mountpoint_optional') }}
          </label>
          <InputText
            v-model="createPoolForm.mountpoint"
            class="w-full font-mono text-sm"
            :placeholder="`/${createPoolForm.name || 'tank'}`"
          />
        </div>
        <label class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
          <input type="checkbox" v-model="createPoolForm.force" />
          {{ t('settings.storage.zfs_force') }}
        </label>
        <pre
          v-if="createPoolDryRunOutput"
          class="max-h-48 overflow-auto whitespace-pre-wrap rounded-lg bg-[var(--cn-surface-hover)] p-3 font-mono text-[11px] text-[var(--cn-text)]"
        >{{ createPoolDryRunOutput }}</pre>
        <p v-if="createPoolError" class="text-xs text-red-500">{{ createPoolError }}</p>
      </div>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          :disabled="createPoolBusy"
          @click="createPoolVisible = false"
        />
        <Button
          :label="t('settings.storage.zfs_preview')"
          severity="secondary"
          :loading="createPoolBusy"
          :disabled="createPoolBusy || !createPoolReady"
          @click="previewCreatePool"
        />
        <Button
          :label="t('settings.storage.zfs_create_pool_action')"
          :loading="createPoolBusy"
          :disabled="createPoolBusy || !createPoolReady"
          @click="submitCreatePool"
        />
      </template>
    </Dialog>

    <!-- Destroy pool. Confirm-by-typing: user must echo the pool name
         exactly before the danger button enables. -->
    <Dialog
      v-model:visible="destroyPoolVisible"
      modal
      :style="{ width: '30rem' }"
      :header="t('settings.storage.zfs_destroy_title')"
      :dismissable-mask="!destroyPoolBusy"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.zfs_destroy_warning', { pool: destroyPoolTarget }) }}
      </p>
      <div class="mt-3">
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('settings.storage.zfs_destroy_confirm_label', { pool: destroyPoolTarget }) }}
        </label>
        <InputText
          v-model="destroyPoolConfirm"
          class="w-full font-mono text-sm"
          :placeholder="destroyPoolTarget"
        />
      </div>
      <label class="mt-3 flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
        <input type="checkbox" v-model="destroyPoolForce" />
        {{ t('settings.storage.zfs_force_destroy') }}
      </label>
      <p v-if="destroyPoolError" class="mt-3 text-xs text-red-500">{{ destroyPoolError }}</p>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          :disabled="destroyPoolBusy"
          @click="destroyPoolVisible = false"
        />
        <Button
          :label="t('settings.storage.zfs_destroy_action')"
          severity="danger"
          :loading="destroyPoolBusy"
          :disabled="destroyPoolBusy || destroyPoolConfirm !== destroyPoolTarget"
          @click="submitDestroyPool"
        />
      </template>
    </Dialog>

    <!-- Replace vdev. Operator picks one of the available devices from
         the unmounted list (so we don't accidentally feed an in-use
         device into zpool replace). -->
    <Dialog
      v-model:visible="replaceVdevVisible"
      modal
      :style="{ width: '30rem' }"
      :header="t('settings.storage.zfs_replace_title')"
      :dismissable-mask="!replaceVdevBusy"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.storage.zfs_replace_desc', {
          pool: replaceVdevForm.pool,
          old: replaceVdevForm.oldDevice,
        }) }}
      </p>
      <div class="mt-3">
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('settings.storage.zfs_replace_new_device') }}
        </label>
        <Select
          v-model="replaceVdevForm.newDevice"
          :options="unmountedDisks.map((d) => ({ value: d.device, label: `${d.device} — ${d.model} (${fmtBytes(d.sizeBytes)})` }))"
          option-label="label"
          option-value="value"
          class="w-full"
          :placeholder="t('settings.storage.zfs_pick_device')"
        />
      </div>
      <label class="mt-3 flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
        <input type="checkbox" v-model="replaceVdevForm.force" />
        {{ t('settings.storage.zfs_force') }}
      </label>
      <p v-if="replaceVdevError" class="mt-3 text-xs text-red-500">{{ replaceVdevError }}</p>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          :disabled="replaceVdevBusy"
          @click="replaceVdevVisible = false"
        />
        <Button
          :label="t('settings.storage.zfs_replace_action')"
          :loading="replaceVdevBusy"
          :disabled="replaceVdevBusy || !replaceVdevForm.newDevice"
          @click="submitReplaceVdev"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
