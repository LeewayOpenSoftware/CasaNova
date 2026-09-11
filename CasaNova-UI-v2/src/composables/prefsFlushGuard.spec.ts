// A PUT to /current/custom/ui-prefs REPLACES the whole blob — api/users.ts says
// so in as many words: "there's no diff/patch protocol, so callers MUST send
// the full blob". So a flush built from a cache we never managed to fill does
// not fail to save the user's change; it DELETES every other preference they
// ever set. The trigger is a hydrate that threw, which is most likely during a
// deploy — the same operation that produced the original report.
//
// These tests pin the rule: no PUT without a successful hydrate behind it.
import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.hoisted(() => {
  const mem = new Map<string, string>()
  const localStorage = {
    get length() { return mem.size },
    key: (i: number) => [...mem.keys()][i] ?? null,
    getItem: (k: string) => (mem.has(k) ? mem.get(k)! : null),
    setItem: (k: string, v: string) => void mem.set(k, String(v)),
    removeItem: (k: string) => void mem.delete(k),
  }
  Object.assign(globalThis, {
    window: {
      localStorage,
      addEventListener: () => {},
      removeEventListener: () => {},
      setTimeout: (fn: () => void, ms: number) => setTimeout(fn, ms),
      clearTimeout: (id: number) => clearTimeout(id),
    },
    localStorage,
  })
})

// The server, as this suite sees it.
const server: {
  blob: Record<string, string> | null
  failGet: boolean
  gets: number
} = { blob: null, failGet: false, gets: 0 }

const puts: Array<Record<string, string>> = []

vi.mock('@/api/users', () => ({
  UI_PREFS_KEY: 'ui-prefs',
  getUiPrefs: async () => {
    server.gets++
    if (server.failGet) throw new Error('backend restarting (deploy)')
    return server.blob ? { version: 1, values: server.blob } : null
  },
  putUiPrefs: async (blob: { values: Record<string, string> }) => {
    puts.push({ ...blob.values })
    server.blob = { ...blob.values }
  },
}))

import { getPref, setPref, hydrateFromBackend, clearLocalPrefs } from './syncedPrefs'

// Longer than syncedPrefs' 750ms debounce, so a flush that was going to
// happen has happened by the time we assert.
const AFTER_DEBOUNCE = 900
const settle = () => new Promise((resolve) => setTimeout(resolve, AFTER_DEBOUNCE))

beforeEach(() => {
  clearLocalPrefs()
  puts.length = 0
  server.blob = null
  server.failGet = false
  server.gets = 0
  localStorage.clear?.()
})

describe('a flush never runs blind', () => {
  it('sends NOTHING when the hydrate failed', async () => {
    server.blob = {
      'casanova.app_icon_style': 'transparent',
      'casanova.theme': 'dark',
      'casanova.widgets.order': '["uptime"]',
    }
    server.failGet = true

    await hydrateFromBackend(1)
    setPref('casanova.icon_glow', 'true')
    await settle()

    expect(puts).toEqual([])
    // ...and the server still holds everything it held before.
    expect(server.blob).toEqual({
      'casanova.app_icon_style': 'transparent',
      'casanova.theme': 'dark',
      'casanova.widgets.order': '["uptime"]',
    })
    // The user's change is not lost either — it is local, pending a retry.
    expect(getPref('casanova.icon_glow')).toBe('true')
  })

  // POSITIVE CONTROL for the above: the guard must not be "never flush".
  it('sends the COMPLETE blob when the hydrate succeeded', async () => {
    server.blob = { 'casanova.app_icon_style': 'transparent', 'casanova.theme': 'dark' }

    await hydrateFromBackend(1)
    setPref('casanova.icon_glow', 'true')
    await settle()

    expect(puts.length).toBe(1)
    expect(puts[0]).toEqual({
      'casanova.app_icon_style': 'transparent',
      'casanova.theme': 'dark',
      'casanova.icon_glow': 'true',
    })
  })

  it('recovers on the next change once the backend is back, keeping the edit', async () => {
    // The edit COLLIDES with a key the server already holds, on purpose. An
    // edit to some key the server has never heard of survives the recovery
    // hydrate by accident — hydration only overwrites keys it received — so a
    // test using one would pass with the merge deleted. This is the case that
    // actually needs it: hydration is about to overwrite the user's new value
    // with the server's older one on the way in, and the blob going straight
    // back up would then ship the server its own stale value as if the user
    // had chosen it.
    server.blob = { 'casanova.app_icon_style': 'tile', 'casanova.theme': 'dark' }
    server.failGet = true
    await hydrateFromBackend(1)

    setPref('casanova.app_icon_style', 'transparent')
    await settle()
    expect(puts).toEqual([])

    // Backend recovers. The next change must re-hydrate, merge, and ship.
    server.failGet = false
    setPref('casanova.file_icon_style', 'line')
    await settle()

    expect(puts.length).toBe(1)
    expect(puts[0]).toEqual({
      // the user's change, not the server's older 'tile'
      'casanova.app_icon_style': 'transparent',
      // ...and the key only the server knew about is still there
      'casanova.theme': 'dark',
      'casanova.file_icon_style': 'line',
    })
  })

  it('does not cache a failed hydrate as the answer for the session', async () => {
    // The bug this closes: hydrateFromBackend short-circuits on the in-flight
    // promise, so a rejection parked there made ONE transient failure last the
    // whole session — prefs never arriving and every flush blocked with it.
    server.blob = { 'casanova.app_icon_style': 'transparent' }
    server.failGet = true
    await hydrateFromBackend(1)
    expect(getPref('casanova.app_icon_style')).toBeNull()

    server.failGet = false
    await hydrateFromBackend(1)
    expect(server.gets).toBe(2)
    expect(getPref('casanova.app_icon_style')).toBe('transparent')
  })
})

describe('an empty server is not a blind client', () => {
  it("lets a user who has never saved anything save their first preference", async () => {
    // TRAP: a fresh install answers the GET successfully with no blob. Reading
    // that as "not hydrated" would block this user's every write forever.
    server.blob = null

    await hydrateFromBackend(42)
    setPref('casanova.app_icon_style', 'transparent')
    await settle()

    expect(puts.length).toBeGreaterThan(0)
    expect(puts[puts.length - 1]['casanova.app_icon_style']).toBe('transparent')
  })

  it('still performs the one-shot localStorage migration', async () => {
    // TRAP: the documented upgrade path — backend empty, localStorage
    // populated — pushes the local values up exactly once. It is a flush that
    // happens while the server is empty, so the new rule must permit it.
    clearLocalPrefs()
    setPref('casanova.theme', 'dark')
    setPref('casanova.app_icon_style', 'transparent')
    puts.length = 0
    server.blob = null

    await hydrateFromBackend(7)

    expect(puts.length).toBe(1)
    expect(puts[0]).toEqual({
      'casanova.theme': 'dark',
      'casanova.app_icon_style': 'transparent',
    })
    expect(localStorage.getItem('casanova.prefs.migrated.7')).toBe('1')
  })
})

describe('logout still clears, and clearing still blinds', () => {
  it('wipes the casanova.* slice so the next account inherits nothing', async () => {
    server.blob = { 'casanova.theme': 'dark' }
    await hydrateFromBackend(1)
    expect(getPref('casanova.theme')).toBe('dark')

    clearLocalPrefs()

    expect(getPref('casanova.theme')).toBeNull()
    expect(localStorage.getItem('casanova.theme')).toBeNull()
  })

  it('refuses to flush for a signed-out session instead of writing defaults', async () => {
    clearLocalPrefs()
    setPref('casanova.app_icon_style', 'tile')
    await settle()
    expect(puts).toEqual([])
  })
})
