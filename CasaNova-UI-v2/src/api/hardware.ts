// Hardware API — detects motherboard, CPU, USB ports, NICs, and other
// system components from multiple Linux sources. Uses a combination of
// real-time detection (lsusb, lspci, /sys) and known motherboard
// profiles to show the correct port layout even when nothing is plugged.
//
// Detection sources:
//   GET /v1/sys/hardware    → CPU model, cores, architecture, kernel, hostname
//   GET /v1/sys/utilization → sys_usb (connected USB devices), net (NICs)
//
// Known motherboard profiles fill in physical port counts/layouts that
// Linux can't auto-detect (e.g., how many USB-A ports are on the back
// panel vs. internal headers).

import client from './client'

// ── Types ───────────────────────────────────────────────────────────

export interface CpuInfo {
  model: string
  cores: number
  threads: number
  architecture: string
  baseFreqMHz: number
  socket: string
}

export interface MemorySlot {
  slot: string        // e.g., "DIMM_A1"
  sizeGB: number      // 0 if empty
  type: string        // "DDR4", "DDR5", etc.
  speedMHz: number
}

export type PortKind = 'usb3' | 'usb2' | 'usbc' | 'ethernet' | 'hdmi' | 'vga' | 'displayport' | 'audio' | 'ps2' | 'sata' | 'm2' | 'pcie'

export interface PhysicalPort {
  id: string
  kind: PortKind
  label: string           // "USB 3.1 Gen 1", "HDMI 1.4", "RJ-45"
  location: 'rear' | 'front' | 'internal'
  connected: boolean
  deviceName?: string     // what's plugged in (if anything)
  speed?: string          // "1 Gbps", "5 Gbps", "480 Mbps"
}

export interface NicInfo {
  name: string           // "enp3s0", "wlp2s0"
  mac: string
  speed: string          // "1000 Mbps"
  state: 'up' | 'down'
  ipv4?: string
  driver?: string
}

export interface MotherboardProfile {
  manufacturer: string
  model: string
  chipset: string
  socket: string
  formFactor: string     // "Micro-ATX", "ATX", etc.
  memorySlots: number
  memoryType: string
  maxMemoryGB: number
  ports: PhysicalPort[]  // physical port layout
  storageSlots: Array<{ kind: 'sata' | 'm2'; label: string }>
  pcieSlots: Array<{ label: string; gen: string; lanes: number }>
}

export interface SystemHardware {
  motherboard: MotherboardProfile | null
  cpu: CpuInfo
  memory: MemorySlot[]
  totalMemoryGB: number
  totalMemoryBytes: number
  ports: PhysicalPort[]  // merged: profile ports + live detection
  nics: NicInfo[]
  kernelVersion: string
  hostname: string
  // ── System identity (raw DMI, may contain BIOS placeholders) ──────
  productName: string    // /sys/class/dmi/id/product_name (often a placeholder)
  sysVendor: string      // /sys/class/dmi/id/sys_vendor  (often a placeholder)
  boardName: string      // /sys/class/dmi/id/board_name  (reliable on consumer boards)
  boardVendor: string    // /sys/class/dmi/id/board_vendor
  biosVersion: string
  // Resolved, display-safe server model. Prefers product_name when it's
  // a real value; falls back to the motherboard (vendor + board_name)
  // because consumer boards ship BIOS placeholders like "System Product
  // Name" / "System manufacturer" that are useless to show a user.
  model: string
}

// ── DMI placeholder detection ───────────────────────────────────────
// Consumer motherboards ship with generic DMI strings the vendor never
// overrode. These are noise — never surface them in the UI. Compared
// case-insensitively after trimming.
const DMI_PLACEHOLDERS = new Set([
  'system product name',
  'system manufacturer',
  'system version',
  'to be filled by o.e.m.',
  'default string',
  'not specified',
  'not applicable',
  'oem',
  'o.e.m.',
  'none',
])

/**
 * isDmiPlaceholder — true when a DMI string is empty or one of the known
 * BIOS placeholder values, so callers can fall back to reliable data.
 */
export function isDmiPlaceholder(value: string | undefined | null): boolean {
  if (!value) return true
  return DMI_PLACEHOLDERS.has(value.trim().toLowerCase())
}

// ── Known Motherboard Profiles ──────────────────────────────────────
// Each profile describes the physical hardware layout. Add new boards
// as CasaNova gets deployed on more hardware.

const KNOWN_BOARDS: Record<string, MotherboardProfile> = {
  // Asus Prime A320M-K — our first test subject
  'PRIME A320M-K': {
    manufacturer: 'ASUS',
    model: 'PRIME A320M-K',
    chipset: 'AMD A320',
    socket: 'AM4',
    formFactor: 'Micro-ATX',
    memorySlots: 2,
    memoryType: 'DDR4',
    maxMemoryGB: 32,
    ports: [
      // Rear I/O
      { id: 'rear-ps2', kind: 'ps2', label: 'PS/2', location: 'rear', connected: false },
      { id: 'rear-usb2-1', kind: 'usb2', label: 'USB 2.0', location: 'rear', connected: false, speed: '480 Mbps' },
      { id: 'rear-usb2-2', kind: 'usb2', label: 'USB 2.0', location: 'rear', connected: false, speed: '480 Mbps' },
      { id: 'rear-usb3-1', kind: 'usb3', label: 'USB 3.1 Gen 1', location: 'rear', connected: false, speed: '5 Gbps' },
      { id: 'rear-usb3-2', kind: 'usb3', label: 'USB 3.1 Gen 1', location: 'rear', connected: false, speed: '5 Gbps' },
      { id: 'rear-hdmi', kind: 'hdmi', label: 'HDMI 1.4', location: 'rear', connected: false },
      { id: 'rear-vga', kind: 'vga', label: 'VGA (D-Sub)', location: 'rear', connected: false },
      { id: 'rear-eth', kind: 'ethernet', label: 'RJ-45 Gigabit', location: 'rear', connected: false, speed: '1 Gbps', deviceName: 'Realtek RTL8111H' },
      { id: 'rear-audio-1', kind: 'audio', label: 'Line Out', location: 'rear', connected: false },
      { id: 'rear-audio-2', kind: 'audio', label: 'Line In', location: 'rear', connected: false },
      { id: 'rear-audio-3', kind: 'audio', label: 'Mic In', location: 'rear', connected: false },
      // Internal headers
      { id: 'int-usb3-header', kind: 'usb3', label: 'USB 3.1 Header', location: 'internal', connected: false, speed: '5 Gbps' },
      { id: 'int-usb2-header-1', kind: 'usb2', label: 'USB 2.0 Header 1', location: 'internal', connected: false, speed: '480 Mbps' },
      { id: 'int-usb2-header-2', kind: 'usb2', label: 'USB 2.0 Header 2', location: 'internal', connected: false, speed: '480 Mbps' },
    ],
    storageSlots: [
      { kind: 'sata', label: 'SATA6G_1' },
      { kind: 'sata', label: 'SATA6G_2' },
      { kind: 'sata', label: 'SATA6G_3' },
      { kind: 'sata', label: 'SATA6G_4' },
      { kind: 'm2', label: 'M.2 (M Key, PCIe 3.0 x4)' },
    ],
    pcieSlots: [
      { label: 'PCIEX16_1', gen: '3.0', lanes: 16 },
      { label: 'PCIEX1_1', gen: '2.0', lanes: 1 },
      { label: 'PCIEX1_2', gen: '2.0', lanes: 1 },
    ],
  },
}

// ── Detection ───────────────────────────────────────────────────────

/**
 * detectMotherboard — tries to match the system's board name against
 * our known profiles. Falls back to null (generic mode) if unrecognized.
 * The board name comes from the backend's /v1/sys/hardware endpoint or
 * from DMI data exposed in /v1/sys/utilization.
 */
function matchBoard(boardName: string): MotherboardProfile | null {
  if (!boardName) return null
  // Try exact match first
  const upper = boardName.toUpperCase().trim()
  for (const [key, profile] of Object.entries(KNOWN_BOARDS)) {
    if (upper.includes(key.toUpperCase())) return { ...profile }
  }
  return null
}

/**
 * nicIdentity — builds a human-readable identity string for a NIC row.
 * Interface name is always present; IPv4 and MAC are appended only when
 * actually known, so an interface with no address renders as plain
 * "enp1s0" instead of the old dangling "enp1s0 — ". No trailing/empty
 * separators are ever emitted.
 */
function nicIdentity(nic: NicInfo): string {
  // Show IPv4 first (most useful) then MAC. Both are optional; the empty
  // strings we filter out are exactly what produced the stray "—".
  const detail = [nic.ipv4, nic.mac].filter((v): v is string => !!v).join(' · ')
  return detail ? `${nic.name} — ${detail}` : nic.name
}

/**
 * mergePortStatus — takes the static profile ports and overlays live
 * USB/NIC detection data so connected devices show their real names.
 */
function mergePortStatus(
  profilePorts: PhysicalPort[],
  connectedUsbs: Array<{ name?: string; model?: string }>,
  nics: NicInfo[],
): PhysicalPort[] {
  const ports = profilePorts.map((p) => ({ ...p }))

  // Match connected USB devices to available USB ports (rear first)
  const usbPorts = ports
    .filter((p) => (p.kind === 'usb3' || p.kind === 'usb2' || p.kind === 'usbc') && p.location === 'rear')
  let usbIdx = 0
  for (const usb of connectedUsbs) {
    if (usbIdx >= usbPorts.length) break
    usbPorts[usbIdx].connected = true
    usbPorts[usbIdx].deviceName = usb.model || usb.name || 'USB Device'
    usbIdx++
  }

  // Mark ethernet as connected if any NIC is up
  const ethPorts = ports.filter((p) => p.kind === 'ethernet')
  for (const nic of nics) {
    if (nic.state === 'up') {
      const ethPort = ethPorts.find((p) => !p.connected)
      if (ethPort) {
        ethPort.connected = true
        ethPort.deviceName = nicIdentity(nic)
        // Prefer the NIC's live link speed, but never blank out the
        // profile's known port speed (e.g. "1 Gbps") when the kernel
        // reports 0/unknown for the interface.
        if (nic.speed) ethPort.speed = nic.speed
      }
    }
  }

  return ports
}

// ── Public API ──────────────────────────────────────────────────────

export async function getSystemHardware(): Promise<SystemHardware> {
  // Fetch from both endpoints in parallel
  const [hwRes, utilRes] = await Promise.allSettled([
    client.get<{ success: number; data: {
      cpu_model?: string; cpuModel?: string; cpuCores?: number; totalMemoryBytes?: number;
      architecture?: string; kernelVersion?: string; hostname?: string;
      board_name?: string; board_vendor?: string;
      product_name?: string; sys_vendor?: string; bios_version?: string;
      drive_model?: string; arch?: string;
    } }>('/v1/sys/hardware'),
    client.get<{ success: number; data: {
      cpu?: { model?: string; num?: number };
      mem?: { total?: number };
      sys_usb?: Array<{ name?: string; model?: string; size?: number }>;
      net?: Array<{ name?: string; state?: string; speed?: number; mac?: string; ipv4?: string; driver?: string }>;
    } }>('/v1/sys/utilization'),
  ])

  const hw = hwRes.status === 'fulfilled' ? hwRes.value.data.data : {} as Record<string, unknown>
  const util = utilRes.status === 'fulfilled' ? utilRes.value.data.data : {} as Record<string, unknown>

  const hwAny = hw as Record<string, unknown>
  const utilAny = util as Record<string, unknown>
  const cpuUtil = (utilAny.cpu || {}) as Record<string, unknown>
  const memUtil = (utilAny.mem || {}) as Record<string, unknown>
  const sysUsb = (utilAny.sys_usb || []) as Array<{ name?: string; model?: string; size?: number }>
  const netRaw = (utilAny.net || []) as Array<{ name?: string; state?: string; speed?: number; mac?: string; ipv4?: string; driver?: string }>

  // Build NIC list from utilization data
  const nics: NicInfo[] = netRaw
    .filter((n) => n.name && !n.name.startsWith('lo') && !n.name.startsWith('docker') && !n.name.startsWith('br-') && !n.name.startsWith('veth'))
    .map((n) => ({
      name: n.name || '',
      mac: n.mac || '',
      speed: n.speed ? `${n.speed} Mbps` : '',
      state: (n.state === 'up' ? 'up' : 'down') as 'up' | 'down',
      ipv4: n.ipv4,
      driver: n.driver,
    }))

  // Try to detect motherboard. The backend returns `board_name` from
  // /sys/class/dmi/id/board_name when DMI is available (x86 hosts and
  // some ARM boards). Compose a "vendor + name" hint when both fields
  // present — matchBoard's lookup table is forgiving on whitespace.
  const boardName = (hwAny.board_name || '') as string
  const boardVendor = (hwAny.board_vendor || '') as string
  const profile = matchBoard(boardName ? `${boardVendor} ${boardName}`.trim() : '')

  // Prefer the real CPU model from /v1/sys/hardware (e.g. "Intel Core
  // i5-8259U"). The utilization endpoint's cpu.model is a *tier* string
  // ("intel" / "amd" / "arm") — useful as a fallback only.
  const cpuModel = (hwAny.cpu_model || hwAny.cpuModel || cpuUtil.model || 'Unknown CPU') as string
  // Core count lives on the utilization endpoint (cpu.num); the hardware
  // endpoint doesn't carry it. Never coerce a missing count to a real 0 —
  // callers treat 0 as "unknown" and hide the cores line rather than lie.
  const cpuCores = (hwAny.cpuCores || cpuUtil.num || 0) as number
  const totalMem = (hwAny.totalMemoryBytes || memUtil.total || 0) as number

  // Resolve a display-safe server model. product_name is preferred when
  // real, but consumer boards ship BIOS placeholders, so fall back to the
  // motherboard identity (vendor + board_name) which is reliable.
  const productName = (hwAny.product_name || '') as string
  const sysVendor = (hwAny.sys_vendor || '') as string
  const boardLabel = boardName ? `${boardVendor} ${boardName}`.trim() : ''
  const serverModel = !isDmiPlaceholder(productName)
    ? productName.trim()
    : boardLabel

  // Build port list — use profile if available, otherwise generate from live data
  let ports: PhysicalPort[]
  if (profile) {
    ports = mergePortStatus(profile.ports, sysUsb, nics)
  } else {
    // Generic: build from what we can detect
    ports = []
    for (const [idx, usb] of sysUsb.entries()) {
      ports.push({
        id: `usb-${idx}`,
        kind: 'usb3',
        label: `USB ${idx + 1}`,
        location: 'rear',
        connected: true,
        deviceName: usb.model || usb.name || 'USB Device',
      })
    }
    for (const nic of nics) {
      ports.push({
        id: `nic-${nic.name}`,
        kind: 'ethernet',
        label: nic.name,
        location: 'rear',
        connected: nic.state === 'up',
        deviceName: nic.driver || nic.name,
        speed: nic.speed,
      })
    }
  }

  return {
    motherboard: profile,
    cpu: {
      model: cpuModel,
      cores: cpuCores,
      threads: ((hwAny.cpuThreads || cpuUtil.threads || 0) as number) || cpuCores, // fallback to cores if threads unknown
      architecture: (hwAny.architecture || hwAny.arch || 'x86_64') as string,
      baseFreqMHz: 0,
      socket: profile?.socket || '',
    },
    memory: profile
      ? Array.from({ length: profile.memorySlots }, (_, i) => ({
          slot: `DIMM_${String.fromCharCode(65 + i)}1`,
          sizeGB: Math.round(totalMem / profile.memorySlots / (1024 ** 3)),
          type: profile.memoryType,
          speedMHz: 0,
        }))
      : [{ slot: 'DIMM_A1', sizeGB: Math.round(totalMem / (1024 ** 3)), type: 'DDR4', speedMHz: 0 }],
    totalMemoryGB: Math.round(totalMem / (1024 ** 3)),
    totalMemoryBytes: totalMem,
    ports,
    nics,
    kernelVersion: (hwAny.kernelVersion || '') as string,
    hostname: (hwAny.hostname || '') as string,
    productName,
    sysVendor,
    boardName,
    boardVendor,
    biosVersion: (hwAny.bios_version || '') as string,
    model: serverModel,
  }
}
