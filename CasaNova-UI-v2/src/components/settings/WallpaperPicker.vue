<script setup lang="ts">
// WallpaperPicker — thumbnail grid for selecting a background image.
//
// Two-way bound via v-model to a URL string (or null for "no
// background"). Offers a "None" tile, the bundled stock wallpapers, an
// "Upload" tile for the user's own image, and a preview tile for the
// currently-selected custom upload.
//
// Uploaded images are downscaled + JPEG re-encoded to a data: URL on the
// CLIENT and stored straight into the theme pref — we deliberately do NOT
// round-trip through the backend: the login background must render BEFORE
// auth, but every server image route is JWT-gated (would 401 pre-login),
// whereas a data URL renders from localStorage with zero network.
//
// SIZE: the synced ui-prefs blob (all casanova.* keys) is PUT in one body
// capped at 2 MiB server-side, so we hard-cap the encoded size (see
// TARGET_BYTES) — stepping quality then dimensions down — to leave room
// for the rest of the prefs. base64 has no single quote, so it's safe
// inside apply()'s url('...') wrapper.

import { computed, ref } from 'vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import { wallpapers as wallpapersFixture } from '@/data/wallpapers'

const props = defineProps<{
  modelValue: string | null
  allowNone?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string | null): void
}>()

const selected = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

// True when the value is a user upload (a data: URL, or any URL that isn't
// one of the bundled fixtures). Drives the extra preview tile so an
// uploaded background still shows a lit-up thumbnail.
const isCustom = computed(
  () => !!selected.value && !wallpapersFixture.some((w) => w.url === selected.value),
)

const fileInput = ref<HTMLInputElement | null>(null)
const busy = ref(false)
const error = ref('')

// Encoded data-URL string ceiling (~700 KB). Keeps login + dashboard +
// the rest of the prefs comfortably under the 2 MiB ui-prefs body cap.
const TARGET_BYTES = 700 * 1024
const MAX_EDGE = 1920
const FALLBACK_EDGE = 1280

function pickFile() {
  error.value = ''
  fileInput.value?.click()
}

async function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  // Reset immediately so re-picking the SAME file re-fires @change.
  input.value = ''
  if (!file) return
  if (!file.type.startsWith('image/')) {
    error.value = 'Please choose an image file.'
    return
  }
  busy.value = true
  error.value = ''
  try {
    const dataUrl = await encodeToBoundedDataUrl(file)
    if (!dataUrl) {
      error.value = 'Image is too large even after compression — try a smaller one.'
      return
    }
    selected.value = dataUrl
  } catch {
    error.value = 'Could not read that image.'
  } finally {
    busy.value = false
  }
}

// Decode the file, downscale to a max edge, and JPEG-encode, stepping
// quality (and finally dimension) down until the data URL fits TARGET_BYTES.
async function encodeToBoundedDataUrl(file: File): Promise<string | null> {
  const img = await loadImage(file)
  for (const edge of [MAX_EDGE, FALLBACK_EDGE]) {
    const canvas = drawScaled(img, edge)
    for (const q of [0.82, 0.72, 0.6, 0.5]) {
      const url = canvas.toDataURL('image/jpeg', q)
      if (url.length <= TARGET_BYTES) return url
    }
  }
  return null
}

function loadImage(file: File): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file)
    const img = new Image()
    img.onload = () => {
      URL.revokeObjectURL(url)
      resolve(img)
    }
    img.onerror = () => {
      URL.revokeObjectURL(url)
      reject(new Error('decode failed'))
    }
    img.src = url
  })
}

function drawScaled(img: HTMLImageElement, maxEdge: number): HTMLCanvasElement {
  let width = img.width
  let height = img.height
  const scale = Math.min(1, maxEdge / Math.max(width, height))
  width = Math.round(width * scale)
  height = Math.round(height * scale)
  const canvas = document.createElement('canvas')
  canvas.width = width
  canvas.height = height
  const ctx = canvas.getContext('2d')!
  ctx.drawImage(img, 0, 0, width, height)
  return canvas
}
</script>

<template>
  <div class="flex flex-col gap-2">
    <div class="flex flex-wrap gap-2">
      <!-- "None" tile — clear the wallpaper without remembering which is set. -->
      <button
        v-if="allowNone !== false"
        type="button"
        :class="[
          'relative h-16 w-24 overflow-hidden rounded-lg border-2 transition-all',
          selected === null
            ? 'border-blue-500 shadow-[0_0_0_2px_rgb(59_130_246/0.25)]'
            : 'border-[var(--cn-border)] hover:border-[var(--cn-text-muted)]',
        ]"
        @click="selected = null"
      >
        <div class="flex h-full w-full items-center justify-center bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]">
          <AppIcon name="Ban" :size="22" />
        </div>
        <div
          v-if="selected === null"
          class="absolute right-1 top-1 flex h-4 w-4 items-center justify-center rounded-full bg-blue-500 text-white"
        >
          <AppIcon name="Check" :size="10" />
        </div>
      </button>

      <!-- Upload tile — pick your own image (downscaled + stored as data URL). -->
      <button
        type="button"
        :disabled="busy"
        class="relative flex h-16 w-24 flex-col items-center justify-center gap-1 rounded-lg border-2 border-dashed border-[var(--cn-border)] text-[var(--cn-text-muted)] transition-all hover:border-blue-500 hover:text-[var(--cn-text)] disabled:opacity-60"
        title="Upload your own image"
        @click="pickFile"
      >
        <AppIcon :name="busy ? 'Loader2' : 'Upload'" :size="18" :class="busy ? 'animate-spin' : ''" />
        <span class="text-[10px] font-medium">{{ busy ? '…' : 'Upload' }}</span>
      </button>
      <input
        ref="fileInput"
        type="file"
        accept="image/*"
        class="hidden"
        @change="onFileChange"
      />

      <!-- Current custom upload — a lit-up preview tile so the user sees
           their own image is the active selection. -->
      <div
        v-if="isCustom"
        class="relative h-16 w-24 overflow-hidden rounded-lg border-2 border-blue-500 shadow-[0_0_0_2px_rgb(59_130_246/0.25)]"
        title="Your uploaded image"
      >
        <img :src="selected!" alt="Custom" class="h-full w-full object-cover" />
        <div class="absolute right-1 top-1 flex h-4 w-4 items-center justify-center rounded-full bg-blue-500 text-white">
          <AppIcon name="Check" :size="10" />
        </div>
      </div>

      <button
        v-for="wp in wallpapersFixture"
        :key="wp.id"
        type="button"
        :title="wp.label"
        :class="[
          'relative h-16 w-24 overflow-hidden rounded-lg border-2 transition-all',
          selected === wp.url
            ? 'border-blue-500 shadow-[0_0_0_2px_rgb(59_130_246/0.25)]'
            : 'border-[var(--cn-border)] hover:border-[var(--cn-text-muted)]',
        ]"
        @click="selected = wp.url"
      >
        <img :src="wp.url" :alt="wp.label" class="h-full w-full object-cover" loading="lazy" />
        <div
          v-if="selected === wp.url"
          class="absolute right-1 top-1 flex h-4 w-4 items-center justify-center rounded-full bg-blue-500 text-white"
        >
          <AppIcon name="Check" :size="10" />
        </div>
      </button>
    </div>

    <p v-if="error" class="text-xs text-red-500">{{ error }}</p>
  </div>
</template>
