<script setup lang="ts">
// FileBreadcrumb — the path display strip above the main file list.
//
// Shows a clickable segment for each component of the current path
// plus back/forward/up arrows. Clicking any segment navigates directly
// there — the quickest way to jump back out of a deeply-nested browse.
// ChevronRight separators between segments are pure chrome and not
// clickable.

import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useFilesStore } from '@/stores/files'

const { t } = useI18n()
const files = useFilesStore()
</script>

<template>
  <div class="flex items-center gap-2">
    <!-- Back / forward / up history buttons. Disabled state renders as
         a muted icon without the hover highlight. -->
    <button
      class="cn-icon-btn h-8 w-8 disabled:cursor-not-allowed disabled:opacity-40"
      :disabled="!files.canGoBack"
      :aria-label="t('files.breadcrumb.back')"
      @click="files.goBack"
    >
      <AppIcon name="ArrowLeft" :size="16" />
    </button>
    <button
      class="cn-icon-btn h-8 w-8 disabled:cursor-not-allowed disabled:opacity-40"
      :disabled="!files.canGoForward"
      :aria-label="t('files.breadcrumb.forward')"
      @click="files.goForward"
    >
      <AppIcon name="ArrowRight" :size="16" />
    </button>
    <button
      class="cn-icon-btn h-8 w-8"
      :aria-label="t('files.breadcrumb.up')"
      @click="files.goUp"
    >
      <AppIcon name="ArrowUp" :size="16" />
    </button>

    <!-- Segment list. The first segment is always clickable even when
         it matches the current path — clicking it re-fires the cd()
         which is idempotent at the root. -->
    <div class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
      <template v-for="(seg, idx) in files.breadcrumbs" :key="seg.path">
        <AppIcon
          v-if="idx > 0"
          name="ChevronRight"
          :size="14"
          class="flex-shrink-0 text-[var(--cn-text-muted)]"
        />
        <button
          class="flex-shrink-0 rounded px-2 py-1 text-sm text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
          :class="{
            'font-semibold': idx === files.breadcrumbs.length - 1,
          }"
          @click="files.cd(seg.path)"
        >
          {{ seg.label }}
        </button>
      </template>
    </div>
  </div>
</template>
