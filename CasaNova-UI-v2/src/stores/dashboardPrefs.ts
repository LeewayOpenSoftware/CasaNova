// Dashboard personalization store — lightweight Pinia store for
// per-user dashboard preferences that outlive individual sessions.
//
// Currently tracks which dashboard tips the user has dismissed. The
// TipsCarousel reads this set to filter out hidden tips, and the
// "Restore tips" button in General settings wipes it. We use a Pinia
// store rather than a plain composable because the singleton guarantee
// survives Vite HMR — composable module-level refs can duplicate
// across hot reloads and desync from components already mounted.
//
// Persisted to localStorage under `casanova.dashboard.dismissedTips`
// as a JSON array. Loaded on store creation, written back via a
// watcher on every change.

import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'

const STORAGE_KEY = 'casanova.dashboard.dismissedTips'

function loadDismissed(): Set<string> {
  if (typeof window === 'undefined') return new Set()
  try {
    // Wave 29: read through syncedPrefs so dismissed tips follow the
    // user, not the browser.
    const raw = getPref(STORAGE_KEY)
    if (!raw) return new Set()
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? new Set(parsed.filter((s) => typeof s === 'string')) : new Set()
  } catch {
    return new Set()
  }
}

export const useDashboardPrefsStore = defineStore('dashboardPrefs', () => {
  const dismissedTipIds = ref<Set<string>>(loadDismissed())

  // Expose as an array computed so templates using v-for or length
  // stay straightforward; internal writes use the Set for O(1) adds.
  const dismissedList = computed(() => [...dismissedTipIds.value])

  function isDismissed(id: string): boolean {
    return dismissedTipIds.value.has(id)
  }

  function dismissTip(id: string) {
    if (dismissedTipIds.value.has(id)) return
    // Reassign a fresh Set so reactive consumers see the change — Vue's
    // reactivity system doesn't track in-place mutations on Set/Map
    // by default.
    const next = new Set(dismissedTipIds.value)
    next.add(id)
    dismissedTipIds.value = next
  }

  function restoreAllTips() {
    dismissedTipIds.value = new Set()
  }

  // Skip the next persist tick after a backend hydrate so we don't
  // echo freshly downloaded values straight back to the server.
  let suppressNextPersist = false

  // Persist on every change. Arrays serialize cleanly to JSON; Sets
  // don't, so we spread first.
  watch(
    dismissedTipIds,
    (v) => {
      if (suppressNextPersist) {
        suppressNextPersist = false
        return
      }
      try {
        // Wave 29: setPref also schedules a debounced backend flush
        // so dismissed tips sync per-user across devices.
        setPref(STORAGE_KEY, JSON.stringify([...v]))
      } catch {
        /* sandboxed contexts — ignore */
      }
    },
  )

  // Wave 29: re-read the dismissed-tips set when the backend hydrate
  // finishes so a fresh login on a new device picks up what the user
  // already dismissed elsewhere.
  const gen = prefsGeneration()
  watch(gen, () => {
    suppressNextPersist = true
    dismissedTipIds.value = loadDismissed()
  })

  return {
    dismissedTipIds,
    dismissedList,
    isDismissed,
    dismissTip,
    restoreAllTips,
  }
})
