<script setup lang="ts">
// FileDetailsPane — persistent, docked right-hand Details+Preview pane,
// the Windows-11-Explorer "Details pane" analogue. Three states keyed on
// the current selection:
//   0 selected  → empty hint
//   1 selected  → large preview (image/video/audio/pdf/text/icon) + the
//                 same metadata rows FilePropertiesDialog shows
//   N selected  → count + combined size
//
// Reuses the preview logic from FilePreviewModal (kind classifier +
// blob-URL fetch/revoke lifecycle, re-gated on the selected `path`
// instead of a `visible` prop) and the metadata/dimensions derivations
// from FilePropertiesDialog. It is store-direct (no props) to match the
// sibling convention; emits 'close' for its X button.
//
// Deliberately NO global keydown listener (the grid owns selection keys
// and FilePreviewModal owns carousel keys — a third would fight them)
// and NO inline text editing (that stays in FilePreviewModal).

import { computed, ref, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import AppIcon from '@/components/ui/AppIcon.vue'
import FileIcon from './FileIcon.vue'
import { useFilesStore, formatBytes } from '@/stores/files'
import { filesApi } from '@/api'
import type { FileEntry } from '@/types/files'

const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()
const files = useFilesStore()

// Set on unmount. Async preview fetches may resolve AFTER the pane is closed
// (v-if unmount) — the path guard alone stays true because the selection is
// unchanged, so without this flag a late fetch would assign an object URL to a
// dead component that nothing revokes. Every continuation checks it.
let disposed = false

// ----- Selection resolution -----
// Selected paths are always children of the active tab's currentPath
// (selection operates on visibleEntries), so one lookup resolves them.
const selEntries = computed<FileEntry[]>(() => {
  const sel = files.selected
  if (sel.size === 0) return []
  return files.getChildrenOf(files.currentPath).filter((e) => sel.has(e.path))
})
const count = computed(() => selEntries.value.length)
const entry = computed<FileEntry | null>(() =>
  count.value === 1 ? selEntries.value[0] : null,
)
const path = computed<string | null>(() => entry.value?.path ?? null)
const parentPath = computed(() =>
  path.value ? path.value.substring(0, path.value.lastIndexOf('/')) || '/' : '/',
)
const multiSize = computed(() =>
  selEntries.value.filter((e) => !e.isDir).reduce((s, e) => s + (e.size || 0), 0),
)

// ----- Metadata derivations (ported from FilePropertiesDialog) -----
const typeLabel = computed(() => {
  const e = entry.value
  if (!e) return ''
  if (e.isDir) return t('files.properties.folder')
  const dot = e.name.lastIndexOf('.')
  if (dot <= 0) return t('files.properties.file')
  return e.name.substring(dot + 1).toUpperCase() + ' ' + t('files.properties.file')
})
// null when: not a dir, OR a dir whose children were never fetched into the
// store tree (browsing past a folder doesn't load its contents). Returning 0
// there would render a misleading "0 items" for a full folder, indistinguishable
// from a genuinely empty one — the template shows "—" instead.
const folderItemCount = computed(() => {
  const e = entry.value
  if (!e?.isDir) return null
  return files.tree.has(e.path) ? files.getChildrenOf(e.path).length : null
})

// ----- Content kind (ported from FilePreviewModal) -----
const kind = computed<'image' | 'text' | 'video' | 'audio' | 'pdf' | 'dir' | 'other'>(() => {
  const e = entry.value
  if (!e) return 'other'
  if (e.isDir) return 'dir'
  const name = e.name.toLowerCase()
  if (/\.(jpg|jpeg|png|gif|webp|svg|bmp|avif|heic)$/.test(name)) return 'image'
  if (/\.(mp4|webm|mov|m4v|ogv)$/.test(name)) return 'video'
  if (/\.(mp3|flac|wav|ogg|m4a|opus|aac)$/.test(name)) return 'audio'
  if (/\.pdf$/.test(name)) return 'pdf'
  if (/\.(txt|md|log|json|yaml|yml|xml|csv|js|ts|vue|go|py|rs|sh|conf|ini)$/.test(name))
    return 'text'
  return 'other'
})

// ----- Image preview + dimensions -----
// previewUrl resolves: store blob (just-uploaded) → static fixture url →
// async-fetched original. The dimensions probe reads natural size off a
// throwaway <img> once a URL is available.
const fetchedImageUrl = ref<string | undefined>(undefined)
const fetchingImage = ref(false)
const imageError = ref<string | null>(null)
const previewUrl = computed<string | undefined>(() => {
  if (!path.value) return undefined
  return files.getBlobUrl(path.value) ?? fetchedImageUrl.value ?? entry.value?.url
})
const dimensions = ref<{ w: number; h: number } | null>(null)

watch(
  () => [path.value, kind.value] as const,
  async ([p, k]) => {
    // Release the previous fetched image before loading a new target.
    if (fetchedImageUrl.value) {
      URL.revokeObjectURL(fetchedImageUrl.value)
      fetchedImageUrl.value = undefined
    }
    imageError.value = null
    fetchingImage.value = false
    dimensions.value = null
    if (!p || k !== 'image') return
    // A store blob or a static fixture URL needs no fetch.
    if (files.getBlobUrl(p) || entry.value?.url) return
    fetchingImage.value = true
    try {
      const requested = p
      const url = await filesApi.fetchImageOriginal(p)
      if (disposed || requested !== path.value) URL.revokeObjectURL(url)
      else fetchedImageUrl.value = url
    } catch (err) {
      imageError.value = err instanceof Error ? err.message : 'load failed'
    } finally {
      if (p === path.value) fetchingImage.value = false
    }
  },
  { immediate: true },
)

// Probe natural dimensions whenever a preview URL becomes available.
watch(
  () => previewUrl.value,
  (url) => {
    dimensions.value = null
    if (!url || kind.value !== 'image') return
    const img = new Image()
    img.onload = () => {
      if (previewUrl.value === url) dimensions.value = { w: img.naturalWidth, h: img.naturalHeight }
    }
    img.src = url
  },
  { immediate: true },
)

// ----- Video / audio / pdf streaming (ported from FilePreviewModal) -----
const mediaUrl = ref<string | undefined>(undefined)
const mediaLoading = ref(false)
const mediaError = ref<string | null>(null)

watch(
  () => [path.value, kind.value] as const,
  async ([p, k]) => {
    if (mediaUrl.value) {
      URL.revokeObjectURL(mediaUrl.value)
      mediaUrl.value = undefined
    }
    mediaError.value = null
    mediaLoading.value = false
    if (!p || (k !== 'video' && k !== 'audio' && k !== 'pdf')) return
    mediaLoading.value = true
    try {
      const requested = p
      const url = await filesApi.fetchFileBlob(p)
      if (disposed || requested !== path.value) URL.revokeObjectURL(url)
      else mediaUrl.value = url
    } catch (err) {
      mediaError.value = err instanceof Error ? err.message : 'load failed'
    } finally {
      if (p === path.value) mediaLoading.value = false
    }
  },
  { immediate: true },
)

// ----- Text preview (read-only) -----
const textContent = ref<string | null>(null)
const textError = ref<string | null>(null)
watch(
  () => [path.value, kind.value] as const,
  async ([p, k]) => {
    textContent.value = null
    textError.value = null
    if (!p || k !== 'text') return
    try {
      const requested = p
      const blob = files.getBlobUrl(p)
      const content = blob ? await (await fetch(blob)).text() : await filesApi.readFileContent(p)
      if (requested === path.value) textContent.value = content
    } catch (err) {
      textError.value = err instanceof Error ? err.message : 'read failed'
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  disposed = true
  if (fetchedImageUrl.value) URL.revokeObjectURL(fetchedImageUrl.value)
  if (mediaUrl.value) URL.revokeObjectURL(mediaUrl.value)
})
</script>

<template>
  <aside
    class="hidden h-full w-full shrink-0 flex-col border-l border-[var(--cn-border)] bg-[var(--cn-surface)] md:flex md:w-[300px]"
  >
    <!-- Header -->
    <div class="flex items-center justify-between border-b border-[var(--cn-border)] px-3 py-2">
      <span class="text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
        {{ t('files.details.title') }}
      </span>
      <button
        type="button"
        class="flex h-6 w-6 items-center justify-center rounded-md text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        :aria-label="t('files.details.hide')"
        :title="t('files.details.hide')"
        @click="emit('close')"
      >
        <AppIcon name="X" :size="14" />
      </button>
    </div>

    <!-- 0 selected -->
    <div
      v-if="count === 0"
      class="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center"
    >
      <AppIcon name="PanelRight" :size="32" class="text-[var(--cn-text-muted)] opacity-50" />
      <p class="text-xs text-[var(--cn-text-muted)]">{{ t('files.details.empty') }}</p>
    </div>

    <!-- N selected -->
    <div
      v-else-if="count > 1"
      class="flex flex-1 flex-col items-center justify-center gap-3 px-6 text-center"
    >
      <AppIcon name="Files" :size="40" class="text-[var(--cn-text-muted)]" />
      <p class="text-sm font-medium text-[var(--cn-text)]">
        {{ t('files.details.multi', { n: count }, count) }}
      </p>
      <p v-if="multiSize > 0" class="text-xs text-[var(--cn-text-muted)]">
        {{ formatBytes(multiSize) }}
      </p>
    </div>

    <!-- 1 selected -->
    <div v-else-if="entry" class="flex min-h-0 flex-1 flex-col">
      <!-- Preview chamber -->
      <div
        class="m-3 flex min-h-[180px] flex-1 items-center justify-center overflow-hidden rounded-[var(--cn-radius)] bg-[var(--cn-bg)]"
      >
        <!-- image -->
        <template v-if="kind === 'image'">
          <img
            v-if="previewUrl"
            :src="previewUrl"
            :alt="entry.name"
            class="max-h-full max-w-full object-contain"
          />
          <div v-else-if="fetchingImage" class="flex flex-col items-center gap-2">
            <AppIcon name="Loader2" :size="28" class="animate-spin text-[var(--cn-text-muted)]" />
          </div>
          <div v-else class="flex flex-col items-center gap-2 p-4 text-center">
            <AppIcon name="FileQuestion" :size="40" class="text-[var(--cn-text-muted)]" />
            <p v-if="imageError" class="text-xs text-red-500">{{ imageError }}</p>
          </div>
        </template>

        <!-- video -->
        <template v-else-if="kind === 'video'">
          <div v-if="mediaLoading" class="flex flex-col items-center gap-2 text-[var(--cn-text-muted)]">
            <AppIcon name="Loader2" :size="28" class="animate-spin" />
          </div>
          <video
            v-else-if="mediaUrl"
            :src="mediaUrl"
            controls
            playsinline
            class="max-h-full max-w-full rounded-md bg-black"
          />
          <p v-else-if="mediaError" class="p-4 text-xs text-red-500">{{ mediaError }}</p>
        </template>

        <!-- audio -->
        <template v-else-if="kind === 'audio'">
          <div class="flex w-full flex-col items-center gap-3 p-4">
            <AppIcon name="Music" :size="40" class="text-[var(--cn-text-muted)]" />
            <div v-if="mediaLoading" class="text-xs text-[var(--cn-text-muted)]">…</div>
            <audio v-else-if="mediaUrl" :src="mediaUrl" controls class="w-full" />
            <p v-else-if="mediaError" class="text-xs text-red-500">{{ mediaError }}</p>
          </div>
        </template>

        <!-- pdf -->
        <template v-else-if="kind === 'pdf'">
          <div v-if="mediaLoading" class="flex flex-col items-center gap-2 text-[var(--cn-text-muted)]">
            <AppIcon name="Loader2" :size="28" class="animate-spin" />
          </div>
          <iframe
            v-else-if="mediaUrl"
            :src="mediaUrl"
            class="h-full w-full bg-white"
            :title="entry.name"
          />
          <p v-else-if="mediaError" class="p-4 text-xs text-red-500">{{ mediaError }}</p>
        </template>

        <!-- text -->
        <template v-else-if="kind === 'text'">
          <div v-if="textContent !== null" class="h-full w-full overflow-auto">
            <pre class="whitespace-pre-wrap break-words p-3 font-mono text-[11px] leading-relaxed text-[var(--cn-text)]">{{ textContent }}</pre>
          </div>
          <p v-else-if="textError" class="p-4 text-xs text-red-500">{{ textError }}</p>
          <AppIcon v-else name="Loader2" :size="24" class="animate-spin text-[var(--cn-text-muted)]" />
        </template>

        <!-- dir / other -->
        <FileIcon v-else :entry="entry" :size="64" />
      </div>

      <!-- Filename + metadata (scrolls if long) -->
      <div class="min-h-0 overflow-auto px-4 pb-4">
        <div class="truncate text-sm font-semibold text-[var(--cn-text)]" :title="entry.name">
          {{ entry.name }}
        </div>
        <div class="mb-3 text-xs text-[var(--cn-text-muted)]">{{ typeLabel }}</div>

        <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 text-xs">
          <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.location') }}</dt>
          <dd class="truncate font-mono text-[var(--cn-text)]" :title="parentPath">{{ parentPath }}</dd>

          <template v-if="!entry.isDir">
            <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.size') }}</dt>
            <dd class="text-[var(--cn-text)]">{{ formatBytes(entry.size) }}</dd>
          </template>
          <template v-else>
            <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.contents') }}</dt>
            <dd class="text-[var(--cn-text)]">
              <template v-if="folderItemCount === null">—</template>
              <template v-else>{{ t('files.properties.item_count', { n: folderItemCount }, folderItemCount) }}</template>
            </dd>
          </template>

          <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.modified') }}</dt>
          <dd class="text-[var(--cn-text)]">{{ dayjs(entry.modified).format('MMM D, YYYY HH:mm') }}</dd>

          <template v-if="dimensions">
            <dt class="text-[var(--cn-text-muted)]">{{ t('files.properties.dimensions') }}</dt>
            <dd class="text-[var(--cn-text)]">{{ dimensions.w }} × {{ dimensions.h }} px</dd>
          </template>
        </dl>
      </div>
    </div>
  </aside>
</template>
