<script setup lang="ts">
// FilePreviewModal — full-screen-ish preview for the selected file.
// Supports three content modes:
//   1. Image — <img> with the blob URL from the store (if uploaded)
//              or a friendly "no preview" illustration for fixture files
//   2. Text  — reads the text content via FileReader when the file has
//              a blob URL; fixture files show a "content not available"
//              message
//   3. Other — generic "no preview available" with a download affordance
//
// The modal is opened from the context menu's "preview" action, from
// a double-click on a file in the grid/list, and from the Enter key
// when a single file is selected.

import { computed, ref, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import FileIcon from './FileIcon.vue'
import { useFilesStore, formatBytes } from '@/stores/files'
import { filesApi } from '@/api'
import dayjs from 'dayjs'

const props = defineProps<{
  visible: boolean
  path: string | null
}>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'update:path', v: string): void
}>()

const { t } = useI18n()
const files = useFilesStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Resolve the path to a live entry. The entry is looked up fresh each
// time the modal opens so stale paths (e.g. after a rename) don't
// break rendering.
const entry = computed(() => {
  if (!props.path) return null
  const parent = props.path.substring(0, props.path.lastIndexOf('/')) || '/'
  return files.getChildrenOf(parent).find((e) => e.path === props.path) ?? null
})

// Extension-derived MIME category for the content router below. Audio
// and video extensions follow what the browser <video>/<audio> tags
// natively decode without a third-party codec. PDF gets its own branch
// so we can render it through a sandboxed <iframe> that respects the
// blob URL (browsers ship a PDF viewer for blob: scheme).
const kind = computed<'image' | 'text' | 'video' | 'audio' | 'pdf' | 'other'>(() => {
  if (!entry.value) return 'other'
  const name = entry.value.name.toLowerCase()
  if (/\.(jpg|jpeg|png|gif|webp|svg|bmp|avif|heic)$/.test(name)) return 'image'
  if (/\.(mp4|webm|mov|m4v|ogv)$/.test(name)) return 'video'
  if (/\.(mp3|flac|wav|ogg|m4a|opus|aac)$/.test(name)) return 'audio'
  if (/\.pdf$/.test(name)) return 'pdf'
  if (/\.(txt|md|log|json|yaml|yml|xml|csv|js|ts|vue|go|py|rs|sh|conf|ini)$/.test(name))
    return 'text'
  return 'other'
})

// Preview URL resolution order:
//   1. Blob URL from the store (for files uploaded via drag-drop or
//      the file picker — created via URL.createObjectURL)
//   2. Static entry.url (fixture files that ship with real content
//      under /public/assets/, e.g. the Gallery wallpapers)
//   3. For image files on disk: a blob streamed from GET /v1/image
//      via the authenticated axios client. That third path is
//      asynchronous so we keep it in a ref that the watch below
//      populates whenever the preview target changes.
// Falls through to undefined when none are set — the template
// renders the "no content available" placeholder in that case.
const fetchedImageUrl = ref<string | undefined>(undefined)
const fetchedImageError = ref<string | null>(null)
// True while the axios GET /v1/image call is in flight. The template
// uses this to render a spinner instead of the "unsupported / no
// content" placeholder, which was flashing on-screen for ~1-2 seconds
// every time a photo opened and made the preview feel broken. The
// fetch only happens on disk-backed image entries (neither
// store-provided blob nor static fixture URL is available), so the
// flag is limited to that code path.
const fetchingImage = ref(false)

const previewUrl = computed<string | undefined>(() => {
  if (!props.path) return undefined
  const blob = files.getBlobUrl(props.path)
  if (blob) return blob
  if (fetchedImageUrl.value) return fetchedImageUrl.value
  return entry.value?.url
})

// Back-compat alias — the template references `blobUrl` in several
// places and I'd rather touch one line here than search-replace the
// template. Semantically `previewUrl` is the source of truth.
const blobUrl = previewUrl

// When the modal switches to an image entry that has no in-memory
// blob, fetch the original bytes from the server. Revokes the old
// object URL when the path changes so we don't leak Blobs as the
// user clicks through a carousel of 50 photos.
watch(
  () => [props.visible, kind.value, props.path, files.getBlobUrl(props.path ?? '')] as const,
  async ([open, k, path, storeBlob]) => {
    // Drop the previous fetched URL (release its Blob back to the GC).
    if (fetchedImageUrl.value) {
      URL.revokeObjectURL(fetchedImageUrl.value)
      fetchedImageUrl.value = undefined
    }
    fetchedImageError.value = null
    fetchingImage.value = false
    if (!open || k !== 'image' || !path) return
    // If the store already has a blob (just-uploaded file) we don't
    // need to re-fetch — the <img> will render that blob directly.
    if (storeBlob) return
    // Same for fixture files that already shipped with a static URL:
    // no need to hit the server, and flipping on `fetchingImage`
    // would flash a spinner over already-ready content.
    if (entry.value?.url) return
    fetchingImage.value = true
    try {
      const requested = path
      const url = await filesApi.fetchImageOriginal(path)
      if (requested === props.path) {
        fetchedImageUrl.value = url
      } else {
        // Path changed mid-flight; the URL is for a stale target.
        URL.revokeObjectURL(url)
      }
    } catch (err) {
      fetchedImageError.value = err instanceof Error ? err.message : 'load failed'
    } finally {
      // Clear the flag only if this fetch was for the currently
      // displayed path — otherwise a concurrent fetch for a newer
      // path is still in flight and we shouldn't pretend we're done.
      if (path === props.path) {
        fetchingImage.value = false
      }
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  if (fetchedImageUrl.value) {
    URL.revokeObjectURL(fetchedImageUrl.value)
    fetchedImageUrl.value = undefined
  }
  if (mediaUrl.value) {
    URL.revokeObjectURL(mediaUrl.value)
    mediaUrl.value = undefined
  }
})

// ----- Video / Audio / PDF streaming -----
// Same trade-off as the image branch: blob-URL via authenticated GET
// /v1/file keeps the bearer token off the <video src>. The browser
// holds the entire file in memory — acceptable for typical NAS use,
// suboptimal for multi-GB media. Iteration later: tokenised stream URL
// so the browser can seek without buffering the whole file first.
const mediaUrl = ref<string | undefined>(undefined)
const mediaLoading = ref(false)
const mediaError = ref<string | null>(null)

watch(
  () => [props.visible, kind.value, props.path] as const,
  async ([open, k, path]) => {
    if (mediaUrl.value) {
      URL.revokeObjectURL(mediaUrl.value)
      mediaUrl.value = undefined
    }
    mediaError.value = null
    mediaLoading.value = false
    if (!open || !path) return
    if (k !== 'video' && k !== 'audio' && k !== 'pdf') return
    mediaLoading.value = true
    try {
      const requested = path
      const url = await filesApi.fetchFileBlob(path)
      if (requested === props.path) {
        mediaUrl.value = url
      } else {
        URL.revokeObjectURL(url)
      }
    } catch (err) {
      mediaError.value = err instanceof Error ? err.message : 'load failed'
    } finally {
      if (path === props.path) mediaLoading.value = false
    }
  },
  { immediate: true },
)

// ----- Gallery carousel -----
// When the preview is showing an image, collect all sibling images in
// the same directory so the user can flip through them with prev/next
// buttons or the ← / → arrow keys. Non-image previews have no carousel.
const IMAGE_EXT_RE = /\.(jpe?g|png|gif|webp|svg|bmp|avif)$/i
const siblingImages = computed(() => {
  if (!props.path || kind.value !== 'image') return []
  const parent = props.path.substring(0, props.path.lastIndexOf('/')) || '/'
  return files
    .getChildrenOf(parent)
    .filter((e) => !e.isDir && IMAGE_EXT_RE.test(e.name))
})
const currentIndex = computed(() =>
  siblingImages.value.findIndex((e) => e.path === props.path),
)
const hasPrev = computed(() => currentIndex.value > 0)
const hasNext = computed(
  () => currentIndex.value >= 0 && currentIndex.value < siblingImages.value.length - 1,
)
const carouselCount = computed(() => siblingImages.value.length)

function goPrev() {
  if (!hasPrev.value) return
  const target = siblingImages.value[currentIndex.value - 1]
  emit('update:path', target.path)
  files.selectOnly(target.path)
}
function goNext() {
  if (!hasNext.value) return
  const target = siblingImages.value[currentIndex.value + 1]
  emit('update:path', target.path)
  files.selectOnly(target.path)
}

// Arrow key navigation while modal is open. Bound at window level so
// focus can live on any child of the dialog (buttons, image, etc.)
// without breaking key capture.
function onKeyDown(e: KeyboardEvent) {
  if (!props.visible) return
  if (carouselCount.value < 2) return
  if (e.key === 'ArrowLeft') {
    e.preventDefault()
    goPrev()
  } else if (e.key === 'ArrowRight') {
    e.preventDefault()
    goNext()
  }
}
watch(
  () => props.visible,
  (v) => {
    if (v) window.addEventListener('keydown', onKeyDown)
    else window.removeEventListener('keydown', onKeyDown)
  },
  { immediate: true },
)
onBeforeUnmount(() => window.removeEventListener('keydown', onKeyDown))

// Lazy-loaded text content for text-kind files. Prefers the in-memory
// blob URL (just-uploaded files); falls back to GET /v1/file/content
// for disk-backed files — previously those rendered "no content".
const textContent = ref<string | null>(null)
const textError = ref<string | null>(null)
// Edit mode — flips the <pre> into a <textarea> backed by editBuffer.
// Save PUTs through writeFileContent; dirty tracking gates the button.
const editing = ref(false)
const editBuffer = ref('')
const saving = ref(false)
const saveError = ref('')
const isDirty = computed(() => editing.value && editBuffer.value !== (textContent.value ?? ''))

watch(
  () => [props.visible, kind.value, props.path, blobUrl.value] as const,
  async ([open, k, path, url]) => {
    textContent.value = null
    textError.value = null
    editing.value = false
    saveError.value = ''
    if (!open || k !== 'text' || !path) return
    try {
      if (url) {
        const res = await fetch(url)
        textContent.value = await res.text()
      } else {
        textContent.value = await filesApi.readFileContent(path)
      }
    } catch (err) {
      textError.value = err instanceof Error ? err.message : 'read failed'
    }
  },
  { immediate: true },
)

function startEdit() {
  editBuffer.value = textContent.value ?? ''
  editing.value = true
}

async function saveEdit() {
  if (!props.path || saving.value) return
  saving.value = true
  saveError.value = ''
  try {
    await filesApi.writeFileContent(props.path, editBuffer.value)
    textContent.value = editBuffer.value
    editing.value = false
  } catch (err) {
    saveError.value = err instanceof Error ? err.message : 'save failed'
  } finally {
    saving.value = false
  }
}

function cancelEdit() {
  if (isDirty.value && !window.confirm(t('files.editor.discard_confirm'))) return
  editing.value = false
}

function onClose() {
  dialogVisible.value = false
}

// Stub download handler — triggers a download of the blob URL when one
// exists. Fixture files have no content, so this is effectively a no-op
// for them. The real backend wiring replaces this with a GET to /v3/file.
function onDownload() {
  if (!entry.value || !blobUrl.value) return
  const a = document.createElement('a')
  a.href = blobUrl.value
  a.download = entry.value.name
  a.click()
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '80vw', maxWidth: '900px', height: '80vh' }"
    :dismissable-mask="true"
  >
    <template #header>
      <div v-if="entry" class="flex items-center gap-3">
        <FileIcon :entry="entry" :size="20" />
        <div class="min-w-0 flex-1">
          <div class="truncate text-sm font-semibold text-[var(--cn-text)]">
            {{ entry.name }}
          </div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ formatBytes(entry.size) }} · {{ dayjs(entry.modified).format('MMM D, YYYY HH:mm') }}
          </div>
        </div>
      </div>
    </template>

    <!-- Content router: image, text, or fallback. Each branch handles
         its own empty state (no blob → placeholder). -->
    <div class="flex h-full min-h-[300px] items-center justify-center">
      <!-- Image mode -->
      <template v-if="kind === 'image'">
        <div v-if="blobUrl" class="relative flex h-full w-full items-center justify-center">
          <img
            :src="blobUrl"
            :alt="entry?.name"
            class="max-h-full max-w-full object-contain"
          />
          <!-- Carousel nav — only shown when the folder has >1 image. -->
          <template v-if="carouselCount > 1">
            <button
              type="button"
              :disabled="!hasPrev"
              class="absolute left-2 flex h-10 w-10 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur transition hover:bg-black/70 disabled:opacity-30"
              :aria-label="t('files.preview.previous')"
              @click="goPrev"
            >
              <AppIcon name="ChevronLeft" :size="22" />
            </button>
            <button
              type="button"
              :disabled="!hasNext"
              class="absolute right-2 flex h-10 w-10 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur transition hover:bg-black/70 disabled:opacity-30"
              :aria-label="t('files.preview.next')"
              @click="goNext"
            >
              <AppIcon name="ChevronRight" :size="22" />
            </button>
            <div class="absolute bottom-2 rounded-full bg-black/60 px-3 py-1 text-xs text-white backdrop-blur">
              {{ currentIndex + 1 }} / {{ carouselCount }}
            </div>
          </template>
        </div>
        <!-- Loading: GET /v1/image is in flight. Render a centered
             spinner instead of the "no content" fallback so the user
             doesn't see a flash of "unsupported format" for the
             second before the blob arrives. -->
        <div v-else-if="fetchingImage" class="flex flex-col items-center gap-3 text-center">
          <AppIcon name="RotateCw" :size="36" class="animate-spin text-[var(--cn-text-muted)]" />
          <p class="text-sm text-[var(--cn-text-muted)]">{{ t('common.loading') }}</p>
        </div>
        <!-- Error: fetch finished and threw. Real "couldn't read" state. -->
        <div v-else-if="fetchedImageError" class="flex flex-col items-center gap-3 text-center">
          <AppIcon name="FileQuestion" :size="48" class="text-[var(--cn-text-muted)]" />
          <p class="text-sm text-red-500">
            {{ fetchedImageError }}
          </p>
        </div>
        <!-- Empty: no path / no entry. Generic placeholder. -->
        <div v-else class="flex flex-col items-center gap-3 text-center">
          <AppIcon name="FileQuestion" :size="48" class="text-[var(--cn-text-muted)]" />
          <p class="text-sm text-[var(--cn-text-muted)]">
            {{ t('files.preview.no_content') }}
          </p>
        </div>
      </template>

      <!-- Video — <video> tag wired to the streamed blob URL. Browser
           PIP / fullscreen / playback-rate controls come for free. -->
      <template v-else-if="kind === 'video'">
        <div v-if="mediaLoading" class="flex flex-col items-center gap-2 text-[var(--cn-text-muted)]">
          <AppIcon name="Loader2" :size="32" class="animate-spin" />
          <p class="text-xs">{{ t('common.loading') }}</p>
        </div>
        <video
          v-else-if="mediaUrl"
          :src="mediaUrl"
          controls
          playsinline
          class="max-h-[70vh] w-full rounded-lg bg-black"
        />
        <p v-else-if="mediaError" class="text-sm text-red-500">{{ mediaError }}</p>
      </template>

      <!-- Audio — bare <audio> in a centered card. -->
      <template v-else-if="kind === 'audio'">
        <div v-if="mediaLoading" class="flex flex-col items-center gap-2 text-[var(--cn-text-muted)]">
          <AppIcon name="Loader2" :size="32" class="animate-spin" />
          <p class="text-xs">{{ t('common.loading') }}</p>
        </div>
        <div v-else-if="mediaUrl" class="flex w-full max-w-md flex-col items-center gap-4 rounded-lg bg-[var(--cn-bg)] p-6">
          <AppIcon name="Music" :size="48" class="text-[var(--cn-text-muted)]" />
          <div class="truncate text-sm text-[var(--cn-text)]">{{ entry?.name }}</div>
          <audio :src="mediaUrl" controls class="w-full" />
        </div>
        <p v-else-if="mediaError" class="text-sm text-red-500">{{ mediaError }}</p>
      </template>

      <!-- PDF — browsers ship a viewer for blob: scheme inside an
           iframe sandbox. No third-party JS PDF lib needed. -->
      <template v-else-if="kind === 'pdf'">
        <div v-if="mediaLoading" class="flex flex-col items-center gap-2 text-[var(--cn-text-muted)]">
          <AppIcon name="Loader2" :size="32" class="animate-spin" />
          <p class="text-xs">{{ t('common.loading') }}</p>
        </div>
        <iframe
          v-else-if="mediaUrl"
          :src="mediaUrl"
          class="h-[75vh] w-full rounded-lg border border-[var(--cn-border)] bg-white"
          :title="entry?.name"
        />
        <p v-else-if="mediaError" class="text-sm text-red-500">{{ mediaError }}</p>
      </template>

      <!-- Text mode -->
      <template v-else-if="kind === 'text'">
        <textarea
          v-if="editing"
          v-model="editBuffer"
          spellcheck="false"
          class="h-full min-h-[50vh] w-full resize-none rounded-lg border border-[var(--cn-border)] bg-[var(--cn-bg)] p-4 font-mono text-xs text-[var(--cn-text)] focus:border-blue-500 focus:outline-none"
        />
        <div v-else-if="textContent !== null" class="h-full w-full overflow-auto">
          <pre class="whitespace-pre-wrap break-words rounded-lg bg-[var(--cn-bg)] p-4 font-mono text-xs text-[var(--cn-text)]">{{ textContent }}</pre>
        </div>
        <p v-else-if="textError" class="text-sm text-red-500">{{ textError }}</p>
        <p v-else class="text-sm text-[var(--cn-text-muted)]">{{ t('common.loading') }}</p>
        <p v-if="saveError" class="mt-2 text-xs text-red-500">{{ saveError }}</p>
      </template>

      <!-- Fallback -->
      <template v-else>
        <div class="flex flex-col items-center gap-3 text-center">
          <AppIcon name="FileQuestion" :size="48" class="text-[var(--cn-text-muted)]" />
          <p class="text-sm text-[var(--cn-text-muted)]">
            {{ t('files.preview.unsupported') }}
          </p>
        </div>
      </template>
    </div>

    <template #footer>
      <Button :label="t('common.close')" severity="secondary" @click="onClose" />
      <!-- Edit / Save for text files. Edit shows only in view mode;
           Save/Discard only while editing. -->
      <Button
        v-if="kind === 'text' && !editing && textContent !== null"
        :label="t('files.editor.edit')"
        severity="secondary"
        @click="startEdit"
      >
        <template #icon><AppIcon name="Pencil" :size="14" /></template>
      </Button>
      <Button
        v-if="editing"
        :label="t('common.cancel')"
        severity="secondary"
        :disabled="saving"
        @click="cancelEdit"
      />
      <Button
        v-if="editing"
        :label="t('common.save')"
        :loading="saving"
        :disabled="saving || !isDirty"
        @click="saveEdit"
      >
        <template #icon><AppIcon name="Save" :size="14" /></template>
      </Button>
      <Button
        v-if="blobUrl && !editing"
        :label="t('common.download')"
        @click="onDownload"
      >
        <template #icon>
          <AppIcon name="Download" :size="14" />
        </template>
      </Button>
    </template>
  </Dialog>
</template>
