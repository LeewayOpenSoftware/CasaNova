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

import FilesSidebar from '@/components/files/FilesSidebar.vue'
</script>

<template>
  <!-- h-full fills AppLayout's <main> (flex-1 of h-screen), so the file
       pane becomes a fixed-height window whose chrome stays pinned and
       whose content scrolls internally instead of the whole page moving.
       Full width (no max-w) so the pane uses the whole screen laterally —
       the file manager is the one view that wants every pixel. -->
  <div class="flex h-full flex-col p-3">
    <!-- grid-rows force the content row to FILL the flex-1 height (grid
         rows are auto/content-height by default, which would collapse the
         file pane to its chrome). Narrow: sidebar auto + pane fills;
         md+: single row fills. -->
    <div class="grid min-h-0 flex-1 grid-cols-1 grid-rows-[auto_minmax(0,1fr)] gap-4 md:grid-cols-[240px_minmax(0,1fr)] md:grid-rows-[minmax(0,1fr)]">
      <FilesSidebar />
      <main class="min-h-0 min-w-0">
        <router-view />
      </main>
    </div>
  </div>
</template>
