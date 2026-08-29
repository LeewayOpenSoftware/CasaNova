// Mock antivirus detection fixture.
//
// The real backend probes the host for a supported AV engine (ClamAV
// is the realistic one on Linux; a handful of commercial engines like
// Sophos, ESET, or Comodo Linux also ship a CLI we can query). The
// response tells the UI whether to render the antivirus card at all
// and, if so, what to display. If `detected: false`, the whole card
// is hidden — no empty state, no "install ClamAV" pitch, because a
// home server user who didn't install an AV probably doesn't want the
// dashboard suggesting one.
//
// Shape a future GET /v2/casaos/antivirus/status would return:
//   { detected: true, engine: 'clamav', version: '1.3.1',
//     signatureDbVersion: '27293', signatureDbUpdatedAt: '...',
//     lastScanAt: '...', lastScanResult: 'clean' | 'threats',
//     threatsFound: 0, daemonRunning: true }

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

// Toggle `detected` between true/false during development to verify
// the card shows / hides correctly. Real backend call replaces this
// entirely.
export const antivirusFixture: AntivirusStatus = {
  detected: true,
  engine: 'clamav',
  version: '1.3.1',
  signatureDbVersion: '27293',
  signatureDbUpdatedAt: '2026-04-05T06:30:00Z',
  lastScanAt: '2026-04-04T23:00:00Z',
  lastScanResult: 'clean',
  threatsFound: 0,
  daemonRunning: true,
}
