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

import SettingsNav from '@/components/settings/SettingsNav.vue'
</script>

<template>
  <!-- Same responsive approach as the dashboard: engage the side-by-
       side layout at `sm` (640px) so desktop-ish windows don't fall
       into the mobile single-column path with the nav stretched full-
       width. Column widths step up across sm/md/lg so the nav takes
       less real estate on narrow screens and more on wide ones. -->
  <div class="mx-auto max-w-[1400px] p-3 sm:p-4 md:p-6">
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-[190px_minmax(0,1fr)] sm:gap-4 md:grid-cols-[220px_minmax(0,1fr)] md:gap-5 lg:grid-cols-[240px_minmax(0,1fr)] lg:gap-6">
      <SettingsNav />
      <main class="min-w-0">
        <router-view />
      </main>
    </div>
  </div>
</template>
