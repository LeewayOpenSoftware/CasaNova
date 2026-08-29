// useUserPrefs — small user preferences composable.
//
// Backs the "minor" General settings that don't need a full Pinia
// store: search engine choice and dashboard tips visibility. Each
// preference is a ref persisted to localStorage via a module-level
// watcher so every consumer (Settings page that edits it, Dashboard
// components that read it) sees the same reactive value.
//
// Kept separate from the theme store because these have nothing to
// do with visual theming — they're per-user behavior knobs that
// would sync to the server under the same "user prefs" endpoint in
// a real backend.

import { ref, watch } from 'vue'
import { getPref, setPref, removePref, prefsGeneration } from '@/composables/syncedPrefs'

// ----- Search engine -----
// Maps a short code to a query-URL template. The URL has {q} as the
// placeholder for the (already URL-encoded) search term. Adding a new
// engine is a one-line change here + a new entry in the settings
// picker; the dashboard SearchBar reads the resolved template.
export type SearchEngine = 'duckduckgo' | 'google' | 'bing' | 'startpage' | 'kagi'

export const SEARCH_ENGINES: Record<
  SearchEngine,
  { label: string; url: string }
> = {
  duckduckgo: { label: 'DuckDuckGo', url: 'https://duckduckgo.com/?q={q}' },
  google: { label: 'Google', url: 'https://www.google.com/search?q={q}' },
  bing: { label: 'Bing', url: 'https://www.bing.com/search?q={q}' },
  startpage: { label: 'Startpage', url: 'https://www.startpage.com/do/search?q={q}' },
  kagi: { label: 'Kagi', url: 'https://kagi.com/search?q={q}' },
}

const SEARCH_KEY = 'casanova.prefs.searchEngine'
const TIPS_KEY = 'casanova.prefs.showTips'
const GAUGE_KEY = 'casanova.prefs.gaugeStyle'
const BRAND_KEY = 'casanova.prefs.brandName'

// ----- System state gauge style -----
// The dashboard's System State widget can render its CPU/RAM meters
// in four visual styles. Users pick from a small menu in the widget
// header; the choice persists across sessions via this key.
export type GaugeStyle = 'ring' | 'digital' | 'analog' | 'cockpit'

// Wave 29: every read/write goes through syncedPrefs so search engine,
// dashboard tips, and gauge style sync per-user across devices.

function readGauge(): GaugeStyle {
  const raw = getPref(GAUGE_KEY)
  if (raw === 'digital' || raw === 'analog' || raw === 'cockpit' || raw === 'ring') {
    return raw
  }
  return 'ring'
}

function readSearch(): SearchEngine {
  const raw = getPref(SEARCH_KEY)
  if (raw && raw in SEARCH_ENGINES) return raw as SearchEngine
  return 'duckduckgo'
}

function readTips(): boolean {
  const raw = getPref(TIPS_KEY)
  // Default true — first-time users benefit from the hints; the toggle
  // is an opt-out for experienced users who've seen them all.
  return raw === null ? true : raw === 'true'
}

// ----- Branding name (top-left product wordmark) -----
//   null  = never customized  -> callers render the default "CasaNova"
//   ''    = explicitly cleared -> icon-only (name hidden)
//   other = custom name the user typed
function readBrandName(): string | null {
  return getPref(BRAND_KEY)
}

const searchEngine = ref<SearchEngine>(readSearch())
const showTips = ref<boolean>(readTips())
const gaugeStyle = ref<GaugeStyle>(readGauge())
const brandName = ref<string | null>(readBrandName())

// Skip the next persist tick after a backend hydrate so we don't echo
// freshly downloaded values straight back to the server.
let suppressNextPersist = false

watch(searchEngine, (v) => {
  if (suppressNextPersist) return
  try {
    setPref(SEARCH_KEY, v)
  } catch {
    /* sandboxed contexts — ignore */
  }
})
watch(showTips, (v) => {
  if (suppressNextPersist) return
  try {
    setPref(TIPS_KEY, v ? 'true' : 'false')
  } catch {
    /* sandboxed contexts — ignore */
  }
})
watch(gaugeStyle, (v) => {
  if (suppressNextPersist) return
  try {
    setPref(GAUGE_KEY, v)
  } catch {
    /* sandboxed contexts — ignore */
  }
})
watch(brandName, (v) => {
  if (suppressNextPersist) return
  try {
    // null -> drop the key so it reads as "default" again;
    // '' (icon-only) and custom names persist verbatim.
    if (v === null) removePref(BRAND_KEY)
    else setPref(BRAND_KEY, v)
  } catch {
    /* sandboxed contexts — ignore */
  }
})

// Wave 29: re-read all three values when syncedPrefs.hydrateFromBackend
// finishes so the values follow the user across devices. Set the
// suppress flag for one tick so the resulting writes don't echo back
// to the backend.
const gen = prefsGeneration()
watch(gen, () => {
  suppressNextPersist = true
  searchEngine.value = readSearch()
  showTips.value = readTips()
  gaugeStyle.value = readGauge()
  brandName.value = readBrandName()
  // Three sequential ref writes can each trigger a watcher tick. The
  // suppress flag is single-shot in the watchers above, so flip it
  // back on between assignments to cover all three.
  // We use a microtask to clear it after the current sync chain
  // settles, leaving normal user-driven writes free to persist.
  queueMicrotask(() => {
    suppressNextPersist = false
  })
})

export function useUserPrefs() {
  return { searchEngine, showTips, gaugeStyle, brandName }
}

/**
 * Resolve a search query into the full URL to open, based on the
 * currently selected engine. Encodes the query so callers don't have
 * to remember the rules.
 */
export function buildSearchUrl(query: string): string {
  const engine = SEARCH_ENGINES[searchEngine.value] ?? SEARCH_ENGINES.duckduckgo
  return engine.url.replace('{q}', encodeURIComponent(query))
}
