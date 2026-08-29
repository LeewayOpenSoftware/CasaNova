// Icon Pack preference store.
// v0.1: persisted via syncedPrefs so the choice follows the user
// across browsers / devices, not just per-browser.

import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import { getPref, setPref } from '@/composables/syncedPrefs'

export type IconPackId = 'lucide' | 'phosphor'
// App-icon STYLE is a separate axis from the glyph pack: 'tile' = the
// current colored rounded square, 'transparent' = ZimaOS-style traditional
// (logo on a light card, no per-app color, no glass sheen).
export type AppIconStyle = 'tile' | 'transparent'
// File/folder icon STYLE: 'line' follows the glyph pack (lucide/phosphor
// outline) and honors the accent + glow below; 'classic' uses filled,
// vividly-colored icons (traditional yellow folder, etc.) regardless of pack.
export type FileIconStyle = 'line' | 'classic'

const STORAGE_KEY = 'casanova.icon_pack'
const APPICON_KEY = 'casanova.app_icon_style'
const FILEICON_KEY = 'casanova.file_icon_style'
const ICON_ACCENT_KEY = 'casanova.icon_accent'
const ICON_GLOW_KEY = 'casanova.icon_glow'

export const useIconPackStore = defineStore('iconPack', () => {
  const pack = ref<IconPackId>(
    (getPref(STORAGE_KEY) as IconPackId) || 'lucide',
  )
  const appIconStyle = ref<AppIconStyle>(
    (getPref(APPICON_KEY) as AppIconStyle) || 'tile',
  )
  const fileIconStyle = ref<FileIconStyle>(
    (getPref(FILEICON_KEY) as FileIconStyle) || 'classic',
  )
  // '' = use each kind's default color. A hex overrides every line icon
  // with the accent (folders, files, all kinds) so the grid reads as one
  // accented set.
  const iconAccent = ref<string>(getPref(ICON_ACCENT_KEY) || '')
  const iconGlow = ref<boolean>(getPref(ICON_GLOW_KEY) === 'true')

  watch(pack, (v) => setPref(STORAGE_KEY, v))
  watch(appIconStyle, (v) => setPref(APPICON_KEY, v))
  watch(fileIconStyle, (v) => setPref(FILEICON_KEY, v))
  watch(iconAccent, (v) => setPref(ICON_ACCENT_KEY, v))
  watch(iconGlow, (v) => setPref(ICON_GLOW_KEY, v ? 'true' : 'false'))

  function setPack(id: IconPackId) {
    pack.value = id
  }
  function setAppIconStyle(id: AppIconStyle) {
    appIconStyle.value = id
  }
  function setFileIconStyle(id: FileIconStyle) {
    fileIconStyle.value = id
  }
  function setIconAccent(hex: string) {
    iconAccent.value = hex
  }
  function setIconGlow(on: boolean) {
    iconGlow.value = on
  }

  return {
    pack, setPack,
    appIconStyle, setAppIconStyle,
    fileIconStyle, setFileIconStyle,
    iconAccent, setIconAccent,
    iconGlow, setIconGlow,
  }
})
