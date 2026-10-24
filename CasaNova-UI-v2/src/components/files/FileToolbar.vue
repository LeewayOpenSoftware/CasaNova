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

import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import { useFilesStore, type SortKey, type SortDir } from '@/stores/files'

const { t } = useI18n()
const files = useFilesStore()

// ui-fm-toolbar-suelo-820: below ~820px of PANEL width (not viewport width —
// this toolbar can sit in a panel narrower than the window) the row used to
// wrap onto a second line. Tailwind's responsive prefixes key off viewport
// width, the wrong axis here, so the toolbar measures its OWN rendered width
// with a ResizeObserver and compacts (drops text labels, then shrinks the
// search box) instead of wrapping. flex-wrap is removed from the template on
// purpose: a wrap is exactly the failure this replaces, so a mis-tuned
// threshold should show as a harmless one-line overflow during a resize
// frame, never as a silent return to two rows.
const rootRef = ref<HTMLElement | null>(null)
const toolbarWidth = ref(0)
let resizeObserver: ResizeObserver | null = null
onMounted(() => {
  if (!rootRef.value) return
  resizeObserver = new ResizeObserver((entries) => {
    toolbarWidth.value = entries[0]?.contentRect.width ?? 0
  })
  resizeObserver.observe(rootRef.value)
})
onBeforeUnmount(() => {
  resizeObserver?.disconnect()
  resizeObserver = null
})

// Two tiers, measured against the real rendered markup (see the card for the
// harness): full labels need 821px; icon-only alone (compact) needs 653px;
// icon-only + Share/Upload-folder folded into "more" + a narrower search box
// (veryCompact) needs 489px. Thresholds sit ABOVE each state's own measured
// need, not at a round number picked by eye — 860 clears full's 821, and
// veryCompact's 670 clears compact's OWN 653: without that second margin,
// a width between compact's true floor (653) and a threshold picked too low
// would fall into neither tier and overflow anyway, the exact bug this
// replaces, just moved to a narrower range instead of removed.
// `> 0` guards the one frame before the observer's first callback fires,
// where an un-measured 0 would otherwise read as "narrower than everything".
const compact = computed(() => toolbarWidth.value > 0 && toolbarWidth.value < 860)
const veryCompact = computed(() => toolbarWidth.value > 0 && toolbarWidth.value < 670)
// Below veryCompact's own floor (489px) nothing left can be folded without
// hiding commands a touch screen has no other way to reach (no right-click),
// and on a phone the pane is ~330px: the row overflowed and Upload sat
// clipped off the right edge. There, and only there, the row wraps — two
// short lines beat commands nobody can see.
const phone = computed(() => toolbarWidth.value > 0 && toolbarWidth.value < 489)

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
// "More" popover — Share + Upload folder, only rendered at veryCompact
// (see the template). Same self-contained-popover shape as sortOpen above.
const moreOpen = ref(false)
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
  <div ref="rootRef" :class="['flex items-center justify-between gap-2', phone && 'flex-wrap gap-x-1 gap-y-1']">
    <!-- LEFT: command groups. On a phone the group dissolves into the root
         (contents) so its buttons and the search share the wrapped lines. -->
    <div :class="['flex items-center gap-1', phone && 'contents']">
      <!-- New folder -->
      <button
        type="button"
        class="flex h-8 items-center gap-1.5 rounded-md px-2 text-xs text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
        :title="compact ? t('files.toolbar.new_folder') : undefined"
        :aria-label="compact ? t('files.toolbar.new_folder') : undefined"
        @click="emit('action', 'new-folder')"
      >
        <AppIcon name="FolderPlus" :size="15" />
        <span v-if="!compact">{{ t('files.toolbar.new_folder') }}</span>
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
      <!-- Folded into the "more" menu at veryCompact — see below the Upload
           group, not removed, just relocated when there is no room. -->
      <button
        v-if="!veryCompact"
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
          :title="compact ? t('files.sort.label') : undefined"
          :aria-label="compact ? t('files.sort.label') : undefined"
          @click="sortOpen = !sortOpen"
        >
          <AppIcon name="ArrowUpDown" :size="15" />
          <span v-if="!compact">{{ t('files.sort.label') }}</span>
          <AppIcon v-if="!compact" name="ChevronDown" :size="12" />
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
        :class="[
          'flex h-8 items-center gap-1.5 rounded-lg bg-[var(--cn-accent)] text-xs font-medium text-white transition-colors hover:bg-[var(--cn-accent-hover)]',
          compact ? 'w-8 justify-center' : 'px-3',
        ]"
        :title="compact ? t('files.toolbar.upload') : undefined"
        :aria-label="compact ? t('files.toolbar.upload') : undefined"
        @click="emit('action', 'upload')"
      >
        <AppIcon name="Upload" :size="14" />
        <span v-if="!compact">{{ t('files.toolbar.upload') }}</span>
      </button>
      <button
        v-if="!veryCompact"
        type="button"
        :class="iconBtn"
        :title="t('files.toolbar.upload_folder')"
        :aria-label="t('files.toolbar.upload_folder')"
        @click="emit('action', 'upload-folder')"
      >
        <AppIcon name="FolderUp" :size="15" />
      </button>

      <!-- "More" — Share + Upload folder, folded here only at veryCompact so
           the row keeps fitting in one line instead of wrapping. Same
           self-contained-popover shape as Sort above (fixed backdrop +
           absolute panel), not a new pattern. -->
      <div v-if="veryCompact" class="relative" @keydown.esc="moreOpen = false">
        <button
          type="button"
          :class="iconBtn"
          :aria-expanded="moreOpen"
          :title="t('files.toolbar.more')"
          :aria-label="t('files.toolbar.more')"
          @click="moreOpen = !moreOpen"
        >
          <AppIcon name="MoreHorizontal" :size="15" />
        </button>
        <template v-if="moreOpen">
          <div class="fixed inset-0 z-40" @click="moreOpen = false" />
          <div
            class="absolute right-0 top-9 z-50 w-40 rounded-[var(--cn-radius-sm)] border border-[var(--cn-border)] bg-[var(--cn-surface)] py-1 shadow-lg"
          >
            <button
              type="button"
              class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-[var(--cn-text)] hover:bg-[var(--cn-surface-hover)] disabled:opacity-40"
              :disabled="!oneSel"
              @click="moreOpen = false; emit('action', 'share')"
            >
              <AppIcon name="Share2" :size="14" />
              <span>{{ t('files.context.share') }}</span>
            </button>
            <button
              type="button"
              class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-[var(--cn-text)] hover:bg-[var(--cn-surface-hover)]"
              @click="moreOpen = false; emit('action', 'upload-folder')"
            >
              <AppIcon name="FolderUp" :size="14" />
              <span>{{ t('files.toolbar.upload_folder') }}</span>
            </button>
          </div>
        </template>
      </div>
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
          :placeholder="veryCompact ? '' : t('files.toolbar.search_placeholder')"
          :title="t('files.toolbar.search_placeholder')"
          :class="[
            'h-8 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] pl-7 pr-2 text-xs text-[var(--cn-text)] focus:border-[var(--cn-accent)] focus:outline-none',
            veryCompact ? 'w-12' : 'w-44',
          ]"
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
