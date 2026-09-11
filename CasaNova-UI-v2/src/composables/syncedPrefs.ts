// syncedPrefs — per-user UI preference cache (Wave 29).
//
// Until this composable existed, every UI preference was a per-browser
// setting: theme, accent color, dashboard tile order, app folders,
// dismissed tips, search engine, gauge style — all of it lived in
// localStorage keyed by name and was completely invisible to the
// backend. Logging in from a second device showed defaults. Logging in
// as a different user on the same device exposed the previous user's
// layout. That's wrong: in a multi-user system, prefs belong to the
// account, not the browser tab.
//
// Design
// ──────
// We replace direct localStorage access with two helpers — getPref /
// setPref — that funnel through an in-memory cache. The cache is:
//
//   1. Bootstrapped from localStorage on module load so synchronous
//      consumers (Pinia store init, top-of-template reads) get
//      something on the first render.
//
//   2. Hydrated from the backend (/v1/users/current/custom/ui-prefs)
//      after the auth store reports a logged-in session. Any keys the
//      backend knows about overwrite their localStorage values, and
//      the merged blob is mirrored back to localStorage so the warm
//      cache stays useful for offline reloads.
//
//   3. Flushed back to the backend on every change, debounced so a
//      drag-reorder doesn't hammer the API. The full blob is sent on
//      each flush — there's no diff protocol, the existing
//      /current/custom/:key endpoint just stores raw bodies.
//
//   4. Cleared on logout (both the in-memory cache and the casanova.*
//      slice of localStorage) so a different user logging in on the
//      same browser starts from defaults instead of inheriting the
//      previous occupant's layout.
//
// Migration: existing installs already have prefs in localStorage. The
// first hydrate after this version ships sees the backend empty AND
// localStorage populated; we treat that as a one-shot migration and
// push the local values up. A `casanova.prefs.migrated.<userId>` flag
// stops it from repeating, so it can't accidentally re-migrate user A's
// data into user B's account if user B logs in on a stale browser.
//
// Auth tokens are explicitly NOT routed through here. They live in
// auth.ts under their own keys and stay strictly per-device.

import { ref } from 'vue'
import { getUiPrefs, putUiPrefs, type UiPrefsBlob } from '@/api/users'

// Every UI preference key in this codebase starts with `casanova.`
// (excluding auth keys). The shim treats anything matching this prefix
// as backend-syncable and ignores everything else.
const KEY_PREFIX = 'casanova.'

// Auth/session keys that must NEVER be sent to the backend or wiped on
// logout-driven cache clears (auth.ts owns its own teardown for these).
const EXCLUDED_KEYS = new Set<string>([
  'casanova.access_token',
  'casanova.refresh_token',
  'casanova.user',
  // The migration flag itself is per-browser bookkeeping — never sync.
])

// The UI language. It IS synced to the backend (follows the account across
// browsers) but is deliberately preserved by clearLocalPrefs, and applied to
// vue-i18n the moment hydration brings a different value down — otherwise the
// running session keeps rendering in the pre-login language until a reload.
const LOCALE_KEY = 'casanova.lang'

// Push a hydrated language onto the live vue-i18n instance. Imported lazily
// (inside the function) because src/i18n imports nothing from here, and a
// static import would create a module cycle at startup.
function applyHydratedLocale(): void {
  const lang = cache[LOCALE_KEY]
  if (!lang) return
  void import('@/i18n').then((mod) => {
    // createI18n's generic overload types `global` as the legacy VueI18n
    // shape; in Composition mode (legacy:false) it is really a Composer,
    // where `locale` is a writable ref. Narrow it explicitly.
    const g = mod.default.global as unknown as {
      locale: { value: string }
      availableLocales: string[]
    }
    if (!g.availableLocales.includes(lang)) return
    if (g.locale.value !== lang) g.locale.value = lang
  })
}

// Debounce window for the backend write. Long enough to coalesce a
// rapid drag-reorder into a single PUT, short enough that a deliberate
// click-and-wait still feels instant if the user reloads.
const FLUSH_DELAY_MS = 750

// In-memory cache of every casanova.* key. Reads come from here so
// individual stores don't need to know whether they hit the cache or
// localStorage. Writes go through setPref which keeps the cache,
// localStorage, and the pending backend flush in lockstep.
const cache: Record<string, string> = {}

// Bumped whenever a setPref / removePref happens, so consumers that
// want to react to upstream sync (e.g. a second tab pushing changes
// in the future) can `watch` it. Currently we only bump it on
// hydrateFromBackend so individual store watchers re-pick-up.
const cacheGeneration = ref(0)

// True once hydrateFromBackend has run to completion at least once
// for the current session, success or failure. Stores can `await`
// this if they want to delay rendering until the real values are in.
let hydrateInflight: Promise<void> | null = null

// Set during hydration so the localStorage writes triggered by store
// reactivity inside hydration don't immediately schedule a flush back
// to the backend (which would just echo what we just received).
let isHydrating = false

let flushTimer: number | null = null
let lastKnownUserId: number | null = null

// Whether a hydrate for lastKnownUserId has COMPLETED SUCCESSFULLY — i.e. the
// GET came back and we know what the server holds. This is the gate on every
// PUT, because putUiPrefs REPLACES the whole blob (there is no diff protocol):
// flushing a cache we never filled deletes every preference the server has.
//
// "Succeeded" deliberately includes a server that answered with NOTHING. A
// fresh install, or a user who has never saved a preference, legitimately has
// no blob, and that round-trip tells us just as much as a full one — treating
// it as "not hydrated" would mean such a user could never persist their first
// preference at all. The only state that blocks a PUT is a GET that THREW.
let hydratedOk = false

// Keys written locally while un-hydrated. They are the only thing standing
// between the user's change and a recovery hydrate that would overwrite it
// with the server's older value on its way back up. Value `null` records a
// removePref. Emptied once a flush has actually shipped them.
const unsyncedEdits = new Map<string, string | null>()

function noteUnsyncedEdit(key: string, value: string | null): void {
  if (!hydratedOk && isSyncableKey(key)) unsyncedEdits.set(key, value)
}

function isSyncableKey(k: string): boolean {
  return k.startsWith(KEY_PREFIX) && !EXCLUDED_KEYS.has(k)
}

// Walk localStorage once on module load to seed the cache. Anything
// already there is treated as authoritative until hydrateFromBackend
// runs and (potentially) overwrites it with the user's actual prefs.
function bootstrapFromLocalStorage(): void {
  if (typeof window === 'undefined') return
  for (let i = 0; i < window.localStorage.length; i++) {
    const k = window.localStorage.key(i)
    if (!k || !isSyncableKey(k)) continue
    const v = window.localStorage.getItem(k)
    if (v !== null) cache[k] = v
  }
}

bootstrapFromLocalStorage()

// ─── Public API ──────────────────────────────────────────────────────

/**
 * Read a preference. Returns null when the key has never been set.
 *
 * Falls back to localStorage on the off chance the cache was missed
 * (e.g. a key written before bootstrap finished, or by code outside
 * this shim) so we degrade gracefully instead of returning null for
 * a value that physically exists.
 */
export function getPref(key: string): string | null {
  if (key in cache) return cache[key]
  if (typeof window === 'undefined') return null
  const v = window.localStorage.getItem(key)
  if (v !== null) cache[key] = v
  return v
}

/**
 * Write a preference. Updates the cache, mirrors to localStorage for
 * the warm cache, and schedules a debounced backend flush. The flush
 * is suppressed during hydration to avoid echoing freshly downloaded
 * values right back.
 */
export function setPref(key: string, value: string): void {
  cache[key] = value
  noteUnsyncedEdit(key, value)
  if (typeof window !== 'undefined') {
    try {
      window.localStorage.setItem(key, value)
    } catch {
      /* sandboxed contexts — ignore */
    }
  }
  if (isSyncableKey(key)) scheduleFlush()
}

/**
 * Remove a preference. Symmetric with setPref — clears all three
 * persistence layers (cache, localStorage, pending backend flush).
 */
export function removePref(key: string): void {
  delete cache[key]
  noteUnsyncedEdit(key, null)
  if (typeof window !== 'undefined') {
    try {
      window.localStorage.removeItem(key)
    } catch {
      /* sandboxed contexts — ignore */
    }
  }
  if (isSyncableKey(key)) scheduleFlush()
}

/**
 * Reactive generation counter — bumped after a successful backend
 * hydrate. Stores can `watch` this to re-read values they may have
 * cached at module init time before the backend response landed.
 */
export function prefsGeneration() {
  return cacheGeneration
}

// ─── Hydration / flush ───────────────────────────────────────────────

/**
 * Pull the current user's preference blob from the backend and merge
 * it into the cache. Idempotent for a given userId — if called twice
 * for the same user the second call short-circuits to the in-flight
 * promise. A new userId resets the in-flight promise so we always
 * fetch fresh prefs after a user switch.
 */
export async function hydrateFromBackend(userId: number): Promise<void> {
  if (lastKnownUserId === userId && hydrateInflight) return hydrateInflight
  if (lastKnownUserId !== userId) {
    // A different account: nothing we know about the previous one carries over.
    hydratedOk = false
    unsyncedEdits.clear()
  }
  lastKnownUserId = userId

  hydrateInflight = (async () => {
    isHydrating = true
    try {
      const remote = await getUiPrefs()
      // The GET came back. Whatever it contained, we now know what the server
      // holds, so a flush may proceed — see hydratedOk. Set BEFORE the
      // migration push below, which is itself a flush and must be allowed.
      hydratedOk = true
      if (remote && remote.values) {
        // Backend wins for any key it knows about. Local-only keys
        // (e.g. a setting added on this browser before the user logged
        // in) are kept and will be pushed up on the next setPref.
        for (const [k, v] of Object.entries(remote.values)) {
          if (!isSyncableKey(k)) continue
          cache[k] = v
          if (typeof window !== 'undefined') {
            try {
              window.localStorage.setItem(k, v)
            } catch {
              /* ignore */
            }
          }
        }
        cacheGeneration.value++
        applyHydratedLocale()
        return
      }

      // Backend has nothing for this user. If we have local values AND
      // we haven't migrated for this user before on this browser, push
      // the local values up as a one-shot. The migration flag prevents
      // this from re-firing if user A logs out and user B logs in
      // (B's backend will also be empty, but B's local cache was
      // wiped on logout).
      const migratedFlag = `casanova.prefs.migrated.${userId}`
      const alreadyMigrated =
        typeof window !== 'undefined' && window.localStorage.getItem(migratedFlag) === '1'
      const haveLocalValues = Object.keys(cache).length > 0
      if (haveLocalValues && !alreadyMigrated) {
        await flushNow()
        if (typeof window !== 'undefined') {
          try {
            window.localStorage.setItem(migratedFlag, '1')
          } catch {
            /* ignore */
          }
        }
      }
    } catch (err) {
      // Backend unreachable, expired token, mid-deploy restart. Keep using
      // local values, and — critically — do NOT leave this rejection cached
      // as the answer for the session. The guard at the top of this function
      // short-circuits on hydrateInflight, so a failed attempt that stayed
      // parked there turned a transient blip into a permanent un-hydrated
      // session: the prefs never arrive AND every flush stays blocked. Drop
      // it so the next caller genuinely retries.
      hydrateInflight = null
      console.warn('[syncedPrefs] hydrate failed; using local cache', err)
    } finally {
      isHydrating = false
    }
  })()

  return hydrateInflight
}

/**
 * Wipe the in-memory cache and the casanova.* slice of localStorage.
 * Called from auth.ts on logout so the next user on this browser
 * doesn't inherit the previous user's layout.
 *
 * Excluded keys (auth tokens) are left alone — auth.ts handles those
 * itself in its own logout teardown.
 */
export function clearLocalPrefs(): void {
  for (const k of Object.keys(cache)) delete cache[k]
  if (typeof window !== 'undefined') {
    const toRemove: string[] = []
    for (let i = 0; i < window.localStorage.length; i++) {
      const k = window.localStorage.key(i)
      // KEEP the UI language across logout. Wiping it dropped the user back
      // to the browser default on the login screen and until the next
      // hydrate landed ("the language keeps resetting"). It is not private,
      // and the next user's hydrate overwrites it with their own choice.
      if (k === LOCALE_KEY) continue
      if (k && isSyncableKey(k)) toRemove.push(k)
    }
    for (const k of toRemove) {
      try {
        window.localStorage.removeItem(k)
      } catch {
        /* ignore */
      }
    }
  }
  if (flushTimer !== null) {
    window.clearTimeout(flushTimer)
    flushTimer = null
  }
  hydrateInflight = null
  lastKnownUserId = null
  // The cache this function just emptied is not a hydrated cache, and the
  // edits it held are gone with it. Both must reset or the next account's
  // first flush would ship this one's leftovers — or ship nothing at all.
  // The wipe itself is unchanged: it is why a second user on this browser
  // does not inherit the first one's layout.
  hydratedOk = false
  unsyncedEdits.clear()
  cacheGeneration.value++
}

function scheduleFlush(): void {
  if (isHydrating) return
  if (typeof window === 'undefined') return
  if (flushTimer !== null) window.clearTimeout(flushTimer)
  flushTimer = window.setTimeout(() => {
    flushTimer = null
    flushNow().catch((err) => {
      // Network blip — keep the cache in localStorage so we don't lose
      // the user's change, and let the next setPref retry the flush.
      console.warn('[syncedPrefs] flush failed; will retry on next change', err)
    })
  }, FLUSH_DELAY_MS) as unknown as number
}

async function flushNow(): Promise<void> {
  // A PUT replaces the ENTIRE server blob, so it may only be built from a
  // cache we know is complete. Without a successful hydrate behind us the
  // cache holds, at most, the handful of keys this session happened to write
  // — and shipping that deletes everything else the user ever saved. Their
  // first click after a failed hydrate would wipe theme, layout, icons and
  // the rest, silently and permanently.
  if (!hydratedOk) {
    if (lastKnownUserId === null) {
      // Nobody is signed in (pre-login screens still write prefs such as the
      // UI language). Keep it local; the hydrate at login decides what to do
      // with it, including the one-shot migration push.
      return
    }
    // Try to learn the server's state first, then send the merge.
    const pending = new Map(unsyncedEdits)
    await hydrateFromBackend(lastKnownUserId)
    if (!hydratedOk) {
      // Still blind. Send NOTHING: the change is already in localStorage and
      // the next setPref retries. A preference the user has to set twice is a
      // nuisance; a preference set silently destroys the other nineteen.
      throw new Error('[syncedPrefs] refusing to flush before a successful hydrate')
    }
    // Hydration overwrites cache keys with the server's copy, which for the
    // keys the user just edited is the OLDER value. Re-apply their edits over
    // what came down so the merge going up keeps the change that triggered it.
    for (const [k, v] of pending) {
      if (v === null) delete cache[k]
      else cache[k] = v
    }
  }

  // Build the blob from the live cache. Excluded keys never reach the
  // backend even if something else accidentally added them.
  const values: Record<string, string> = {}
  for (const [k, v] of Object.entries(cache)) {
    if (isSyncableKey(k)) values[k] = v
  }
  const blob: UiPrefsBlob = { version: 1, values }
  await putUiPrefs(blob)
  // Shipped: these are no longer unsynced. Anything written after this point
  // while still hydrated needs no special handling — the full cache goes up.
  unsyncedEdits.clear()
}
