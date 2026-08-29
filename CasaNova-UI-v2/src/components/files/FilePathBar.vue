<script setup lang="ts">
import MorphIcon from '@/components/ui/MorphIcon.vue'
// FilePathBar — bottom bar inside the file browser that shows the
// current path as a clickable breadcrumb (each segment navigates)
// plus a small capacity readout on the right for the active storage
// root. Inspired by the ZimaOS file manager footer.
//
// The pencil icon flips the breadcrumb into a raw text input so
// power users can paste or type an absolute path and Enter to jump.
// Matches how most file managers let users toggle between "path bar"
// and "edit mode" without leaving the view.

import { ref, computed, watch, nextTick } from 'vue'
import { useFilesStore } from '@/stores/files'

const files = useFilesStore()

// Edit-mode state. When true, the breadcrumb is replaced by a text
// input pre-populated with the current path. Confirming (Enter or
// check button) calls the store's cd() and flips back to breadcrumb.
const editing = ref(false)
const draft = ref('')
const inputRef = ref<HTMLInputElement | null>(null)

function enterEdit() {
  draft.value = files.currentPath
  editing.value = true
  nextTick(() => inputRef.value?.focus())
}
function cancelEdit() {
  editing.value = false
}
function confirmEdit() {
  const target = draft.value.trim()
  if (target && target !== files.currentPath) {
    files.cd(target)
  }
  editing.value = false
}

// Reset edit mode whenever the path changes externally (e.g. via the
// sidebar). Avoids stale draft values.
watch(
  () => files.currentPath,
  () => {
    editing.value = false
  },
)

// Fixed capacity fixture for the active storage root. Real backend
// wiring reads this from GET /v2/local_storage/storage for the
// current path's mount point. For the dev scaffold we hard-code the
// /DATA root numbers that match the dashboard StorageWidget.
const capacity = computed(() => {
  return {
    usedBytes: 1.27 * 1024 ** 4, // 1.27 TB
    totalBytes: 6.28 * 1024 ** 4, // 6.28 TB
  }
})

function formatTb(bytes: number): string {
  const tb = bytes / 1024 ** 4
  return tb.toFixed(2) + ' TB'
}

const freeBytes = computed(() => capacity.value.totalBytes - capacity.value.usedBytes)
const usedRatio = computed(() => capacity.value.usedBytes / capacity.value.totalBytes)

// Build crumbs from the current path. The root crumb is always
// present so clicking it returns to the top of the current storage.
const crumbs = computed(() => {
  const parts = files.currentPath.split('/').filter(Boolean)
  const out: { label: string; path: string }[] = []
  let running = ''
  for (const p of parts) {
    running += '/' + p
    out.push({ label: p, path: running })
  }
  return out
})
</script>

<template>
  <div class="flex items-center justify-between gap-3 rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] px-3 py-2">
    <!-- Left: pencil edit button + breadcrumb or input. -->
    <div class="flex min-w-0 flex-1 items-center gap-2">
      <button
        type="button"
        class="flex h-7 w-7 shrink-0 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        :aria-label="editing ? 'Confirm' : 'Edit path'"
        @click="editing ? confirmEdit() : enterEdit()"
      >
        <MorphIcon :name="editing ? 'Check' : 'Pencil'" :size="13" />
      </button>

      <!-- Text input (edit mode) -->
      <input
        v-if="editing"
        ref="inputRef"
        v-model="draft"
        type="text"
        class="w-full rounded-md border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-1 font-mono text-xs text-[var(--cn-text)] focus:outline-none focus:ring-2 focus:ring-blue-500/40"
        @keyup.enter="confirmEdit"
        @keyup.escape="cancelEdit"
      />

      <!-- Breadcrumb segments (default) -->
      <nav v-else class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto font-mono text-xs">
        <template v-for="(c, i) in crumbs" :key="c.path">
          <button
            type="button"
            class="shrink-0 rounded px-1 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            @click="files.cd(c.path)"
          >
            {{ c.label }}
          </button>
          <span v-if="i < crumbs.length - 1" class="shrink-0 text-[var(--cn-text-muted)]">/</span>
        </template>
      </nav>
    </div>

    <!-- Right: capacity indicator. Mini bar + "X.XX TB Available / Y.YY TB" -->
    <div class="flex shrink-0 items-center gap-2 text-xs">
      <div class="flex h-1.5 w-16 overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
        <div
          class="h-full rounded-full bg-blue-500 transition-all"
          :style="{ width: Math.round(usedRatio * 100) + '%' }"
        />
      </div>
      <span class="font-mono text-[var(--cn-text-muted)]">
        <span class="text-[var(--cn-text)]">{{ formatTb(freeBytes) }}</span>
        {{ ' Available / ' + formatTb(capacity.totalBytes) }}
      </span>
    </div>
  </div>
</template>
