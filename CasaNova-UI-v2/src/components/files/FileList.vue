<script setup lang="ts">
// FileList — detail table renderer. Same click + selection semantics
// as FileGrid (shift-click range, ctrl-click toggle, right-click
// context menu) so users get consistent behavior across view modes.

import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import { useFilesStore } from '@/stores/files'
import { filesApi } from '@/api'
import FileThumbnail from './FileThumbnail.vue'
import { observeVisible } from './thumbnailLoader'
import { formatBytes, type FileEntry } from '@/types/files'

const files = useFilesStore()
const { t } = useI18n()

// ---- Incremental rendering ----
// Same reasoning as FileGrid: the table used to emit one <tr> per
// directory entry, so a 20 000-file folder built a 20 000-row table on
// the first paint. We mount the first RENDER_STEP rows and grow by
// RENDER_STEP whenever the sentinel row scrolls into view.
//
// Rows are only ever appended, never recycled, so drag-and-drop, the
// context menu and scroll position are untouched. Selection still runs
// off the store's entry list (selectAll / selectRange / status counts),
// so Ctrl+A keeps covering rows that aren't mounted yet.
const RENDER_STEP = 200
const renderCount = ref(RENDER_STEP)
const renderedEntries = computed(() => files.sortedEntries.slice(0, renderCount.value))
const hasMore = computed(() => renderCount.value < files.sortedEntries.length)

const sentinelRef = ref<HTMLElement | null>(null)
let disarmSentinel: (() => void) | null = null

// One-shot observer, so each grown window has to re-arm it — appending
// rows moves the sentinel without producing a new notification when it
// never left the viewport.
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

// Reset the window on a directory change only. Re-sorting or deleting
// inside the current listing keeps it, otherwise the view would jump
// back to the top under the user.
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
const dragEnterCount = ref(0)

function onDragStart(e: DragEvent, entry: FileEntry) {
  if (!e.dataTransfer) return
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

  files.moveEntries(paths, entry.path)

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

// Thumbnails are now handled by <FileThumbnail>, which renders a
// real backend-generated thumbnail via /v1/image?type=thumbnail and
// falls back to FileIcon on non-image or fetch failure.

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

function isSelected(entry: FileEntry): boolean {
  return files.selected.has(entry.path)
}

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, YYYY HH:mm')
}
</script>

<template>
  <div class="overflow-hidden rounded-xl border border-[var(--cn-border)]">
    <table class="w-full text-sm">
      <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
        <tr>
          <th class="px-4 py-2 text-left font-medium">{{ t('files.list.name') }}</th>
          <th class="hidden w-32 px-4 py-2 text-right font-medium sm:table-cell">
            {{ t('files.list.size') }}
          </th>
          <th class="hidden w-48 px-4 py-2 text-right font-medium md:table-cell">
            {{ t('files.list.modified') }}
          </th>
        </tr>
      </thead>
      <tbody class="divide-y divide-[var(--cn-border)]">
        <!-- v0.1 perf: v-memo skips the row's vnode patch unless one
             of the watched dependencies changes. Without this, every
             selection toggle / clipboard change re-rendered all N rows
             in a 5000-file directory. With it, only the rows whose
             selection/drag state actually flipped re-patch. -->
        <tr
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
            'cursor-pointer transition-colors',
            isDragOverTarget(entry)
              ? 'bg-blue-500/15 ring-1 ring-inset ring-blue-500/40'
              : isSelected(entry)
                ? 'bg-blue-500/10'
                : 'hover:bg-[var(--cn-surface-hover)]',
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
          <td class="px-4 py-2">
            <div class="flex items-center gap-3">
              <div class="h-6 w-6 shrink-0 overflow-hidden rounded">
                <FileThumbnail :entry="entry" :size="18" />
              </div>
              <span class="truncate text-[var(--cn-text)]">{{ entry.name }}</span>
            </div>
          </td>
          <td class="hidden px-4 py-2 text-right text-[var(--cn-text-muted)] sm:table-cell">
            {{ entry.isDir ? '—' : formatBytes(entry.size) }}
          </td>
          <td class="hidden px-4 py-2 text-right text-[var(--cn-text-muted)] md:table-cell">
            {{ formatDate(entry.modified) }}
          </td>
        </tr>
      </tbody>
    </table>

    <!-- Sentinel for incremental rendering. Kept outside <tbody> so the
         divide-y rule doesn't draw a stray rule under the last row. -->
    <div v-if="hasMore" ref="sentinelRef" class="h-px w-full" aria-hidden="true" />
  </div>
</template>
