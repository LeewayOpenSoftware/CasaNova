<script setup lang="ts">
// AppFolderCard — a folder tile on the dashboard grid.
//
// Collapsed state: renders as an 80×80 tile showing a 2×2 mini-grid
// of the first 4 member app icons (or initials) plus the folder name
// underneath. Clicking expands inline.
//
// Expanded state: renders a header row (folder name + collapse button)
// followed by full-size AppCard tiles for every member app. Apps can
// be dragged out of the folder, and external apps can be dragged in.
//
// The folder tile itself is draggable at the top level (reorder among
// other tiles/folders). Inside the expanded view, member tiles are
// reorderable via intra-group drag.

import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import AppCard from './AppCard.vue'
import type { AppFixture, AppGroup } from '@/types/dashboard'

const props = defineProps<{
  group: AppGroup
  apps: AppFixture[]
}>()

const emit = defineEmits<{
  toggle: []
  rename: [id: string]
  delete: [id: string]
  'set-color': [id: string, color: string]
  /** Fired when an app is dragged out of this folder */
  'remove-app': [appId: string]
  /** Fired when an app is dropped into this folder */
  'add-app': [appId: string]
  /** Intra-group reorder */
  'move-in-group': [sourceAppId: string, targetAppId: string]
}>()

const { t } = useI18n()

// Inline rename state
const isRenaming = ref(false)
const renameInput = ref('')

function startRename() {
  renameInput.value = props.group.name
  isRenaming.value = true
}
function confirmRename() {
  const trimmed = renameInput.value.trim()
  if (trimmed && trimmed !== props.group.name) {
    emit('rename', trimmed)
  }
  isRenaming.value = false
}
function cancelRename() {
  isRenaming.value = false
}

// Color picker
const showColorPicker = ref(false)
const folderColors = [
  '#475569', '#7b4397', '#4a5bf7', '#0082c9', '#18bcf2',
  '#609926', '#f60d1a', '#e67e22', '#1abc9c', '#9b59b6',
]

// Mini-grid: first 4 apps for collapsed preview
const previewApps = computed(() => props.apps.slice(0, 4))
const extraCount = computed(() => Math.max(0, props.apps.length - 4))

// Drag state for intra-group reorder
const dragOverAppId = ref<string | null>(null)

function onMemberDragStart(event: DragEvent, appId: string) {
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', appId)
    event.dataTransfer.setData('application/x-folder-source', props.group.id)
  }
}

function onMemberDragOver(event: DragEvent, appId: string) {
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  dragOverAppId.value = appId
}

function onMemberDragLeave(appId: string) {
  if (dragOverAppId.value === appId) dragOverAppId.value = null
}

function onMemberDrop(event: DragEvent, targetAppId: string) {
  event.preventDefault()
  event.stopPropagation()
  dragOverAppId.value = null
  const sourceId = event.dataTransfer?.getData('text/plain')
  const sourceFolder = event.dataTransfer?.getData('application/x-folder-source')
  if (!sourceId) return

  if (sourceFolder === props.group.id) {
    // Intra-group reorder
    emit('move-in-group', sourceId, targetAppId)
  } else {
    // App dragged in from outside
    emit('add-app', sourceId)
  }
}

// Drop on the folder tile itself (collapsed) — add app to group
function onFolderDrop(event: DragEvent) {
  event.preventDefault()
  event.stopPropagation()
  const sourceId = event.dataTransfer?.getData('text/plain')
  if (sourceId) {
    emit('add-app', sourceId)
  }
}

function onFolderDragOver(event: DragEvent) {
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
}

// Running count for the status summary
const runningCount = computed(() => props.apps.filter((a) => a.status === 'running').length)
</script>

<template>
  <!-- ─── Collapsed: compact folder tile ─── -->
  <div v-if="group.collapsed" class="flex flex-col items-center">
    <button
      type="button"
      class="group relative flex flex-col items-center focus:outline-none"
      :aria-label="group.name"
      @click="emit('toggle')"
      @dragover="onFolderDragOver"
      @drop="onFolderDrop"
    >
      <div
        class="flex h-20 w-20 flex-wrap items-center justify-center gap-0.5 rounded-2xl p-1.5 shadow-md transition-all duration-200 ease-out group-hover:-translate-y-1 group-hover:scale-[1.03] group-hover:shadow-[0_12px_30px_rgba(0,0,0,0.4)]"
        :style="{ backgroundColor: group.color || '#475569' }"
      >
        <!-- 2×2 mini-icon grid -->
        <template v-if="previewApps.length > 0">
          <div
            v-for="app in previewApps"
            :key="app.id"
            class="flex h-8 w-8 items-center justify-center overflow-hidden rounded-lg bg-black/20"
          >
            <img
              v-if="app.iconUrl"
              :src="app.iconUrl"
              :alt="app.name"
              class="h-6 w-6 object-contain"
            />
            <span v-else class="text-xs font-semibold text-white/90">{{ app.initial }}</span>
          </div>
        </template>
        <!-- Empty folder -->
        <AppIcon v-else name="Folder" :size="32" class="text-white/70" />
        <!-- +N badge -->
        <span
          v-if="extraCount > 0"
          class="absolute -right-1 -top-1 flex h-5 w-5 items-center justify-center rounded-full bg-blue-500 text-[10px] font-bold text-white shadow"
        >
          +{{ extraCount }}
        </span>
      </div>
    </button>
    <span class="mt-2 max-w-[5.5rem] truncate text-xs font-medium text-[var(--cn-text)]">
      {{ group.name }}
    </span>
    <span class="text-[10px] text-[var(--cn-text-muted)]">
      {{ apps.length }} {{ t('dashboard.folder.apps_count') }}
      <template v-if="runningCount > 0"> · {{ runningCount }} {{ t('dashboard.folder.running') }}</template>
    </span>
  </div>

  <!-- ─── Expanded: header + inline app grid ─── -->
  <div
    v-else
    class="col-span-full rounded-2xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-4"
  >
    <!-- Header row -->
    <div class="mb-3 flex items-center gap-2">
      <AppIcon name="FolderOpen" :size="18" class="text-[var(--cn-text-muted)]" :style="{ color: group.color }" />

      <!-- Editable name -->
      <template v-if="isRenaming">
        <input
          v-model="renameInput"
          class="flex-1 rounded border border-[var(--cn-border)] bg-[var(--cn-surface)] px-2 py-1 text-sm text-[var(--cn-text)] outline-none focus:border-blue-500"
          autofocus
          @keydown.enter="confirmRename"
          @keydown.escape="cancelRename"
          @blur="confirmRename"
        />
      </template>
      <template v-else>
        <h4 class="flex-1 text-sm font-semibold text-[var(--cn-text)]">{{ group.name }}</h4>
      </template>

      <span class="text-xs text-[var(--cn-text-muted)]">
        {{ apps.length }} {{ t('dashboard.folder.apps_count') }}
      </span>

      <!-- Action buttons -->
      <button
        type="button"
        class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        :title="t('dashboard.folder.rename')"
        @click="startRename"
      >
        <AppIcon name="Pencil" :size="13" />
      </button>

      <!-- Color picker trigger -->
      <div class="relative">
        <button
          type="button"
          class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :title="t('dashboard.folder.color')"
          @click="showColorPicker = !showColorPicker"
        >
          <AppIcon name="Palette" :size="13" />
        </button>
        <div
          v-if="showColorPicker"
          class="absolute right-0 top-8 z-20 flex flex-wrap gap-1.5 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] p-2 shadow-lg"
          style="width: 140px"
        >
          <button
            v-for="c in folderColors"
            :key="c"
            type="button"
            class="h-6 w-6 rounded-full border-2 transition-transform hover:scale-110"
            :class="c === group.color ? 'border-white ring-2 ring-blue-500' : 'border-transparent'"
            :style="{ backgroundColor: c }"
            @click="emit('set-color', group.id, c); showColorPicker = false"
          />
        </div>
      </div>

      <button
        type="button"
        class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-red-500/10 hover:text-red-500"
        :title="t('dashboard.folder.delete')"
        @click="emit('delete', group.id)"
      >
        <AppIcon name="Trash2" :size="13" />
      </button>

      <button
        type="button"
        class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        :title="t('dashboard.folder.collapse')"
        @click="emit('toggle')"
      >
        <AppIcon name="ChevronUp" :size="14" />
      </button>
    </div>

    <!-- Member app grid (same column layout as the main grid) -->
    <div
      v-if="apps.length > 0"
      class="grid grid-cols-2 gap-4 sm:grid-cols-4 lg:grid-cols-6"
    >
      <div
        v-for="app in apps"
        :key="app.id"
        draggable="true"
        :class="[
          'transition-transform',
          dragOverAppId === app.id ? 'ring-2 ring-blue-500 rounded-2xl' : '',
        ]"
        @dragstart="onMemberDragStart($event, app.id)"
        @dragover="onMemberDragOver($event, app.id)"
        @dragleave="onMemberDragLeave(app.id)"
        @drop="onMemberDrop($event, app.id)"
      >
        <AppCard :app="app" />
      </div>
    </div>
    <div v-else class="flex flex-col items-center justify-center py-6 text-center">
      <AppIcon name="Folder" :size="28" class="mb-2 text-[var(--cn-text-muted)]" />
      <p class="text-sm text-[var(--cn-text-muted)]">{{ t('dashboard.folder.empty') }}</p>
      <p class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.folder.empty_hint') }}</p>
    </div>
  </div>
</template>
