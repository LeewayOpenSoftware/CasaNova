<script setup lang="ts">
// FilesLayout — the parent view for the file manager. Matches the
// SettingsLayout pattern: left sidebar + right content column, both
// children of AppLayout so the top bar + notification sidebar stay
// persistent.
//
// The sidebar uses the same visual language as SettingsNav (blue
// accent for active entries, section headers for grouping) so users
// moving between Settings and Files feel like they're inside the same
// coherent shell, just with different domain content.

import { ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import FilesSidebar from '@/components/files/FilesSidebar.vue'
import { useFilesStore } from '@/stores/files'

const { t } = useI18n()
const route = useRoute()
const files = useFilesStore()

// Phone only (the toggle and the hiding classes stop at `md`): picking a
// place in the sidebar goes back to the pane showing it.
const placesOpen = ref(false)
watch(() => [files.currentPath, route.path], () => { placesOpen.value = false })
</script>

<template>
  <!-- h-full fills AppLayout's <main> (flex-1 of h-screen), so the file
       pane becomes a fixed-height window whose chrome stays pinned and
       whose content scrolls internally instead of the whole page moving.
       Full width (no max-w) so the pane uses the whole screen laterally —
       the file manager is the one view that wants every pixel. -->
  <div class="flex h-full flex-col p-3">
    <!-- Below `md` the sidebar and the pane are two screens: the pane by
         default, the sidebar behind this toggle. Stacked, the sidebar's
         auto row took the whole height and left the file list none. -->
    <button
      type="button"
      class="mb-3 inline-flex shrink-0 items-center gap-2 self-start rounded-lg px-2 py-1 text-sm text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] md:hidden"
      :aria-expanded="placesOpen"
      @click="placesOpen = !placesOpen"
    >
      <AppIcon :name="placesOpen ? 'ChevronLeft' : 'FolderTree'" :size="16" />
      <span>{{ placesOpen ? t('files.sidebar.back_to_files') : t('files.sidebar.places') }}</span>
    </button>
    <!-- grid-rows force the content row to FILL the flex-1 height (grid
         rows are auto/content-height by default, which would collapse the
         file pane to its chrome). Narrow: one row, sidebar or pane;
         md+: sidebar column + pane, single row fills. -->
    <div class="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,1fr)] gap-4 md:grid-cols-[240px_minmax(0,1fr)]">
      <FilesSidebar :class="placesOpen ? 'min-h-0 overflow-y-auto md:overflow-visible' : 'hidden md:flex'" />
      <main :class="['min-h-0 min-w-0', placesOpen ? 'hidden md:block' : '']">
        <router-view />
      </main>
    </div>
  </div>
</template>
