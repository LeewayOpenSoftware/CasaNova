// Storage types extracted from fixtures/storage.ts
// so production code can import these without pulling in fake data.

export type SmartHealth = 'good' | 'warning' | 'critical' | 'unknown'

export interface DiskEntry {
  device: string
  model: string
  serial: string
  fsUUID: string
  sizeBytes: number
  rotational: boolean
  isUSB: boolean
  smartHealth: SmartHealth
  temperature: number | null
  powerOnHours: number
  reallocatedSectors: number
  mountPoint: string | null
  filesystem: string | null
  usedBytes?: number
}
