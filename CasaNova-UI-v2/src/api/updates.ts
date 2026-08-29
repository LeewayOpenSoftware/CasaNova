// Updates API — wraps the CasaNova self-update endpoints. This is the
// system-update surface (new Go binaries, web assets, systemd units,
// migrations) — NOT per-app container image updates (that lives in
// containers.ts as auto-update + checkForUpdates).
//
// Backend routes (to be implemented in CasaNova main repo under
// /v1/sys/update/*):
//   GET    /v1/sys/update/status      → current engine state
//   GET    /v1/sys/update/check       → force a check against release channel
//   POST   /v1/sys/update/start       → begin download + apply
//   POST   /v1/sys/update/cancel      → abort an in-flight update
//   GET    /v1/sys/update/changelog   → markdown changelog for the target
//   POST   /v1/sys/update/channel     → switch release channel (stable|beta|nightly)
//
// The engine runs out-of-band from the HTTP handler that triggers it:
// `start` enqueues a job, returns immediately, and the UI polls
// `status` (or subscribes via MessageBus) for progress. Applying the
// update restarts the CasaNova services via systemd, so the last
// progress step the UI ever sees is usually "restarting" — after
// which the frontend reconnects to the upgraded backend.

import client from './client'

export type UpdateChannel = 'stable' | 'dev' | 'beta' | 'nightly'

export type UpdateStage =
  | 'idle' // nothing in progress, possibly an update available
  | 'checking' // polling release channel
  | 'downloading' // fetching the package
  | 'verifying' // checksum + signature
  | 'applying' // writing files, running migrations
  | 'restarting' // systemd unit restart in flight
  | 'completed' // last attempt succeeded (may clear to idle)
  | 'failed' // last attempt errored (message populated)

export interface UpdateReport {
  status: 'completed' | 'failed' | string
  from_version: string
  to_version: string
  installed_version: string
  channel: string
  services_ok: number
  services_total: number
  services_failed: string[]
  gateway_port: number
  gateway_ping: boolean
  frontend_ok: boolean
  containers_before: number
  containers_after: number
  backup_location: string
  timestamp: string
}

export interface UpdateStatus {
  currentVersion: string
  latestVersion: string | null
  channel: UpdateChannel
  updateAvailable: boolean
  stage: UpdateStage
  progressPercent: number // 0-100, only meaningful during downloading/applying
  message: string // human readable detail for the current stage
  lastCheckedAt: string | null // ISO timestamp
  lastAppliedAt: string | null // ISO timestamp of last successful apply
  sizeBytes: number | null // package size when known
  requiresReboot: boolean // true for kernel-adjacent updates, rare
  lastReport?: UpdateReport // populated from update-report.json on each poll
}

export async function rollbackUpdate(): Promise<{ backupLocation: string; fromVersion: string; toVersion: string }> {
  const res = await client.post<{
    success: number
    data: { backupLocation: string; fromVersion: string; toVersion: string }
  }>('/v1/sys/update/rollback')
  return res.data.data
}

export interface ChangelogEntry {
  version: string
  publishedAt: string
  body: string // markdown
}

export async function getStatus(): Promise<UpdateStatus> {
  const res = await client.get<{ success: number; data: UpdateStatus }>(
    '/v1/sys/update/status',
  )
  return res.data.data
}

export async function checkForUpdate(): Promise<UpdateStatus> {
  const res = await client.get<{ success: number; data: UpdateStatus }>(
    '/v1/sys/update/check',
  )
  return res.data.data
}

export interface StartUpdateRequest {
  targetVersion?: string // omit to install latest from channel
  allowReboot?: boolean // user pre-approves reboot if required
}

export interface StartUpdateResponse {
  jobId: string
  stage: UpdateStage
}

// The default 10s client timeout is wrong for both of these. They are "begin
// the operation" calls, but the server does real work before answering —
// validating and checksumming a 320 MiB package takes well past ten seconds on
// this hardware. When axios gave up first the UI reported "could not start the
// update" while the request was still being served, which is the worst possible
// lie: it invites a second click on an operation that must never run twice.
// The work itself is still polled through /status; this only covers the ack.
const START_ACK_TIMEOUT_MS = 120_000

export async function startUpdate(
  body: StartUpdateRequest = {},
): Promise<StartUpdateResponse> {
  const res = await client.post<{ success: number; data: StartUpdateResponse }>(
    '/v1/sys/update/start',
    body,
    { timeout: START_ACK_TIMEOUT_MS },
  )
  return res.data.data
}

export async function cancelUpdate(): Promise<void> {
  await client.post('/v1/sys/update/cancel')
}

// Remove staged .tar.gz packages from the cache to free space. Returns the
// refreshed status (updateAvailable recomputed).
export async function clearCache(): Promise<UpdateStatus> {
  const res = await client.post<{ success: number; data: UpdateStatus }>('/v1/sys/update/clear-cache')
  return res.data.data
}

// Reinstall the current version's system files from its cached package (repair
// corrupted files). Requires a cached package matching the running version.
export async function repairSystem(): Promise<StartUpdateResponse> {
  const res = await client.post<{ success: number; data: StartUpdateResponse }>(
    '/v1/sys/update/repair',
    undefined,
    { timeout: START_ACK_TIMEOUT_MS },
  )
  return res.data.data
}

export async function getChangelog(targetVersion?: string): Promise<ChangelogEntry[]> {
  const res = await client.get<{ success: number; data: ChangelogEntry[] }>(
    '/v1/sys/update/changelog',
    { params: targetVersion ? { version: targetVersion } : undefined },
  )
  return res.data.data
}

export async function setChannel(channel: UpdateChannel): Promise<UpdateStatus> {
  const res = await client.post<{ success: number; data: UpdateStatus }>(
    '/v1/sys/update/channel',
    { channel },
  )
  return res.data.data
}

// ─── Disk scanning types + endpoint ────────────────────────────────────────

export interface DiskPackage {
  filename: string
  path: string
  version: string
  arch: string
  sizeBytes: number
  modifiedAt: string
}

export interface DiskMount {
  mountPoint: string
  device: string
  label: string
  packages: DiskPackage[]
}

/**
 * scanDisks — scans mounted storage devices (USB, external drives) for
 * CasaNova update packages matching casanova-*.tar.gz.
 *
 * GET /v1/sys/update/scan-disks
 */
export async function scanDisks(): Promise<{ mounts: DiskMount[] }> {
  const res = await client.get<{ success: number; data: { mounts: DiskMount[] } }>(
    '/v1/sys/update/scan-disks',
  )
  return res.data.data
}

/**
 * startUpdateFromDisk — triggers an update from a .tar.gz file already
 * on the server's filesystem (e.g. uploaded via the file manager or on
 * a mounted USB drive). The backend extracts the package and runs the
 * same upgrade pipeline as an OTA update.
 *
 * POST /v1/sys/update/from-disk  { path, allowReboot }
 */
/**
 * uploadPackage — uploads a .tar.gz update package from the user's browser
 * to the server. The server saves it to a temp location and validates it,
 * returning the detected version, arch, and the server-side path.
 *
 * POST /v1/sys/update/upload-package  (multipart/form-data)
 */
export async function uploadPackage(
  file: File,
  onProgress?: (loaded: number, total: number) => void,
): Promise<{ path: string; version: string; arch: string }> {
  const form = new FormData()
  form.append('file', file)
  const res = await client.post<{
    success: number
    data: { path: string; version: string; arch: string }
  }>('/v1/sys/update/upload-package', form, {
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: 1800000, // 30 min — accommodates slow LAN on 350+ MiB bundles
    onUploadProgress: (e) => {
      if (!onProgress) return
      const total = e.total ?? file.size
      onProgress(e.loaded, total)
    },
  })
  return res.data.data
}

export async function startUpdateFromDisk(
  filePath: string,
  allowReboot = false,
): Promise<StartUpdateResponse> {
  const res = await client.post<{ success: number; data: StartUpdateResponse }>(
    '/v1/sys/update/from-disk',
    { path: filePath, allowReboot },
  )
  return res.data.data
}
