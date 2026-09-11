// useDevMode — Developer mode flag.
//
// Inspired by ZimaOS's General → Developer mode toggle. The purpose
// here is the same: give us one place to gate experimental or not-yet-
// implemented features so the default dashboard stays clean, but power
// users can flip one switch and see everything we're working on.
//
// Scope in CasaNova: any feature whose backend ships but whose UI is
// intentionally not production-ready (VM manager, experimental app
// settings, raw JSON viewers, etc.). Production-ready features should
// never be gated by this.
//
// Persistence: localStorage under `casanova.devmode`. Reactive across
// the whole app because we back it with a single module-level ref.

import { ref, watch } from 'vue'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'

const STORAGE_KEY = 'casanova.devmode'
const ADVANCED_KEY = 'casanova.advancedmode'

function readInitial(): boolean {
  if (typeof window === 'undefined') return false
  // v0.1: read via the synced cache so the toggle follows the user
  // across devices once they've logged in once.
  return getPref(STORAGE_KEY) === 'true'
}

function readAdvanced(): boolean {
  if (typeof window === 'undefined') return false
  return getPref(ADVANCED_KEY) === 'true'
}

const devMode = ref<boolean>(readInitial())
// `advancedMode` is the higher-trust gate for destructive / lifecycle
// actions: zpool create / destroy / replace, future btrfs balance,
// firmware flashes, etc. Independent of devMode (which surfaces
// preview UI) so an operator can sanity-check the panel without
// gaining the ability to wipe a pool.
const advancedMode = ref<boolean>(readAdvanced())

// Persist every flip. Watching is cheaper than writing from the setter
// because it keeps the composable usable as a plain ref in templates.
watch(devMode, (v) => {
  try {
    const next = v ? 'true' : 'false'
    if (getPref(STORAGE_KEY) !== next) setPref(STORAGE_KEY, next)
  } catch {
    // localStorage access can throw in some sandboxed contexts; ignore.
  }
})
watch(advancedMode, (v) => {
  try {
    const next = v ? 'true' : 'false'
    if (getPref(ADVANCED_KEY) !== next) setPref(ADVANCED_KEY, next)
  } catch {
    // localStorage access can throw in some sandboxed contexts; ignore.
  }
})

// Module-level refs are built at IMPORT time, which is long before the
// backend hydrate lands on a fresh login. Re-read when it does, or both
// toggles silently revert to off for the whole session. See iconPack.ts.
watch(prefsGeneration(), () => {
  devMode.value = readInitial()
  advancedMode.value = readAdvanced()
})

export function useDevMode() {
  return { devMode, advancedMode }
}
