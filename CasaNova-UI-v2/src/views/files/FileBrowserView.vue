<script setup lang="ts">
// FileBrowserView — the main content of the file manager. Composes
// breadcrumb + toolbar + grid/list + empty state, plus every
// dialog / modal / context menu the full-featured file manager needs.
//
// Responsibilities owned here (not in child components):
//   1. Keyboard shortcuts — listening at the window level so Delete,
//      F2, Enter, Backspace, and Ctrl+A fire regardless of which
//      file tile has focus
//   2. Drop zone — drag-and-drop upload handlers on the outer
//      container, with a visible overlay while dragging
//   3. Dialog open/close state — New folder, Rename, Delete,
//      Preview are all controlled from here
//   4. Context menu orchestration — right-click on a file tile
//      shows the menu and sets the target
//
// Children emit intent (`@action="rename"`, `@request-delete"`, etc.)
// and this view turns intent into dialog open / mutator call.

import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useRouter } from 'vue-router'
import { useFilesStore } from '@/stores/files'
import * as filesApi from '@/api/files'
import { safeCall } from '@/api/safeCall'
import FileTabBar from '@/components/files/FileTabBar.vue'
import FileBreadcrumb from '@/components/files/FileBreadcrumb.vue'
import FileToolbar from '@/components/files/FileToolbar.vue'
import PasteConflictDialog from '@/components/files/PasteConflictDialog.vue'
import FileGrid from '@/components/files/FileGrid.vue'
import FileList from '@/components/files/FileList.vue'
import FileEmptyState from '@/components/files/FileEmptyState.vue'
import NewFolderDialog from '@/components/files/NewFolderDialog.vue'
import RenameDialog from '@/components/files/RenameDialog.vue'
import DeleteConfirmDialog from '@/components/files/DeleteConfirmDialog.vue'
import FilePreviewModal from '@/components/files/FilePreviewModal.vue'
import FilePropertiesDialog from '@/components/files/FilePropertiesDialog.vue'
import FileContextMenu from '@/components/files/FileContextMenu.vue'
import FileDetailsPane from '@/components/files/FileDetailsPane.vue'
import ShareLinkDialog from '@/components/files/ShareLinkDialog.vue'
import ShareFolderDialog from '@/components/files/ShareFolderDialog.vue'
import FileOperationProgress from '@/components/files/FileOperationProgress.vue'
import FileHudNotice from '@/components/files/FileHudNotice.vue'

const { t } = useI18n()
const files = useFilesStore()
const router = useRouter()

// Close + minimize both navigate away from the file browser, but
// they communicate different intent. Close is "I'm done here"; the
// files store is unchanged but the user clears it out of their
// current context. Minimize is "I'm stepping away temporarily — keep
// my upload/copy progress alive", which is functionally identical
// today (the store state survives navigation because it's module-
// level) but the topbar Files icon gains a progress dot so the user
// sees their operations are still running. Both simply route back
// to the dashboard; the semantic difference is the dot on the icon.
function closeFiles() {
  router.push('/app')
}
function minimizeFiles() {
  router.push('/app')
}

// ----- Empty state -----
const isEmpty = computed(() => files.visibleEntries.length === 0)

// ----- Dialogs -----
const newFolderDialogVisible = ref(false)
const renameDialogVisible = ref(false)
const deleteDialogVisible = ref(false)
const previewDialogVisible = ref(false)
const propertiesDialogVisible = ref(false)
const shareDialogVisible = ref(false)

// Target path for single-item operations (rename, preview). Delete
// is multi-item so it reads from selection directly.
const operationTargetPath = ref<string | null>(null)

// ----- Context menu -----
const contextMenuRef = ref<InstanceType<typeof FileContextMenu> | null>(null)
const contextTargetIsDir = ref(false)

function onContextMenu(event: MouseEvent, path: string, isDir: boolean) {
  event.preventDefault()
  // Select the right-clicked item if it isn't already selected.
  // Multi-select stays intact if the right-click lands on an already-
  // selected item (matches macOS Finder / Windows Explorer behavior).
  if (!files.selected.has(path)) {
    files.selectOnly(path)
  }
  operationTargetPath.value = path
  contextTargetIsDir.value = isDir
  contextMenuRef.value?.show(event)
}

type ContextAction =
  | 'open'
  | 'preview'
  | 'rename'
  | 'download'
  | 'delete'
  | 'info'
  | 'copy-path'
  | 'share'
  | 'share-link'
  | 'compress'
  | 'extract'
  | 'cut'
  | 'copy'

// Archive extensions the Extract menu item lights up for. Mirrors what
// archiver.Unarchive auto-detects on the backend.
const ARCHIVE_RE = /\.(zip|tar|tar\.gz|tgz|tar\.bz2|tar\.xz|rar|7z)$/i
const contextTargetIsArchive = computed(() =>
  ARCHIVE_RE.test(operationTargetPath.value ?? ''),
)

async function copyPathToClipboard(path: string) {
  try {
    await navigator.clipboard.writeText(path)
  } catch {
    /* clipboard blocked — silent */
  }
}

function onContextAction(action: ContextAction) {
  const target = operationTargetPath.value
  if (!target) return
  switch (action) {
    case 'open':
      files.cd(target)
      break
    case 'preview':
      previewDialogVisible.value = true
      break
    case 'rename':
      renameDialogVisible.value = true
      break
    case 'delete':
      deleteDialogVisible.value = true
      break
    case 'download': {
      // Multi-select or folder → ZIP via the archive download endpoint;
      // a single plain file streams directly (cheaper, resumable).
      const multi = files.selected.has(target) && files.selected.size > 1
      if (multi || contextTargetIsDir.value) {
        void downloadArchive(multi ? [...files.selected] : [target])
      } else {
        void downloadPath(target)
      }
      break
    }
    case 'info':
      propertiesDialogVisible.value = true
      break
    case 'copy-path':
      copyPathToClipboard(target)
      break
    case 'share':
      shareDialogVisible.value = true
      break
    case 'share-link':
      shareLinkDialogVisible.value = true
      break
    case 'compress':
      void compressTarget(target)
      break
    case 'extract':
      void extractTarget(target)
      break
    case 'cut':
      files.clipboardCut([...files.selected])
      break
    case 'copy':
      files.clipboardCopy([...files.selected])
      break
  }
}

// ----- Archive ops -----
// Compress runs over the full selection when the right-clicked entry is
// part of it; otherwise just the entry. The archive lands next to the
// first source as <name>.zip — no dialog for v1; pick-format/destination
// arrives with the batch-queue migration.
const archiveBusy = ref(false)
async function compressTarget(target: string) {
  if (archiveBusy.value) return
  const sources = files.selected.has(target) && files.selected.size > 1
    ? [...files.selected]
    : [target]
  const base = target.substring(target.lastIndexOf('/') + 1).replace(/\.[^.]+$/, '') || 'archive'
  const destDir = target.substring(0, target.lastIndexOf('/')) || '/'
  archiveBusy.value = true
  let useInstallProgressStore: typeof import('@/stores/installProgress').useInstallProgressStore
  try {
    ;({ useInstallProgressStore } = await import('@/stores/installProgress'))
  } catch (err) {
    archiveBusy.value = false
    console.error('FileBrowserView: failed to load install-progress store', err)
    return
  }
  const toasts = useInstallProgressStore()
  const res = await safeCall(() =>
    filesApi.compressFiles(sources, `${destDir}/${base}.zip`, 'zip'),
  )
  archiveBusy.value = false
  if (res.error) {
    toasts.pushToast('error', `Compress failed: ${res.error}`)
    return
  }
  toasts.pushToast('success', t('files.archive.compressed', { name: `${base}.zip` }))
  files.invalidatePath(destDir)
  void files.fetchDir(destDir)
}

async function extractTarget(target: string) {
  if (archiveBusy.value) return
  archiveBusy.value = true
  let useInstallProgressStore: typeof import('@/stores/installProgress').useInstallProgressStore
  try {
    ;({ useInstallProgressStore } = await import('@/stores/installProgress'))
  } catch (err) {
    archiveBusy.value = false
    console.error('FileBrowserView: failed to load install-progress store', err)
    return
  }
  const toasts = useInstallProgressStore()
  const res = await safeCall(() => filesApi.extractArchive(target))
  archiveBusy.value = false
  if (res.error) {
    toasts.pushToast('error', `Extract failed: ${res.error}`)
    return
  }
  toasts.pushToast('success', t('files.archive.extracted', { dest: res.data ?? '' }))
  const parent = target.substring(0, target.lastIndexOf('/')) || '/'
  files.invalidatePath(parent)
  void files.fetchDir(parent)
}

// ----- Share link dialog state -----
const shareLinkDialogVisible = ref(false)

// ----- Multi/folder download as archive -----
// Both download paths now run through the store's operation tracker: one
// panel row with live progress + cancel, dedupe of repeated clicks, and no
// axios timeout — see runDownloadOp in stores/files.ts.
async function downloadArchive(paths: string[]) {
  await files.downloadEntriesAsArchive(paths)
}

function onOpenFile(path: string) {
  operationTargetPath.value = path
  previewDialogVisible.value = true
}

async function downloadPath(path: string) {
  // Wave 34-Y auth rationale still applies (no token in <a href> — see
  // files.ts `downloadFileUrl`); the streaming + anchor mechanics moved to
  // the store so the operation is VISIBLE (progress row + cancel) and
  // repeated clicks dedupe instead of stacking parallel full downloads.
  await files.downloadEntry(path)
}

// ----- Toolbar action bridges -----
// The Win11 command bar surfaces clipboard/edit/view actions that used to
// live only in the right-click menu. They route through the SAME handlers
// as onContextAction — the primary selection stands in for the right-click
// target, so there's no duplicated logic.
type ToolbarAction =
  | 'new-folder'
  | 'upload'
  | 'search'
  | 'cut'
  | 'copy'
  | 'paste'
  | 'rename'
  | 'share'
  | 'delete'
  | 'sort'
  | 'view'
  | 'toggle-hidden'
  | 'toggle-details'
  | 'upload-folder'

function onToolbarAction(action: ToolbarAction, payload?: string) {
  const primary = [...files.selected][0] ?? null
  switch (action) {
    case 'new-folder':
      newFolderDialogVisible.value = true
      break
    case 'upload':
      pickFiles()
      break
    case 'upload-folder':
      pickFolder()
      break
    case 'search':
      void runSearch(payload ?? '')
      break
    case 'cut':
      if (files.selected.size > 0) files.clipboardCut([...files.selected])
      break
    case 'copy':
      if (files.selected.size > 0) files.clipboardCopy([...files.selected])
      break
    case 'paste':
      void requestPaste()
      break
    case 'rename':
      // Single-target ops: require exactly one selection so a keyboard-
      // activated button (which bypasses pointer-events) can't act on an
      // arbitrary member of a multi-selection.
      if (primary && files.selected.size === 1) {
        operationTargetPath.value = primary
        renameDialogVisible.value = true
      }
      break
    case 'share':
      if (primary && files.selected.size === 1) {
        operationTargetPath.value = primary
        shareDialogVisible.value = true
      }
      break
    case 'delete':
      if (files.selected.size > 0) deleteDialogVisible.value = true
      break
    case 'sort':
      if (payload) files.setSort(payload as 'name' | 'modified' | 'size' | 'type' | 'asc' | 'desc')
      break
    case 'view':
      if (payload === 'grid' || payload === 'list') files.setViewMode(payload)
      break
    case 'toggle-hidden':
      files.toggleHidden()
      break
    case 'toggle-details':
      files.toggleDetailsPane()
      break
  }
}

// ----- Recursive search -----
// State lives at the view level so the toolbar can stay stateless; the
// store gets a synthetic "search results" tab rendered in place of the
// normal listing when query is non-empty.
const searchQuery = ref('')
const searchBusy = ref(false)
const searchResults = ref<import('@/api/files').FileSearchHit[]>([])
const searchTruncated = ref(false)
const searchVisited = ref(0)
const searchError = ref<string | null>(null)
// Monotonic guard: only the newest in-flight search may write results, so a
// slow reply for an earlier query can't clobber a faster later one.
let searchSeq = 0

// ----- Paste with conflict detection -----
// requestPaste replaces the bare clipboardPaste call so we can pre-flight
// destination collisions and ask the user before either overwriting or
// silently skipping. Empty clipboard / no collisions → straight through.
const pasteConflictVisible = ref(false)
const pasteCollisions = ref<string[]>([])
const pasteDestination = ref('')

async function requestPaste() {
  if (!files.clipboard) return
  const dest = files.currentPath || '/'
  const destChildren = files.getChildrenOf(dest).map((e) => e.name.toLowerCase())
  const destSet = new Set(destChildren)
  const collisions = files.clipboard.paths
    .map((p) => p.substring(p.lastIndexOf('/') + 1))
    .filter((n) => destSet.has(n.toLowerCase()))
  if (collisions.length === 0) {
    void files.clipboardPaste()
    return
  }
  pasteCollisions.value = collisions
  pasteDestination.value = dest
  pasteConflictVisible.value = true
}

function onPasteChoice(choice: 'overwrite' | 'skip' | 'cancel') {
  if (choice === 'cancel') return
  void files.clipboardPaste(choice === 'skip' ? 'skip' : undefined)
}

async function runSearch(q: string) {
  const mine = ++searchSeq
  searchQuery.value = q
  if (!q) {
    searchResults.value = []
    searchTruncated.value = false
    searchError.value = null
    return
  }
  searchBusy.value = true
  searchError.value = null
  const root = files.currentPath || '/'
  let filesApi: typeof import('@/api').filesApi
  let safeCall: typeof import('@/api/safeCall').safeCall
  try {
    ;({ filesApi } = await import('@/api'))
    ;({ safeCall } = await import('@/api/safeCall'))
  } catch {
    if (mine !== searchSeq) return
    searchBusy.value = false
    searchError.value = 'Failed to load search'
    return
  }
  const res = await safeCall(() => filesApi.fileSearch(root, q, { hidden: files.showHidden }))
  if (mine !== searchSeq) return // a newer search superseded this one
  searchBusy.value = false
  if (res.error) {
    searchError.value = res.error
    return
  }
  if (res.data) {
    searchResults.value = res.data.hits
    searchTruncated.value = res.data.truncated
    searchVisited.value = res.data.visited
  }
}

// ----- Upload: file picker + drop zone -----

const fileInputRef = ref<HTMLInputElement | null>(null)
const folderInputRef = ref<HTMLInputElement | null>(null)

function pickFiles() {
  fileInputRef.value?.click()
}

function pickFolder() {
  folderInputRef.value?.click()
}

function onFileInputChange(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.files) {
    files.uploadFiles(input.files)
    input.value = '' // reset so picking the same file twice re-fires
  }
}

// Drop zone counter pattern handles descendant dragenter fires so the
// overlay doesn't flicker as the cursor moves over child elements.
const dragCounter = ref(0)
const isDragging = computed(() => dragCounter.value > 0)

function onDragEnter(e: DragEvent) {
  // Only handle external file drops, not internal moves
  if (e.dataTransfer?.types.includes('application/x-casanova-files')) return
  if (!e.dataTransfer?.types.includes('Files')) return
  e.preventDefault()
  dragCounter.value++
}

function onDragLeave() {
  if (dragCounter.value > 0) dragCounter.value--
}

function onDragOver(e: DragEvent) {
  if (e.dataTransfer?.types.includes('application/x-casanova-files')) return
  if (e.dataTransfer?.types.includes('Files')) {
    e.preventDefault()
  }
}

function onDrop(e: DragEvent) {
  if (e.dataTransfer?.types.includes('application/x-casanova-files')) return
  e.preventDefault()
  dragCounter.value = 0
  if (e.dataTransfer?.files) {
    files.uploadFiles(e.dataTransfer.files)
  }
}

// ----- Keyboard shortcuts -----
function onKeyDown(e: KeyboardEvent) {
  // Ignore shortcuts while any input or editable is focused — dialogs
  // with text fields need to capture Delete / Enter themselves.
  const target = e.target as HTMLElement
  if (
    target?.tagName === 'INPUT' ||
    target?.tagName === 'TEXTAREA' ||
    target?.isContentEditable
  ) {
    return
  }

  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    files.selectAll()
    return
  }

  // Ctrl+C / Ctrl+X / Ctrl+V — clipboard operations across tabs
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'c' && files.selected.size > 0) {
    e.preventDefault()
    files.clipboardCopy([...files.selected])
    return
  }
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'x' && files.selected.size > 0) {
    e.preventDefault()
    files.clipboardCut([...files.selected])
    return
  }
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'v' && files.clipboard) {
    e.preventDefault()
    void requestPaste()
    return
  }

  // Ctrl+T — new tab
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 't') {
    e.preventDefault()
    files.newTab(files.currentPath)
    return
  }
  // Ctrl+W — close current tab
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'w' && files.tabs.length > 1) {
    e.preventDefault()
    files.closeTab(files.activeTabId)
    return
  }

  if (e.key === 'Backspace') {
    e.preventDefault()
    files.goUp()
    return
  }

  if (e.key === 'Delete' && files.selected.size > 0) {
    e.preventDefault()
    deleteDialogVisible.value = true
    return
  }

  if (e.key === 'F2' && files.selected.size === 1) {
    e.preventDefault()
    const [first] = files.selected
    operationTargetPath.value = first
    renameDialogVisible.value = true
    return
  }

  if (e.key === 'Enter' && files.selected.size === 1) {
    e.preventDefault()
    const [first] = files.selected
    const parent = first.substring(0, first.lastIndexOf('/')) || '/'
    const entry = files.getChildrenOf(parent).find((en) => en.path === first)
    if (!entry) return
    if (entry.isDir) {
      files.cd(entry.path)
    } else {
      operationTargetPath.value = first
      previewDialogVisible.value = true
    }
    return
  }

  if (e.key === 'Escape') {
    files.clearSelect()
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeyDown)
  // Ensure the active tab's directory is fetched — the store no longer
  // eagerly loads '/' on creation so the first mount triggers the fetch.
  files.ensureCurrentDir()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeyDown)
})

const deletePaths = computed(() => [...files.selected])
</script>

<template>
  <!-- Window frame: a real bordered pane so the chrome (tabs, breadcrumb,
       toolbar, status bar) is anchored to the frame edges instead of
       floating over the wallpaper. overflow-hidden + min-h-0 make the
       content row the ONLY scroller; every chrome row is shrink-0. -->
  <div
    class="relative flex h-full min-h-0 flex-col overflow-hidden rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] shadow-lg"
    @dragenter="onDragEnter"
    @dragleave="onDragLeave"
    @dragover="onDragOver"
    @drop="onDrop"
  >
    <!-- Title bar: tabs (fill) + a notification heads-up display in the
         free space + window controls, all on the frame's top edge. -->
    <div class="flex shrink-0 items-stretch justify-between gap-2 border-b border-[var(--cn-border)] pr-2">
      <FileTabBar class="min-w-0 flex-1" />
      <div class="flex shrink-0 items-center gap-2 self-center">
        <FileHudNotice />
        <button
          type="button"
          class="flex h-8 w-8 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :aria-label="t('files.minimize')"
          :title="t('files.minimize')"
          @click="minimizeFiles"
        >
          <AppIcon name="Minus" :size="16" />
        </button>
        <button
          type="button"
          class="flex h-8 w-8 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :aria-label="t('common.close')"
          :title="t('common.close')"
          @click="closeFiles"
        >
          <AppIcon name="X" :size="16" />
        </button>
      </div>
    </div>

    <!-- Breadcrumb row. The toolbar used to share this row; it does not fit
         beside a deep path. The breadcrumb sizes to its content while the
         toolbar only got the leftover width, so FileToolbar's `flex-wrap`
         root broke into three sub-rows inside that gap — and because this
         row is `items-center`, the tall block centred and its first sub-row
         floated ABOVE the breadcrumb. Giving the toolbar its own row below
         costs ~40px of permanent chrome; that is the accepted trade. -->
    <div class="flex shrink-0 items-center gap-3 border-b border-[var(--cn-border)] px-4 py-2">
      <FileBreadcrumb />
    </div>

    <!-- Toolbar row: full width, one line. FileToolbar's own
         `justify-between` root keeps the command groups left and the search
         + details toggle right, so it needs no wrapper here. -->
    <div class="shrink-0 border-b border-[var(--cn-border)] px-4 py-2">
      <FileToolbar @action="onToolbarAction" />
    </div>

    <!-- Content row: file listing on the left, Win11-style Details pane
         docked on the right when toggled on. This is the ONLY scroller —
         the chrome rows above/below stay pinned. -->
    <div class="flex min-h-0 flex-1 gap-3 overflow-hidden px-4 py-3">
      <div class="min-h-0 min-w-0 flex-1 overflow-y-auto" @click.self="files.clearSelect()">
      <!-- Search results overlay. Replaces the normal listing while a
           query is active; cleared by emptying the toolbar input. -->
      <div v-if="searchQuery" class="h-full overflow-auto">
        <div class="flex items-center justify-between border-b border-[var(--cn-border)] px-3 py-2 text-xs text-[var(--cn-text-muted)]">
          <div>
            <span v-if="searchBusy">{{ t('files.search.searching') }}</span>
            <span v-else>
              {{ t('files.search.results_count', { n: searchResults.length }, searchResults.length) }}
              <span v-if="searchTruncated" class="text-amber-500">
                · {{ t('files.search.truncated', { visited: searchVisited }) }}
              </span>
            </span>
          </div>
          <button
            type="button"
            class="text-blue-500 hover:underline"
            @click="runSearch('')"
          >
            {{ t('common.clear') }}
          </button>
        </div>
        <div v-if="searchError" class="p-3 text-xs text-red-500">{{ searchError }}</div>
        <div v-else-if="!searchBusy && searchResults.length === 0" class="p-8 text-center text-xs text-[var(--cn-text-muted)]">
          {{ t('files.search.no_results', { q: searchQuery }) }}
        </div>
        <ul v-else class="flex flex-col">
          <li
            v-for="hit in searchResults"
            :key="hit.path"
            class="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs hover:bg-[var(--cn-surface-hover)]"
            @click="hit.is_dir ? (runSearch(''), files.cd(hit.path)) : onOpenFile(hit.path)"
          >
            <AppIcon
              :name="hit.is_dir ? 'Folder' : 'File'"
              :size="14"
              :class="hit.is_dir ? 'text-amber-500' : 'text-[var(--cn-text-muted)]'"
            />
            <span class="flex-1 truncate font-medium text-[var(--cn-text)]">{{ hit.name }}</span>
            <span class="truncate text-[var(--cn-text-muted)]">{{ hit.path }}</span>
          </li>
        </ul>
      </div>

      <FileEmptyState
        v-else-if="isEmpty"
        @upload="pickFiles"
        @new-folder="newFolderDialogVisible = true"
      />
      <FileGrid
        v-else-if="files.viewMode === 'grid'"
        @contextmenu-on-entry="onContextMenu"
        @open-file="onOpenFile"
      />
      <FileList v-else @contextmenu-on-entry="onContextMenu" @open-file="onOpenFile" />
      </div>

      <!-- Details / preview pane (Win11 Details pane analogue). -->
      <FileDetailsPane v-if="files.detailsPaneOpen" @close="files.toggleDetailsPane()" />
    </div>

    <!-- Status bar — item count on the left, selection count + quick
         view toggles on the right (mirrors the Win11 status bar). -->
    <div
      class="flex shrink-0 items-center justify-between border-t border-[var(--cn-border)] px-4 py-2 text-xs text-[var(--cn-text-muted)]"
    >
      <span>
        {{ t('files.status.items', { n: files.visibleEntries.length }, files.visibleEntries.length) }}
        <template v-if="files.selected.size > 0">
          · {{ t('files.status.selected', { n: files.selected.size }) }}
        </template>
      </span>
      <div class="flex items-center gap-0.5">
        <button
          type="button"
          :class="[
            'flex h-6 w-6 items-center justify-center rounded-md transition-colors',
            files.viewMode === 'grid'
              ? 'bg-[var(--cn-accent-soft)] text-[var(--cn-accent)]'
              : 'hover:bg-[var(--cn-surface-hover)]',
          ]"
          :aria-label="t('files.toolbar.grid_view')"
          :title="t('files.toolbar.grid_view')"
          @click="files.setViewMode('grid')"
        >
          <AppIcon name="LayoutGrid" :size="13" />
        </button>
        <button
          type="button"
          :class="[
            'flex h-6 w-6 items-center justify-center rounded-md transition-colors',
            files.viewMode === 'list'
              ? 'bg-[var(--cn-accent-soft)] text-[var(--cn-accent)]'
              : 'hover:bg-[var(--cn-surface-hover)]',
          ]"
          :aria-label="t('files.toolbar.list_view')"
          :title="t('files.toolbar.list_view')"
          @click="files.setViewMode('list')"
        >
          <AppIcon name="List" :size="13" />
        </button>
      </div>
    </div>

    <!-- Drop zone overlay — shown while a drag is over the browser.
         Pointer-events-none so drag events continue to land on the
         container underneath; visual-only. -->
    <div
      v-if="isDragging"
      class="pointer-events-none absolute inset-0 flex items-center justify-center rounded-xl border-2 border-dashed border-blue-500 bg-blue-500/10"
    >
      <div class="flex flex-col items-center gap-2 text-blue-500">
        <AppIcon name="UploadCloud" :size="48" />
        <p class="text-lg font-semibold">{{ t('files.drop.title') }}</p>
        <p class="text-sm text-[var(--cn-text-muted)]">{{ t('files.drop.subtitle') }}</p>
      </div>
    </div>

    <!-- Hidden file input wired to the toolbar upload button + empty
         state CTA. multiple attribute so users can pick many at once. -->
    <input
      ref="fileInputRef"
      type="file"
      multiple
      class="hidden"
      @change="onFileInputChange"
    />
    <!-- Folder picker: webkitdirectory makes each File carry its
         webkitRelativePath, which the upload path preserves server-side. -->
    <input
      ref="folderInputRef"
      type="file"
      webkitdirectory
      class="hidden"
      @change="onFileInputChange"
    />

    <NewFolderDialog v-model:visible="newFolderDialogVisible" />
    <RenameDialog v-model:visible="renameDialogVisible" :target-path="operationTargetPath" />
    <DeleteConfirmDialog v-model:visible="deleteDialogVisible" :paths="deletePaths" />
    <PasteConflictDialog
      v-model:visible="pasteConflictVisible"
      :collisions="pasteCollisions"
      :destination="pasteDestination"
      @choose="onPasteChoice"
    />
    <FilePreviewModal
      v-model:visible="previewDialogVisible"
      v-model:path="operationTargetPath"
    />
    <FilePropertiesDialog
      v-model:visible="propertiesDialogVisible"
      :path="operationTargetPath"
    />
    <ShareFolderDialog
      v-model:visible="shareDialogVisible"
      :folder-path="operationTargetPath"
    />

    <FileContextMenu
      ref="contextMenuRef"
      :target-is-dir="contextTargetIsDir"
      :target-is-archive="contextTargetIsArchive"
      @action="onContextAction"
    />

    <ShareLinkDialog
      v-model:visible="shareLinkDialogVisible"
      :path="operationTargetPath"
    />

    <FileOperationProgress />
  </div>
</template>
