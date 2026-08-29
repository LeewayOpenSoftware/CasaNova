<script setup lang="ts">
// AppGrid — the main dashboard content area.
//
// Renders all installed apps as a responsive grid of AppCard tiles,
// AppFolderCard groups, and supports HTML5 drag-to-reorder with
// localStorage persistence via the useAppOrder composable.
//
// Folder grouping: users can create folders via a "+" → "New Folder"
// action, then drag apps into the folder tile. Folders render as a
// compact 2×2 preview tile (collapsed) or an inline section with a
// header row + full-size tiles (expanded). Dropping an app onto
// another standalone app auto-creates a folder containing both.
//
// A trailing "+" tile launches the install-app flow. Empty state shows
// when no apps are installed yet.
//
// Grid responsiveness: auto-fill tracks with a 6rem minimum, so the row
// holds as many 80px tiles as actually fit. The previous 2/4/6 fixed
// breakpoints capped the desktop row at six columns, which left ~70px of
// dead space in every cell on a wide monitor. The tile itself is
// unchanged — the tracks flex around it.

import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import AppCard from './AppCard.vue'
import AppFolderCard from './AppFolderCard.vue'
import InstallAppDialog from './InstallAppDialog.vue'
import { useAppOrder } from '@/composables/useAppOrder'
import { useContainersStore } from '@/stores/containers'
import { useLibraryStore } from '@/stores/library'

const { t } = useI18n()
const {
  orderedApps,
  gridItems,
  moveApp,
  createGroup,
  renameGroup,
  setGroupColor,
  toggleGroup,
  deleteGroup,
  addToGroup,
  removeFromGroup,
  moveInGroup,
  createGroupFromApps,
} = useAppOrder()
const containerStore = useContainersStore()
const libraryStore = useLibraryStore()
const installDialogVisible = ref(false)

// ── Top-level drag state ─────────────────────────────────────────

const draggingId = ref<string | null>(null)
const hoverTargetId = ref<string | null>(null)

function onDragStart(event: DragEvent, id: string) {
  draggingId.value = id
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', id)
  }
}

function onDragOver(event: DragEvent, id: string) {
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  if (id !== draggingId.value) hoverTargetId.value = id
}

function onDragLeave(id: string) {
  if (hoverTargetId.value === id) hoverTargetId.value = null
}

function onDrop(event: DragEvent, targetId: string) {
  event.preventDefault()
  const sourceId = event.dataTransfer?.getData('text/plain') ?? draggingId.value
  if (sourceId && sourceId !== targetId) {
    // Check if both are standalone apps — if so, auto-create a folder.
    const sourceIsApp = gridItems.value.some(
      (item) => item.kind === 'app' && item.app.id === sourceId,
    )
    const targetIsApp = gridItems.value.some(
      (item) => item.kind === 'app' && item.app.id === targetId,
    )
    const targetIsGroup = gridItems.value.some(
      (item) => item.kind === 'group' && item.group.id === targetId,
    )

    if (sourceIsApp && targetIsGroup) {
      // Drop app onto a folder → add to group
      addToGroup(sourceId, targetId)
    } else if (sourceIsApp && targetIsApp) {
      // Drop app onto another app → create folder from both
      createGroupFromApps(sourceId, targetId)
    } else {
      // Reorder at top level
      moveApp(sourceId, targetId)
    }
  }
  draggingId.value = null
  hoverTargetId.value = null
}

function onDragEnd() {
  draggingId.value = null
  hoverTargetId.value = null
}

// ── Folder action handlers ───────────────────────────────────────

function onFolderRename(groupId: string, name: string) {
  renameGroup(groupId, name)
}

function onFolderDelete(groupId: string) {
  deleteGroup(groupId)
}

function onFolderSetColor(groupId: string, color: string) {
  setGroupColor(groupId, color)
}

function onFolderToggle(groupId: string) {
  toggleGroup(groupId)
}

function onFolderAddApp(groupId: string, appId: string) {
  addToGroup(appId, groupId)
}

function onFolderRemoveApp(appId: string) {
  removeFromGroup(appId)
}

function onFolderMoveInGroup(groupId: string, sourceAppId: string, targetAppId: string) {
  moveInGroup(groupId, sourceAppId, targetAppId)
}

// ── New Folder shortcut ──────────────────────────────────────────

function onCreateFolder() {
  createGroup(t('dashboard.folder.new_folder'))
}

// Helper to get the ID for a grid item (app or group).
function itemId(item: (typeof gridItems.value)[number]): string {
  return item.kind === 'app' ? item.app.id : item.group.id
}
</script>

<template>
  <section class="cn-widget p-5">
    <div class="mb-4 flex items-center justify-between">
      <div class="flex items-center gap-2">
        <h3 class="text-sm font-semibold text-[var(--cn-text)]">{{ t('dashboard.apps') }}</h3>
        <button
          type="button"
          class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-blue-500/10 hover:text-blue-500"
          :aria-label="t('dashboard.install_app')"
          :title="t('dashboard.install_app')"
          @click="installDialogVisible = true"
        >
          <AppIcon name="PlusCircle" :size="16" />
        </button>
        <!-- New Folder shortcut -->
        <button
          v-if="orderedApps.length > 0"
          type="button"
          class="flex h-6 w-6 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-blue-500/10 hover:text-blue-500"
          :aria-label="t('dashboard.folder.create')"
          :title="t('dashboard.folder.create')"
          @click="onCreateFolder"
        >
          <AppIcon name="FolderPlus" :size="16" />
        </button>
      </div>
      <span class="text-xs text-[var(--cn-text-muted)]">
        {{ orderedApps.length }} {{ t('dashboard.apps_installed') }}
      </span>
    </div>

    <!-- Loading skeleton -->
    <div
      v-if="containerStore.loading && orderedApps.length === 0"
      class="grid grid-cols-[repeat(auto-fill,minmax(6rem,1fr))] gap-4"
    >
      <div v-for="n in 6" :key="n" class="flex flex-col items-center">
        <div class="h-20 w-20 animate-pulse rounded-2xl bg-[var(--cn-surface-hover)]" />
        <div class="mt-2 h-3 w-14 animate-pulse rounded bg-[var(--cn-surface-hover)]" />
      </div>
    </div>

    <!-- Empty state -->
    <div
      v-else-if="orderedApps.length === 0"
      class="flex flex-col items-center justify-center py-12 text-center"
    >
      <div class="mb-4 flex h-16 w-16 items-center justify-center rounded-2xl bg-blue-500/10 text-blue-500">
        <AppIcon name="Store" :size="32" />
      </div>
      <h4 class="text-base font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.empty_title') }}
      </h4>
      <p class="mt-1 max-w-xs text-sm text-[var(--cn-text-muted)]">
        {{ t('dashboard.empty_subtitle') }}
      </p>
      <button
        type="button"
        class="mt-4 inline-flex items-center gap-2 rounded-lg bg-blue-500 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-blue-600"
        @click="libraryStore.openPanel()"
      >
        <AppIcon name="Store" :size="15" />
        {{ t('dashboard.browse_store') }}
      </button>
    </div>

    <!-- Main grid with folders + apps -->
    <div
      v-else
      class="grid grid-cols-[repeat(auto-fill,minmax(6rem,1fr))] gap-4"
    >
      <template v-for="item in gridItems" :key="itemId(item)">
        <!-- ─── Folder (collapsed or expanded) ─── -->
        <template v-if="item.kind === 'group'">
          <div
            v-if="item.group.collapsed"
            draggable="true"
            :class="[
              'transition-transform',
              draggingId === item.group.id ? 'opacity-40 scale-95' : '',
              hoverTargetId === item.group.id ? 'ring-2 ring-blue-500 rounded-2xl' : '',
            ]"
            @dragstart="onDragStart($event, item.group.id)"
            @dragover="onDragOver($event, item.group.id)"
            @dragleave="onDragLeave(item.group.id)"
            @drop="onDrop($event, item.group.id)"
            @dragend="onDragEnd"
          >
            <AppFolderCard
              :group="item.group"
              :apps="item.apps"
              @toggle="onFolderToggle(item.group.id)"
              @rename="(name) => onFolderRename(item.group.id, name)"
              @delete="onFolderDelete"
              @set-color="onFolderSetColor"
              @add-app="(appId) => onFolderAddApp(item.group.id, appId)"
              @remove-app="onFolderRemoveApp"
              @move-in-group="(src, tgt) => onFolderMoveInGroup(item.group.id, src, tgt)"
            />
          </div>
          <!-- Expanded folder spans the full grid row -->
          <AppFolderCard
            v-else
            :group="item.group"
            :apps="item.apps"
            @toggle="onFolderToggle(item.group.id)"
            @rename="(name) => onFolderRename(item.group.id, name)"
            @delete="onFolderDelete"
            @set-color="onFolderSetColor"
            @add-app="(appId) => onFolderAddApp(item.group.id, appId)"
            @remove-app="onFolderRemoveApp"
            @move-in-group="(src, tgt) => onFolderMoveInGroup(item.group.id, src, tgt)"
          />
        </template>

        <!-- ─── Standalone app tile ─── -->
        <div
          v-else
          draggable="true"
          :class="[
            'transition-transform',
            draggingId === item.app.id ? 'opacity-40 scale-95' : '',
            hoverTargetId === item.app.id ? 'ring-2 ring-blue-500 rounded-2xl' : '',
          ]"
          @dragstart="onDragStart($event, item.app.id)"
          @dragover="onDragOver($event, item.app.id)"
          @dragleave="onDragLeave(item.app.id)"
          @drop="onDrop($event, item.app.id)"
          @dragend="onDragEnd"
        >
          <AppCard :app="item.app" />
        </div>
      </template>

      <!-- Install-new tile -->
      <button
        class="group flex flex-col items-center focus:outline-none"
        :aria-label="t('dashboard.install_app')"
        @click="installDialogVisible = true"
      >
        <div
          class="flex h-20 w-20 items-center justify-center rounded-2xl border-2 border-dashed border-[var(--cn-border)] text-[var(--cn-text-muted)] transition-colors group-hover:border-blue-500 group-hover:text-blue-500"
        >
          <AppIcon name="Plus" :size="28" />
        </div>
        <span class="mt-2 text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('dashboard.install_app') }}
        </span>
      </button>
    </div>

    <InstallAppDialog v-model:visible="installDialogVisible" />
  </section>
</template>
