<script setup lang="ts">
// FileThumbnail — renders a real server-generated thumbnail for image
// files and falls back to the generic FileIcon for everything else
// (folders, non-images, or images whose thumbnail fetch fails).
//
// Why a dedicated component and not a composable: FileGrid and
// FileList render a long v-for of entries, and we need each row to
// own its own fetch lifecycle (one GET /v1/image?path=...&type=
// thumbnail per image, cached, shared across mounts). Putting that
// state inside a per-row component keeps it local and automatic —
// the surrounding file view never has to track a Map of refs keyed
// by path or remember to invalidate when a directory changes.
//
// Fetching is NOT done here: the request goes through thumbnailLoader,
// which gates it on the tile being on screen and caps how many GETs run
// at once. See that file for why (a 500-photo folder used to fire 500
// simultaneous requests from this watcher). All this component owns is
// the per-entry state machine: arm the visibility gate, take the blob
// URL if it arrives while we still want it, fall back to the icon
// otherwise, and drop the request when the entry changes or unmounts.

import { ref, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import FileIcon from './FileIcon.vue'
import { observeVisible, requestThumbnail, type ThumbnailRequest } from './thumbnailLoader'
import type { FileEntry } from '@/types/files'

const props = defineProps<{
  entry: FileEntry
  /** Rendered glyph size for the FileIcon fallback (px). */
  size?: number
}>()

// Extensions the backend's thumbnail generator understands. This list
// intentionally matches what GetFileImage → file.GetImage supports in
// CasaNova/pkg/utils/file/image.go — we don't even attempt the fetch
// for formats the backend can't decode (raw, heic, etc.) so those
// files render the generic FileIcon immediately.
const IMAGE_EXT_RE = /\.(jpe?g|png|gif|webp|bmp)$/i

const src = ref<string | undefined>(undefined)
const errored = ref(false)

// The fallback icon is the only element that exists before the image
// loads, so it doubles as the visibility probe for this tile.
const iconRef = ref<InstanceType<typeof FileIcon> | null>(null)

let disarm: (() => void) | null = null
let active: ThumbnailRequest | null = null

function teardown() {
  disarm?.()
  disarm = null
  active?.cancel()
  active = null
}

async function fetchThumb(path: string) {
  const request = requestThumbnail(path)
  active = request
  try {
    const url = await request.promise
    // `active` is reassigned/cleared whenever the row is pointed at a
    // different entry, so a slow response can't overwrite a newer
    // mount's thumbnail — and a cancelled request stays silent instead
    // of flipping the tile to the error fallback.
    if (active === request) src.value = url
  } catch {
    if (active === request) errored.value = true
  }
}

function refresh(path: string) {
  teardown()
  src.value = undefined
  errored.value = false
  if (props.entry.isDir) return
  if (!IMAGE_EXT_RE.test(props.entry.name)) return
  // Wait for the icon to be in the DOM: on a path change the previous
  // state may still be showing the <img> branch this tick.
  void nextTick(() => {
    if (props.entry.path !== path) return
    const el = iconRef.value?.$el as Element | undefined
    if (!el) return
    disarm = observeVisible(el, () => {
      disarm = null
      void fetchThumb(path)
    })
  })
}

onMounted(() => refresh(props.entry.path))

// Re-fetch whenever the path changes. Paths are stable identifiers in
// the files store so this only fires when the row is reassigned to a
// different entry (virtualized lists would trigger it too).
watch(() => props.entry.path, refresh)

onBeforeUnmount(teardown)
</script>

<template>
  <img
    v-if="src && !errored"
    :src="src"
    :alt="entry.name"
    loading="lazy"
    class="h-full w-full object-cover"
    @error="errored = true"
  />
  <FileIcon v-else ref="iconRef" :entry="entry" :size="size ?? 40" />
</template>
