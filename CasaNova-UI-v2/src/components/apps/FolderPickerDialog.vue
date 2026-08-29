<script setup lang="ts">
// FolderPickerDialog — mini file explorer for selecting a host-side
// folder to use as a Docker bind mount source.
//
// Uses the real files API (GET /v1/folder) instead of fixture data.
// Only folders are selectable; files show in the list as dim entries
// for orientation but can't be picked.
//
// Usage:
//   <FolderPickerDialog v-model:visible="open" @select="onPath" />

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { listDir as apiListDir, type ObjResp } from '@/api/files'

const props = defineProps<{
  visible: boolean
  startPath?: string
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'select', path: string): void
}>()

const { t } = useI18n()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

const currentPath = ref('/')
const selectedPath = ref<string | null>(null)
const entries = ref<ObjResp[]>([])
const loading = ref(false)

async function loadDir(path: string) {
  loading.value = true
  try {
    entries.value = await apiListDir(path, false)
  } catch {
    entries.value = []
  }
  loading.value = false
}

watch(
  () => props.visible,
  (v) => {
    if (v) {
      currentPath.value = props.startPath || '/'
      selectedPath.value = null
      loadDir(currentPath.value)
    }
  },
)

watch(currentPath, (p) => {
  loadDir(p)
})

const crumbs = computed(() => {
  const parts = currentPath.value.split('/').filter(Boolean)
  const out: { label: string; path: string }[] = [{ label: '/', path: '/' }]
  let acc = ''
  for (const p of parts) {
    acc += '/' + p
    out.push({ label: p, path: acc })
  }
  return out
})

function goUp() {
  if (currentPath.value === '/') return
  const up = currentPath.value.replace(/\/[^/]+$/, '') || '/'
  currentPath.value = up
  selectedPath.value = null
}

function onRowClick(entry: ObjResp) {
  if (!entry.isDir) return
  if (selectedPath.value === entry.path) {
    currentPath.value = entry.path
    selectedPath.value = null
  } else {
    selectedPath.value = entry.path
  }
}

function confirmSelection() {
  const picked = selectedPath.value ?? currentPath.value
  emit('select', picked)
  dialogVisible.value = false
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '32rem' }"
    :header="t('apps.folder_picker.title')"
    :dismissable-mask="true"
  >
    <!-- Breadcrumb / up-button bar -->
    <div class="mb-3 flex items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2">
      <button
        type="button"
        class="rounded-md p-1 text-blue-500 hover:bg-blue-500/10 disabled:opacity-30"
        :disabled="currentPath === '/'"
        :aria-label="t('apps.folder_picker.up')"
        @click="goUp"
      >
        <AppIcon name="ArrowUp" :size="15" />
      </button>
      <span class="text-[var(--cn-text-muted)]">|</span>
      <div class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto font-mono text-xs">
        <template v-for="(c, i) in crumbs" :key="i">
          <button
            type="button"
            class="shrink-0 text-[var(--cn-text)] hover:text-blue-500"
            @click="currentPath = c.path; selectedPath = null"
          >
            {{ c.label }}
          </button>
          <span v-if="i < crumbs.length - 1" class="shrink-0 text-[var(--cn-text-muted)]">
            /
          </span>
        </template>
      </div>
    </div>

    <!-- Entry list -->
    <div class="max-h-80 overflow-y-auto rounded-lg border border-[var(--cn-border)]">
      <div v-if="loading" class="py-8 text-center text-sm text-[var(--cn-text-muted)]">
        {{ t('common.loading') }}
      </div>
      <div v-else-if="entries.length === 0" class="py-8 text-center text-sm text-[var(--cn-text-muted)]">
        {{ t('apps.folder_picker.empty') }}
      </div>
      <button
        v-for="entry in entries"
        v-else
        :key="entry.path"
        type="button"
        :disabled="!entry.isDir"
        :class="[
          'flex w-full items-center gap-3 border-b border-[var(--cn-border)] px-3 py-2.5 text-left text-sm last:border-b-0 transition-colors',
          entry.isDir
            ? selectedPath === entry.path
              ? 'bg-blue-500/15 text-[var(--cn-text)]'
              : 'hover:bg-[var(--cn-surface-hover)] text-[var(--cn-text)]'
            : 'cursor-not-allowed text-[var(--cn-text-muted)]',
        ]"
        @click="onRowClick(entry)"
      >
        <AppIcon
          :name="entry.isDir ? 'Folder' : 'File'"
          :size="18"
          :class="entry.isDir ? 'text-blue-500' : 'text-[var(--cn-text-muted)]'"
          :fill="entry.isDir ? 'currentColor' : 'none'"
          :fill-opacity="entry.isDir ? 0.15 : 0"
        />
        <span class="flex-1 truncate">{{ entry.name }}</span>
        <AppIcon
          v-if="selectedPath === entry.path"
          name="Check"
          :size="14"
          class="text-blue-500"
        />
      </button>
    </div>

    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        @click="dialogVisible = false"
      />
      <Button :label="t('apps.folder_picker.select')" @click="confirmSelection" />
    </template>
  </Dialog>
</template>
