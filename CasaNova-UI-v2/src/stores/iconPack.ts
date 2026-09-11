// Icon Pack preference store.
// v0.1: persisted via syncedPrefs so the choice follows the user
// across browsers / devices, not just per-browser.

import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'

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

// Every stored value read in ONE place, so store creation and the post-
// hydration re-read below cannot drift apart. A default that only the
// constructor knows about is a default the re-read would silently reinstate.
function readPack(): IconPackId {
  return (getPref(STORAGE_KEY) as IconPackId) || 'lucide'
}
function readAppIconStyle(): AppIconStyle {
  return (getPref(APPICON_KEY) as AppIconStyle) || 'tile'
}
function readFileIconStyle(): FileIconStyle {
  return (getPref(FILEICON_KEY) as FileIconStyle) || 'classic'
}
function readIconAccent(): string {
  return getPref(ICON_ACCENT_KEY) || ''
}
function readIconGlow(): boolean {
  return getPref(ICON_GLOW_KEY) === 'true'
}

// Persist a change, but never re-write a value the cache already holds.
// Adopting a hydrated value sets the ref, which fires the watcher, which
// would otherwise echo the freshly-downloaded value straight back up as a
// PUT on every single login.
function persist(key: string, value: string): void {
  if (getPref(key) !== value) setPref(key, value)
}

export const useIconPackStore = defineStore('iconPack', () => {
  const pack = ref<IconPackId>(readPack())
  const appIconStyle = ref<AppIconStyle>(readAppIconStyle())
  const fileIconStyle = ref<FileIconStyle>(readFileIconStyle())
  // '' = use each kind's default color. A hex overrides every line icon
  // with the accent (folders, files, all kinds) so the grid reads as one
  // accented set.
  const iconAccent = ref<string>(readIconAccent())
  const iconGlow = ref<boolean>(readIconGlow())

  watch(pack, (v) => persist(STORAGE_KEY, v))
  watch(appIconStyle, (v) => persist(APPICON_KEY, v))
  watch(fileIconStyle, (v) => persist(FILEICON_KEY, v))
  watch(iconAccent, (v) => persist(ICON_ACCENT_KEY, v))
  watch(iconGlow, (v) => persist(ICON_GLOW_KEY, v ? 'true' : 'false'))

  // These refs are built ONCE, synchronously, from whatever the cache holds
  // at store-creation time. On a fresh login that is nothing: logout wiped
  // the casanova.* slice on purpose, and the backend hydrate lands later.
  // Without this watcher the refs keep the constructor's defaults for the
  // rest of the session and the user's saved icon style silently reverts —
  // which is exactly what a deploy looked like from the user's chair.
  // Same idiom as theme.ts, dashboardPrefs.ts, useAppOrder.ts and
  // useUserPrefs.ts, which already re-read on this counter.
  const gen = prefsGeneration()
  watch(gen, () => {
    pack.value = readPack()
    appIconStyle.value = readAppIconStyle()
    fileIconStyle.value = readFileIconStyle()
    iconAccent.value = readIconAccent()
    iconGlow.value = readIconGlow()
  })

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
