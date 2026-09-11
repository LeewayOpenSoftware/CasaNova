// Preferences must be APPLIED when the backend hydrate lands, not only when
// the store that holds them happens to be constructed.
//
// The regression, from the user's chair: a deploy restarts the services, the
// session drops, logout wipes the casanova.* slice of localStorage on purpose
// (so the next account does not inherit this one's layout), the user signs
// back in, and every pref-holding store is now constructed against an EMPTY
// cache. It takes the constructor's defaults. The backend hydrate arrives a
// moment later and fills the cache correctly — but nothing re-reads it, so
// the refs keep the defaults for the rest of the session and the user sees
// their icon style, file view and dashboard layout silently reverted.
//
// syncedPrefs already exports prefsGeneration() for exactly this, and four
// modules already watch it. These tests pin the four that did not.
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { nextTick } from 'vue'
import { setActivePinia, createPinia } from 'pinia'

// A browser-shaped global, installed BEFORE syncedPrefs is imported (vi.hoisted
// runs ahead of the import block) because that module bootstraps its cache from
// localStorage at module load. There is no jsdom in this project, and the
// stores legitimately touch window.addEventListener and window.setTimeout, so
// a small real stub is more faithful here than pretending window is absent.
vi.hoisted(() => {
  const mem = new Map<string, string>()
  const localStorage = {
    get length() {
      return mem.size
    },
    key: (i: number) => [...mem.keys()][i] ?? null,
    getItem: (k: string) => (mem.has(k) ? mem.get(k)! : null),
    setItem: (k: string, v: string) => void mem.set(k, String(v)),
    removeItem: (k: string) => void mem.delete(k),
    clear: () => mem.clear(),
  }
  const w = {
    localStorage,
    addEventListener: () => {},
    removeEventListener: () => {},
    setTimeout: (fn: () => void, ms: number) => setTimeout(fn, ms),
    clearTimeout: (id: number) => clearTimeout(id),
    dispatchEvent: () => true,
  }
  Object.assign(globalThis, { window: w, localStorage })
})

const remote: { values: Record<string, string> } = { values: {} }

// The stores under test reach the api layer, whose axios client imports the
// router, which calls createWebHashHistory() and needs a DOM `location` this
// project has no jsdom to provide. Stub the transport; none of these tests
// make a request through it.
vi.mock('@/api/client', () => {
  const noop = async () => ({ data: { data: null } })
  return {
    default: { get: noop, post: noop, put: noop, delete: noop, interceptors: { request: { use: () => {} }, response: { use: () => {} } } },
    resetAuthClientLatch: () => {},
  }
})

const puts: Array<Record<string, string>> = []

vi.mock('@/api/users', () => ({
  UI_PREFS_KEY: 'ui-prefs',
  getUiPrefs: async () => ({ version: 1, values: remote.values }),
  putUiPrefs: async (blob: { values: Record<string, string> }) => {
    puts.push({ ...blob.values })
  },
}))

import { getPref, setPref, hydrateFromBackend, clearLocalPrefs } from './syncedPrefs'
import { useIconPackStore } from '@/stores/iconPack'
import { useWidgetLayoutStore } from '@/stores/widgetLayout'
import { useFilesStore } from '@/stores/files'
import { useDevMode } from './useDevMode'

// One login: the cache starts empty exactly as it does after logout, the
// store is built, and only then does the backend answer.
async function hydrateWith(values: Record<string, string>): Promise<void> {
  remote.values = values
  await hydrateFromBackend(1)
  await nextTick()
}

beforeEach(() => {
  clearLocalPrefs()
  setActivePinia(createPinia())
})

describe('icon preferences survive the login that follows a deploy', () => {
  it('adopts the stored icon style once hydration lands', async () => {
    const icons = useIconPackStore()
    // POSITIVE CONTROL: the store really does start on the default, so the
    // assertion below is measuring the fix and not a value that was already
    // correct. This is the state the user was stuck in.
    expect(icons.appIconStyle).toBe('tile')

    await hydrateWith({
      'casanova.app_icon_style': 'transparent',
      'casanova.icon_pack': 'phosphor',
      'casanova.file_icon_style': 'line',
      'casanova.icon_accent': '#ff8800',
      'casanova.icon_glow': 'true',
    })

    expect(icons.appIconStyle).toBe('transparent')
    expect(icons.pack).toBe('phosphor')
    expect(icons.fileIconStyle).toBe('line')
    expect(icons.iconAccent).toBe('#ff8800')
    expect(icons.iconGlow).toBe(true)
  })

  it('leaves a genuine default alone when the backend has no opinion', async () => {
    // The mirror image, so "adopt whatever hydration says" cannot be
    // satisfied by a rule that simply flips everything to non-default.
    const icons = useIconPackStore()
    await hydrateWith({ 'casanova.icon_pack': 'lucide' })
    expect(icons.appIconStyle).toBe('tile')
    expect(icons.pack).toBe('lucide')
  })

  it('does not echo the hydrated value back up as a write', async () => {
    // Adopting a value fires the persist watcher. Writing it back would
    // schedule a PUT on every login, and — worse — would make the store the
    // author of a value it only just received.
    const icons = useIconPackStore()
    await hydrateWith({ 'casanova.app_icon_style': 'transparent' })
    expect(icons.appIconStyle).toBe('transparent')
    expect(getPref('casanova.app_icon_style')).toBe('transparent')

    // POSITIVE CONTROL: a real user change still persists.
    icons.setAppIconStyle('tile')
    await nextTick()
    expect(getPref('casanova.app_icon_style')).toBe('tile')
  })
})

describe('the other stores that read prefs once at construction', () => {
  it('re-reads the dashboard widget layout', async () => {
    const widgets = useWidgetLayoutStore()
    expect(widgets.order).toEqual(['clock', 'system', 'storage', 'network'])

    await hydrateWith({
      'casanova.widgets.order': JSON.stringify(['uptime', 'clock', 'traffic']),
      'casanova.widgets.hidden': JSON.stringify(['clock']),
    })

    expect(widgets.order).toEqual(['uptime', 'clock', 'traffic'])
    expect(widgets.hidden).toEqual(['clock'])
    expect(widgets.visible).toEqual(['uptime', 'traffic'])
  })

  it('re-reads the file browser view preferences', async () => {
    const files = useFilesStore()
    expect(files.viewMode).toBe('grid')
    expect(files.showHidden).toBe(false)

    await hydrateWith({
      'casanova.files.view_mode': 'list',
      'casanova.files.show_hidden': 'true',
      'casanova.files.sort_key': 'size',
      'casanova.files.sort_dir': 'desc',
    })

    expect(files.viewMode).toBe('list')
    expect(files.showHidden).toBe(true)
    expect(files.sortKey).toBe('size')
    expect(files.sortDir).toBe('desc')
  })
})

describe('developer mode, whose refs are built at import time', () => {
  it('re-reads both toggles when hydration lands', async () => {
    // These are module-level refs, created when the module is first imported
    // — earlier than any store, and far earlier than the backend answers.
    const { devMode, advancedMode } = useDevMode()
    expect(devMode.value).toBe(false)
    expect(advancedMode.value).toBe(false)

    // The two keys are hydrated to DIFFERENT values on purpose. Asserting
    // only "both became true" would be satisfied by a re-read that blanket-
    // enables everything; this pins that each toggle follows its own key.
    // (One hydrate, not two: hydrateFromBackend is idempotent per user id and
    // a second call for the same user returns the settled promise without
    // re-fetching — so a two-step test would silently measure nothing.)
    await hydrateWith({ 'casanova.devmode': 'true', 'casanova.advancedmode': 'false' })

    expect(devMode.value).toBe(true)
    expect(advancedMode.value).toBe(false)
  })
})

describe('what a flush sends, once hydration has landed', () => {
  it('sends the hydrated cache, not the stores\' stale refs', async () => {
    // The worry worth ruling out: a store still holding constructor defaults
    // writes one preference, and the debounced PUT — which has no diff
    // protocol and replaces the whole blob — ships those defaults over the
    // server's good values.
    //
    // It does not, and the reason is structural: flushNow() builds the blob
    // from the syncedPrefs CACHE, which hydration filled correctly, and never
    // reads a store ref. Pinned here because "the flush source is the cache"
    // is the invariant that makes the stale-ref bug a display fault rather
    // than a data-loss one, and a future refactor could quietly break it.
    puts.length = 0
    await hydrateWith({
      'casanova.app_icon_style': 'transparent',
      'casanova.widgets.order': JSON.stringify(['uptime']),
    })
    // A store that is created late and never re-read still must not matter.
    useIconPackStore()

    setPref('casanova.icon_glow', 'true')
    await new Promise((resolve) => setTimeout(resolve, 900))

    expect(puts.length).toBeGreaterThan(0)
    const sent = puts[puts.length - 1]
    // The change went up...
    expect(sent['casanova.icon_glow']).toBe('true')
    // ...and so did everything hydration had put in the cache. A blob missing
    // these keys is a blob that deletes them server-side.
    expect(sent['casanova.app_icon_style']).toBe('transparent')
    expect(sent['casanova.widgets.order']).toBe(JSON.stringify(['uptime']))
  })
})
