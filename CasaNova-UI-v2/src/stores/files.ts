// Files store — tab-aware file manager with shared clipboard and tree.
//
// Each tab owns its own: currentPath, backStack, forwardStack, selected.
// Shared across all tabs: tree (filesystem cache), clipboard (cut/copy
// buffer), viewMode, showHidden, activeOperationCount.
//
// This lets users copy in tab 1 and paste in tab 2 seamlessly.

import { defineStore } from 'pinia'
import { ref, shallowRef, computed, watch, onScopeDispose, triggerRef } from 'vue'
import {
  type FileEntry,
  formatBytes as _fmt,
} from '@/types/files'
import { filesApi } from '@/api'
import type { UploadFileResult } from '@/api/files'
import { safeCall } from '@/api/safeCall'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'
import { useInstallProgressStore } from './installProgress'

export const formatBytes = _fmt
export type ViewMode = 'grid' | 'list'
export type SortKey = 'name' | 'modified' | 'size' | 'type'
export type SortDir = 'asc' | 'desc'

// ── File operation tracking ────────────────────────────────────────
export interface FileOperation {
  id: string
  type: 'upload' | 'download' | 'copy' | 'move' | 'delete'
  status: 'running' | 'done' | 'error'
  label: string
  progress: number       // 0-100, -1 for indeterminate
  currentFile?: string
  totalFiles: number
  completedFiles: number
  totalBytes?: number
  loadedBytes?: number
  startedAt: number
  error?: string
}

let _opIdCounter = 0
function generateOpId(): string {
  return `op-${++_opIdCounter}-${Date.now()}`
}

const STORAGE_VIEW_MODE = 'casanova.files.view_mode'
const STORAGE_SHOW_HIDDEN = 'casanova.files.show_hidden'
const STORAGE_DETAILS_PANE = 'casanova.files.details_pane'
const STORAGE_SORT_KEY = 'casanova.files.sort_key'
const STORAGE_SORT_DIR = 'casanova.files.sort_dir'

// ── Tab type ────────────────────────────────────────────────────────
export interface FileTab {
  id: string
  label: string            // Display label (folder name or path)
  currentPath: string
  backStack: string[]
  forwardStack: string[]
  selected: Set<string>
  selectionAnchor: string | null
}

let _tabCounter = 0
function createTab(path = '/'): FileTab {
  const label = path === '/' ? '/' : path.split('/').filter(Boolean).pop() || '/'
  return {
    id: `tab-${++_tabCounter}`,
    label,
    currentPath: path,
    backStack: [],
    forwardStack: [],
    selected: new Set(),
    selectionAnchor: null,
  }
}

// ── Clipboard (shared across tabs) ──────────────────────────────────
export interface ClipboardState {
  paths: string[]
  mode: 'cut' | 'copy'
}

const blobUrls = new Map<string, string>()

export const useFilesStore = defineStore('files', () => {
  // ── Shared state ──────────────────────────────────────────────────
  const tree = shallowRef<Map<string, FileEntry[]>>(new Map())
  // v0.1: route through syncedPrefs so personalization follows the
  // account, not the browser. getPref reads the in-memory cache that's
  // bootstrapped from localStorage + hydrated from the backend on
  // login; setPref writes localStorage + debounce-syncs to the backend.
  const viewMode = ref<ViewMode>((getPref(STORAGE_VIEW_MODE) as ViewMode) || 'grid')
  const showHidden = ref<boolean>(getPref(STORAGE_SHOW_HIDDEN) === 'true')
  const detailsPaneOpen = ref<boolean>(getPref(STORAGE_DETAILS_PANE) === 'true')
  const sortKey = ref<SortKey>((getPref(STORAGE_SORT_KEY) as SortKey) || 'name')
  const sortDir = ref<SortDir>((getPref(STORAGE_SORT_DIR) as SortDir) || 'asc')

  // The five reads above happen once, at store creation, against a cache the
  // backend hydrate has usually not filled yet on a fresh login. Re-read when
  // it lands. These prefs are written from the setters below rather than from
  // watchers, so adopting a hydrated value here cannot echo back. Same idiom
  // as theme.ts / dashboardPrefs.ts / useAppOrder.ts / useUserPrefs.ts.
  watch(prefsGeneration(), () => {
    viewMode.value = (getPref(STORAGE_VIEW_MODE) as ViewMode) || 'grid'
    showHidden.value = getPref(STORAGE_SHOW_HIDDEN) === 'true'
    detailsPaneOpen.value = getPref(STORAGE_DETAILS_PANE) === 'true'
    sortKey.value = (getPref(STORAGE_SORT_KEY) as SortKey) || 'name'
    sortDir.value = (getPref(STORAGE_SORT_DIR) as SortDir) || 'asc'
  })
  const activeOperationCount = ref(0)
  const fixtureMode = ref(false)
  const clipboard = ref<ClipboardState | null>(null)

  // ── Tracked operations ────────────────────────────────────────────
  const operations = ref<FileOperation[]>([])

  function addOperation(op: FileOperation) {
    operations.value = [...operations.value, op]
  }

  function updateOperation(id: string, partial: Partial<FileOperation>) {
    operations.value = operations.value.map((o) =>
      o.id === id ? { ...o, ...partial } : o,
    )
  }

  function removeOperation(id: string) {
    operations.value = operations.value.filter((o) => o.id !== id)
  }

  function clearCompleted() {
    operations.value = operations.value.filter(
      (o) => o.status !== 'done' && o.status !== 'error',
    )
  }

  function autoRemoveAfter(id: string, ms = 5000) {
    setTimeout(() => removeOperation(id), ms)
  }

  // ── Tabs ──────────────────────────────────────────────────────────
  const tabs = ref<FileTab[]>([createTab('/')])
  const activeTabId = ref<string>(tabs.value[0].id)

  const activeTab = computed(() =>
    tabs.value.find((t) => t.id === activeTabId.value) ?? tabs.value[0],
  )

  // ── Proxy: expose active tab's state as flat refs for backward compat
  const currentPath = computed(() => activeTab.value.currentPath)
  const selected = computed(() => activeTab.value.selected)
  const canGoBack = computed(() => activeTab.value.backStack.length > 0)
  const canGoForward = computed(() => activeTab.value.forwardStack.length > 0)

  // ── Visible entries (from active tab's path) ─────────────────────
  //
  // v0.1 perf: memoize sort by entry-array identity. The previous
  // computed re-sorted the entire directory on every reactive tick
  // — every `triggerRef(tabs)`, every clipboard change, every
  // selection toggle. For a 5000-file directory that's a ~50ms hit
  // on every keystroke / click. Now we cache the sort result keyed
  // by (entries-array, showHidden) so the sort only re-runs when
  // the directory itself changes or the user toggles hidden-files.
  let _sortCacheKey:
    | { entries: FileEntry[]; showHidden: boolean; sortKey: SortKey; sortDir: SortDir }
    | null = null
  let _sortCacheResult: FileEntry[] = []
  const visibleEntries = computed<FileEntry[]>(() => {
    const all = tree.value.get(activeTab.value.currentPath) ?? []
    const showHiddenNow = showHidden.value
    const keyNow = sortKey.value
    const dirNow = sortDir.value
    if (
      _sortCacheKey &&
      _sortCacheKey.entries === all &&
      _sortCacheKey.showHidden === showHiddenNow &&
      _sortCacheKey.sortKey === keyNow &&
      _sortCacheKey.sortDir === dirNow
    ) {
      return _sortCacheResult
    }
    const filtered = showHiddenNow ? all : all.filter((e) => !e.name.startsWith('.'))
    const mul = dirNow === 'desc' ? -1 : 1
    const ext = (name: string) =>
      name.includes('.') ? name.slice(name.lastIndexOf('.') + 1).toLowerCase() : ''
    const byName = (a: FileEntry, b: FileEntry) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
    const sorted = [...filtered].sort((a, b) => {
      // Folders always group first, in both directions (matches Win11).
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
      let cmp: number
      switch (keyNow) {
        case 'modified':
          cmp = (Date.parse(a.modified) || 0) - (Date.parse(b.modified) || 0)
          break
        case 'size':
          cmp = (a.size ?? 0) - (b.size ?? 0)
          break
        case 'type':
          cmp = ext(a.name).localeCompare(ext(b.name)) || byName(a, b)
          break
        default:
          cmp = byName(a, b)
      }
      return (cmp || byName(a, b)) * mul
    })
    _sortCacheKey = { entries: all, showHidden: showHiddenNow, sortKey: keyNow, sortDir: dirNow }
    _sortCacheResult = sorted
    return sorted
  })
  const sortedEntries = visibleEntries

  const breadcrumbs = computed(() => {
    const parts = activeTab.value.currentPath.split('/').filter(Boolean)
    const out: Array<{ label: string; path: string }> = []
    let running = ''
    for (const p of parts) {
      running += '/' + p
      out.push({ label: p, path: running })
    }
    return out
  })

  // ── Tab management ────────────────────────────────────────────────
  function newTab(path = '/') {
    const tab = createTab(path)
    tabs.value = [...tabs.value, tab]
    activeTabId.value = tab.id
    void fetchDir(path)
    return tab
  }

  function closeTab(tabId: string) {
    if (tabs.value.length <= 1) return // always keep at least one
    const idx = tabs.value.findIndex((t) => t.id === tabId)
    if (idx === -1) return
    tabs.value = tabs.value.filter((t) => t.id !== tabId)
    // If we closed the active tab, activate the nearest one
    if (activeTabId.value === tabId) {
      const newIdx = Math.min(idx, tabs.value.length - 1)
      activeTabId.value = tabs.value[newIdx].id
    }
  }

  function switchTab(tabId: string) {
    if (tabs.value.some((t) => t.id === tabId)) {
      activeTabId.value = tabId
    }
  }

  function duplicateTab() {
    return newTab(activeTab.value.currentPath)
  }

  // ── Operations counter ────────────────────────────────────────────
  function beginOperation() { activeOperationCount.value++ }
  function endOperation() { activeOperationCount.value = Math.max(0, activeOperationCount.value - 1) }

  // ── Backend-or-fixture fetch ──────────────────────────────────────
  //
  // v0.1 perf: LRU-capped fetched-paths set. The previous Set grew
  // unbounded — every directory the user ever browsed stayed in
  // memory and counted toward the cache-hit shortcut. A power user
  // exploring a deep tree could easily push it into thousands of
  // entries, and the cache shortcut became wrong (stale listings
  // never re-fetched). The cap evicts the oldest path when we go
  // over MAX_FETCHED, mirroring browser-style "back" depth.
  const fetched = new Set<string>()
  const fetchedOrder: string[] = []
  const MAX_FETCHED = 64

  function rememberFetched(path: string): void {
    if (fetched.has(path)) {
      // Move to the front of recency.
      const i = fetchedOrder.indexOf(path)
      if (i !== -1) fetchedOrder.splice(i, 1)
      fetchedOrder.push(path)
      return
    }
    fetched.add(path)
    fetchedOrder.push(path)
    while (fetchedOrder.length > MAX_FETCHED) {
      const evict = fetchedOrder.shift()
      if (evict) fetched.delete(evict)
    }
  }

  async function fetchDir(path: string): Promise<void> {
    if (fetched.has(path)) return
    const result = await safeCall(
      () => filesApi.listDir(path, showHidden.value),
    )
    if (result.error && !result.fixture) return
    fixtureMode.value = result.fixture
    // v0.1 perf: mutate the shallowRef Map in place + triggerRef
    // instead of cloning. The previous `new Map(tree.value)` was an
    // O(n) copy of every cached directory on every fetch — wasteful
    // when only one key changes.
    tree.value.set(path, [...(result.data ?? [])])
    triggerRef(tree)
    rememberFetched(path)
  }

  function invalidatePath(path: string) {
    fetched.delete(path)
    const i = fetchedOrder.indexOf(path)
    if (i !== -1) fetchedOrder.splice(i, 1)
  }

  // Root directory is fetched lazily on first cd('/') or when FilesView
  // mounts — NOT eagerly on store creation. This avoids a ~100-300ms
  // API call on dashboard-only page reloads where files are never accessed.

  // ── Helper: update the active tab's label from its path ───────────
  function _updateTabLabel(tab: FileTab) {
    tab.label = tab.currentPath === '/'
      ? '/'
      : tab.currentPath.split('/').filter(Boolean).pop() || '/'
  }

  // ── Navigation (operates on active tab) ───────────────────────────
  function cd(path: string) {
    const tab = activeTab.value
    if (path === tab.currentPath) return
    // Always re-fetch the target directory so mount changes, new files,
    // etc. are visible immediately — not stale from a previous visit.
    invalidatePath(path)
    void fetchDir(path)
    tab.backStack.push(tab.currentPath)
    tab.forwardStack = []
    tab.currentPath = path
    tab.selected = new Set()
    _updateTabLabel(tab)
    // Force reactivity
    triggerRef(tabs)
  }

  function goBack() {
    const tab = activeTab.value
    const prev = tab.backStack.pop()
    if (prev === undefined) return
    tab.forwardStack.push(tab.currentPath)
    tab.currentPath = prev
    tab.selected = new Set()
    _updateTabLabel(tab)
    triggerRef(tabs)
  }

  function goForward() {
    const tab = activeTab.value
    const next = tab.forwardStack.pop()
    if (next === undefined) return
    tab.backStack.push(tab.currentPath)
    tab.currentPath = next
    tab.selected = new Set()
    _updateTabLabel(tab)
    triggerRef(tabs)
  }

  function goUp() {
    const tab = activeTab.value
    if (tab.currentPath === '/') return
    const parent = tab.currentPath.substring(0, tab.currentPath.lastIndexOf('/')) || '/'
    cd(parent)
  }

  // ── View preferences ──────────────────────────────────────────────
  function setViewMode(mode: ViewMode) {
    viewMode.value = mode
    setPref(STORAGE_VIEW_MODE, mode)
  }
  function toggleHidden() {
    showHidden.value = !showHidden.value
    setPref(STORAGE_SHOW_HIDDEN, String(showHidden.value))
  }
  function toggleDetailsPane() {
    detailsPaneOpen.value = !detailsPaneOpen.value
    setPref(STORAGE_DETAILS_PANE, String(detailsPaneOpen.value))
  }
  // One setter for both axes: a sort-key value sets the field, an
  // 'asc'/'desc' value sets the direction. Lets the Sort menu emit a
  // single 'sort' action with either payload.
  function setSort(payload: SortKey | SortDir) {
    if (payload === 'asc' || payload === 'desc') {
      sortDir.value = payload
      setPref(STORAGE_SORT_DIR, payload)
    } else {
      sortKey.value = payload
      setPref(STORAGE_SORT_KEY, payload)
    }
  }

  // ── Selection (operates on active tab) ────────────────────────────
  function selectOnly(path: string) {
    const tab = activeTab.value
    tab.selected = new Set([path])
    tab.selectionAnchor = path
    triggerRef(tabs)
  }

  function toggleSelect(path: string) {
    const tab = activeTab.value
    const next = new Set(tab.selected)
    if (next.has(path)) next.delete(path)
    else next.add(path)
    tab.selected = next
    tab.selectionAnchor = path
    triggerRef(tabs)
  }

  function selectRange(path: string) {
    const tab = activeTab.value
    if (!tab.selectionAnchor) { selectOnly(path); return }
    const items = visibleEntries.value.map((e) => e.path)
    const startIdx = items.indexOf(tab.selectionAnchor)
    const endIdx = items.indexOf(path)
    if (startIdx === -1 || endIdx === -1) { selectOnly(path); return }
    const [lo, hi] = startIdx < endIdx ? [startIdx, endIdx] : [endIdx, startIdx]
    tab.selected = new Set(items.slice(lo, hi + 1))
    triggerRef(tabs)
  }

  function selectAll() {
    const tab = activeTab.value
    tab.selected = new Set(visibleEntries.value.map((e) => e.path))
    if (visibleEntries.value.length > 0) {
      tab.selectionAnchor = visibleEntries.value[0].path
    }
    triggerRef(tabs)
  }

  function clearSelect() {
    const tab = activeTab.value
    if (tab.selected.size === 0) return
    tab.selected = new Set()
    tab.selectionAnchor = null
    triggerRef(tabs)
  }

  // ── Clipboard (shared — works across tabs) ────────────────────────
  function clipboardCut(paths: string[]) {
    clipboard.value = { paths: [...paths], mode: 'cut' }
  }
  function clipboardCopy(paths: string[]) {
    clipboard.value = { paths: [...paths], mode: 'copy' }
  }
  function clipboardClear() {
    clipboard.value = null
  }

  /**
   * Paste clipboard contents into the active tab's current directory.
   * Cut = move, Copy = duplicate. Uses the real backend when available.
   *
   * `style` is forwarded to the batch backend: "skip" leaves any
   * existing destination entries alone. Omitted (default) = overwrite,
   * matching the prior behaviour for callers that don't pass an
   * explicit strategy. The view layer pre-flights collisions and asks
   * the user before calling with a non-default style.
   */
  async function clipboardPaste(style?: 'skip'): Promise<number> {
    if (!clipboard.value || clipboard.value.paths.length === 0) return 0
    const { paths, mode } = clipboard.value
    const dest = activeTab.value.currentPath
    let count = 0

    if (mode === 'cut') {
      if (!fixtureMode.value) {
        // Real backend: use batch move, then invalidate + refetch
        const opId = generateOpId()
        const firstName = paths.length === 1
          ? paths[0].substring(paths[0].lastIndexOf('/') + 1)
          : `${paths.length} files`
        addOperation({
          id: opId,
          type: 'move',
          status: 'running',
          label: firstName,
          progress: 0,
          totalFiles: paths.length,
          completedFiles: 0,
          startedAt: Date.now(),
        })
        beginOperation()
        const result = await safeCall(
          () => filesApi.operateBatch({ operation: 'move', sources: paths, destination: dest, style }),
        )
        if (!result.error || result.fixture) {
          // Poll batch status if the backend returned a batch id
          const batchResp = result.data
          if (batchResp && batchResp.id && (batchResp.status === 'pending' || batchResp.status === 'running')) {
            await _pollBatchStatus(opId, batchResp.id, paths.length)
          } else {
            updateOperation(opId, { status: 'done', progress: 100, completedFiles: paths.length })
          }
          // Invalidate source dirs and destination so tree reflects the move
          const parentDirs = new Set(paths.map((p) => p.substring(0, p.lastIndexOf('/')) || '/'))
          for (const dir of parentDirs) { invalidatePath(dir); void fetchDir(dir) }
          invalidatePath(dest)
          void fetchDir(dest)
          count = paths.length
        } else {
          updateOperation(opId, { status: 'error', error: result.error ?? 'Move failed', progress: -1 })
          const toasts = useInstallProgressStore()
          toasts.pushToast('error', `Move failed: ${result.error ?? 'Unknown error'}`)
        }
        endOperation()
        autoRemoveAfter(opId)
      } else {
        // Fixture mode: local-only move
        count = moveEntries(paths, dest)
      }
      clipboard.value = null // clear after cut-paste
    } else {
      // Copy: use backend batch copy, or local duplicate
      if (!fixtureMode.value) {
        const opId = generateOpId()
        const firstName = paths.length === 1
          ? paths[0].substring(paths[0].lastIndexOf('/') + 1)
          : `${paths.length} files`
        addOperation({
          id: opId,
          type: 'copy',
          status: 'running',
          label: firstName,
          progress: 0,
          totalFiles: paths.length,
          completedFiles: 0,
          startedAt: Date.now(),
        })
        beginOperation()
        const result = await safeCall(
          () => filesApi.operateBatch({ operation: 'copy', sources: paths, destination: dest, style }),
        )
        if (!result.error || result.fixture) {
          const batchResp = result.data
          if (batchResp && batchResp.id && (batchResp.status === 'pending' || batchResp.status === 'running')) {
            await _pollBatchStatus(opId, batchResp.id, paths.length)
          } else {
            updateOperation(opId, { status: 'done', progress: 100, completedFiles: paths.length })
          }
          // Invalidate and re-fetch the destination so the pasted entries appear.
          invalidatePath(dest)
          void fetchDir(dest)
          count = paths.length
        } else {
          updateOperation(opId, { status: 'error', error: result.error ?? 'Copy failed', progress: -1 })
        }
        endOperation()
        autoRemoveAfter(opId)
      } else {
        // Local fixture copy — duplicate entries under the dest
        for (const p of paths) {
          const name = p.substring(p.lastIndexOf('/') + 1)
          const existing = getChildrenOf(dest)
          if (existing.some((e) => e.name === name)) continue
          const srcParent = p.substring(0, p.lastIndexOf('/')) || '/'
          const srcEntry = getChildrenOf(srcParent).find((e) => e.path === p)
          if (!srcEntry) continue
          const newPath = dest + '/' + name
          const copy: FileEntry = { ...srcEntry, path: newPath }
          const next = new Map(tree.value)
          next.set(dest, [...existing, copy])
          if (srcEntry.isDir) {
            next.set(newPath, [...(getChildrenOf(p))])
          }
          tree.value = next
          count++
        }
      }
    }
    return count
  }

  // ── Batch polling helper ───────────────────────────────────────────
  async function _pollBatchStatus(opId: string, batchId: string, totalFiles: number) {
    const MAX_POLLS = 300 // 5 minutes at 1s interval
    for (let i = 0; i < MAX_POLLS; i++) {
      await new Promise((r) => setTimeout(r, 1000))
      const statusResult = await safeCall(() => filesApi.getBatchStatus())
      if (statusResult.error) {
        updateOperation(opId, { status: 'error', error: statusResult.error, progress: -1 })
        return
      }
      const batch = (statusResult.data ?? []).find((b) => b.id === batchId)
      if (!batch) {
        // Batch gone — assume completed
        updateOperation(opId, { status: 'done', progress: 100, completedFiles: totalFiles })
        return
      }
      if (batch.status === 'done') {
        updateOperation(opId, { status: 'done', progress: 100, completedFiles: totalFiles })
        return
      }
      if (batch.status === 'error') {
        updateOperation(opId, { status: 'error', error: batch.message ?? 'Batch operation failed', progress: -1 })
        return
      }
      // Still running/pending — update progress
      updateOperation(opId, {
        progress: batch.progress ?? 0,
        completedFiles: Math.round(((batch.progress ?? 0) / 100) * totalFiles),
      })
    }
  }

  // ── Mutations ─────────────────────────────────────────────────────
  function getChildrenOf(path: string): FileEntry[] {
    return tree.value.get(path) ?? []
  }

  function entryExistsAt(parentPath: string, name: string): boolean {
    return getChildrenOf(parentPath).some((e) => e.name === name)
  }

  function createFolder(name: string): FileEntry | null {
    const trimmed = name.trim()
    if (!trimmed) return null
    if (entryExistsAt(activeTab.value.currentPath, trimmed)) return null

    const path = activeTab.value.currentPath + '/' + trimmed
    const entry: FileEntry = {
      name: trimmed,
      path,
      isDir: true,
      size: 0,
      modified: new Date().toISOString(),
    }
    const next = new Map(tree.value)
    next.set(activeTab.value.currentPath, [...getChildrenOf(activeTab.value.currentPath), entry])
    next.set(path, [])
    tree.value = next

    if (!fixtureMode.value) {
      void (async () => {
        const result = await safeCall(
          () => filesApi.mkdir(path),
        )
        if (result.error && !result.fixture) {
          const m = new Map(tree.value)
          m.set(
            activeTab.value.currentPath,
            getChildrenOf(activeTab.value.currentPath).filter((e) => e.path !== path),
          )
          m.delete(path)
          tree.value = m
        } else {
          invalidatePath(activeTab.value.currentPath)
        }
      })()
    }
    return entry
  }

  // One AbortController per in-flight batch (dropping several files twice in
  // a row runs two batches): cancel aborts ALL of them, and a finished batch
  // removes only its own controller — no clobbering.
  const uploadAborts = new Set<AbortController>()

  function cancelUploads() {
    for (const c of uploadAborts) c.abort()
  }

  // Per-OPERATION abort handles (downloads, and upload batches by opId) so
  // the ops panel can cancel exactly the row the user clicked.
  const opAborts = new Map<string, AbortController>()

  function cancelOperation(opId: string) {
    opAborts.get(opId)?.abort()
  }

  // Dedupe: a download with no visible feedback used to invite repeated
  // clicks, and EACH click streamed the whole file again in parallel (the
  // "press download 5 times" bug). One in-flight download per key.
  const activeDownloads = new Set<string>()

  function triggerBrowserDownload(url: string, name: string) {
    const a = document.createElement('a')
    a.href = url
    a.download = name
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  }

  async function runDownloadOp(
    key: string,
    label: string,
    fetcher: (onProgress: (loaded: number, total: number) => void, signal: AbortSignal) => Promise<{ url: string; name: string }>,
  ): Promise<void> {
    if (activeDownloads.has(key)) return // already in flight — the panel shows it
    activeDownloads.add(key)
    const opId = generateOpId()
    const abort = new AbortController()
    opAborts.set(opId, abort)
    addOperation({
      id: opId,
      type: 'download',
      status: 'running',
      label,
      progress: -1, // indeterminate until the first progress event with a total
      currentFile: label,
      totalFiles: 1,
      completedFiles: 0,
      loadedBytes: 0,
      startedAt: Date.now(),
    })
    beginOperation()
    try {
      const { url, name } = await fetcher((loaded, total) => {
        updateOperation(opId, {
          loadedBytes: loaded,
          totalBytes: total > 0 ? total : undefined,
          progress: total > 0 ? Math.round((loaded / total) * 100) : -1,
        })
      }, abort.signal)
      updateOperation(opId, { status: 'done', progress: 100, completedFiles: 1 })
      triggerBrowserDownload(url, name)
      // The browser has queued the save; the blob can be released shortly.
      setTimeout(() => URL.revokeObjectURL(url), 1000)
    } catch (e) {
      updateOperation(opId, {
        status: 'error',
        progress: -1,
        error: abort.signal.aborted ? '__canceled__' : String((e as { message?: string })?.message ?? e),
      })
    } finally {
      endOperation()
      opAborts.delete(opId)
      activeDownloads.delete(key)
      autoRemoveAfter(opId)
    }
  }

  async function downloadEntry(path: string): Promise<void> {
    const name = path.substring(path.lastIndexOf('/') + 1) || path
    // Always fetch the SERVER copy. The in-memory blob cache is backed by the
    // user's local File (from a just-completed upload) — using it for a
    // download would hand back a stale/invalid copy if the local file changed
    // or the server copy was updated by Samba/another client. Freshness wins
    // over the round-trip.
    await runDownloadOp(path, name, async (onProgress, signal) => {
      const url = await filesApi.fetchFileBlob(path, onProgress, signal)
      return { url, name }
    })
  }

  async function downloadEntriesAsArchive(paths: string[]): Promise<void> {
    if (paths.length === 0) return
    const label = paths.length === 1
      ? (paths[0].substring(paths[0].lastIndexOf('/') + 1) || paths[0]) + '.zip'
      : `${paths.length} → zip`
    // Dedupe key is order-independent: selecting the same files in a different
    // order is the same archive and must collapse to one in-flight op.
    const key = [...paths].sort().join('\n')
    await runDownloadOp(key, label, async (onProgress, signal) => {
      const res = await filesApi.downloadAsArchive(paths, 'zip', onProgress, signal)
      return { url: res.url, name: res.suggestedName }
    })
  }

  function uploadFiles(fileList: FileList | File[]): FileEntry[] {
    const files = Array.from(fileList)
    if (files.length === 0) return []

    const uploadDir = activeTab.value.currentPath
    const existing = getChildrenOf(uploadDir)
    const relOf = (f: File) =>
      (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
    const isFlat = (f: File) => !relOf(f).includes('/')
    // Same skip-existing rule as before for flat files; folder uploads
    // always proceed (nested paths get created/refreshed server-side).
    // Dedupe by relative path — a duplicate in one batch would corrupt the
    // per-file progress accounting and double the optimistic entries.
    const seenRel = new Set<string>()
    const fresh = files.filter((f) => {
      const rel = relOf(f)
      if (seenRel.has(rel)) return false
      seenRel.add(rel)
      return !isFlat(f) || !existing.some((e) => e.name === f.name)
    })
    if (fresh.length === 0) return []

    // Optimistic entries only for flat files — folder contents appear on the
    // post-upload refresh instead of faking a nested tree.
    const added: FileEntry[] = fresh.filter(isFlat).map((f) => {
      const path = uploadDir + '/' + f.name
      blobUrls.set(path, URL.createObjectURL(f))
      return {
        name: f.name,
        path,
        isDir: false,
        size: f.size,
        modified: new Date(f.lastModified || Date.now()).toISOString(),
      }
    })
    if (added.length > 0) {
      const next = new Map(tree.value)
      next.set(uploadDir, [...existing, ...added])
      tree.value = next
    }

    const opId = generateOpId()
    const totalBytes = fresh.reduce((sum, f) => sum + f.size, 0)
    const sizeByRel = new Map(fresh.map((f) => [relOf(f), f.size]))
    addOperation({
      id: opId,
      type: 'upload',
      status: 'running',
      label: fresh.length === 1 ? relOf(fresh[0]) : `${fresh.length} files`,
      progress: 0,
      currentFile: relOf(fresh[0]),
      totalFiles: fresh.length,
      completedFiles: 0,
      totalBytes,
      loadedBytes: 0,
      startedAt: Date.now(),
    })

    // Fixture/demo mode: no network — simulate an instant success so the
    // optimistic entries stay and the op panel shows a completed upload.
    if (fixtureMode.value) {
      updateOperation(opId, { status: 'done', progress: 100, completedFiles: fresh.length })
      autoRemoveAfter(opId)
      return added
    }

    const abort = new AbortController()
    uploadAborts.add(abort)
    opAborts.set(opId, abort)
    const signal = abort.signal

    beginOperation()
    void (async () => {
      // Per-file loaded map: progress survives chunk retries and duplicate
      // final events without double-counting (keyed by relative path).
      const loadedByRel = new Map<string, number>()
      const result = await safeCall(
        () => filesApi.uploadFiles(uploadDir, fresh, (ev) => {
          loadedByRel.set(ev.file, Math.min(ev.loaded, ev.total))
          let loadedSum = 0
          let completed = 0
          for (const [rel, loaded] of loadedByRel) {
            loadedSum += loaded
            if (loaded >= (sizeByRel.get(rel) ?? Infinity)) completed++
          }
          updateOperation(opId, {
            loadedBytes: Math.min(loadedSum, totalBytes),
            progress: totalBytes > 0 ? Math.round((loadedSum / totalBytes) * 100) : -1,
            currentFile: ev.file,
            completedFiles: completed,
          })
        }, signal),
      )
      endOperation()
      uploadAborts.delete(abort)
      opAborts.delete(opId)

      const canceled = signal.aborted
      const outcomes = Array.isArray(result.data)
        ? (result.data as UploadFileResult[]).filter((o) => typeof o?.ok === 'boolean')
        : []
      const failed = outcomes.filter((o) => !o.ok)
      const okSet = new Set(outcomes.filter((o) => o.ok).map((o) => o.file))

      if (!result.fixture && (result.error || failed.length > 0 || canceled)) {
        // Roll back ONLY the optimistic entries that did not make it —
        // succeeded files stay (the old code wiped the whole batch, erasing
        // files that were already safely on the server).
        const failedAdded = added.filter((a) => !okSet.has(a.name))
        if (failedAdded.length > 0) {
          const m = new Map(tree.value)
          m.set(
            uploadDir,
            getChildrenOf(uploadDir).filter(
              (e) => !failedAdded.some((a) => a.path === e.path),
            ),
          )
          tree.value = m
          for (const a of failedAdded) {
            const url = blobUrls.get(a.path)
            if (url) { URL.revokeObjectURL(url); blobUrls.delete(a.path) }
          }
        }
        updateOperation(opId, {
          status: 'error',
          error: canceled
            ? '__canceled__'
            : (result.error ?? failed.map((f) => `${f.file}: ${f.error ?? 'error'}`).join(' · ')),
          progress: -1,
        })
      } else {
        updateOperation(opId, {
          status: 'done',
          progress: 100,
          completedFiles: fresh.length,
        })
      }
      // Re-sync from the server: folder uploads have no optimistic entries,
      // and partial failures may have left half-finished state. invalidate
      // alone only clears the cache marker — an explicit fetch is what makes
      // the fresh entries actually appear without navigating away.
      invalidatePath(uploadDir)
      void fetchDir(uploadDir)
      autoRemoveAfter(opId)
    })()

    return added
  }

  function renameEntry(oldPath: string, newName: string): boolean {
    const trimmed = newName.trim()
    if (!trimmed) return false

    const parentPath = oldPath.substring(0, oldPath.lastIndexOf('/')) || '/'
    const siblings = getChildrenOf(parentPath)
    const target = siblings.find((e) => e.path === oldPath)
    if (!target) return false
    if (trimmed === target.name) return true
    if (siblings.some((e) => e.name === trimmed)) return false

    const newPath = parentPath + '/' + trimmed
    const next = new Map(tree.value)
    const newSiblings = siblings.map((e) =>
      e.path === oldPath
        ? { ...e, name: trimmed, path: newPath, modified: new Date().toISOString() }
        : e,
    )
    next.set(parentPath, newSiblings)

    if (target.isDir) {
      for (const [key, children] of next.entries()) {
        if (key === oldPath || key.startsWith(oldPath + '/')) {
          const rewrittenKey = newPath + key.substring(oldPath.length)
          const rewrittenChildren = children.map((c) => ({
            ...c,
            path: newPath + c.path.substring(oldPath.length),
          }))
          next.delete(key)
          next.set(rewrittenKey, rewrittenChildren)
        }
      }
    } else {
      const url = blobUrls.get(oldPath)
      if (url) { blobUrls.delete(oldPath); blobUrls.set(newPath, url) }
    }

    tree.value = next

    if (activeTab.value.selected.has(oldPath)) {
      const nextSel = new Set(activeTab.value.selected)
      nextSel.delete(oldPath)
      nextSel.add(newPath)
      activeTab.value.selected = nextSel
      triggerRef(tabs)
    }

    if (!fixtureMode.value) {
      void safeCall(
        () => filesApi.renamePath(oldPath, newPath),
      ).then((result) => {
        if (result.fixture) return
        if (result.error) {
          // Revert the local name change
          const revertNext = new Map(tree.value)
          const currentSiblings = revertNext.get(parentPath)
          if (currentSiblings) {
            const revertedSiblings = currentSiblings.map((e) =>
              e.path === newPath
                ? { ...e, name: oldPath.substring(oldPath.lastIndexOf('/') + 1), path: oldPath }
                : e,
            )
            revertNext.set(parentPath, revertedSiblings)
            if (target.isDir) {
              for (const [key, children] of revertNext.entries()) {
                if (key === newPath || key.startsWith(newPath + '/')) {
                  const revertedKey = oldPath + key.substring(newPath.length)
                  const revertedChildren = children.map((c) => ({
                    ...c,
                    path: oldPath + c.path.substring(newPath.length),
                  }))
                  revertNext.delete(key)
                  revertNext.set(revertedKey, revertedChildren)
                }
              }
            }
            tree.value = revertNext
          }
          const toasts = useInstallProgressStore()
          toasts.pushToast('error', `Rename failed: ${result.error}`)
          return
        }
        invalidatePath(parentPath)
      })
    }
    return true
  }

  function deleteEntries(paths: string[]): number {
    if (paths.length === 0) return 0
    const next = new Map(tree.value)
    let count = 0

    for (const p of paths) {
      const parentPath = p.substring(0, p.lastIndexOf('/')) || '/'
      const siblings = next.get(parentPath)
      if (!siblings) continue
      const entry = siblings.find((e) => e.path === p)
      if (!entry) continue
      next.set(parentPath, siblings.filter((e) => e.path !== p))
      if (entry.isDir) {
        for (const key of [...next.keys()]) {
          if (key === p || key.startsWith(p + '/')) next.delete(key)
        }
        for (const url of [...blobUrls.keys()]) {
          if (url.startsWith(p + '/')) { URL.revokeObjectURL(blobUrls.get(url)!); blobUrls.delete(url) }
        }
      } else {
        const url = blobUrls.get(p)
        if (url) { URL.revokeObjectURL(url); blobUrls.delete(p) }
      }
      count++
    }

    tree.value = next
    clearSelect()

    if (!fixtureMode.value && count > 0) {
      const opId = generateOpId()
      const firstName = paths.length === 1
        ? paths[0].substring(paths[0].lastIndexOf('/') + 1)
        : `${paths.length} files`
      addOperation({
        id: opId,
        type: 'delete',
        status: 'running',
        label: firstName,
        progress: -1,
        totalFiles: paths.length,
        completedFiles: 0,
        startedAt: Date.now(),
      })
      void safeCall(
        () => filesApi.deleteBatch(paths),
      ).then((result) => {
        if (result.error && !result.fixture) {
          updateOperation(opId, { status: 'error', error: result.error, progress: -1 })
          const toasts = useInstallProgressStore()
          toasts.pushToast('error', `Delete failed: ${result.error}`)
        } else {
          updateOperation(opId, { status: 'done', progress: 100, completedFiles: paths.length })
          if (!result.fixture) {
            const parents = new Set<string>()
            for (const p of paths) parents.add(p.substring(0, p.lastIndexOf('/')) || '/')
            for (const parent of parents) invalidatePath(parent)
          }
        }
        autoRemoveAfter(opId)
      })
    }
    return count
  }

  function moveEntries(paths: string[], destDir: string): number {
    let moved = 0
    for (const p of paths) {
      const name = p.substring(p.lastIndexOf('/') + 1)
      const currentParent = p.substring(0, p.lastIndexOf('/'))
      if (currentParent === destDir) continue
      const success = renameEntryFullPath(p, destDir + '/' + name)
      if (success) moved++
    }
    return moved
  }

  function renameEntryFullPath(oldPath: string, newPath: string): boolean {
    const oldParent = oldPath.substring(0, oldPath.lastIndexOf('/')) || '/'
    const newParent = newPath.substring(0, newPath.lastIndexOf('/')) || '/'
    const newName = newPath.substring(newPath.lastIndexOf('/') + 1)

    const oldSiblings = getChildrenOf(oldParent)
    const target = oldSiblings.find((e) => e.path === oldPath)
    if (!target) return false
    const newSiblings = getChildrenOf(newParent)
    if (newSiblings.some((e) => e.name === newName)) return false

    const next = new Map(tree.value)
    next.set(oldParent, oldSiblings.filter((e) => e.path !== oldPath))
    const movedEntry: FileEntry = { ...target, name: newName, path: newPath, modified: new Date().toISOString() }
    next.set(newParent, [...newSiblings, movedEntry])

    if (target.isDir) {
      for (const [key, children] of [...next.entries()]) {
        if (key === oldPath || key.startsWith(oldPath + '/')) {
          const rewrittenKey = newPath + key.substring(oldPath.length)
          const rewrittenChildren = children.map((c) => ({
            ...c, path: newPath + c.path.substring(oldPath.length),
          }))
          next.delete(key)
          next.set(rewrittenKey, rewrittenChildren)
        }
      }
    } else {
      const url = blobUrls.get(oldPath)
      if (url) { blobUrls.delete(oldPath); blobUrls.set(newPath, url) }
    }

    tree.value = next
    return true
  }

  function getBlobUrl(path: string): string | undefined {
    return blobUrls.get(path)
  }

  // Wave 33: clear every session-bound piece of state when the user
  // logs out so the next user logging in on the same browser does NOT
  // see the previous occupant's directory tree, breadcrumb, clipboard,
  // tabs, or in-progress upload list. The blob URLs are the worst leak
  // — without revoking them, every uploaded file's bytes stay pinned
  // in browser memory until the tab is closed.
  function _onAuthLogout() {
    for (const u of blobUrls.values()) {
      try { URL.revokeObjectURL(u) } catch { /* ignore */ }
    }
    blobUrls.clear()
    tree.value = new Map()
    fetched.clear()
    clipboard.value = null
    operations.value = []
    activeOperationCount.value = 0
    fixtureMode.value = false
    // Reset tabs to a single root tab so the breadcrumb / open
    // directory don't leak across users.
    const root = createTab('/')
    tabs.value = [root]
    activeTabId.value = root.id
  }
  window.addEventListener('auth-logout', _onAuthLogout)
  onScopeDispose(() => {
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  return {
    // Shared state
    tree, viewMode, showHidden, activeOperationCount, fixtureMode,
    // Tabs
    tabs, activeTabId, activeTab,
    newTab, closeTab, switchTab, duplicateTab,
    // Backward-compat proxies (read from active tab)
    currentPath, selected, canGoBack, canGoForward,
    // Computed
    visibleEntries, sortedEntries, breadcrumbs,
    // Clipboard (shared across tabs)
    clipboard, clipboardCut, clipboardCopy, clipboardClear, clipboardPaste,
    // Operations
    beginOperation, endOperation, fetchDir, invalidatePath,
    // Tracked operations
    operations, clearCompleted,
    // Navigation (active tab)
    cd, goBack, goForward, goUp,
    // View preferences
    setViewMode, toggleHidden,
    detailsPaneOpen, toggleDetailsPane,
    sortKey, sortDir, setSort,
    // Selection (active tab)
    selectOnly, toggleSelect, selectRange, selectAll, clearSelect,
    // Mutations
    createFolder, uploadFiles, cancelUploads, cancelOperation,
    downloadEntry, downloadEntriesAsArchive,
    renameEntry, deleteEntries, moveEntries,
    // Helpers
    getBlobUrl, getChildrenOf,
    // Lazy init — called when FileBrowserView mounts
    ensureCurrentDir: () => void fetchDir(activeTab.value.currentPath),
  }
})
