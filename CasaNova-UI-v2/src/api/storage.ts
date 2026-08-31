// Storage API — CasaNova-LocalStorage service (/v1/disks, /v2/storage).
//
// Covers the disk list, SMART health, mount/unmount/format, merged
// storage pools, and the SSD trim + standby settings the Settings →
// Storage page needs. Shape matches the structs in
// CasaNova-LocalStorage/service/model/ and the route handlers in
// CasaNova-LocalStorage/route/v1/disk.go + storage.go.

import client from './client'

export type SmartHealth = 'good' | 'warning' | 'critical' | 'unknown'

export interface DiskEntry {
  device: string // /dev/sda, /dev/nvme0n1
  model: string
  serial: string
  fsUUID: string
  sizeBytes: number
  usedBytes: number  // bytes consumed across mounted children; 0 if nothing mounted
  availBytes: number // bytes free across mounted children; 0 if nothing mounted
  rotational: boolean
  isUSB: boolean
  smartHealth: SmartHealth
  temperature: number | null
  powerOnHours: number
  reallocatedSectors: number
  mountPoint: string | null
  filesystem: string | null
}

// ----- Disk enumeration + SMART -----

// The real backend returns disks at GET /v1/disks in a different shape
// than the DiskEntry the settings page expects. Transform here.
interface RawDisk {
  name: string
  size: number
  model: string
  health: string // "true" | "false" | "unknown" — string, not boolean
  temperature: number
  disk_type: string // "SSD" | "HDD" | "USB"
  need_format: boolean
  serial: string
  path: string
  uuid?: string
  format?: string
  used_bytes?: number
  avail_bytes?: number
  children_number: number
  children?: Array<{
    name: string
    size: number
    format: string
    supported: boolean
    uuid?: string
    mount_point?: string
    used_bytes?: number
    avail_bytes?: number
  }>
  supported: boolean
  power_on_hours?: number
  reallocated_sectors?: number
}

/** Both enumerations walk real hardware: lsblk is bounded at 30s and smartctl
 *  at 6s per disk (command_helper.go:33, :25). Timing out discards the
 *  degraded inventory the backend assembled — which it assembles precisely
 *  when a disk is sick, i.e. when you most need to see it. */
const DISK_ENUM_TIMEOUT_MS = 60_000
const STORAGE_ROOTS_TIMEOUT_MS = 45_000

export async function listDisks(): Promise<DiskEntry[]> {
  // Fetch both the disk list and storage mounts to cross-reference
  const [disksRes, storageRes, utilRes] = await Promise.all([
    client.get<{ success: number; data: { disks: RawDisk[]; avail: RawDisk[] } }>('/v1/disks', {
      timeout: DISK_ENUM_TIMEOUT_MS,
    }),
    client.get<{ success: number; data: Array<{
      disk_name: string; path: string; type: string
      children?: Array<{ drive_name: string; mount_point: string; type: string; size: string; avail: string }>
    }> }>('/v1/storage', { timeout: DISK_ENUM_TIMEOUT_MS }).catch(() => ({ data: { data: [] } })),
    client.get<{ success: number; data: { sys_disk?: { size: number; used: number; avail: number } } }>(
      '/v1/sys/utilization',
    ).catch(() => ({ data: { data: { sys_disk: null } } })),
  ])

  const all = [...(disksRes.data.data?.disks ?? []), ...(disksRes.data.data?.avail ?? [])]
  const storageMounts = (storageRes as any).data?.data ?? []
  const sysDisk = (utilRes as any).data?.data?.sys_disk

  // Build a mount-point map from /v1/storage
  const mountMap = new Map<string, string>() // device path -> mount point
  for (const sd of storageMounts) {
    for (const child of sd.children ?? []) {
      if (child.mount_point) {
        mountMap.set(`/dev/${child.drive_name}`, child.mount_point)
      }
    }
  }

  return all.map((d) => {
    // Check if any child partition is mounted
    let mountPoint: string | null = null
    const isSystemDisk = d.name.startsWith('nvme') || d.model === 'System'

    // System disk is always mounted at /
    if (isSystemDisk && sysDisk) {
      mountPoint = '/'
    }

    // Check children for mounts
    for (const child of d.children ?? []) {
      const childPath = `/dev/${child.name}`
      if (mountMap.has(childPath)) {
        mountPoint = mountMap.get(childPath) || mountPoint
      }
    }

    // For USB drives, also check if the first child is in storage mounts
    if (!mountPoint && d.disk_type === 'USB') {
      for (const sd of storageMounts) {
        if (sd.path === d.path) {
          for (const child of sd.children ?? []) {
            if (child.mount_point) mountPoint = child.mount_point
          }
        }
      }
    }

    // Pick the first child that actually has a filesystem; a whole-disk
    // fs (no partitions) reports its format on the parent. This avoids
    // the prior pattern where partition[0] being unformatted hid a
    // perfectly good ext4 on partition[1].
    const fsChild = (d.children ?? []).find((c: any) => c.format)
    const filesystem = fsChild?.format ?? d.format ?? null
    // Stable filesystem UUID — prefer the partition with a filesystem,
    // fall back to the disk-level UUID for whole-disk filesystems.
    const fsUUID = fsChild?.uuid ?? d.uuid ?? ''

    return {
      device: d.path,
      model: d.model,
      serial: d.serial,
      fsUUID,
      sizeBytes: d.size,
      usedBytes: d.used_bytes ?? 0,
      availBytes: d.avail_bytes ?? 0,
      rotational: d.disk_type === 'HDD',
      isUSB: d.disk_type === 'USB',
      smartHealth: (d.health === 'true' ? 'good' : d.health === 'false' ? 'warning' : 'unknown') as SmartHealth,
      temperature: d.temperature || null,
      powerOnHours: d.power_on_hours ?? 0,
      reallocatedSectors: d.reallocated_sectors ?? 0,
      mountPoint,
      filesystem,
    }
  })
}

// getDiskSmart — dedicated SMART query per device.
// The v1 backend doesn't expose a per-disk SMART endpoint; instead
// SmartCTL runs inside GetDiskList() and populates temperature + health
// inline. For powerOnHours / reallocatedSectors the backend struct
// (SmartctlA) has PowerOnTime.Hours but doesn't expose them through
// the REST response. This function is a placeholder that will 404
// until we add a dedicated /v1/disks/smart?path=... endpoint. Callers
// should prefer the data already embedded in listDisks().
export async function getDiskSmart(_device: string): Promise<{
  health: SmartHealth
  temperature: number | null
  powerOnHours: number
  reallocatedSectors: number
}> {
  // Fall back to a minimal response — the data is already in listDisks().
  return { health: 'unknown', temperature: null, powerOnHours: 0, reallocatedSectors: 0 }
}

// ----- SMART Health Monitoring (via /v1/sys/smart) -----

export interface SmartAttribute {
  id: number
  name: string
  value: number
  worst: number
  thresh: number
  raw: number
}

export interface SmartDiskInfo {
  device: string
  model: string
  serial: string
  size: string
  type: 'ssd' | 'hdd'
  transport: string
  smart_available: boolean
  smart_healthy: boolean
  temperature_c: number | null
  power_on_hours: number
  reallocated_sectors: number
  pending_sectors: number
  attributes: SmartAttribute[]
}

export interface SmartSummaryResponse {
  success: number
  message: string
  data: SmartDiskInfo[]
}

/**
 * getSmartSummary — fetch SMART health for all physical disks.
 * Uses the dedicated /v1/sys/smart endpoint that runs smartctl per disk.
 */
export async function getSmartSummary(): Promise<SmartSummaryResponse> {
  const res = await client.get<SmartSummaryResponse>('/v1/sys/smart')
  return res.data
}

/**
 * runSmartTest — start a SMART self-test on a specific device.
 * @param device  e.g. "/dev/sda"
 * @param type    "short" (~2 min) or "long" (hours)
 */
export async function runSmartTest(
  device: string,
  type: 'short' | 'long' = 'short',
): Promise<{ status: string; type?: string; device?: string }> {
  const res = await client.post<{ success: number; message: string; data: { status: string; type?: string; device?: string } }>(
    '/v1/sys/smart/test',
    { device, type },
  )
  return res.data.data
}

// ----- Mount / unmount / format -----
// These map to the real CasaNova-LocalStorage v1 endpoints:
//   POST   /v1/storage  → PostAddStorage  (mount, optionally format first)
//   DELETE /v1/disks     → DeleteDisksUmount (unmount by device path)
//   PUT    /v1/storage  → PutFormatStorage (format + remount)

/**
 * mountDisk — mounts an unmounted disk. The v1 backend (PostAddStorage)
 * expects { path: "/dev/sdb", name: "label", format: false }. It
 * creates the mount point, mounts all partitions, and saves to the DB.
 */
/** Both of these shell out through a helper that had no bound at all until
 *  now — `umount` blocking on writeback, or a mount waiting on a wedged
 *  device, held the handler with nothing able to end it. There was no server
 *  deadline to size a client timeout against; there is now (90s for a single
 *  umount, 2min for the shared shell ceiling), so these sit above it.
 *
 *  It matters most for unmount: told nothing for ten seconds and then
 *  "unmount failed", a user pulls the drive while writeback is still
 *  flushing. */
const MOUNT_TIMEOUT_MS = 150_000

export async function mountDisk(
  device: string,
  name?: string,
): Promise<void> {
  await client.post(
    '/v1/storage',
    {
      path: device,
      name: name || '',
      format: false,
    },
    { timeout: MOUNT_TIMEOUT_MS },
  )
}

/**
 * unmountDisk — unmounts all partitions of a disk. The v1 backend
 * (DeleteDisksUmount) expects { path: "/dev/sdb" } in the body.
 * It unmounts, removes the mount directory, and deletes the DB entry.
 */
export async function unmountDisk(device: string): Promise<void> {
  await client.delete('/v1/disks', {
    data: { path: device },
    timeout: MOUNT_TIMEOUT_MS,
  })
}

/**
 * formatDisk — formats a partition with the requested filesystem and
 * remounts it. The backend dispatches to mkfs.<fs> via the supportedMkfs
 * map in CasaNova-LocalStorage/pkg/partition. Empty fs defaults to ext4
 * on the backend side, kept here so older callers still compile.
 */
export type FormatFsType =
  | 'ext4'
  | 'ext3'
  | 'ext2'
  | 'xfs'
  | 'btrfs'
  | 'f2fs'
  | 'vfat'
  | 'exfat'
  | 'ntfs'

export async function formatDisk(
  device: string,
  fs: FormatFsType = 'ext4',
): Promise<void> {
  await client.put('/v1/storage', {
    path: device,
    volume: '',
    fs,
  })
}

// ----- Disk ops: resize + benchmark -----

export interface ResizeResult {
  fs: string
  target: string
  output: string
}

// Both operations below shell out to a tool that works on a real disk, and
// both inherited the shared client's 10 second timeout — which is correct for
// the reads that make up most of this file and wrong for these two.
//
// What that looked like: axios aborted at 10s and rendered "timeout of 10000ms
// exceeded" in the dialog. The benchmark had already started and went on to
// finish on the server; only the answer was thrown away. The message says
// nothing about the disk, so there was no way to tell a slow drive from a
// broken feature — and the same shape of bug had already cost this codebase a
// download that appeared to run five times.
//
// The rule these follow: the client limit must EXCEED the server's own bound.
// The server kills a wedged fio and reports why; if axios gives up first, that
// real diagnosis is replaced by a generic timeout, and the useful error is the
// one nobody sees. Keep these above the ceilings in
// CasaNova-LocalStorage/route/v1/disk_ops.go.

/** Server bounds a quick run at 90s and a deep run at 3min (two fio phases,
 *  plus laying out the scratch file). These sit above both. */
const BENCHMARK_TIMEOUT_MS: Record<'quick' | 'deep', number> = {
  quick: 120_000,
  deep: 210_000,
}

/** resize2fs / xfs_growfs on a multi-terabyte filesystem is minutes of real
 *  work, and the handler bounds itself at 10 minutes. */
const RESIZE_TIMEOUT_MS = 11 * 60_000

export async function resizeDisk(device: string): Promise<ResizeResult> {
  const res = await client.post<{ success: number; data: ResizeResult }>(
    '/v1/storage/resize',
    { path: device },
    { timeout: RESIZE_TIMEOUT_MS },
  )
  return res.data.data
}

export interface BenchmarkResult {
  mode: 'quick' | 'deep'
  engine: 'fio' | 'dd'
  durationMs: number
  seqReadMBps: number
  seqWriteMBps: number
  randReadIOPS: number
  randWriteIOPS: number
  notes?: string
}

export async function benchmarkDisk(
  device: string,
  mode: 'quick' | 'deep' = 'quick',
): Promise<BenchmarkResult> {
  const res = await client.post<{ success: number; data: BenchmarkResult }>(
    '/v1/storage/benchmark',
    { path: device, mode },
    { timeout: BENCHMARK_TIMEOUT_MS[mode] },
  )
  return res.data.data
}

// ----- Merged storage pools -----

export interface MergedPool {
  id: string
  name: string
  mountPoint: string
  memberDevices: string[]
  totalSizeBytes: number
  usedBytes: number
}

export async function listMergedPools(): Promise<MergedPool[]> {
  const res = await client.get<{ success: number; data: MergedPool[] }>(
    '/v2/local_storage/merged',
  )
  return res.data.data ?? []
}

export async function createMergedPool(body: {
  name: string
  devices: string[]
}): Promise<MergedPool> {
  const res = await client.post<{ success: number; data: MergedPool }>(
    '/v2/local_storage/merged',
    body,
  )
  return res.data.data
}

export async function deleteMergedPool(id: string): Promise<void> {
  await client.delete(`/v2/local_storage/merged/${encodeURIComponent(id)}`)
}

// ----- Disk standby (HDD spin-down) -----

export async function getStandbySettings(): Promise<{ minutes: number }> {
  const res = await client.get<{ success: number; data: { minutes: number } }>(
    '/v2/local_storage/standby',
  )
  return res.data.data
}

export async function setStandbySettings(minutes: number): Promise<void> {
  await client.put('/v2/local_storage/standby', { minutes })
}

// ----- Storage roots (for the files sidebar) -----

export interface StorageRoot {
  id: string
  label: string
  path: string
  kind: 'internal' | 'external' | 'cloud' | 'network'
  totalBytes: number
  usedBytes: number
}

export async function listStorageRoots(): Promise<StorageRoot[]> {
  const res = await client.get<{ success: number; data: StorageRoot[] }>(
    '/v2/local_storage/storage',
    { timeout: STORAGE_ROOTS_TIMEOUT_MS },
  )
  return res.data.data ?? []
}

// ----- Cloud provider storage (rclone-backed) -----

export type CloudProvider = 'gdrive' | 'dropbox' | 'onedrive'

export interface CloudMount {
  id: string
  provider: CloudProvider
  displayName: string
  mountPoint: string
  connected: boolean
}

export async function listCloudMounts(): Promise<CloudMount[]> {
  const res = await client.get<{ success: number; data: CloudMount[] }>(
    '/v1/cloud',
  )
  return res.data.data ?? []
}

/**
 * connectCloudProvider — kicks off the OAuth flow for the given
 * provider. Returns the URL the UI should open in a new tab for the
 * user to authorize. The backend webhook handles the callback and
 * completes the mount without the UI needing to proxy the redirect.
 */
export async function connectCloudProvider(
  provider: CloudProvider,
): Promise<{ authorize_url: string; state: string }> {
  const res = await client.post<{
    success: number
    data: { authorize_url: string; state: string }
  }>(`/v1/cloud/${encodeURIComponent(provider)}/connect`)
  return res.data.data
}

export async function disconnectCloudMount(id: string): Promise<void> {
  await client.delete(`/v1/cloud/${encodeURIComponent(id)}`)
}

// ----- ZFS Snapshot Management -----

export interface ZFSSnapshot {
  name: string
  dataset: string
  snapshot: string
  creation: string
  used: string
  referenced: string
}

export interface ZFSDataset {
  name: string
  used: string
  available: string
  referenced: string
  mountpoint: string
}

export async function getZFSSnapshots(): Promise<{ success: number; message: string; data: ZFSSnapshot[] }> {
  const res = await client.get<{ success: number; message: string; data: ZFSSnapshot[] }>('/v1/sys/zfs/snapshots')
  return res.data
}

export async function createZFSSnapshot(dataset: string, name: string): Promise<void> {
  await client.post('/v1/sys/zfs/snapshots', { dataset, name })
}

export async function deleteZFSSnapshot(name: string): Promise<void> {
  await client.delete('/v1/sys/zfs/snapshots', { data: { name } })
}

export async function rollbackZFSSnapshot(name: string): Promise<void> {
  await client.post('/v1/sys/zfs/snapshots/rollback', { name })
}

// ----- ZFS detection + pool inventory + safe ops (Wave 3) -----

export interface ZFSStatus {
  available: boolean
  zpoolPath?: string
  zfsPath?: string
  version?: string
  poolCount: number
}

export interface ZFSVdev {
  name: string
  type: string
  state: string
  read: number
  write: number
  cksum: number
  children?: ZFSVdev[]
}

export interface ZFSScrubState {
  state: 'none' | 'scrubbing' | 'finished' | 'canceled' | string
  percentComplete: string
  errors: number
  startedAt?: string
  finishedAt?: string
  speed?: string
  repaired?: string
}

export interface ZFSPool {
  name: string
  health: string
  sizeBytes: number
  allocBytes: number
  freeBytes: number
  frag: string
  dedup: string
  vdevs: ZFSVdev[]
  scrub: ZFSScrubState
}

export async function getZFSStatus(): Promise<ZFSStatus> {
  const res = await client.get<{ success: number; data: ZFSStatus }>('/v1/sys/zfs/status')
  return res.data.data
}

export async function getZFSPools(): Promise<ZFSPool[]> {
  const res = await client.get<{ success: number; data: ZFSPool[] }>('/v1/sys/zfs/pools')
  return res.data.data ?? []
}

export async function startZFSScrub(pool: string): Promise<void> {
  await client.post('/v1/sys/zfs/scrub', { pool, action: 'start' })
}

export async function stopZFSScrub(pool: string): Promise<void> {
  await client.post('/v1/sys/zfs/scrub', { pool, action: 'stop' })
}

export async function importZFSPool(pool: string, force = false): Promise<void> {
  await client.post('/v1/sys/zfs/import', { pool, force })
}

export async function exportZFSPool(pool: string, force = false): Promise<void> {
  await client.post('/v1/sys/zfs/export', { pool, force })
}

// ----- ZFS Tier 3: destructive lifecycle (advanced mode) -----

export type ZFSTopology = 'stripe' | 'mirror' | 'raidz1' | 'raidz2' | 'raidz3'

export interface CreateZFSPoolRequest {
  name: string
  topology: ZFSTopology
  devices: string[]
  mountpoint?: string
  force?: boolean
  dryRun?: boolean
}

export interface CreateZFSPoolResponse {
  pool: string
  dryRun: string
  output: string
}

export async function createZFSPool(
  req: CreateZFSPoolRequest,
): Promise<CreateZFSPoolResponse> {
  const res = await client.post<{ success: number; data: CreateZFSPoolResponse }>(
    '/v1/sys/zfs/pools',
    req,
  )
  return res.data.data
}

export async function destroyZFSPool(
  pool: string,
  confirmName: string,
  force = false,
): Promise<void> {
  await client.delete('/v1/sys/zfs/pools', {
    data: { pool, confirmName, force },
  })
}

export async function replaceZFSVdev(
  pool: string,
  oldDevice: string,
  newDevice: string,
  force = false,
): Promise<void> {
  await client.post('/v1/sys/zfs/replace', { pool, oldDevice, newDevice, force })
}

export async function getZFSDatasets(): Promise<{ success: number; message: string; data: ZFSDataset[] }> {
  const res = await client.get<{ success: number; message: string; data: ZFSDataset[] }>('/v1/sys/zfs/datasets')
  return res.data
}

// ----- SMART Test Scheduling -----

export interface SmartScheduleEntry {
  device: string
  test_type: 'short' | 'long'
  interval: 'daily' | 'weekly' | 'monthly'
  day_of_week: number
  hour: number
  enabled: boolean
  last_run: string
  next_run: string
}

export async function getSmartSchedules(): Promise<{ success: number; message: string; data: SmartScheduleEntry[] }> {
  const res = await client.get<{ success: number; message: string; data: SmartScheduleEntry[] }>('/v1/sys/smart/schedule')
  return res.data
}

export async function postSmartSchedule(schedule: {
  device: string
  test_type: 'short' | 'long'
  interval: 'daily' | 'weekly' | 'monthly'
  day_of_week: number
  hour: number
  enabled: boolean
}): Promise<{ success: number; message: string; data: SmartScheduleEntry }> {
  const res = await client.post<{ success: number; message: string; data: SmartScheduleEntry }>(
    '/v1/sys/smart/schedule',
    schedule,
  )
  return res.data
}

export async function deleteSmartSchedule(device: string): Promise<void> {
  await client.delete('/v1/sys/smart/schedule', {
    params: { device },
  })
}

// ----- SMART Test Results -----

export interface SmartTestResultEntry {
  type: string
  status: string
  passed: boolean
  remaining: string
  lifetime_hours: number
}

export interface SmartDeviceResults {
  device: string
  tests: SmartTestResultEntry[]
}

export async function getSmartResults(): Promise<{ success: number; message: string; data: SmartDeviceResults[] }> {
  const res = await client.get<{ success: number; message: string; data: SmartDeviceResults[] }>('/v1/sys/smart/results')
  return res.data
}
