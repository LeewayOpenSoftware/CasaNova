<script setup lang="ts">
// FilePropertiesDialog — right-click "Properties" target. Surfaces the
// metadata we actually have on each entry: name, type, size, full
// path, last-modified time, and — for image files with a resolvable
// URL — the real pixel dimensions read from a transient <img>.
//
// Everything is derived from the store's live tree so rename /
// mutation actions are reflected if the dialog is re-opened, but we
// don't watch the entry reactively while open: properties are a
// point-in-time snapshot, not a live view.

import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import dayjs from 'dayjs'
import FileIcon from './FileIcon.vue'
import { useFilesStore, formatBytes } from '@/stores/files'

const props = defineProps<{
  visible: boolean
  path: string | null
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const files = useFilesStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Resolve the path to a live entry the same way the preview modal does.
const entry = computed(() => {
  if (!props.path) return null
  const parent = props.path.substring(0, props.path.lastIndexOf('/')) || '/'
  return files.getChildrenOf(parent).find((e) => e.path === props.path) ?? null
})

const parentPath = computed(() => {
  if (!props.path) return '/'
  return props.path.substring(0, props.path.lastIndexOf('/')) || '/'
})

// Type string — "Folder" for directories, the uppercased extension for
// files, or "File" when there's no extension.
const typeLabel = computed(() => {
  if (!entry.value) return ''
  if (entry.value.isDir) return t('files.properties.folder')
  const dot = entry.value.name.lastIndexOf('.')
  if (dot <= 0) return t('files.properties.file')
  return entry.value.name.substring(dot + 1).toUpperCase() + ' ' + t('files.properties.file')
})

// For folders, count children (respecting the current hidden-files
// toggle so the number matches what the user sees in the grid).
const folderItemCount = computed(() => {
  if (!entry.value?.isDir) return null
  return files.getChildrenOf(entry.value.path).length
})

// ----- Image dimensions probe -----
// When the entry is an image with a resolvable URL (blob or static),
// load it into a throwaway HTMLImageElement and read natural size.
// Runs whenever the dialog opens onto a new path so stale dimensions
// from a previous entry don't leak through.
const IMAGE_EXT_RE = /\.(jpe?g|png|gif|webp|svg|bmp|avif)$/i
const imageUrl = computed<string | undefined>(() => {
  if (!entry.value || entry.value.isDir) return undefined
  if (!IMAGE_EXT_RE.test(entry.value.name)) return undefined
  return files.getBlobUrl(entry.value.path) ?? entry.value.url
})

const dimensions = ref<{ w: number; h: number } | null>(null)
watch(
  () => [props.visible, imageUrl.value] as const,
  ([open, url]) => {
    dimensions.value = null
    if (!open || !url) return
    const img = new Image()
    img.onload = () => {
      dimensions.value = { w: img.naturalWidth, h: img.naturalHeight }
    }
    img.src = url
  },
  { immediate: true },
)

function onClose() {
  dialogVisible.value = false
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '28rem' }"
    :dismissable-mask="true"
    :header="t('files.properties.title')"
  >
    <div v-if="entry" class="flex flex-col gap-4">
      <!-- Header: icon/thumbnail + name -->
      <div class="flex items-center gap-3">
        <div
          class="flex h-14 w-14 shrink-0 items-center justify-center overflow-hidden rounded-lg"
          :class="entry.isDir ? 'bg-blue-500/10' : 'bg-[var(--cn-surface-hover)]'"
        >
          <img
            v-if="imageUrl"
            :src="imageUrl"
            :alt="entry.name"
            class="h-full w-full object-cover"
          />
          <FileIcon v-else :entry="entry" :size="28" />
        </div>
        <div class="min-w-0 flex-1">
          <div class="truncate text-sm font-semibold text-[var(--cn-text)]">
            {{ entry.name }}
          </div>
          <div class="text-xs text-[var(--cn-text-muted)]">{{ typeLabel }}</div>
        </div>
      </div>

      <!-- Metadata rows -->
      <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
        <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.location') }}</dt>
        <dd class="truncate font-mono text-xs text-[var(--cn-text)]">{{ parentPath }}</dd>

        <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.full_path') }}</dt>
        <dd class="truncate font-mono text-xs text-[var(--cn-text)]">{{ entry.path }}</dd>

        <template v-if="!entry.isDir">
          <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.size') }}</dt>
          <dd class="text-[var(--cn-text)]">
            {{ formatBytes(entry.size) }}
            <span class="text-[var(--cn-text-muted)]">({{ entry.size.toLocaleString() }} B)</span>
          </dd>
        </template>

        <template v-else>
          <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.contents') }}</dt>
          <dd class="text-[var(--cn-text)]">
            {{ t('files.properties.item_count', { n: folderItemCount ?? 0 }, folderItemCount ?? 0) }}
          </dd>
        </template>

        <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.modified') }}</dt>
        <dd class="text-[var(--cn-text)]">
          {{ dayjs(entry.modified).format('MMM D, YYYY HH:mm:ss') }}
        </dd>

        <template v-if="dimensions">
          <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.dimensions') }}</dt>
          <dd class="text-[var(--cn-text)]">
            {{ dimensions.w }} × {{ dimensions.h }} px
          </dd>
        </template>
      </dl>
    </div>

    <template #footer>
      <Button :label="t('common.close')" severity="secondary" @click="onClose" />
    </template>
  </Dialog>
</template>
