// System API — wraps every /v1/sys/* endpoint the UI cares about.
//
// Matches the routes defined in CasaNova/route/v1.go v1SysGroup:
//   GET    /v1/sys/utilization      → SystemUtilization (CPU/MEM live)
//   GET    /v1/sys/version/current  → current version string
//   GET    /v1/sys/version/check    → latest version + is_new flag
//   GET    /v1/sys/hardware         → hardware info (CPU model, RAM, etc)
//   GET    /v1/sys/diagnosis        → self-diagnosis engine results
//   GET    /v1/sys/audit            → audit event log
//   GET    /v1/sys/debug            → verbose config dump
//   POST   /v1/sys/update           → trigger system update
//   POST   /v1/sys/ssh-login        → SSH session login
//   PUT    /v1/sys/state/restart    → reboot the host
//   PUT    /v1/sys/state/off        → power off the host
//
// Every function here is a thin typed wrapper. The store layer wraps
// these calls in `safeCall()` to opt into fixture fallback.

import client from './client'

export interface NetInterfaceStat {
  name: string
  bytesSent: number
  bytesRecv: number
  state: string
  time: number
  ipv4?: string
}

export interface SystemUtilization {
  cpu: {
    percent: number
    temp: number
  }
  mem: {
    total: number
    used: number
    free: number
    usedPercent: number
  }
  sysDisk: {
    avail: number
    health: boolean
    size: number
    used: number
  }
  uptime: number
  loadAverage: [number, number, number]
  net: NetInterfaceStat[]
}

// The backend returns snake_case keys; normalize here.
interface RawUtilization {
  cpu: { percent: number; temperature: number; num: number; model: string }
  mem: { total: number; used: number; free: number; available: number; usedPercent: number }
  sys_disk?: { avail: number; health: boolean; size: number; used: number }
  uptime?: number
  loadavg?: number[]
  net?: Array<{ name: string; bytesSent: number; bytesRecv: number; state: string; time: number; ipv4?: string }>
  sys_usb?: unknown[]
}

export async function getUtilization(): Promise<SystemUtilization> {
  const res = await client.get<{ success: number; data: RawUtilization }>(
    '/v1/sys/utilization',
  )
  const raw = res.data.data
  return {
    cpu: { percent: raw.cpu.percent, temp: raw.cpu.temperature ?? 0 },
    mem: {
      total: raw.mem.total,
      used: raw.mem.used,
      free: raw.mem.free,
      usedPercent: raw.mem.usedPercent,
    },
    sysDisk: raw.sys_disk ?? { avail: 0, health: true, size: 0, used: 0 },
    uptime: raw.uptime ?? 0,
    loadAverage: (raw.loadavg && raw.loadavg.length >= 3
      ? [raw.loadavg[0], raw.loadavg[1], raw.loadavg[2]]
      : [0, 0, 0]) as [number, number, number],
    net: (raw.net ?? []).map(n => ({
      name: n.name,
      bytesSent: n.bytesSent,
      bytesRecv: n.bytesRecv,
      state: n.state,
      time: n.time,
      ipv4: n.ipv4,
    })),
  }
}

export interface VersionInfo {
  current: string
  latest: string
  is_new: boolean
  changelog?: string
}

export async function getCurrentVersion(): Promise<string> {
  const res = await client.get<{ success: number; data: string } | string>(
    '/v1/sys/version/current',
  )
  // Handle both envelope ({ success, data }) and bare string responses
  // so the function works with every CasaNova backend revision.
  if (typeof res.data === 'object' && res.data !== null && 'data' in res.data) {
    return String((res.data as { data: unknown }).data)
  }
  return String(res.data)
}

export async function checkVersion(): Promise<VersionInfo> {
  const res = await client.get<{ success: number; data: VersionInfo }>(
    '/v1/sys/version/check',
  )
  return res.data.data
}

export interface HardwareInfo {
  cpuModel: string
  cpuCores: number
  totalMemoryBytes: number
  architecture: string
  kernelVersion: string
  hostname: string
}

export async function getHardware(): Promise<HardwareInfo> {
  const res = await client.get<{ success: number; data: HardwareInfo }>(
    '/v1/sys/hardware',
  )
  return res.data.data
}

// ----- Audit log -----

export type AuditSeverity = 'info' | 'warning' | 'error' | 'critical'
export type AuditCategory =
  | 'auth'
  | 'app'
  | 'storage'
  | 'network'
  | 'system'
  | 'security'
  | 'backup'

export interface AuditEvent {
  id: string
  timestamp: string
  severity: AuditSeverity
  category: AuditCategory
  actor: string
  ip: string
  message: string
  metadata?: Record<string, unknown>
}

export interface AuditListResult {
  events: AuditEvent[]
  total: number
}

export async function getAuditEvents(params?: {
  severity?: AuditSeverity
  category?: AuditCategory
  since?: string
  until?: string
  limit?: number
  offset?: number
}): Promise<AuditListResult> {
  const res = await client.get<{ success: number; data: AuditListResult }>(
    '/v1/sys/audit',
    { params },
  )
  return res.data.data
}

// ----- Diagnosis -----

// Mirrors CasaNova/pkg/diagnosis/diagnosis.go exactly. NB: this endpoint is
// the repo outlier — it returns the object BARE, without the {success, data}
// envelope, and always HTTP 200.
export type DiagnosisSeverity = 'ok' | 'info' | 'warning' | 'error'
export type DiagnosisCategory =
  | 'system' | 'services' | 'storage' | 'network' | 'container' | 'security'

export interface DiagnosticResult {
  category: DiagnosisCategory
  check: string
  title: string
  summary: string
  details: string
  severity: DiagnosisSeverity
  fix?: string
  checked_at: string
}

export interface DiagnosisResponse {
  results: DiagnosticResult[]
  summary: Record<DiagnosisSeverity, number>
  by_category: Partial<Record<DiagnosisCategory, DiagnosticResult[]>>
  has_error: boolean
}

/** getDiagnosis — GET /v1/sys/diagnosis. force=true bypasses the 5-min
 * server-side cache (the "Run diagnostics" button). Checks run in parallel
 * server-side with a 30s ceiling — give the request room. */
export async function getDiagnosis(force = false): Promise<DiagnosisResponse> {
  const res = await client.get<DiagnosisResponse>('/v1/sys/diagnosis', {
    params: force ? { force: 'true' } : undefined,
    timeout: 45_000,
  })
  return res.data
}

// ----- Power management -----

export async function reboot(): Promise<void> {
  await client.put('/v1/sys/state/restart')
}

export async function shutdown(): Promise<void> {
  await client.put('/v1/sys/state/off')
}

// ----- System identity -----

export async function getHostname(): Promise<string> {
  const res = await client.get<{ success: number; data: { hostname: string } }>(
    '/v1/sys/hostname',
  )
  return res.data.data.hostname
}

export async function setHostname(hostname: string): Promise<void> {
  await client.put('/v1/sys/hostname', { hostname })
}

export interface TimezoneInfo {
  timezone: string
  list: string[]
}

export async function getTimezone(): Promise<TimezoneInfo> {
  const res = await client.get<{ success: number; data: TimezoneInfo }>(
    '/v1/sys/timezone',
  )
  return res.data.data
}

export async function setTimezone(timezone: string): Promise<void> {
  await client.put('/v1/sys/timezone', { timezone })
}

// ----- TLS configuration -----

export interface TLSConfig {
  enabled: boolean
  domain: string
  email: string
}

export async function getTLSConfig(): Promise<TLSConfig> {
  const res = await client.get<{ success: number; data: TLSConfig }>(
    '/v1/sys/certs/config',
  )
  return res.data.data
}

export async function setTLSConfig(cfg: TLSConfig): Promise<void> {
  await client.put('/v1/sys/certs/config', cfg)
}

// ----- Gateway IP whitelist -----

export async function getGatewayWhitelist(): Promise<string[]> {
  const res = await client.get<{ success: number; data: string[] }>(
    '/v1/gateway/whitelist',
  )
  return res.data.data ?? []
}

export async function addGatewayWhitelist(cidr: string): Promise<string[]> {
  const res = await client.post<{ success: number; data: string[] }>(
    '/v1/gateway/whitelist',
    { cidr },
  )
  return res.data.data ?? []
}

export async function removeGatewayWhitelist(cidr: string): Promise<string[]> {
  const res = await client.delete<{ success: number; data: string[] }>(
    '/v1/gateway/whitelist',
    { params: { cidr } },
  )
  return res.data.data ?? []
}

// ----- SSH -----

export interface SshLoginRequest {
  username: string
  password: string
  port: string
}

export interface SshLoginResponse {
  session_id: string
  expires_at: number
}

export async function sshLogin(body: SshLoginRequest): Promise<SshLoginResponse> {
  const res = await client.post<{ success: number; data: SshLoginResponse }>(
    '/v1/sys/ssh-login',
    body,
  )
  return res.data.data
}

// ----- System journal logs -----

export interface LogEntry {
  timestamp: string
  unit: string
  priority: number
  message: string
}

export interface LogsParams {
  unit?: string
  lines?: number
  priority?: string
  since?: string
}

export async function getSystemLogs(params?: LogsParams): Promise<LogEntry[]> {
  const res = await client.get<{ success: number; data: LogEntry[] }>(
    '/v1/sys/logs/journal',
    { params },
  )
  return res.data.data
}

export async function getLogUnits(): Promise<string[]> {
  const res = await client.get<{ success: number; data: string[] }>(
    '/v1/sys/logs/units',
  )
  return res.data.data
}

// ----- Virtualisation capabilities -----

export interface VirtCapabilities {
  cpu_virt: boolean
  cpu_virt_type: string
  kvm_available: boolean
  libvirt_installed: boolean
}

export async function getVirtCapabilities(): Promise<VirtCapabilities> {
  const res = await client.get<{ success: number; data: VirtCapabilities }>(
    '/v1/sys/virt/capabilities',
  )
  return res.data.data
}

// ----- Troubleshooter -----

export type TroubleshootAction =
  | 'fix-docker-network'
  | 'restart-gateway'
  | 'restart-all'
  | 'flush-dns'

export async function runTroubleshoot(action: TroubleshootAction): Promise<string> {
  const res = await client.post<{ success: number; message: string }>(
    `/v1/sys/troubleshoot/${action}`,
  )
  return res.data.message
}
