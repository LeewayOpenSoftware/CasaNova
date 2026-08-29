<script setup lang="ts">
// FileToolbar — the Windows-11-Explorer command bar. One compact,
// icon-forward row: New folder · Cut/Copy/Paste/Rename/Share/Delete
// (enabled by selection) · Sort · Hidden · Upload — then, at the far
// right, recursive search + the Details-pane toggle.
//
// It stays presentation-only: every action is emitted to FileBrowserView,
// which routes them through the same handlers the right-click menu uses.
// Only reads from the store (selection / clipboard / sort / pane state)
// happen here, to drive each button's enabled + active styling.

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import { useFilesStore, type SortKey, type SortDir } from '@/stores/files'

const { t } = useI18n()
const files = useFilesStore()

const emit = defineEmits<{
  (
    e: 'action',
    action:
      | 'new-folder'
      | 'upload'
      | 'upload-folder'
      | 'search'
      | 'cut'
      | 'copy'
      | 'paste'
      | 'rename'
      | 'share'
      | 'delete'
      | 'sort'
      | 'toggle-hidden'
      | 'toggle-details',
    payload?: string,
  ): void
}>()

// Selection-driven enabled states.
const hasSel = computed(() => files.selected.size > 0)
const oneSel = computed(() => files.selected.size === 1)
const canPaste = computed(() => !!files.clipboard)

// Search — debounced so the recursive walk doesn't fire per keystroke.
const searchText = ref('')
let searchDebounce: ReturnType<typeof setTimeout> | null = null
watch(searchText, (v) => {
  if (searchDebounce) clearTimeout(searchDebounce)
  searchDebounce = setTimeout(() => emit('action', 'search', v.trim()), 250)
})

// Sort dropdown — a small self-contained popover (no PrimeVue Menu in the
// bundle). A transparent fixed backdrop closes it on any outside click.
const sortOpen = ref(false)
const SORT_KEYS: { key: SortKey; label: string }[] = [
  { key: 'name', label: 'files.sort.name' },
  { key: 'modified', label: 'files.sort.modified' },
  { key: 'size', label: 'files.sort.size' },
  { key: 'type', label: 'files.sort.type' },
]
const SORT_DIRS: { dir: SortDir; label: string }[] = [
  { dir: 'asc', label: 'files.sort.asc' },
  { dir: 'desc', label: 'files.sort.desc' },
]
function pickSort(value: SortKey | SortDir) {
  emit('action', 'sort', value)
  sortOpen.value = false
}

// Shared button classes. Disabled state uses the native `disabled` attribute
// (see each button) + these `disabled:` variants, so a greyed-out control also
// leaves the tab order and can't be keyboard-activated — not just pointer-none.
const iconBtn =
  'flex h-8 w-8 items-center justify-center rounded-md text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-[var(--cn-text-muted)]'
</script>

<template>
  <div class="flex flex-wrap items-center justify-between gap-2">
    <!-- LEFT: command groups -->
    <div class="flex flex-wrap items-center gap-1">
      <!-- New folder -->
      <button
        type="button"
        class="flex h-8 items-center gap-1.5 rounded-md px-2 text-xs text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
        @click="emit('action', 'new-folder')"
      >
        <AppIcon name="FolderPlus" :size="15" />
        <span>{{ t('files.toolbar.new_folder') }}</span>
      </button>

      <div class="mx-1 h-5 w-px bg-[var(--cn-border)]" />

      <!-- Clipboard / edit group -->
      <button
        type="button"
        :class="iconBtn"
        :disabled="!hasSel"
        :title="t('files.context.cut')"
        :aria-label="t('files.context.cut')"
        @click="emit('action', 'cut')"
      >
        <AppIcon name="Scissors" :size="15" />
      </button>
      <button
        type="button"
        :class="iconBtn"
        :disabled="!hasSel"
        :title="t('files.context.copy')"
        :aria-label="t('files.context.copy')"
        @click="emit('action', 'copy')"
      >
        <AppIcon name="Copy" :size="15" />
      </button>
      <button
        type="button"
        :class="iconBtn"
        :disabled="!canPaste"
        :title="t('files.context.paste')"
        :aria-label="t('files.context.paste')"
        @click="emit('action', 'paste')"
      >
        <AppIcon name="ClipboardPaste" :size="15" />
      </button>
      <button
        type="button"
        :class="iconBtn"
        :disabled="!oneSel"
        :title="t('files.context.rename')"
        :aria-label="t('files.context.rename')"
        @click="emit('action', 'rename')"
      >
        <AppIcon name="Pencil" :size="15" />
      </button>
      <button
        type="button"
        :class="iconBtn"
        :disabled="!oneSel"
        :title="t('files.context.share')"
        :aria-label="t('files.context.share')"
        @click="emit('action', 'share')"
      >
        <AppIcon name="Share2" :size="15" />
      </button>
      <button
        type="button"
        :class="[iconBtn, 'hover:!text-red-500']"
        :disabled="!hasSel"
        :title="t('files.context.delete')"
        :aria-label="t('files.context.delete')"
        @click="emit('action', 'delete')"
      >
        <AppIcon name="Trash2" :size="15" />
      </button>

      <div class="mx-1 h-5 w-px bg-[var(--cn-border)]" />

      <!-- Sort -->
      <div class="relative" @keydown.esc="sortOpen = false">
        <button
          type="button"
          class="flex h-8 items-center gap-1.5 rounded-md px-2 text-xs text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
          :aria-expanded="sortOpen"
          @click="sortOpen = !sortOpen"
        >
          <AppIcon name="ArrowUpDown" :size="15" />
          <span>{{ t('files.sort.label') }}</span>
          <AppIcon name="ChevronDown" :size="12" />
        </button>
        <template v-if="sortOpen">
          <div class="fixed inset-0 z-40" @click="sortOpen = false" />
          <div
            class="absolute left-0 top-9 z-50 w-44 rounded-[var(--cn-radius-sm)] border border-[var(--cn-border)] bg-[var(--cn-surface)] py-1 shadow-lg"
          >
            <button
              v-for="opt in SORT_KEYS"
              :key="opt.key"
              type="button"
              class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-[var(--cn-text)] hover:bg-[var(--cn-surface-hover)]"
              @click="pickSort(opt.key)"
            >
              <AppIcon
                name="Check"
                :size="13"
                :class="files.sortKey === opt.key ? 'text-[var(--cn-accent)]' : 'opacity-0'"
              />
              <span>{{ t(opt.label) }}</span>
            </button>
            <div class="my-1 h-px bg-[var(--cn-border)]" />
            <button
              v-for="opt in SORT_DIRS"
              :key="opt.dir"
              type="button"
              class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-[var(--cn-text)] hover:bg-[var(--cn-surface-hover)]"
              @click="pickSort(opt.dir)"
            >
              <AppIcon
                name="Check"
                :size="13"
                :class="files.sortDir === opt.dir ? 'text-[var(--cn-accent)]' : 'opacity-0'"
              />
              <span>{{ t(opt.label) }}</span>
            </button>
          </div>
        </template>
      </div>

      <!-- Hidden files toggle -->
      <button
        type="button"
        :class="[
          iconBtn,
          files.showHidden && '!text-[var(--cn-accent)] bg-[var(--cn-accent-soft)]',
        ]"
        :title="files.showHidden ? t('files.toolbar.hide_hidden') : t('files.toolbar.show_hidden')"
        :aria-label="files.showHidden ? t('files.toolbar.hide_hidden') : t('files.toolbar.show_hidden')"
        @click="emit('action', 'toggle-hidden')"
      >
        <MorphIcon :name="files.showHidden ? 'Eye' : 'EyeOff'" :size="15" />
      </button>

      <div class="mx-1 h-5 w-px bg-[var(--cn-border)]" />

      <!-- Upload (primary) -->
      <button
        type="button"
        class="flex h-8 items-center gap-1.5 rounded-lg bg-[var(--cn-accent)] px-3 text-xs font-medium text-white transition-colors hover:bg-[var(--cn-accent-hover)]"
        @click="emit('action', 'upload')"
      >
        <AppIcon name="Upload" :size="14" />
        <span>{{ t('files.toolbar.upload') }}</span>
      </button>
      <button
        type="button"
        :class="iconBtn"
        :title="t('files.toolbar.upload_folder')"
        :aria-label="t('files.toolbar.upload_folder')"
        @click="emit('action', 'upload-folder')"
      >
        <AppIcon name="FolderUp" :size="15" />
      </button>
    </div>

    <!-- RIGHT: search + details toggle -->
    <div class="flex items-center gap-2">
      <div class="relative">
        <AppIcon
          name="Search"
          :size="13"
          class="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-[var(--cn-text-muted)]"
        />
        <input
          v-model="searchText"
          type="text"
          :placeholder="t('files.toolbar.search_placeholder')"
          class="h-8 w-44 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] pl-7 pr-2 text-xs text-[var(--cn-text)] focus:border-[var(--cn-accent)] focus:outline-none"
        />
      </div>
      <button
        type="button"
        :class="[
          iconBtn,
          files.detailsPaneOpen && '!text-[var(--cn-accent)] bg-[var(--cn-accent-soft)]',
        ]"
        :aria-pressed="files.detailsPaneOpen"
        :title="files.detailsPaneOpen ? t('files.details.hide') : t('files.details.show')"
        :aria-label="files.detailsPaneOpen ? t('files.details.hide') : t('files.details.show')"
        @click="emit('action', 'toggle-details')"
      >
        <AppIcon name="PanelRight" :size="15" />
      </button>
    </div>
  </div>
</template>
