// Mock storage fixture for Stage 5.5 Storage settings development.
//
// Shape mirrors CasaOS-LocalStorage v0.9: disks come from the SMART-
// enriched LSBLK model with UUID + serial + rotational flag; mounts
// are a separate concept keyed to fsUUID so path changes don't break
// anything. Matches what the real API returns from GET /v2/local_storage.

export type SmartHealth = 'good' | 'warning' | 'critical' | 'unknown'

export interface DiskEntry {
  device: string // /dev/sda, /dev/nvme0n1
  model: string
  serial: string
  fsUUID: string
  sizeBytes: number
  rotational: boolean // false = SSD
  isUSB: boolean
  smartHealth: SmartHealth
  temperature: number | null // °C
  powerOnHours: number
  reallocatedSectors: number
  mountPoint: string | null
  filesystem: string | null
  usedBytes?: number
}

export const disksFixture: DiskEntry[] = [
  {
    device: '/dev/nvme0n1',
    model: 'Samsung 980 Pro 1TB',
    serial: 'S6XPNS0W123456',
    fsUUID: '1a2b3c4d-5e6f-7890-abcd-ef0123456789',
    sizeBytes: 1024204886016,
    rotational: false,
    isUSB: false,
    smartHealth: 'good',
    temperature: 42,
    powerOnHours: 4521,
    reallocatedSectors: 0,
    mountPoint: '/DATA',
    filesystem: 'ext4',
  },
  {
    device: '/dev/sda',
    model: 'WD Red Plus 8TB',
    serial: 'WD-WCC7K1234567',
    fsUUID: '9876abcd-ef01-2345-6789-abcdef012345',
    sizeBytes: 8001563222016,
    rotational: true,
    isUSB: false,
    smartHealth: 'warning',
    temperature: 38,
    powerOnHours: 18720,
    reallocatedSectors: 3,
    mountPoint: '/DATA/backup-pool',
    filesystem: 'btrfs',
  },
  {
    device: '/dev/sdb',
    model: 'Seagate IronWolf 4TB',
    serial: 'ZA14TB9876543',
    fsUUID: 'abcdef01-2345-6789-abcd-ef0123456789',
    sizeBytes: 4000787030016,
    rotational: true,
    isUSB: false,
    smartHealth: 'good',
    temperature: 35,
    powerOnHours: 9812,
    reallocatedSectors: 0,
    mountPoint: null,
    filesystem: null,
  },
  {
    device: '/dev/sdc',
    model: 'SanDisk Ultra USB 3.0 64GB',
    serial: 'AA010203040506',
    fsUUID: '11112222-3333-4444-5555-666677778888',
    sizeBytes: 64424509440,
    rotational: false,
    isUSB: true,
    smartHealth: 'unknown',
    temperature: null,
    powerOnHours: 0,
    reallocatedSectors: 0,
    mountPoint: '/mnt/usb',
    filesystem: 'exfat',
  },
]
