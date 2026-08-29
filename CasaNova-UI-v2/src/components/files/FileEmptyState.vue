<script setup lang="ts">
// FileEmptyState — emits intent events to the parent so the empty
// state CTAs trigger the same dialog / file picker as the toolbar.
// No duplicate handler logic in two places.

import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'

const { t } = useI18n()

const emit = defineEmits<{
  (e: 'upload'): void
  (e: 'new-folder'): void
}>()
</script>

<template>
  <div class="flex flex-col items-center justify-center py-16 text-center">
    <div class="mb-4 flex h-20 w-20 items-center justify-center rounded-full bg-blue-500/10 text-blue-500">
      <AppIcon name="FolderUp" :size="40" />
    </div>
    <h3 class="mb-1 text-lg font-semibold text-[var(--cn-text)]">
      {{ t('files.empty.title') }}
    </h3>
    <p class="mb-6 max-w-sm text-sm text-[var(--cn-text-muted)]">
      {{ t('files.empty.subtitle') }}
    </p>
    <div class="flex gap-2">
      <button
        type="button"
        class="flex h-9 items-center gap-1.5 rounded-lg border border-[var(--cn-border)] px-3 text-sm text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
        @click="emit('new-folder')"
      >
        <AppIcon name="FolderPlus" :size="14" />
        <span>{{ t('files.toolbar.new_folder') }}</span>
      </button>
      <button
        type="button"
        class="flex h-9 items-center gap-1.5 rounded-lg bg-blue-500 px-3 text-sm font-medium text-white transition-colors hover:bg-blue-600"
        @click="emit('upload')"
      >
        <AppIcon name="Upload" :size="14" />
        <span>{{ t('files.toolbar.upload') }}</span>
      </button>
    </div>
  </div>
</template>
