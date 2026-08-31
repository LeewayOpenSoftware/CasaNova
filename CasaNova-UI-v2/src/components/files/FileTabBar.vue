<script setup lang="ts">
// FileTabBar — horizontal tab strip above the file browser. Each tab
// represents an independent browsing context with its own path and
// selection. The clipboard is shared so copy in tab 1, paste in tab 2
// works out of the box.
//
// Features:
//   - Click tab to switch
//   - Middle-click or X button to close (min 1 tab)
//   - "+" button to open a new tab at /
//   - Double-click tab label to rename (future)
//   - Drag tabs to reorder (future)

import { ref } from 'vue'
import { useFilesStore } from '@/stores/files'
import { filesApi } from '@/api'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
const files = useFilesStore()

const MIME = 'application/x-casanova-files'
const dragOverTabId = ref<string | null>(null)
const dragEnterCount = ref(0)

function onTabDragOver(e: DragEvent, tabId: string) {
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  e.dataTransfer.dropEffect = 'move'
  dragOverTabId.value = tabId
}

function onTabDragEnter(e: DragEvent, tabId: string) {
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  dragEnterCount.value++
  dragOverTabId.value = tabId
}

function onTabDragLeave(e: DragEvent, tabId: string) {
  e.stopPropagation()
  dragEnterCount.value--
  if (dragEnterCount.value <= 0) {
    dragEnterCount.value = 0
    if (dragOverTabId.value === tabId) {
      dragOverTabId.value = null
    }
  }
}

async function onTabDrop(e: DragEvent, tabId: string) {
  if (!e.dataTransfer?.types.includes(MIME)) return
  e.preventDefault()
  e.stopPropagation()
  dragOverTabId.value = null
  dragEnterCount.value = 0

  const raw = e.dataTransfer.getData(MIME)
  if (!raw) return
  // The payload is whatever the drag source put on the clipboard, so it is
  // untrusted input: a drag from another app (or our own MIME type carrying a
  // half-written payload) throws out of this async handler as an unhandled
  // rejection, and a non-array that happens to parse would sail straight into
  // moveEntries and the batch-move API as `sources`.
  let paths: string[]
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed) || !parsed.every((p): p is string => typeof p === 'string')) return
    paths = parsed
  } catch {
    return
  }
  if (paths.length === 0) return

  const targetTab = files.tabs.find((tab) => tab.id === tabId)
  if (!targetTab) return

  // Optimistic local move
  files.moveEntries(paths, targetTab.currentPath)

  // Real backend call when not in fixture mode
  if (!files.fixtureMode) {
    await filesApi.operateBatch({ operation: 'move', sources: paths, destination: targetTab.currentPath })
  }
}

function onMiddleClick(e: MouseEvent, tabId: string) {
  if (e.button === 1) {
    e.preventDefault()
    files.closeTab(tabId)
  }
}
</script>

<template>
  <div class="flex items-end gap-0.5 overflow-x-auto pb-0">
    <div
      v-for="tab in files.tabs"
      :key="tab.id"
      role="tab"
      tabindex="0"
      :aria-selected="tab.id === files.activeTabId"
      :class="[
        'group relative flex max-w-[200px] min-w-[80px] cursor-pointer items-center gap-1.5 rounded-t-lg px-3 py-1.5 text-xs transition-colors',
        dragOverTabId === tab.id
          ? 'bg-blue-500/10 border-b-2 border-b-blue-500'
          : tab.id === files.activeTabId
            ? 'bg-[var(--cn-surface)] text-[var(--cn-text)] border border-b-0 border-[var(--cn-border)] -mb-px z-10'
            : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
      ]"
      @click="files.switchTab(tab.id)"
      @mousedown="onMiddleClick($event, tab.id)"
      @keydown.enter="files.switchTab(tab.id)"
      @dragover="onTabDragOver($event, tab.id)"
      @dragenter="onTabDragEnter($event, tab.id)"
      @dragleave="onTabDragLeave($event, tab.id)"
      @drop="onTabDrop($event, tab.id)"
    >
      <span class="truncate flex-1 text-left">{{ tab.label }}</span>
      <button
        v-if="files.tabs.length > 1"
        type="button"
        class="ml-1 flex h-4 w-4 shrink-0 items-center justify-center rounded opacity-0 transition-opacity hover:bg-[var(--cn-border)] group-hover:opacity-100"
        :class="{ '!opacity-100': tab.id === files.activeTabId }"
        :aria-label="t('common.close')"
        @click.stop="files.closeTab(tab.id)"
      >
        <AppIcon name="X" :size="10" />
      </button>
    </div>

    <!-- New tab button -->
    <button
      type="button"
      class="flex h-7 w-7 shrink-0 items-center justify-center rounded-t-lg text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
      :aria-label="t('files.tabs.new_tab')"
      @click="files.newTab('/')"
    >
      <AppIcon name="Plus" :size="14" />
    </button>
  </div>
</template>
