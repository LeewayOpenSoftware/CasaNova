// Maintenance API — client for /v1/sys/maintenance/*, the "reclaim space"
// module described in docs/design/maintenance-module.md.
//
// Three things set this file apart from the rest of the API layer:
//
//   1. Responses are PLAIN objects, not the {success, data} envelope. That is
//      the same exception GET /v1/sys/diagnosis already makes and it is
//      deliberate: these are finding payloads, not CRUD results. Read the
//      body directly — there is no `.data.data` here.
//
//   2. The refusals ARE the feature. POST /clean answers 409 with a machine
//      reason (a run is going, an update is staged, dpkg holds the lock) and
//      410 when the scan token has expired. The screen's whole job is to say
//      *why* it will not delete something, so the two calls that can be
//      refused return a result object instead of throwing: axios' generic
//      "Request failed with status code 409" throws the reason away, and the
//      reason is the part the user needs.
//
//   3. The reader is tolerant on purpose. The design fixes one JSON shape and
//      pkg/maintenance grew a slightly different one (totals hoisted to the
//      top level, `failed[].reason` instead of `.error`, an explicit
//      `notices` array for the read-only panel). Both are normalised here, in
//      one place, so the view never carries an `a ?? b` for a field name — and
//      so a server that omits `state` (the async wrapper) still renders as a
//      finished scan instead of a blank screen.
//
// The client never sends a filesystem path. A category is a key out of the
// server's compiled catalogue and an item is an opaque id minted by the scan,
// which is what keeps path traversal out of this module by construction.

import { AxiosError } from 'axios'
import client from './client'

// ── Wire types ──────────────────────────────────────────────────────

export type MaintenanceScanState = 'idle' | 'scanning' | 'done' | 'error'
export type MaintenanceRunState = 'idle' | 'running' | 'done' | 'error'

/** `safe` removes artefacts that are ours or the OS's and already superseded,
 *  `conditional` holds only while a precondition holds, `report_only` is never
 *  actionable — it is the "we are not touching this" panel. */
export type MaintenanceClass = 'safe' | 'conditional' | 'report_only'
export type MaintenanceAction = 'delete' | 'truncate' | 'none'

export interface MaintenanceItem {
  /** Opaque hash of (category, path, inode) minted by the scan. The executor
   *  resolves it against the stored plan, never against what arrives here. */
  id: string
  label: string
  path: string
  bytes: number
  mtime: string
  detail: string
  preselected: boolean
}

export interface MaintenanceFinding {
  category: string
  title: string
  summary: string
  class: MaintenanceClass
  action: MaintenanceAction
  reclaimable_bytes: number
  item_count: number
  /** True when `items` was capped (200/finding); the full list is in the
   *  on-disk manifest, so the UI must not present the array as complete. */
  truncated: boolean
  blocked: boolean
  /** Human sentence from the server, in the server's one language. */
  blocked_reason: string
  /** Stable machine code for the same thing — this is what the UI translates,
   *  falling back to `blocked_reason` for a code it does not know. */
  blocked_code: string
  preselect: boolean
  /** Extra sentences the detector wants on the record (a guard that could not
   *  prove something, a count it had to cap). Rendered, never summarised. */
  notes: string[]
  items: MaintenanceItem[]
}

/** A row of the read-only panel (§3.9). No items, no button, by construction.
 *  `bytes` is 0 when nobody could measure it — most of those numbers live in
 *  Docker, in another service — and a 0 renders as no number at all rather
 *  than as "0 B", which would read as "nothing there". */
export interface MaintenanceNotice {
  key: string
  label: string
  why: string
  link: string
  bytes: number
}

export interface MaintenanceDisk {
  path: string
  total: number
  used: number
  free: number
}

export interface MaintenanceProgress {
  category: string
  index: number
  total: number
}

export interface MaintenanceTotals {
  reclaimable_bytes: number
  by_class: Partial<Record<MaintenanceClass, number>>
}

export interface MaintenanceScan {
  state: MaintenanceScanState
  /** Invalidates plans made against an older view of the disk. Sent back on
   *  POST /clean; a stale one is answered with 410, not a silent delete. */
  scan_token: string
  started_at: string
  finished_at: string
  progress: MaintenanceProgress | null
  disk: MaintenanceDisk | null
  totals: MaintenanceTotals | null
  findings: MaintenanceFinding[]
  notices: MaintenanceNotice[]
  error: string
}

export interface MaintenanceFailure {
  id: string
  path: string
  reason: string
}

export interface MaintenanceRun {
  run_id: string
  state: MaintenanceRunState
  category: string
  progress: { done: number; total: number } | null
  removed_items: number
  removed_bytes: number
  /** statfs before/after. The truth, as opposed to the sum of what we removed:
   *  hardlinks, sparse files and shared extents make the two differ, and the
   *  design is explicit that the gap gets shown rather than hidden. */
  disk_delta_bytes: number
  failed: MaintenanceFailure[]
  /** A guard refusing at execution time. Kept apart from `failed` because it
   *  is the system working, not breaking. */
  skipped: MaintenanceFailure[]
  manifest_id: string
  finished_at: string
  error: string
}

export interface MaintenanceManifestEntry {
  id: string
  category: string
  finished_at: string
  removed_items: number
  removed_bytes: number
  disk_delta_bytes: number
}

export interface CleanRequest {
  scan_token: string
  category: string
  item_ids: string[]
}

/** Result of a call the server is allowed to refuse. `reason` is the machine
 *  code from the body ('' when the failure carried none), `message` is the
 *  human/axios text for the generic error path. */
export interface MaintenanceCallResult {
  ok: boolean
  status: number | null
  reason: string
  message: string
  run_id: string
}

// Every stable refusal/blocked code the module can emit, so the view's i18n
// map and the wire agree on spelling in one place. The first five are the
// design's §5 list; the rest are the block codes pkg/maintenance defines
// (catalog.go). A code that is not here still renders — verbatim — because a
// refusal we cannot name is the one the user most needs to see in full.
export const REFUSAL_REASONS = [
  'upload_in_progress',
  'run_in_progress',
  'update_staged',
  'dpkg_locked',
  'stale_scan',
  'update_running',
  'inconsistent_state',
  'proc_unreadable',
  'upload_registry_unreadable',
  'scan_error',
  'system_busy',
] as const

const BASE = '/v1/sys/maintenance'

/** Pull the machine reason out of an error body. The module answers with
 *  `{reason}`; echo.NewHTTPError answers with `{message}`. Accept both so a
 *  handler that reaches for the framework helper still says something useful. */
function refusalOf(err: unknown): MaintenanceCallResult {
  if (!(err instanceof AxiosError)) {
    return { ok: false, status: null, reason: '', message: String(err), run_id: '' }
  }
  const body = err.response?.data as { reason?: unknown; code?: unknown; message?: unknown } | undefined
  const reason =
    typeof body?.reason === 'string' ? body.reason
      : typeof body?.code === 'string' ? body.code
        : ''
  const message =
    typeof body?.message === 'string' && body.message ? body.message : err.message
  return { ok: false, status: err.response?.status ?? null, reason, message, run_id: '' }
}

// ── Normalisation ───────────────────────────────────────────────────

function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}
function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0
}
function arr(v: unknown): unknown[] {
  return Array.isArray(v) ? v : []
}
/** Go marshals a zero time.Time as "0001-01-01T00:00:00Z". Rendering that as a
 *  date would put the year 1 on screen; treat it as absent. */
function timestamp(v: unknown): string {
  const s = str(v)
  return !s || s.startsWith('0001-01-01') ? '' : s
}

function normalizeFinding(raw: Record<string, unknown>): MaintenanceFinding {
  return {
    category: str(raw.category),
    title: str(raw.title),
    summary: str(raw.summary),
    class: (str(raw.class) || 'conditional') as MaintenanceClass,
    action: (str(raw.action) || 'none') as MaintenanceAction,
    reclaimable_bytes: num(raw.reclaimable_bytes),
    item_count: num(raw.item_count),
    truncated: raw.truncated === true,
    blocked: raw.blocked === true,
    blocked_reason: str(raw.blocked_reason),
    blocked_code: str(raw.blocked_code),
    preselect: raw.preselect === true,
    notes: arr(raw.notes).map(str).filter(Boolean),
    items: arr(raw.items).map((i) => {
      const it = (i ?? {}) as Record<string, unknown>
      return {
        id: str(it.id),
        label: str(it.label),
        path: str(it.path),
        bytes: num(it.bytes),
        mtime: timestamp(it.mtime),
        detail: str(it.detail),
        preselected: it.preselected === true,
      }
    }).filter((i) => !!i.id),
  }
}

function normalizeFailures(raw: unknown): MaintenanceFailure[] {
  return arr(raw).map((f) => {
    const e = (f ?? {}) as Record<string, unknown>
    // `reason` is what pkg/maintenance writes; `error` is what the design's
    // example shows. Whichever arrives, the view reads one field.
    return { id: str(e.id), path: str(e.path), reason: str(e.reason) || str(e.error) }
  })
}

/** Fill in the async wrapper's fields when the server hands back a bare
 *  ScanResult. A payload with a token and findings IS a finished scan; calling
 *  it `idle` because one string is missing would grey out real results. */
function normalizeScan(body: unknown): MaintenanceScan {
  const raw = (body ?? {}) as Record<string, unknown>
  const findings = arr(raw.findings).map((f) => normalizeFinding((f ?? {}) as Record<string, unknown>))
  const token = str(raw.scan_token) || str(raw.token)
  const state = str(raw.state) as MaintenanceScanState
  const totalsRaw = (raw.totals ?? {}) as Record<string, unknown>
  const progressRaw = raw.progress as Record<string, unknown> | null | undefined
  const diskRaw = raw.disk as Record<string, unknown> | null | undefined
  return {
    state: state || (token || findings.length ? 'done' : 'idle'),
    scan_token: token,
    started_at: timestamp(raw.started_at),
    finished_at: timestamp(raw.finished_at),
    progress: progressRaw
      ? {
          category: str(progressRaw.category),
          index: num(progressRaw.index),
          total: num(progressRaw.total),
        }
      : null,
    disk: diskRaw && num(diskRaw.total) > 0
      ? {
          path: str(diskRaw.path),
          total: num(diskRaw.total),
          used: num(diskRaw.used),
          free: num(diskRaw.free),
        }
      : null,
    // Accept the design's nested `totals` and the engine's hoisted fields.
    totals: {
      reclaimable_bytes: num(totalsRaw.reclaimable_bytes ?? raw.reclaimable_bytes),
      by_class: (totalsRaw.by_class ?? raw.by_class ?? {}) as Partial<Record<MaintenanceClass, number>>,
    },
    findings,
    notices: arr(raw.notices).map((n) => {
      const e = (n ?? {}) as Record<string, unknown>
      return {
        key: str(e.key),
        label: str(e.label),
        why: str(e.why),
        link: str(e.link),
        bytes: num(e.bytes),
      }
    }).filter((n) => !!n.key),
    error: str(raw.error),
  }
}

function normalizeRun(body: unknown): MaintenanceRun {
  const raw = (body ?? {}) as Record<string, unknown>
  const state = str(raw.state) as MaintenanceRunState
  const finished = timestamp(raw.finished_at)
  const runID = str(raw.run_id)
  const progressRaw = raw.progress as Record<string, unknown> | null | undefined
  return {
    run_id: runID,
    // Same reasoning as the scan: a result with an end time is a finished run
    // even if the wrapper forgot to say so; no run at all is `idle`.
    state: state || (!runID ? 'idle' : finished ? 'done' : 'running'),
    category: str(raw.category),
    progress: progressRaw
      ? { done: num(progressRaw.done), total: num(progressRaw.total) }
      : null,
    removed_items: num(raw.removed_items),
    removed_bytes: num(raw.removed_bytes),
    disk_delta_bytes: num(raw.disk_delta_bytes),
    failed: normalizeFailures(raw.failed),
    skipped: normalizeFailures(raw.skipped),
    manifest_id: str(raw.manifest_id),
    finished_at: finished,
    error: str(raw.error),
  }
}

// ── Scan ────────────────────────────────────────────────────────────

/** POST /scan — kicks the scan off and answers at once; 409 when one is
 *  already running. The caller polls getScan() for the result either way. */
export async function startScan(): Promise<MaintenanceCallResult> {
  try {
    await client.post(`${BASE}/scan`)
    return { ok: true, status: 200, reason: '', message: '', run_id: '' }
  } catch (err) {
    return refusalOf(err)
  }
}

/** GET /scan — state plus the findings of the last scan. Throws on failure so
 *  callers can put it behind safeCall like every other read in this layer. */
// A build can ship without the maintenance module at all: route/v1.go gates its
// whole route group behind the compile-time `maintenanceRoutesEnabled`, which is
// false, so echo never registers the paths and answers 404 (405 if some sibling
// verb is registered). That is a FACT ABOUT THE BUILD, not a failure — and both
// the page and the settings rail have to reach the same conclusion from it, so
// the rule lives here once instead of as a matching pair of magic numbers.
export function isModuleAbsent(status: number | null | undefined): boolean {
  return status === 404 || status === 405
}

export async function getScan(): Promise<MaintenanceScan> {
  const res = await client.get<unknown>(`${BASE}/scan`, { timeout: 20_000 })
  return normalizeScan(res.data)
}

// ── Clean ───────────────────────────────────────────────────────────

/** POST /clean — runs ONE category. There is no "clean everything" call and
 *  there is deliberately no helper here that loops over categories: each run
 *  is its own confirmation, on purpose (design §4 rule 13). */
export async function startClean(req: CleanRequest): Promise<MaintenanceCallResult> {
  try {
    const res = await client.post<{ run_id?: string }>(`${BASE}/clean`, req)
    return {
      ok: true,
      status: 200,
      reason: '',
      message: '',
      run_id: typeof res.data?.run_id === 'string' ? res.data.run_id : '',
    }
  } catch (err) {
    return refusalOf(err)
  }
}

/** GET /clean — the run in progress, or the last one that finished. */
export async function getRun(): Promise<MaintenanceRun> {
  const res = await client.get<unknown>(`${BASE}/clean`)
  return normalizeRun(res.data)
}

// ── Manifests ───────────────────────────────────────────────────────

/** GET /manifest — past runs, newest first.
 *
 *  Normalised defensively: the design fixes the shape of /scan and /clean but
 *  not of this list, so a bare array and a `{manifests: […]}` wrapper both
 *  work, and anything else degrades to an empty list instead of throwing.
 *  The "Last maintenance" card only ever augments what GET /clean already
 *  reports, so an empty list here costs the screen nothing. */
export async function listManifests(): Promise<MaintenanceManifestEntry[]> {
  const res = await client.get<unknown>(`${BASE}/manifest`)
  const body = res.data
  const raw = Array.isArray(body)
    ? body
    : arr((body as { manifests?: unknown })?.manifests)
  return raw
    .map((e) => {
      const m = (e ?? {}) as Record<string, unknown>
      return {
        id: str(m.id) || str(m.manifest_id),
        category: str(m.category),
        finished_at: timestamp(m.finished_at),
        removed_items: num(m.removed_items),
        removed_bytes: num(m.removed_bytes),
        disk_delta_bytes: num(m.disk_delta_bytes),
      }
    })
    .filter((m) => !!m.id)
}
