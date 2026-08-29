// Antivirus types extracted from fixtures/antivirus.ts
// so production code can import these without pulling in fake data.

export type AntivirusEngine = 'clamav' | 'sophos' | 'eset' | 'comodo'
export type ScanResult = 'clean' | 'threats' | 'unknown'

export interface AntivirusStatus {
  detected: boolean
  engine?: AntivirusEngine
  version?: string
  signatureDbVersion?: string
  signatureDbUpdatedAt?: string
  lastScanAt?: string
  lastScanResult?: ScanResult
  threatsFound?: number
  daemonRunning?: boolean
}
