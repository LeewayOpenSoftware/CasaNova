<script setup lang="ts">
// FileIcon — maps a file entry to a glyph by extension (or the folder
// icon for directories). Centralized so every renderer (grid + list +
// preview) uses the same classification.
//
// Three appearance modes, driven by the icon-pack store (Settings →
// Appearance):
//   • line + lucide/phosphor  → outline/duotone glyphs. Colored per kind,
//     or tinted with a single accent, with an optional soft glow.
//   • classic                 → filled, vividly-colored icons (traditional
//     yellow folder, etc.) — phosphor "fill" weight + a classic palette,
//     independent of the line pack and accent.

import { computed } from 'vue'
import {
  Folder,
  Image as ImageIcon,
  Film,
  Music,
  FileText,
  Archive as ArchiveIcon,
  FileCode,
  File as FileIconDefault,
} from 'lucide-vue-next'
import {
  PhFolder,
  PhImage,
  PhFilmSlate,
  PhMusicNotes,
  PhFileText,
  PhFileZip,
  PhFileCode,
  PhFile,
} from '@phosphor-icons/vue'
import type { FileEntry } from '@/types/files'
import { useIconPackStore } from '@/stores/iconPack'

const props = defineProps<{
  entry: FileEntry
  size?: number
}>()

const iconPack = useIconPackStore()

const KIND_BY_EXT: Record<string, 'image' | 'video' | 'audio' | 'doc' | 'archive' | 'code'> = {
  jpg: 'image', jpeg: 'image', png: 'image', gif: 'image', webp: 'image', svg: 'image',
  mp4: 'video', mkv: 'video', mov: 'video', avi: 'video', webm: 'video',
  mp3: 'audio', wav: 'audio', flac: 'audio', ogg: 'audio', m4a: 'audio', m3u: 'audio',
  pdf: 'doc', doc: 'doc', docx: 'doc', xls: 'doc', xlsx: 'doc', ppt: 'doc', pptx: 'doc',
  txt: 'doc', md: 'doc', rtf: 'doc',
  zip: 'archive', tar: 'archive', gz: 'archive', '7z': 'archive', rar: 'archive',
  iso: 'archive', restic: 'archive',
  js: 'code', ts: 'code', vue: 'code', go: 'code', py: 'code', rs: 'code',
  html: 'code', css: 'code', json: 'code', yaml: 'code', yml: 'code', sh: 'code',
}

type Kind = 'folder' | 'image' | 'video' | 'audio' | 'doc' | 'archive' | 'code' | 'other'

const ICONS: Record<'lucide' | 'phosphor', Record<Kind, unknown>> = {
  lucide: {
    folder: Folder, image: ImageIcon, video: Film, audio: Music,
    doc: FileText, archive: ArchiveIcon, code: FileCode, other: FileIconDefault,
  },
  phosphor: {
    folder: PhFolder, image: PhImage, video: PhFilmSlate, audio: PhMusicNotes,
    doc: PhFileText, archive: PhFileZip, code: PhFileCode, other: PhFile,
  },
}

// Per-kind color for the LINE packs. Hex applied via inline style (not a
// Tailwind text-* class): the active theme's un-layered rules were winning
// the cascade over the layered utility and tinting every icon with the
// accent (folders rendered "black"/amber). Inline style beats any selector.
const LINE_COLOR: Record<Kind, string> = {
  folder: '#3b82f6',
  image: '#10b981',
  video: '#a855f7',
  audio: '#ec4899',
  doc: '#3b82f6',
  archive: '#f59e0b',
  code: '#06b6d4',
  other: '#94a3b8',
}

// Classic (filled) palette — the traditional file-manager look. Folder is
// the warm yellow everyone expects; the rest are vivid so a filled glyph
// reads clearly. Generic values, not lifted from any vendor's assets.
const CLASSIC_COLOR: Record<Kind, string> = {
  folder: '#f5b83d',
  image: '#2eb872',
  video: '#9b5de5',
  audio: '#f15bb5',
  doc: '#4d96ff',
  archive: '#f4a259',
  code: '#00bbf0',
  other: '#8d99ae',
}

const kind = computed<Kind>(() => {
  if (props.entry.isDir) return 'folder'
  const lastDot = props.entry.name.lastIndexOf('.')
  if (lastDot <= 0) return 'other'
  const ext = props.entry.name.substring(lastDot + 1).toLowerCase()
  return KIND_BY_EXT[ext] ?? 'other'
})

const isClassic = computed(() => iconPack.fileIconStyle === 'classic')
const isPhosphor = computed(() => iconPack.pack === 'phosphor')

// Classic always renders via phosphor fill weight (solid, recognizable);
// line mode follows the chosen pack.
const icon = computed(() =>
  isClassic.value
    ? ICONS.phosphor[kind.value]
    : ICONS[isPhosphor.value ? 'phosphor' : 'lucide'][kind.value],
)

// Color is ALWAYS an inline hex so it wins the cascade over theme rules:
//   classic       → classic palette (folder yellow, etc.)
//   line + accent → the single accent
//   line default  → per-kind hex
const colorStyle = computed(() => {
  if (isClassic.value) return { color: CLASSIC_COLOR[kind.value] }
  if (iconPack.iconAccent) return { color: iconPack.iconAccent }
  return { color: LINE_COLOR[kind.value] }
})

const phosphorWeight = computed(() => {
  if (isClassic.value) return 'fill'
  return props.entry.isDir ? 'fill' : 'duotone'
})
</script>

<template>
  <!-- Lucide uses stroke-width; Phosphor uses weight. Classic + phosphor
       render via <component>; the glow class is opt-in (line mode only). -->
  <component
    :is="icon"
    :size="size ?? 20"
    :class="['cn-file-icon', { 'cn-file-icon--glow': iconPack.iconGlow && !isClassic }]"
    :style="colorStyle"
    :stroke-width="isClassic || isPhosphor ? undefined : 1.75"
    :fill="isClassic || isPhosphor ? 'currentColor' : 'none'"
    :weight="isClassic || isPhosphor ? phosphorWeight : undefined"
  />
</template>
