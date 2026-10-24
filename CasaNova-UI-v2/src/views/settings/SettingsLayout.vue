<script setup lang="ts">
// SettingsLayout — the parent view for the entire settings section.
// Vertical-tab rail on the left (SettingsNav), scrollable content
// pane on the right (router-view). This layout itself is a child of
// AppLayout, so the top bar and notification sidebar stay persistent
// above it.
//
// The right pane uses `min-w-0` so long content (URLs, user IDs, SQL
// snippets in the audit log, etc.) doesn't blow out the grid column.
// Without it, CSS grid expands the track to fit the content and the
// nav rail gets squeezed off-screen.

import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import SettingsNav from '@/components/settings/SettingsNav.vue'
import { SETTINGS_INDEX_PATH } from '@/router/settingsIndex'

const { t } = useI18n()
const route = useRoute()
// The bare /app/settings only stays put on a phone (see settingsIndex.ts);
// anywhere else the router has already moved on to a page.
const atIndex = computed(() => route.path.replace(/\/+$/, '') === SETTINGS_INDEX_PATH)
</script>

<template>
  <!-- Same responsive approach as the dashboard: engage the side-by-
       side layout at `sm` (640px) so desktop-ish windows don't fall
       into the mobile single-column path with the nav stretched full-
       width. Column widths step up across sm/md/lg so the nav takes
       less real estate on narrow screens and more on wide ones.

       Below `sm` the two columns become two screens: /app/settings is
       the list (nav only) and every page shows its content with a way
       back to the list. Stacking both put the page under a full-height
       nav, where nobody found it. -->
  <div class="mx-auto max-w-[1400px] p-3 sm:p-4 md:p-6">
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-[190px_minmax(0,1fr)] sm:gap-4 md:grid-cols-[220px_minmax(0,1fr)] md:gap-5 lg:grid-cols-[240px_minmax(0,1fr)] lg:gap-6">
      <div :class="atIndex ? '' : 'hidden sm:block'">
        <SettingsNav />
      </div>
      <main :class="['min-w-0', atIndex ? 'hidden sm:block' : '']">
        <router-link
          to="/app/settings"
          class="mb-3 inline-flex items-center gap-1 rounded-lg px-2 py-1 text-sm text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] sm:hidden"
        >
          <AppIcon name="ChevronLeft" :size="16" />
          <span>{{ t('topbar.settings') }}</span>
        </router-link>
        <router-view />
      </main>
    </div>
  </div>
</template>
