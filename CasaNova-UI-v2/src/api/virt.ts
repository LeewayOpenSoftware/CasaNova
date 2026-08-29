// CasaNova-Virt API — the VM management backend (libvirt/QEMU on the host).
//
// Base path /v2/virtualization is proxied by the gateway. UNLIKE the CasaOS
// v1 endpoints, this service returns RAW JSON (no {success,data} envelope), so
// callers use `response.data` directly with no unwrap.
import client from './client'

export interface VirtInfo {
  hostname: string
  libvirt_version: string
  hypervisor: string
  cpu_model: string
  cpu_count: number
  memory_total: number // MiB
  arch: string
  kvm_enabled: boolean
  active_domains: number
  total_domains: number
}

// libvirt domain state, lower-cased by the backend.
export type DomainStatus =
  | 'nostate' | 'running' | 'blocked' | 'paused'
  | 'shutdown' | 'shutoff' | 'crashed' | 'pmsuspended'

export interface DomainSummary {
  name: string
  uuid: string
  id: number // -1 when not running
  status: DomainStatus
  memory: number // MiB
  vcpu: number
  autostart: boolean
}

// Lifecycle verbs the backend accepts on PUT /domain/:name/status.
export type DomainAction =
  | 'start' | 'shutdown' | 'reboot' | 'stop' | 'suspend' | 'resume'

export interface StoragePool {
  name: string
  uuid: string
  state: string
  capacity: number
  allocation: number
  available: number
}

// Remote-console coordinates (VNC/SPICE). The ws proxy may not be wired yet,
// so the shape is intentionally loose — we surface whatever the backend gives.
export interface DomainConsole {
  type?: string // vnc | spice
  host?: string
  port?: number
  websocket?: string
  password?: string
  [k: string]: unknown
}

const BASE = '/v2/virtualization'

export async function getVirtInfo(): Promise<VirtInfo> {
  return (await client.get<VirtInfo>(`${BASE}/info`)).data
}

export async function listDomains(): Promise<DomainSummary[]> {
  return (await client.get<DomainSummary[]>(`${BASE}/domain`)).data
}

export async function getDomain(name: string): Promise<Record<string, unknown>> {
  return (await client.get(`${BASE}/domain/${encodeURIComponent(name)}`)).data
}

export async function domainAction(name: string, action: DomainAction): Promise<void> {
  await client.put(`${BASE}/domain/${encodeURIComponent(name)}/status`, { action })
}

export async function setAutostart(name: string, enabled: boolean): Promise<void> {
  await client.post(`${BASE}/domain/${encodeURIComponent(name)}/autostart`, { enabled })
}

export async function deleteDomain(name: string): Promise<void> {
  await client.delete(`${BASE}/domain/${encodeURIComponent(name)}`)
}

export async function getConsole(name: string): Promise<DomainConsole> {
  return (await client.get<DomainConsole>(`${BASE}/domain/${encodeURIComponent(name)}/vnc`)).data
}

export async function listPools(): Promise<StoragePool[]> {
  return (await client.get<StoragePool[]>(`${BASE}/pool`)).data
}

// ── ISO images + VM creation (create-VM wizard) ───────────────────────

export interface IsoEntry {
  name: string
  path: string
  size_bytes: number
}

// Simplified wizard inputs, mapped to the backend's full
// VirtualMachineConfiguration on the wire.
export interface VmCreateInput {
  name: string
  memoryMiB: number
  vcpu: number
  diskGiB: number
  firmware: 'bios' | 'uefi'
  isoPath?: string // installer/boot ISO attached as a cdrom
  cloudInitUserData?: string
}

export async function listISOs(): Promise<IsoEntry[]> {
  return (await client.get<IsoEntry[]>(`${BASE}/iso`)).data
}

export async function downloadISO(url: string, name: string): Promise<void> {
  await client.post(`${BASE}/iso/download`, { url, name })
}

export async function createDomain(input: VmCreateInput): Promise<Record<string, unknown>> {
  const disks: Array<Record<string, unknown>> = [{ device: 'disk', size_gib: input.diskGiB }]
  if (input.isoPath) disks.push({ device: 'cdrom', source: input.isoPath })
  const cfg: Record<string, unknown> = {
    name: input.name,
    memory: { value: input.memoryMiB },
    vcpu: { count: input.vcpu },
    os: { type: 'hvm', arch: 'x86_64', firmware: input.firmware },
    disks,
    interfaces: [{ type: 'network', source: 'default', model: 'virtio' }],
    graphics: [{ type: 'vnc', port: -1, autoport: true, listen: '0.0.0.0' }],
  }
  if (input.cloudInitUserData && input.cloudInitUserData.trim()) {
    cfg.cloud_init = { user_data: input.cloudInitUserData }
  }
  return (await client.post(`${BASE}/domain`, cfg)).data
}
