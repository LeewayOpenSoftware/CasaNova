<script setup lang="ts">
// FileGrid — tile grid with full selection semantics.
//
// Click: plain click selects only the clicked item. Ctrl/Cmd+click
// toggles the item in the current selection. Shift+click selects a
// range from the anchor to the target. Double-click opens folders
// (cd) or opens the preview modal for files (emitted to parent).
//
// Each tile exposes two discoverable affordances that CasaNova's file
// manager ships: a selection checkbox in the top-left (visible on
// hover or when the item is selected) and a kebab button in the
// top-right that opens the same action menu users get via right-
// click. Both are emitted upward so the parent view's ContextMenu
// component handles the actual rendering.
//
// Tile size follows the CasaNova reference — larger icons (80×80) with
// name + size + modified date so the grid feels like a proper file
// manager, not a compact list view.

import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { useFilesStore } from '@/stores/files'
import { filesApi } from '@/api'
import FileThumbnail from './FileThumbnail.vue'
import { observeVisible } from './thumbnailLoader'
import AppIcon from '@/components/ui/AppIcon.vue'
import { formatBytes, type FileEntry } from '@/types/files'

const files = useFilesStore()

// ---- Incremental rendering ----
// The grid used to mount one tile per directory entry up front, so a
// folder with thousands of files spent seconds building a DOM the
// browser then had to lay out and repaint on every hover. We mount the
// first RENDER_STEP tiles and grow by RENDER_STEP each time the sentinel
// at the end of the grid scrolls into view, which keeps the DOM
// proportional to how far the user has actually scrolled.
//
// Deliberately NOT windowed virtualization: nothing here is recycled or
// unmounted, so drag-and-drop, the context menu, hover states and
// scroll position behave exactly as before. Selection is unaffected in
// either direction — selectAll / selectRange and the status-bar counts
// all read the store's entry list, never the rendered rows, so Ctrl+A
// still covers entries that haven't been mounted yet.
const RENDER_STEP = 120
const renderCount = ref(RENDER_STEP)
const renderedEntries = computed(() => files.sortedEntries.slice(0, renderCount.value))
const hasMore = computed(() => renderCount.value < files.sortedEntries.length)

const sentinelRef = ref<HTMLElement | null>(null)
let disarmSentinel: (() => void) | null = null

// The observer is one-shot, so growing the window has to re-arm it:
// appending rows moves the sentinel but doesn't produce a new
// intersection notification if it never left the viewport, and a short
// chunk on a tall screen would otherwise stall half-loaded.
function armSentinel() {
  disarmSentinel?.()
  disarmSentinel = null
  const el = sentinelRef.value
  if (!el) return
  disarmSentinel = observeVisible(el, () => {
    disarmSentinel = null
    renderCount.value += RENDER_STEP
    void nextTick(armSentinel)
  })
}

// A new directory starts the window over; in-place changes to the
// current listing (rename, delete, re-sort) must not, or the user's
// scroll position would collapse back to the top under them.
watch(
  () => [files.currentPath, hasMore.value] as const,
  ([path], [prevPath]) => {
    if (path !== prevPath) renderCount.value = RENDER_STEP
    void nextTick(armSentinel)
  },
)

onMounted(armSentinel)
onBeforeUnmount(() => disarmSentinel?.())

// ---- Drag-and-drop: internal file move ----
const MIME = 'application/x-casanova-files'
const draggingPaths = ref<string[]>([])
const dragOverFolder = ref<string | null>(null)
// Counter per folder path to handle nested dragenter/dragleave
const dragEnterCount = ref(0)

function onDragStart(e: DragEvent, entry: FileEntry) {
  if (!e.dataTransfer) return
  // If dragged item isn't selected, select only it first
  if (!files.selected.has(entry.path)) {
    files.selectOnly(entry.path)
  }
  const paths = [...files.selected]
  draggingPaths.value = paths
  e.dataTransfer.setData(MIME, JSON.stringify(paths))
  e.dataTransfer.setData('text/plain', paths.join('\n'))
  e.dataTransfer.effectAllowed = 'move'
}

function onDragEnd() {
  draggingPaths.value = []
  dragOverFolder.value = null
  dragEnterCount.value = 0
}

function onFolderDragOver(e: DragEvent, entry: FileEntry) {
  if (!entry.isDir) return
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  e.dataTransfer.dropEffect = 'move'
  dragOverFolder.value = entry.path
}

function onFolderDragEnter(e: DragEvent, entry: FileEntry) {
  if (!entry.isDir) return
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  dragEnterCount.value++
  dragOverFolder.value = entry.path
}

function onFolderDragLeave(e: DragEvent, entry: FileEntry) {
  if (!entry.isDir) return
  e.stopPropagation()
  dragEnterCount.value--
  if (dragEnterCount.value <= 0) {
    dragEnterCount.value = 0
    if (dragOverFolder.value === entry.path) {
      dragOverFolder.value = null
    }
  }
}

async function onFolderDrop(e: DragEvent, entry: FileEntry) {
  if (!entry.isDir) return
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  dragOverFolder.value = null
  dragEnterCount.value = 0

  const raw = e.dataTransfer.getData(MIME)
  if (!raw) return
  const paths: string[] = JSON.parse(raw)
  if (paths.length === 0) return

  // Optimistic local move
  files.moveEntries(paths, entry.path)

  // Real backend call when not in fixture mode
  if (!files.fixtureMode) {
    await filesApi.operateBatch({ operation: 'move', sources: paths, destination: entry.path })
  }
}

function isDragged(entry: FileEntry): boolean {
  return draggingPaths.value.includes(entry.path)
}

function isDragOverTarget(entry: FileEntry): boolean {
  return entry.isDir && dragOverFolder.value === entry.path
}

// Thumbnails are now rendered by <FileThumbnail>, which owns its own
// fetch + cache lifecycle per-entry. Previously this component
// returned undefined for every real on-disk file (getBlobUrl only
// matches blobs uploaded *this session*, and entry.url is a fixture-
// only field), so the grid showed the generic FileIcon for every
// image the server actually hosted. The dedicated component calls
// GET /v1/image?path=...&type=thumbnail via the authenticated axios
// client and renders the returned blob.

const emit = defineEmits<{
  (e: 'contextmenu-on-entry', event: MouseEvent, path: string, isDir: boolean): void
  (e: 'open-file', path: string): void
}>()

function onClick(event: MouseEvent, entry: FileEntry) {
  if (event.shiftKey) {
    files.selectRange(entry.path)
  } else if (event.ctrlKey || event.metaKey) {
    files.toggleSelect(entry.path)
  } else {
    files.selectOnly(entry.path)
  }
}

function onDoubleClick(entry: FileEntry) {
  if (entry.isDir) {
    files.cd(entry.path)
  } else {
    files.selectOnly(entry.path)
    emit('open-file', entry.path)
  }
}

function onContextMenu(event: MouseEvent, entry: FileEntry) {
  event.preventDefault()
  emit('contextmenu-on-entry', event, entry.path, entry.isDir)
}

// Kebab click → reuse the same contextmenu-on-entry event the parent
// already handles for right-click. The parent's ContextMenu positions
// itself at the event's mouse coordinates, so the menu appears right
// next to the button users clicked.
function onKebabClick(event: MouseEvent, entry: FileEntry) {
  event.stopPropagation()
  onContextMenu(event, entry)
}

// Checkbox click — plain single-select toggle, independent of the
// regular click handler on the tile body. Uses the store's toggle
// directly so shift / ctrl modifiers on the tile body still work.
function onCheckboxClick(event: MouseEvent, entry: FileEntry) {
  event.stopPropagation()
  files.toggleSelect(entry.path)
}

function isSelected(entry: FileEntry): boolean {
  return files.selected.has(entry.path)
}
</script>

<template>
  <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
    <!-- v0.1 perf: v-memo skips tile vnode patch when only this row's
         state hasn't changed (selection toggle on a different tile,
         clipboard mutation, etc.). See FileList.vue for the same
         pattern. -->
    <div
      v-for="entry in renderedEntries"
      :key="entry.path"
      v-memo="[
        entry.path,
        entry.size,
        entry.modified,
        entry.isDir,
        isSelected(entry),
        isDragOverTarget(entry),
        isDragged(entry),
      ]"
      draggable="true"
      :class="[
        'group relative flex flex-col items-center gap-2 rounded-2xl border p-4 text-center transition-colors',
        isDragOverTarget(entry)
          ? 'border-blue-500 bg-blue-500/10'
          : isSelected(entry)
            ? 'border-blue-500 bg-blue-500/10'
            : 'border-transparent hover:border-[var(--cn-border)] hover:bg-[var(--cn-surface-hover)]',
        isDragged(entry) ? 'opacity-50' : '',
      ]"
      @click="onClick($event, entry)"
      @dblclick="onDoubleClick(entry)"
      @contextmenu="onContextMenu($event, entry)"
      @dragstart="onDragStart($event, entry)"
      @dragend="onDragEnd"
      @dragover="onFolderDragOver($event, entry)"
      @dragenter="onFolderDragEnter($event, entry)"
      @dragleave="onFolderDragLeave($event, entry)"
      @drop="onFolderDrop($event, entry)"
    >
      <!-- Selection checkbox — top-left. Visible on hover or when the
           tile is part of the current selection. Circle matches the
           CasaNova file manager style. -->
      <button
        type="button"
        :class="[
          'absolute left-2 top-2 flex h-5 w-5 items-center justify-center rounded-full border-2 transition-all',
          isSelected(entry)
            ? 'border-blue-500 bg-blue-500 text-white opacity-100'
            : 'border-[var(--cn-border)] bg-[var(--cn-surface)] text-transparent opacity-0 group-hover:opacity-100',
        ]"
        :aria-label="isSelected(entry) ? 'Deselect' : 'Select'"
        @click="onCheckboxClick($event, entry)"
      >
        <AppIcon name="Check" :size="12" />
      </button>

      <!-- Kebab button — top-right. Opens the same action menu the
           right-click context menu uses. -->
      <button
        type="button"
        class="absolute right-2 top-2 flex h-5 w-5 items-center justify-center rounded-full bg-[var(--cn-surface)] text-[var(--cn-text-muted)] opacity-0 shadow-sm transition-opacity hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] group-hover:opacity-100"
        :aria-label="`Actions for ${entry.name}`"
        @click="onKebabClick($event, entry)"
      >
        <AppIcon name="MoreHorizontal" :size="14" />
      </button>

      <!-- Icon / thumbnail — 20×20 (80px). The cn-file-tile class
           carries the tile's own hover surface + transition; the
           per-kind class below paints the resting backdrop so the
           icon reads clearly against busy wallpapers.

           The file branch is deliberately written WITHOUT an opacity
           modifier: Tailwind cannot fold an alpha suffix into an
           arbitrary `var(--…)` colour, so tacking a `/60` onto this
           class emitted no rule at all and the tile rendered with no
           backdrop whatsoever. Folders keep `bg-blue-500/10` because that is a
           palette colour, where the modifier does compile. The plain
           token is already a very low-contrast surface, so dropping the
           /60 restores the intended tint rather than over-darkening it.
           If a lighter wash is ever wanted, add a --cn-*-soft token in
           app.css (the pattern this project uses for severity fills)
           instead of reintroducing the modifier. -->
      <div
        class="cn-file-tile flex h-20 w-20 items-center justify-center overflow-hidden rounded-xl"
        :class="entry.isDir ? 'bg-blue-500/10' : 'bg-[var(--cn-surface-hover)]'"
      >
        <FileThumbnail :entry="entry" :size="40" />
      </div>
      <span class="line-clamp-2 w-full break-all text-sm font-medium text-[var(--cn-text)]">
        {{ entry.name }}
      </span>
      <span class="text-[11px] text-[var(--cn-text-muted)]">
        {{ entry.isDir ? '' : formatBytes(entry.size) }}
      </span>
    </div>

    <!-- Sentinel for incremental rendering. col-span-full + h-px so it
         sits on its own zero-height row and never takes a tile slot. -->
    <div v-if="hasMore" ref="sentinelRef" class="col-span-full h-px" aria-hidden="true" />
  </div>
</template>
