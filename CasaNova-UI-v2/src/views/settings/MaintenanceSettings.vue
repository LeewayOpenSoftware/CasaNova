<script setup lang="ts">
// MaintenanceSettings — the "reclaim space" screen (docs/design/maintenance-module.md).
//
// The shape of this page is an argument, not a layout preference:
//
//   • There is no "Clean everything" button and this file must never grow
//     one. Every category runs alone, behind its own confirmation, because a
//     single button over categories of unequal risk is the exact move that
//     turns a cleaner into a ticket generator (design §2, §4 rule 13).
//   • The empty state lists the categories at 0 B in grey instead of hiding
//     them. A screen that says "nothing to reclaim" and shows its work beats
//     one that invents 8 GB by summing things it will never delete.
//   • The biggest number on the page is the one nobody can touch, so it gets
//     a card of its own — collapsed, sizes visible, no checkboxes.
//   • Blocked categories keep their size on screen next to the reason they
//     are blocked. "Why not" is information the user is owed, not an error.
//
// Data flow is poll-based, matching the backend's async contract: POST starts
// the work and answers immediately, GET reports state, and the UI polls every
// 2 s while something is moving. Responses are PLAIN objects (no {success,data}
// envelope) — see src/api/maintenance.ts.

import { skipWhenHidden } from '@/composables/skipWhenHidden'
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import Button from 'primevue/button'
import Checkbox from 'primevue/checkbox'
import Dialog from 'primevue/dialog'
import { useToast } from 'primevue/usetoast'
import AppIcon from '@/components/ui/AppIcon.vue'
import StatusChip from '@/components/ui/StatusChip.vue'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { safeCall } from '@/api/safeCall'
import { maintenanceApi } from '@/api'
import { REFUSAL_REASONS } from '@/api/maintenance'
import { formatBytes } from '@/types/files'
import type {
  MaintenanceScan,
  MaintenanceRun,
  MaintenanceFinding,
  MaintenanceManifestEntry,
} from '@/api/maintenance'

const { t } = useI18n()
const toast = useToast()

// ── Catalogue ───────────────────────────────────────────────────────
// The category keys the server can send in phase 1 (plus casanova-scratch
// from phase 2), in the order the design lists them. This table exists for
// one reason: a category the scan found nothing in is absent from `findings`,
// and the honest empty state needs to list it anyway, at 0 B, in grey.
//
// `label` comes from here rather than from the response even when the server
// sends a title, because the server's title is one fixed language and this
// screen is not. The server's `summary` (counts, versions, dates) is rendered
// verbatim — only the server knows those.
interface CategoryMeta {
  icon: string
  /** i18n slug: settings.maintenance.cat_<slug>_{label,hint,consequence} */
  i18n: string
}

const CATEGORY_META: Record<string, CategoryMeta> = {
  'casanova-update-packages': { icon: 'Package', i18n: 'update_packages' },
  'casanova-upgrade-backups': { icon: 'Archive', i18n: 'upgrade_backups' },
  'system-rotated-logs': { icon: 'ScrollText', i18n: 'rotated_logs' },
  'orphan-journal': { icon: 'FileClock', i18n: 'orphan_journal' },
  'apt-package-cache': { icon: 'Box', i18n: 'apt_cache' },
  'casanova-scratch': { icon: 'FileQuestion', i18n: 'scratch' },
}

const CATEGORY_ORDER = Object.keys(CATEGORY_META)

// The read-only panel (§3.9). The server owns this list — the rows, their
// order and their English wording live in pkg/maintenance/notices.go — and
// this table adds the two things only the frontend can know: which glyph the
// row wears and where "open the section" actually goes.
//
// Routes come from here rather than from the notice's own `link`: the server
// cannot know this app's route table (it currently points at /app/apps, which
// does not exist here — apps live at /app/settings/apps). A key with no entry
// renders without a link, which is the correct outcome for a row we cannot
// send anyone to.
interface NoticeMeta {
  icon: string
  /** i18n slug: settings.maintenance.notice_<slug>_{label,why} */
  i18n: string
  to?: string
}

const NOTICE_META: Record<string, NoticeMeta> = {
  'docker-volumes-unused': { icon: 'Database', i18n: 'docker_volumes', to: '/app/settings/apps' },
  'docker-containers-stopped': { icon: 'Container', i18n: 'docker_stopped', to: '/app/settings/containers' },
  'docker-images-tagged': { icon: 'Layers', i18n: 'docker_tagged', to: '/app/settings/containers' },
  'docker-overlay2': { icon: 'HardDrive', i18n: 'docker_overlay2' },
  'appdata-without-app': { icon: 'Folder', i18n: 'appdata', to: '/app/files' },
  'in-use': { icon: 'Shield', i18n: 'in_use' },
  'user-caches': { icon: 'User2', i18n: 'user_caches' },
  trash: { icon: 'Trash2', i18n: 'trash', to: '/app/files' },
  'docker-dangling-images': { icon: 'Layers', i18n: 'deferred_dangling' },
  'container-log-rotation': { icon: 'ScrollText', i18n: 'deferred_logs' },
}

// Shown only when the server sent no notices at all (an older build, or the
// offline fallback). The design requires the two categories this phase does
// not implement to be visible with their explanation, and that requirement
// does not depend on the backend being there to state it.
const DEFERRED_FALLBACK = ['docker-dangling-images', 'container-log-rotation']

// ── State ───────────────────────────────────────────────────────────

const scan = ref<MaintenanceScan | null>(null)
const run = ref<MaintenanceRun | null>(null)
const manifests = ref<MaintenanceManifestEntry[]>([])
const lastError = ref('')
/** The server does not expose this module (older build). Different from an
 *  error: there is nothing to retry, so the page says so plainly. */
const unavailable = ref(false)
/** Backend unreachable — the catalogue renders, no numbers are invented. */
const offline = ref(false)

const expanded = ref<Record<string, boolean>>({})
const selection = ref<Record<string, string[]>>({})
/** Selection is seeded once per scan token; re-seeding on every poll would
 *  wipe the ticks the user just made while the scan was still finishing. */
let seededToken = ''

const confirming = ref<MaintenanceFinding | null>(null)
const submitting = ref(false)
const showUntouched = ref(false)

let scanTimer: ReturnType<typeof setInterval> | null = null
let runTimer: ReturnType<typeof setInterval> | null = null
/** The run this screen is waiting on. GET /clean answers with the run in
 *  progress OR the last one that finished, so between our POST and the
 *  goroutine registering it can still be describing the PREVIOUS run — the id
 *  is what keeps us from reporting someone else's numbers as our result. */
let pendingRunId = ''
let pendingSince = 0

/** Fixture for safeCall: an idle scan with no findings. Deliberately empty —
 *  a screen that deletes files must never render invented items, so the dev
 *  fallback shows the catalogue in its "not analyzed" state and nothing more. */
function idleScan(): MaintenanceScan {
  return {
    state: 'idle',
    scan_token: '',
    started_at: '',
    finished_at: '',
    progress: null,
    disk: null,
    totals: null,
    findings: [],
    notices: [],
    error: '',
  }
}

// ── Derived ─────────────────────────────────────────────────────────

const scanning = computed(() => scan.value?.state === 'scanning')
const running = computed(() => run.value?.state === 'running')
/** One run at a time, system-wide: while anything moves, no action arms. */
const busy = computed(() => scanning.value || running.value || submitting.value)

const findings = computed<MaintenanceFinding[]>(() => scan.value?.findings ?? [])
const hasScanned = computed(() => scan.value?.state === 'done' && !!scan.value.scan_token)

// The three buckets partition `findings` exactly once each — no finding shows
// up in two cards, and none falls through a gap. Order of the tests matters:
// report_only wins over blocked (a row we never act on is not "blocked", it is
// just reported), and blocked wins over "nothing to do".
const blockedFindings = computed(() =>
  findings.value.filter((f) => f.blocked && f.class !== 'report_only'),
)
// A catalogued category is never demoted into the read-only panel, even when
// this scan found nothing in it: it belongs to card 1, greyed at 0 B, where
// the user expects to find it. Only categories outside the catalogue land here.
const readOnlyFindings = computed(() =>
  findings.value.filter(
    (f) => !CATEGORY_META[f.category] && (f.class === 'report_only' || (!f.blocked && f.action === 'none')),
  ),
)

interface PanelRow {
  key: string
  icon: string
  label: string
  why: string
  to: string
  bytes: number
}

/** Rows of the read-only panel. The server's notices when it sent any, our own
 *  deferred rows when it did not — never both, so nothing appears twice. */
const panelRows = computed<PanelRow[]>(() => {
  const notices = scan.value?.notices ?? []
  const keys = notices.length ? notices.map((n) => n.key) : DEFERRED_FALLBACK
  return keys.map((key) => {
    const meta = NOTICE_META[key]
    const notice = notices.find((n) => n.key === key)
    return {
      key,
      icon: meta?.icon ?? 'Shield',
      // Translated from the stable key; the server's English is the fallback
      // for a row this build has no wording for yet.
      label: meta ? t(`settings.maintenance.notice_${meta.i18n}_label`) : (notice?.label ?? key),
      why: meta ? t(`settings.maintenance.notice_${meta.i18n}_why`) : (notice?.why ?? ''),
      to: meta?.to ?? '',
      bytes: notice?.bytes ?? 0,
    }
  })
})

interface CategoryRow {
  key: string
  label: string
  icon: string
  finding: MaintenanceFinding | null
}

/** The rows of card 1: every catalogued category in a fixed order, whether or
 *  not the scan found anything in it, plus any actionable category the server
 *  adds later (which renders under its own title rather than disappearing). */
const categoryRows = computed<CategoryRow[]>(() => {
  // Catalogued categories keep their finding even when it is not actionable,
  // so the row can show the server's own summary ("nothing older than the
  // installed version") instead of the generic hint. Whether a button appears
  // is rowActionable's call, not this map's.
  const byKey = new Map<string, MaintenanceFinding>()
  for (const f of findings.value) {
    if (f.blocked) continue // blocked categories live in card 2
    byKey.set(f.category, f)
  }
  const rows: CategoryRow[] = CATEGORY_ORDER.filter((key) => !isBlocked(key)).map((key) => ({
    key,
    label: t(`settings.maintenance.cat_${CATEGORY_META[key].i18n}_label`),
    icon: CATEGORY_META[key].icon,
    finding: byKey.get(key) ?? null,
  }))
  for (const [key, f] of byKey) {
    if (CATEGORY_META[key] || f.class === 'report_only' || f.action === 'none') continue
    rows.push({ key, label: f.title || key, icon: 'FileQuestion', finding: f })
  }
  return rows
})

function isBlocked(key: string): boolean {
  return blockedFindings.value.some((f) => f.category === key)
}

/** Headline total. Computed from the rows on screen rather than read from
 *  `totals.reclaimable_bytes` so the number always equals the sum of what the
 *  user can actually see and tick — a headline that outruns its own list is
 *  the dishonesty this screen exists to avoid. */
const totalReclaimable = computed(() =>
  categoryRows.value.reduce((sum, r) => sum + (r.finding?.reclaimable_bytes ?? 0), 0),
)
const activeCategories = computed(
  () => categoryRows.value.filter((r) => (r.finding?.reclaimable_bytes ?? 0) > 0).length,
)
const untouchedTotal = computed(
  () =>
    readOnlyFindings.value.reduce((sum, f) => sum + (f.reclaimable_bytes || 0), 0) +
    panelRows.value.reduce((sum, r) => sum + r.bytes, 0),
)

const diskPercent = computed(() => {
  const d = scan.value?.disk
  if (!d || !d.total) return 0
  return Math.round((d.used / d.total) * 100)
})

const scannedAt = computed(() => {
  const at = scan.value?.finished_at
  return at ? dayjs(at).format('HH:mm') : ''
})

/** The last finished run, for card 5. GET /clean is the authoritative source
 *  (it carries the real statfs delta); the manifest list only adds the date. */
const lastRun = computed(() => (run.value && run.value.state === 'done' ? run.value : null))
/** When the run itself carries no end time, the manifest list is the only
 *  place the date exists. Neither is required for the card to render. */
const lastRunAt = computed(() => {
  const r = lastRun.value
  if (!r) return ''
  if (r.finished_at) return r.finished_at
  const entry = manifests.value.find((m) => m.id === r.manifest_id)
  return entry?.finished_at ?? ''
})
/** Removing 200 MB and seeing the disk drop by 40 is normal (hardlinks, shared
 *  extents, sparse files). Said out loud rather than hidden. */
const deltaShortfall = computed(() => {
  const r = lastRun.value
  if (!r || r.removed_bytes <= 0) return false
  return r.disk_delta_bytes < r.removed_bytes * 0.8
})

// ── Row helpers ─────────────────────────────────────────────────────

function rowBytes(row: CategoryRow): number {
  return row.finding?.reclaimable_bytes ?? 0
}
/** Has something to show — drives the grey/normal treatment of the row. */
function rowHasBytes(row: CategoryRow): boolean {
  return rowBytes(row) > 0
}
/** Has something to ACT on. Kept separate from rowHasBytes on purpose: a
 *  finding that reports bytes but ships no items (a server that measured more
 *  than it is willing to hand over), or one whose action is `none`, still shows
 *  its size and still offers no button — there is nothing to tick, so there is
 *  nothing to delete. */
function rowActionable(row: CategoryRow): boolean {
  const f = row.finding
  if (!f || f.action === 'none' || f.class === 'report_only') return false
  return rowHasBytes(row) && f.items.length > 0
}
function rowSummary(row: CategoryRow): string {
  if (row.finding?.summary) return row.finding.summary
  if (!hasScanned.value) return t('settings.maintenance.not_scanned')
  const meta = CATEGORY_META[row.key]
  return meta ? t(`settings.maintenance.cat_${meta.i18n}_hint`) : ''
}
function categoryLabel(key: string): string {
  const meta = CATEGORY_META[key]
  if (meta) return t(`settings.maintenance.cat_${meta.i18n}_label`)
  const f = findings.value.find((x) => x.category === key)
  return f?.title || key
}
function consequenceFor(key: string): string {
  const meta = CATEGORY_META[key]
  return meta
    ? t(`settings.maintenance.cat_${meta.i18n}_consequence`)
    : t('settings.maintenance.consequence_generic')
}
function actionLabel(f: MaintenanceFinding): string {
  const n = selectedIds(f.category).length
  const key = f.action === 'truncate' ? 'truncate_btn' : 'delete_btn'
  return t(`settings.maintenance.${key}`, { n }, n)
}

function toggleExpand(key: string) {
  expanded.value = { ...expanded.value, [key]: !expanded.value[key] }
}
function selectedIds(category: string): string[] {
  return selection.value[category] ?? []
}
function isSelected(category: string, id: string): boolean {
  return selectedIds(category).includes(id)
}
function toggleItem(category: string, id: string) {
  const current = selectedIds(category)
  selection.value = {
    ...selection.value,
    [category]: current.includes(id) ? current.filter((x) => x !== id) : [...current, id],
  }
}
function setAll(f: MaintenanceFinding, on: boolean) {
  selection.value = {
    ...selection.value,
    [f.category]: on ? f.items.map((i) => i.id) : [],
  }
}

// ── Loading ─────────────────────────────────────────────────────────

function seedSelection(s: MaintenanceScan) {
  if (s.scan_token === seededToken) return
  seededToken = s.scan_token
  const next: Record<string, string[]> = {}
  for (const f of s.findings) {
    if (f.blocked || f.action === 'none') continue
    // Category-level `preselect: false` is a hard veto, applied here as well as
    // server-side: updates_archive is shown with versions and dates and the
    // user ticks it, never the other way round (design §3.1). A response that
    // marked its items preselected anyway does not get to override that.
    next[f.category] = f.preselect ? f.items.filter((i) => i.preselected).map((i) => i.id) : []
  }
  selection.value = next
}

async function loadScan() {
  const res = await safeCall(() => maintenanceApi.getScan(), idleScan)
  if (res.error) {
    stopScanPolling()
    // 404/405 means this build has no maintenance module — a fact, not a fault.
    if (res.status === 404 || res.status === 405) unavailable.value = true
    else lastError.value = res.error
    return
  }
  offline.value = res.fixture
  lastError.value = res.data.error || ''
  scan.value = res.data
  seedSelection(res.data)
  if (res.data.state === 'scanning') startScanPolling()
  else stopScanPolling()
}

async function loadRun() {
  const res = await safeCall(() => maintenanceApi.getRun())
  if (res.error || !res.data) return
  const r = res.data
  run.value = r
  if (r.state === 'running') {
    startRunPolling()
    return
  }
  if (submitting.value) {
    // Still waiting on the run we started. Only accept this as its result when
    // the id matches; when the server gave us no id, give it one poll cycle so
    // we do not mistake the previous run's report for ours.
    const mine = pendingRunId ? r.run_id === pendingRunId : Date.now() - pendingSince > 2500
    if (!mine) {
      startRunPolling()
      return
    }
    stopRunPolling()
    onRunFinished(r)
    return
  }
  stopRunPolling()
}

async function loadManifests() {
  const res = await safeCall(() => maintenanceApi.listManifests(), () => [])
  if (!res.error) manifests.value = res.data
}

function onRunFinished(r: MaintenanceRun) {
  submitting.value = false
  pendingRunId = ''
  if (r.state === 'error') {
    toast.add({
      severity: 'error',
      summary: t('settings.maintenance.run_failed'),
      detail: r.error || '',
      life: 6000,
    })
  } else {
    toast.add({
      severity: 'success',
      summary: t('settings.maintenance.run_done', { size: formatBytes(r.disk_delta_bytes) }),
      detail: categoryLabel(r.category),
      life: 5000,
    })
  }
  void loadManifests()
  // The plan the user acted on is spent: what is on screen no longer matches
  // the disk. Re-scanning is the only way the result card can show the truth.
  void startScan()
}

// ── Polling ─────────────────────────────────────────────────────────

function startScanPolling() {
  if (scanTimer) return
  scanTimer = setInterval(skipWhenHidden(() => void loadScan()), 2000)
}
function stopScanPolling() {
  if (scanTimer) {
    clearInterval(scanTimer)
    scanTimer = null
  }
}
function startRunPolling() {
  if (runTimer) return
  runTimer = setInterval(skipWhenHidden(() => void loadRun()), 2000)
}
function stopRunPolling() {
  if (runTimer) {
    clearInterval(runTimer)
    runTimer = null
  }
}

// ── Actions ─────────────────────────────────────────────────────────

async function startScan() {
  lastError.value = ''
  const res = await maintenanceApi.startScan()
  if (!res.ok) {
    // 409 while a scan is already going is not worth a red toast: just follow
    // the one that is running.
    if (res.status === 409) {
      startScanPolling()
      return
    }
    if (res.status === 404 || res.status === 405) {
      unavailable.value = true
      return
    }
    toast.add({
      severity: 'error',
      summary: t('settings.maintenance.error_generic'),
      detail: refusalText(res.reason, res.message),
      life: 6000,
    })
    return
  }
  await loadScan()
  startScanPolling()
}

function openConfirm(f: MaintenanceFinding) {
  if (selectedIds(f.category).length === 0) return
  confirming.value = f
}

const confirmSelected = computed(() => {
  const f = confirming.value
  if (!f) return []
  const ids = new Set(selectedIds(f.category))
  return f.items.filter((i) => ids.has(i.id))
})
const confirmPreview = computed(() => confirmSelected.value.slice(0, 10))
const confirmRest = computed(() => Math.max(0, confirmSelected.value.length - 10))
const confirmBytes = computed(() => confirmSelected.value.reduce((s, i) => s + i.bytes, 0))

async function executeClean() {
  const f = confirming.value
  const token = scan.value?.scan_token ?? ''
  if (!f) return
  submitting.value = true
  const res = await maintenanceApi.startClean({
    scan_token: token,
    category: f.category,
    item_ids: selectedIds(f.category),
  })
  confirming.value = null
  if (!res.ok) {
    submitting.value = false
    pendingRunId = ''
    toast.add({
      severity: 'warn',
      summary: t('settings.maintenance.refused'),
      detail: refusalText(res.reason, res.message),
      life: 8000,
    })
    // A stale token means the disk moved under the plan. Re-scanning is the
    // only correct recovery, and the user should not have to guess that.
    if (res.reason === 'stale_scan' || res.status === 410) void startScan()
    return
  }
  pendingRunId = res.run_id
  pendingSince = Date.now()
  toast.add({
    severity: 'info',
    summary: t('settings.maintenance.run_started', { category: categoryLabel(f.category) }),
    life: 4000,
  })
  await loadRun()
  startRunPolling()
}

/** Map a machine refusal code to text. A code we have no wording for is shown
 *  verbatim rather than swallowed — a refusal we cannot name is the one the
 *  user most needs to see in full. */
function refusalText(reason: string, message: string): string {
  if (!reason) return message
  if ((REFUSAL_REASONS as readonly string[]).includes(reason)) {
    return t(`settings.maintenance.reason_${reason}`)
  }
  return message ? `${reason} — ${message}` : reason
}

/** What to show under a blocked category. The stable code is preferred so the
 *  sentence is in the user's language; the server's own text is the fallback
 *  for a code this build does not know, and it is shown rather than replaced
 *  by a generic line — a specific reason we cannot translate still beats a
 *  translated reason that says nothing. */
function blockedText(f: MaintenanceFinding): string {
  if (f.blocked_code && (REFUSAL_REASONS as readonly string[]).includes(f.blocked_code)) {
    return t(`settings.maintenance.reason_${f.blocked_code}`)
  }
  return f.blocked_reason || t('settings.maintenance.blocked_generic')
}

// ── Lifecycle ───────────────────────────────────────────────────────

onMounted(() => {
  void loadScan()
  void loadRun()
  void loadManifests()
})

onUnmounted(() => {
  stopScanPolling()
  stopRunPolling()
})
</script>

<template>
  <SettingsPage
    icon="Sparkles"
    :title="t('settings.maintenance.title')"
    :description="t('settings.maintenance.desc')"
  >
    <template #actions>
      <!-- Compact disk bar: the context every number on this page lives in. -->
      <div v-if="scan?.disk" class="hidden items-center gap-2 sm:flex">
        <div class="h-1.5 w-24 overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
          <div class="h-full rounded-full bg-[var(--cn-accent)]" :style="{ width: diskPercent + '%' }" />
        </div>
        <span class="text-xs tabular-nums text-[var(--cn-text-muted)]">
          {{ t('settings.maintenance.disk_line', {
            used: formatBytes(scan.disk.used),
            total: formatBytes(scan.disk.total),
            pct: diskPercent,
          }) }}
        </span>
      </div>
      <!-- The category being walked right now. Next to the counter, because
           "3 of 5" without a name says nothing about what is being read. -->
      <span
        v-if="scanning && scan?.progress?.category"
        class="hidden max-w-[14rem] truncate text-xs text-[var(--cn-text-muted)] md:block"
      >{{ categoryLabel(scan.progress.category) }}</span>
      <Button size="small" :loading="scanning" :disabled="unavailable || running" @click="startScan">
        <AppIcon name="Search" :size="14" class="mr-1.5" />
        <template v-if="scanning && scan?.progress">
          {{ t('settings.maintenance.analyzing', {
            done: scan.progress.index,
            total: scan.progress.total,
          }) }}
        </template>
        <template v-else-if="scanning">{{ t('settings.maintenance.analyzing_short') }}</template>
        <template v-else>{{ t('settings.maintenance.analyze') }}</template>
      </Button>
    </template>

    <!-- Module absent from this build. Nothing to retry, so say it once. -->
    <SettingsCard v-if="unavailable">
      <div class="flex items-start gap-3">
        <AppIcon name="Info" :size="18" class="mt-0.5 shrink-0 text-[var(--cn-text-muted)]" />
        <div class="min-w-0">
          <p class="text-sm font-medium text-[var(--cn-text)]">
            {{ t('settings.maintenance.unavailable_title') }}
          </p>
          <p class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.maintenance.unavailable_body') }}
          </p>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Card 1 — Result. No title: the first line already says what it is. ═══ -->
    <SettingsCard v-else>
      <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span class="text-sm font-medium text-[var(--cn-text)]">
          {{ t('settings.maintenance.headline', {
            size: formatBytes(totalReclaimable),
            n: activeCategories,
          }, activeCategories) }}
        </span>
        <span v-if="scannedAt" class="text-xs text-[var(--cn-text-muted)]">
          · {{ t('settings.maintenance.scanned_at', { time: scannedAt }) }}
        </span>
      </div>

      <p v-if="hasScanned && totalReclaimable === 0" class="mt-1 text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.maintenance.empty_title') }}
      </p>
      <p v-if="offline" class="mt-1 text-xs text-[var(--cn-warning)]">
        {{ t('settings.maintenance.offline_note') }}
      </p>
      <p v-if="lastError" class="mt-1 text-xs text-[var(--cn-danger)]">{{ lastError }}</p>

      <!-- One row per category. Findings are rows inside this card, never a
           card each: the density lesson from the earlier settings sweep. -->
      <div class="mt-3 flex flex-col gap-1.5">
        <div
          v-for="row in categoryRows"
          :key="row.key"
          class="rounded-lg border border-[var(--cn-border)]"
        >
          <div class="flex items-center gap-3 px-3 py-2.5">
            <AppIcon
              :name="row.icon"
              :size="17"
              class="shrink-0"
              :class="rowHasBytes(row) ? 'text-[var(--cn-accent)]' : 'text-[var(--cn-text-muted)]'"
            />
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span
                  class="text-sm font-medium"
                  :class="rowHasBytes(row) ? 'text-[var(--cn-text)]' : 'text-[var(--cn-text-muted)]'"
                >{{ row.label }}</span>
                <StatusChip
                  v-if="row.finding && rowHasBytes(row)"
                  size="sm"
                  :tone="row.finding.class === 'safe' ? 'success' : 'warning'"
                >
                  {{ row.finding.class === 'safe'
                    ? t('settings.maintenance.class_safe')
                    : t('settings.maintenance.class_conditional') }}
                </StatusChip>
              </div>
              <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ rowSummary(row) }}</p>
            </div>
            <span
              class="shrink-0 text-sm tabular-nums"
              :class="rowHasBytes(row) ? 'text-[var(--cn-text)]' : 'text-[var(--cn-text-muted)]'"
            >{{ formatBytes(rowBytes(row)) }}</span>
            <Button v-if="rowActionable(row)" size="small" text @click="toggleExpand(row.key)">
              {{ expanded[row.key] ? t('settings.maintenance.hide') : t('settings.maintenance.review') }}
              <AppIcon :name="expanded[row.key] ? 'ChevronUp' : 'ChevronRight'" :size="14" class="ml-1" />
            </Button>
          </div>

          <!-- Expanded: the exact items, ticked one by one, with the action
               button for THIS category and nothing else. -->
          <div
            v-if="expanded[row.key] && row.finding"
            class="border-t border-[var(--cn-border)] px-3 py-2.5"
          >
            <div class="mb-2 flex flex-wrap items-center gap-3 text-xs">
              <button
                type="button"
                class="text-[var(--cn-accent)] hover:underline"
                @click="setAll(row.finding, true)"
              >{{ t('settings.maintenance.select_all') }}</button>
              <button
                type="button"
                class="text-[var(--cn-text-muted)] hover:underline"
                @click="setAll(row.finding, false)"
              >{{ t('settings.maintenance.select_none') }}</button>
              <span class="ml-auto text-[var(--cn-text-muted)]">
                {{ t('settings.maintenance.items_count', { n: row.finding.item_count }, row.finding.item_count) }}
              </span>
            </div>

            <ul class="max-h-72 overflow-y-auto rounded-lg border border-[var(--cn-border)]">
              <li
                v-for="item in row.finding.items"
                :key="item.id"
                class="flex items-center gap-2.5 border-b border-[var(--cn-border)] px-2.5 py-1.5 last:border-b-0"
              >
                <Checkbox
                  :input-id="`mnt-${item.id}`"
                  :model-value="isSelected(row.key, item.id)"
                  binary
                  @update:model-value="toggleItem(row.key, item.id)"
                />
                <label :for="`mnt-${item.id}`" class="min-w-0 flex-1 cursor-pointer">
                  <div class="truncate text-xs font-medium text-[var(--cn-text)]">{{ item.label }}</div>
                  <div class="truncate font-mono text-[11px] text-[var(--cn-text-muted)]">{{ item.path }}</div>
                  <div v-if="item.detail" class="text-[11px] text-[var(--cn-text-muted)]">{{ item.detail }}</div>
                </label>
                <span class="shrink-0 text-[11px] tabular-nums text-[var(--cn-text-muted)]">
                  {{ formatBytes(item.bytes) }}
                </span>
                <span
                  v-if="item.mtime"
                  class="hidden shrink-0 text-[11px] tabular-nums text-[var(--cn-text-muted)] sm:block"
                >{{ dayjs(item.mtime).format('YYYY-MM-DD') }}</span>
              </li>
            </ul>

            <p v-if="row.finding.truncated" class="mt-2 text-[11px] text-[var(--cn-warning)]">
              {{ t('settings.maintenance.truncated_note', {
                shown: row.finding.items.length,
                total: row.finding.item_count,
              }) }}
            </p>
            <!-- Whatever the detector wanted on the record: a guard it could
                 not prove, a count it had to cap. Shown, never summarised. -->
            <p
              v-for="(note, i) in row.finding.notes"
              :key="i"
              class="mt-1 text-[11px] text-[var(--cn-text-muted)]"
            >{{ note }}</p>

            <div class="mt-2.5 flex justify-end">
              <Button
                size="small"
                severity="danger"
                :disabled="busy || selectedIds(row.key).length === 0"
                @click="openConfirm(row.finding)"
              >
                <AppIcon :name="row.finding.action === 'truncate' ? 'Scissors' : 'Trash2'" :size="14" class="mr-1.5" />
                {{ actionLabel(row.finding) }}
              </Button>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Card 2 — Blocked right now. Self-hiding. ═══ -->
    <SettingsCard v-if="!unavailable && blockedFindings.length" :title="t('settings.maintenance.blocked_title')">
      <div class="flex flex-col gap-1.5">
        <div
          v-for="f in blockedFindings"
          :key="f.category"
          class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2.5"
        >
          <AppIcon name="Lock" :size="16" class="mt-0.5 shrink-0 text-[var(--cn-warning)]" />
          <div class="min-w-0 flex-1">
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ categoryLabel(f.category) }}</span>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ blockedText(f) }}</p>
          </div>
          <!-- The size stays visible: the user is entitled to know what is
               being withheld, not only that something is. -->
          <span class="shrink-0 text-sm tabular-nums text-[var(--cn-text-muted)]">
            {{ formatBytes(f.reclaimable_bytes) }}
          </span>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Card 4 — Space we are not going to touch. Collapsed by default. ═══ -->
    <SettingsCard v-if="!unavailable" :title="t('settings.maintenance.untouched_title')">
      <template #actions>
        <div class="flex items-center gap-3">
          <span v-if="untouchedTotal > 0" class="text-xs tabular-nums text-[var(--cn-text-muted)]">
            {{ formatBytes(untouchedTotal) }}
          </span>
          <Button size="small" text @click="showUntouched = !showUntouched">
            {{ showUntouched ? t('settings.maintenance.hide') : t('settings.maintenance.show') }}
            <AppIcon :name="showUntouched ? 'ChevronUp' : 'ChevronDown'" :size="14" class="ml-1" />
          </Button>
        </div>
      </template>

      <div v-if="showUntouched" class="flex flex-col gap-1.5">
        <!-- Report-only findings, when a category reports itself that way. -->
        <div
          v-for="f in readOnlyFindings"
          :key="f.category"
          class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2.5"
        >
          <AppIcon name="Shield" :size="16" class="mt-0.5 shrink-0 text-[var(--cn-text-muted)]" />
          <div class="min-w-0 flex-1">
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ f.title || categoryLabel(f.category) }}</span>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ f.summary }}</p>
          </div>
          <span class="shrink-0 text-sm tabular-nums text-[var(--cn-text-muted)]">
            {{ formatBytes(f.reclaimable_bytes) }}
          </span>
        </div>

        <!-- The panel proper: one row, one size when anyone could measure it,
             one sentence of why. No checkbox and no button, by construction. -->
        <div
          v-for="row in panelRows"
          :key="row.key"
          class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2.5"
        >
          <AppIcon :name="row.icon" :size="16" class="mt-0.5 shrink-0 text-[var(--cn-text-muted)]" />
          <div class="min-w-0 flex-1">
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ row.label }}</span>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ row.why }}</p>
            <router-link
              v-if="row.to"
              :to="row.to"
              class="mt-1 inline-flex items-center gap-1 text-xs text-[var(--cn-accent)] hover:underline"
            >
              {{ t('settings.maintenance.open_section') }}
              <AppIcon name="ArrowRight" :size="12" />
            </router-link>
          </div>
          <!-- No size rather than "0 B": nobody measured this one, and 0 B
               would read as "there is nothing there". -->
          <span v-if="row.bytes > 0" class="shrink-0 text-sm tabular-nums text-[var(--cn-text-muted)]">
            {{ formatBytes(row.bytes) }}
          </span>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Card 5 — Last maintenance. Only when a run has finished. ═══ -->
    <SettingsCard v-if="!unavailable && lastRun" :title="t('settings.maintenance.last_title')">
      <div class="flex flex-col gap-1.5">
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1 text-sm">
          <AppIcon name="CheckCircle2" :size="16" class="shrink-0 text-[var(--cn-success)]" />
          <span class="font-medium text-[var(--cn-text)]">{{ categoryLabel(lastRun.category) }}</span>
          <span class="text-[var(--cn-text-muted)]">
            · {{ t('settings.maintenance.last_freed', { size: formatBytes(lastRun.disk_delta_bytes) }) }}
          </span>
          <span v-if="lastRunAt" class="text-xs text-[var(--cn-text-muted)]">
            · {{ dayjs(lastRunAt).format('YYYY-MM-DD HH:mm') }}
          </span>
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.maintenance.last_detail', {
            n: lastRun.removed_items,
            size: formatBytes(lastRun.removed_bytes),
          }) }}
          <span v-if="lastRun.manifest_id" class="font-mono">
            · {{ t('settings.maintenance.last_manifest', { id: lastRun.manifest_id }) }}
          </span>
        </p>
        <p v-if="deltaShortfall" class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.maintenance.delta_note', {
            delta: formatBytes(lastRun.disk_delta_bytes),
            removed: formatBytes(lastRun.removed_bytes),
          }) }}
        </p>
        <p v-if="lastRun.failed.length" class="text-xs text-[var(--cn-warning)]">
          {{ t('settings.maintenance.failed_note', { n: lastRun.failed.length }, lastRun.failed.length) }}
          <span class="text-[var(--cn-text-muted)]">
            — {{ lastRun.failed.slice(0, 3).map((x) => x.reason).join(' · ') }}
          </span>
        </p>
        <!-- A skip is a guard doing its job between the scan and the unlink,
             so it is reported in plain text and not as a failure. -->
        <p v-if="lastRun.skipped.length" class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.maintenance.skipped_note', { n: lastRun.skipped.length }, lastRun.skipped.length) }}
          — {{ lastRun.skipped.slice(0, 3).map((x) => x.reason).join(' · ') }}
        </p>
      </div>
    </SettingsCard>

    <!-- ═══ Confirmation — per category, never global ═══ -->
    <Dialog
      :visible="!!confirming"
      modal
      :header="confirming ? categoryLabel(confirming.category) : ''"
      :style="{ width: '34rem' }"
      @update:visible="(v: boolean) => { if (!v) confirming = null }"
    >
      <div v-if="confirming" class="flex flex-col gap-3 text-sm">
        <ul class="max-h-52 overflow-y-auto rounded-lg border border-[var(--cn-border)]">
          <li
            v-for="item in confirmPreview"
            :key="item.id"
            class="flex items-center gap-2 border-b border-[var(--cn-border)] px-2.5 py-1.5 text-xs last:border-b-0"
          >
            <span class="min-w-0 flex-1 truncate font-mono text-[var(--cn-text)]">{{ item.label }}</span>
            <span class="shrink-0 tabular-nums text-[var(--cn-text-muted)]">{{ formatBytes(item.bytes) }}</span>
          </li>
        </ul>
        <p v-if="confirmRest > 0" class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.maintenance.confirm_more', { n: confirmRest }) }}
        </p>
        <p class="font-medium text-[var(--cn-text)]">
          {{ t('settings.maintenance.confirm_size', { size: formatBytes(confirmBytes) }) }}
        </p>
        <!-- One consequence line, in the category's own words. -->
        <div class="flex items-start gap-2 rounded-lg bg-[var(--cn-warning-soft)] px-3 py-2 text-xs text-[var(--cn-warning)]">
          <AppIcon name="TriangleAlert" :size="16" class="mt-0.5 shrink-0" />
          <span>{{ consequenceFor(confirming.category) }}</span>
        </div>
      </div>
      <template #footer>
        <Button severity="secondary" text @click="confirming = null">{{ t('common.cancel') }}</Button>
        <Button v-if="confirming" severity="danger" :loading="submitting" @click="executeClean">
          {{ actionLabel(confirming) }}
        </Button>
      </template>
    </Dialog>
  </SettingsPage>
</template>
