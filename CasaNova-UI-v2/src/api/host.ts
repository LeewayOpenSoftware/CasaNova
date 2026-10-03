// Host administration API — OS-level tunables surfaced in Settings → Host.
//
// Two backends sit behind the gateway (:85):
//   • CPU governor + swappiness → casanova-driver (/v2/driver/tuning/*), the
//     only root service allowed to write /proc/sys and /sys.
//   • Docker memory cap + apt updates → main casanova (/v1/sys/host/*).
//
// Every mutation is gated server-side by PermSystemConfig and validated
// against what the kernel/system actually exposes — the client never sends a
// free-form governor, sysctl path, unit name, or package name.

import client from './client'

// ── CPU governor ────────────────────────────────────────────────────
export interface CPUGovernorInfo {
  available: string[]
  current: string   // common governor across cores, or "mixed"
  perCore: string[]
  driver: string
  supported: boolean
  epp: string
  eppAvailable: string[] | null
}

export async function setEPP(epp: string): Promise<CPUGovernorInfo> {
  const res = await client.put<{ success: number; data: CPUGovernorInfo }>(
    '/v2/driver/tuning/epp', { epp },
  )
  return res.data.data
}

export async function getCPUGovernor(): Promise<CPUGovernorInfo> {
  const res = await client.get<{ success: number; data: CPUGovernorInfo }>(
    '/v2/driver/tuning/cpu-governor',
  )
  return res.data.data
}

export async function setCPUGovernor(governor: string): Promise<CPUGovernorInfo> {
  const res = await client.put<{ success: number; data: CPUGovernorInfo }>(
    '/v2/driver/tuning/cpu-governor',
    { governor },
  )
  return res.data.data
}

// ── Swappiness ──────────────────────────────────────────────────────
export interface SwappinessInfo {
  value: number
  swapTotalKiB: number
  swapFreeKiB: number
  memTotalKiB: number
}

export async function getSwappiness(): Promise<SwappinessInfo> {
  const res = await client.get<{ success: number; data: SwappinessInfo }>(
    '/v2/driver/tuning/swappiness',
  )
  return res.data.data
}

export async function setSwappiness(value: number): Promise<SwappinessInfo> {
  const res = await client.put<{ success: number; data: SwappinessInfo }>(
    '/v2/driver/tuning/swappiness',
    { value },
  )
  return res.data.data
}

// ── Docker memory limit ─────────────────────────────────────────────
export interface HostMemoryInfo {
  memTotalKiB: number
  dockerUsedBytes: number  // -1 when unknown
  memoryMax: string        // "infinity" when unlimited
  memoryHigh: string
  floorMiB: number
}

export async function getHostMemory(): Promise<HostMemoryInfo> {
  const res = await client.get<{ success: number; data: HostMemoryInfo }>(
    '/v1/sys/host/memory',
  )
  return res.data.data
}

// memoryMax is a systemd size like "4G"/"512M", or "infinity" to clear it.
export async function setHostMemory(memoryMax: string): Promise<HostMemoryInfo> {
  const res = await client.put<{ success: number; data: HostMemoryInfo }>(
    '/v1/sys/host/memory',
    { memoryMax },
  )
  return res.data.data
}

// ── APT updates ─────────────────────────────────────────────────────
export interface AptPackage {
  name: string
  currentVersion: string
  newVersion: string
  security: boolean
}

export interface AptStatus {
  supported: boolean
  checkedAt: string
  checking: boolean
  /** True only when NOTHING refreshed. A dead third-party PPA no longer sets it. */
  stale: boolean
  /** Repositories that did not refresh while others did — named so the operator can act. */
  failedRepos?: string[]
  total: number
  security: number
  packages: AptPackage[]
  rebootRequired: boolean
  rebootPackages: string[]
}

export async function getAptUpdates(): Promise<AptStatus> {
  const res = await client.get<{ success: number; data: AptStatus }>(
    '/v1/sys/host/apt/updates',
  )
  return res.data.data
}

export async function runAptCheck(): Promise<void> {
  await client.post('/v1/sys/host/apt/check')
}

export async function runAptUpgrade(): Promise<void> {
  await client.post('/v1/sys/host/apt/upgrade')
}

export interface AptUpgradeStatus {
  state: 'idle' | 'running' | 'success' | 'failed'
  exitCode: number
  tail: string[]
  rebootRequired: boolean
  rebootPackages: string[]
}

export async function getAptUpgradeStatus(): Promise<AptUpgradeStatus> {
  const res = await client.get<{ success: number; data: AptUpgradeStatus }>(
    '/v1/sys/host/apt/upgrade/status',
  )
  return res.data.data
}

// ── I/O scheduler + read-ahead + spindown (per disk) ────────────────
export interface DiskScheduler {
  name: string
  byId: string
  model: string
  rotational: boolean
  removable: boolean
  sizeBytes: number
  available: string[]
  current: string
  readAheadKiB: number
  spindown: string
}

export async function getIOSchedulers(): Promise<DiskScheduler[]> {
  const res = await client.get<{ success: number; data: DiskScheduler[] }>('/v2/driver/tuning/io-scheduler')
  return res.data.data
}

export async function setIOScheduler(disk: string, scheduler?: string, readAheadKiB?: number): Promise<DiskScheduler[]> {
  const res = await client.put<{ success: number; data: DiskScheduler[] }>(
    '/v2/driver/tuning/io-scheduler', { disk, scheduler, readAheadKiB },
  )
  return res.data.data
}

export async function setSpindown(disk: string, timeout: string): Promise<DiskScheduler[]> {
  const res = await client.put<{ success: number; data: DiskScheduler[] }>(
    '/v2/driver/tuning/spindown', { disk, timeout },
  )
  return res.data.data
}

// ── Sysctl allowlist ────────────────────────────────────────────────
export interface SysctlEntry {
  key: string
  label: string
  value: string
  default: string
  kind: string
  min?: number
  max?: number
  options?: string[]
}

export async function getSysctls(): Promise<SysctlEntry[]> {
  const res = await client.get<{ success: number; data: SysctlEntry[] }>('/v2/driver/tuning/sysctl')
  return res.data.data
}

export async function setSysctl(key: string, value: string): Promise<SysctlEntry[]> {
  const res = await client.put<{ success: number; data: SysctlEntry[] }>('/v2/driver/tuning/sysctl', { key, value })
  return res.data.data
}

// ── zram ────────────────────────────────────────────────────────────
export interface ZramInfo {
  enabled: boolean
  algorithm: string
  available: string[]
  diskSizeMiB: number
  origDataMiB: number
  comprDataMiB: number
  memTotalMiB: number
}

export async function getZram(): Promise<ZramInfo> {
  const res = await client.get<{ success: number; data: ZramInfo }>('/v2/driver/tuning/zram')
  return res.data.data
}

export async function setZram(enable: boolean, algorithm: string, sizeMiB: number): Promise<ZramInfo> {
  const res = await client.put<{ success: number; data: ZramInfo }>(
    '/v2/driver/tuning/zram', { enable, algorithm, sizeMiB },
  )
  return res.data.data
}

// ── Sensors (read-only) ─────────────────────────────────────────────
export interface TempReading { label: string; celsius: number; critC?: number }
export interface FanReading { label: string; rpm: number }
export interface SensorChip { name: string; temps: TempReading[]; fans: FanReading[] }
export interface SensorReport { chips: SensorChip[]; supported: boolean }

export async function getSensors(): Promise<SensorReport> {
  const res = await client.get<{ success: number; data: SensorReport }>('/v2/driver/sensors')
  return res.data.data
}

// ── Unattended security upgrades ────────────────────────────────────
export interface AptAutoStatus { supported: boolean; enabled: boolean }

export async function getAptAuto(): Promise<AptAutoStatus> {
  const res = await client.get<{ success: number; data: AptAutoStatus }>('/v1/sys/host/apt/auto')
  return res.data.data
}
export async function setAptAuto(enabled: boolean): Promise<AptAutoStatus> {
  const res = await client.put<{ success: number; data: AptAutoStatus }>('/v1/sys/host/apt/auto', { enabled })
  return res.data.data
}
export async function runSecurityNow(): Promise<void> {
  await client.post('/v1/sys/host/apt/security-now')
}

// ── Curated services ────────────────────────────────────────────────
export interface HostServiceStatus {
  key: string
  unit: string
  label: string
  installed: boolean
  active: boolean
  enabled: boolean
  canToggle: boolean
  warn?: string
}

export async function getServices(): Promise<HostServiceStatus[]> {
  const res = await client.get<{ success: number; data: HostServiceStatus[] }>('/v1/sys/host/services')
  return res.data.data
}
export async function serviceAction(key: string, action: 'restart' | 'enable' | 'disable'): Promise<HostServiceStatus[]> {
  const res = await client.post<{ success: number; data: HostServiceStatus[] }>(`/v1/sys/host/services/${key}/${action}`)
  return res.data.data
}

// ── UPS (read-only) ─────────────────────────────────────────────────
export interface UPSInfo {
  detected: boolean
  tool: string
  remote: boolean
  host: string
  name: string
  status: string
  chargePct: number
  loadPct: number
  runtimeSec: number
  inputVoltage: number
  model: string
  error?: string
}

/** Each upsc/apcaccess call is bounded at 15s with a further 5s WaitDelay
 *  (host_ups.go:133,151,176 and cmd_exec.go:28,31), and a read makes more than
 *  one. Below that the card errored on every poll instead of saying it could
 *  not reach upsd. */
const UPS_TIMEOUT_MS = 50_000

export async function getUPS(): Promise<UPSInfo> {
  const res = await client.get<{ success: number; data: UPSInfo }>('/v1/sys/host/ups', {
    timeout: UPS_TIMEOUT_MS,
  })
  return res.data.data
}

export interface UPSConfig { host: string; upsName: string }

export async function getUPSConfig(): Promise<UPSConfig> {
  const res = await client.get<{ success: number; data: UPSConfig }>('/v1/sys/host/ups/config')
  return res.data.data
}

// Set a remote NUT host (e.g. 192.0.2.10) — empty host reverts to local.
export async function setUPSConfig(host: string, upsName: string): Promise<UPSInfo> {
  const res = await client.put<{ success: number; data: UPSInfo }>(
    '/v1/sys/host/ups/config',
    { host, upsName },
    { timeout: UPS_TIMEOUT_MS },
  )
  return res.data.data
}

// ── UPS-aware power orchestrator ────────────────────────────────────
// Consumes the Forza power daemon (GET /api/power). Opt-in: observes UPS
// state always, only acts on the box (ordered shutdown) when enabled.
export interface ForzaEstimator {
  soc_pct: number
  confidence: string
  est_runtime_min: number
  discharge_rate_pct_min: number
  charge_rate_pct_hr: number
  critical_window?: {
    shutdown_trigger_min: number
    battery_low_threshold: number
    shutdown_delay_sec: number
  } | null
}
export interface ForzaPower {
  state: string
  on_battery: boolean
  soc_pct: number
  est_runtime_min: number
  load_pct: number
  advice: string // normal | prepare_shutdown | shutdown_now
  estimator?: ForzaEstimator | null
  reachable: boolean
  fetchedAt: string
}
export interface PeerReportState {
  registered: boolean
  peerId: string
  lastWatts: number
  powerMode: string
  mac: string
}
export interface LadderView {
  active: boolean
  levers: string[]
}
export interface PowerStatus {
  power: ForzaPower
  error: string
  reachable: boolean
  enabled: boolean
  peer: PeerReportState
  ladder?: LadderView
}
export interface PowerConfig {
  enabled: boolean
  forzaUrl: string
  autoShutdown: boolean
  essentialApps: string[]
  shutdownConfirm: number
  forzaToken: string
  idleWatts: number
  maxWatts: number
  savingsLadder: boolean
  spindownDisks: string[]
  pauseNonEssential: boolean
}

export async function getPower(): Promise<PowerStatus> {
  const res = await client.get<{ success: number; data: PowerStatus }>('/v1/sys/host/power')
  return res.data.data
}
export async function getPowerConfig(): Promise<PowerConfig> {
  const res = await client.get<{ success: number; data: PowerConfig }>('/v1/sys/host/power/config')
  return res.data.data
}
export async function setPowerConfig(cfg: PowerConfig): Promise<PowerConfig> {
  const res = await client.put<{ success: number; data: PowerConfig }>('/v1/sys/host/power/config', cfg)
  return res.data.data
}

// ── Process viewer (read-only) ──────────────────────────────────────
export interface ProcInfo {
  pid: number
  name: string
  user: string
  cpuPct: number
  memRssBytes: number
  owner: string
}

export async function getProcesses(limit = 25): Promise<ProcInfo[]> {
  const res = await client.get<{ success: number; data: ProcInfo[] }>(`/v1/sys/host/processes?limit=${limit}`)
  return res.data.data
}
