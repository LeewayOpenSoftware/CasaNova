// Mock hardware/ports fixture for the Network settings "Ports" card.
//
// Inspired by ZimaOS's hardware-port visualization. The backend will
// enumerate physical ports by walking /sys/class/net/ (ethernet),
// `lsusb` (USB), and /sys/class/drm/ (display outputs) on a real
// CasaNova host. For the dev scaffold we ship a reasonable NUC-style
// layout: 2× RJ45, 4× USB, 1× HDMI, 1× DisplayPort.
//
// Each port carries a `connected` flag and type-specific metadata so
// the UI can flip a live dot, surface the link speed on NICs, and
// show the plugged-in device name for USB.

export type PortKind = 'ethernet' | 'usb' | 'hdmi' | 'displayport'

export interface HardwarePort {
  id: string
  kind: PortKind
  label: string // e.g. "eth0", "USB-A 1", "HDMI"
  connected: boolean
  // Ethernet: link speed in Mbps when connected
  linkSpeedMbps?: number
  // Ethernet: max negotiable speed (so we can show "1 Gbps / 2.5 Gbps")
  maxSpeedMbps?: number
  // Ethernet: IP address (dev/fixture assumes DHCP)
  ipAddress?: string
  // USB: plugged-in device description, if any
  usbDeviceName?: string
  // Display: resolution + refresh, if something is plugged in
  displayResolution?: string
}

export const hardwarePortsFixture: HardwarePort[] = [
  {
    id: 'eth0',
    kind: 'ethernet',
    label: 'eth0',
    connected: true,
    linkSpeedMbps: 1000,
    maxSpeedMbps: 2500,
    ipAddress: '10.0.0.93',
  },
  {
    id: 'eth1',
    kind: 'ethernet',
    label: 'eth1',
    connected: false,
    maxSpeedMbps: 2500,
  },
  { id: 'usb1', kind: 'usb', label: 'USB-A 1', connected: false },
  { id: 'usb2', kind: 'usb', label: 'USB-A 2', connected: true, usbDeviceName: 'SanDisk Ultra 64GB' },
  { id: 'usb3', kind: 'usb', label: 'USB-A 3', connected: false },
  { id: 'usb4', kind: 'usb', label: 'USB-A 4', connected: false },
  { id: 'hdmi', kind: 'hdmi', label: 'HDMI', connected: false },
  { id: 'dp', kind: 'displayport', label: 'DisplayPort', connected: false },
]
