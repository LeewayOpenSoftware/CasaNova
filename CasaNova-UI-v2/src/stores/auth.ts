// Auth store: the single source of truth for the current user, access token,
// and refresh token. Every API call pulls the access token from here via the
// axios request interceptor in @/api/client.ts.
//
// Persistence: tokens live in localStorage under stable keys so a full page
// refresh doesn't lose the session. `hydrate()` is called from the router
// guard on first navigation to pull them back in.
//
// This replaces the Vuex store in legacy CasaNova-UI which split the same
// state across state.js / mutations.js / actions.js / getters.js. Pinia
// collapses all of that into one composable.

import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { hydrateFromBackend, clearLocalPrefs } from '@/composables/syncedPrefs'
import client, { resetAuthClientLatch } from '@/api/client'

// revokeRefreshFamily fires the server-side logout endpoint that
// invalidates the refresh-token family server-side. Best-effort: a
// network failure must not block the local sign-out.
//
// The endpoint is in client.ts's AUTH_EXEMPT_URLS on purpose: this call
// is made from inside logout(), so a 401 answer re-entering the 401
// interceptor would call logout() all over again.
async function revokeRefreshFamily(refresh: string): Promise<void> {
  try {
    await client.post('/v1/users/logout', { refresh_token: refresh })
  } catch {
    // Swallow — the user is logging out either way. The server will
    // eventually expire the family naturally.
  }
}

const STORAGE_ACCESS = 'casanova.access_token'
const STORAGE_REFRESH = 'casanova.refresh_token'
const STORAGE_USER = 'casanova.user'

export interface AuthUser {
  id: number
  username: string
  role: string
  nickname?: string
  email?: string
  avatar?: string
}

export const useAuthStore = defineStore('auth', () => {
  const accessToken = ref<string>('')
  const refreshToken = ref<string>('')
  const user = ref<AuthUser | null>(null)
  const hydrated = ref(false)

  const isAuthenticated = computed(() => accessToken.value !== '')

  const AVATAR_FALLBACK = '/assets/brand/casanova.png'
  // Rendered avatar image.
  //   • data:/blob:/http URL -> use verbatim (a freshly-uploaded picture is a
  //     data: URL held this session)
  //   • server filesystem path (from a fresh login row) -> fetch the bytes
  //     through the AUTHENTICATED axios client and bind a blob: URL. We do NOT
  //     put the JWT in an <img src> query (`?token=`) — that leaks the live
  //     access token into proxy/gateway logs, the browser disk cache (keyed by
  //     the JWT), and the DOM, where anyone with log access can replay it.
  //   • empty -> brand default
  const avatarSrc = ref<string>(AVATAR_FALLBACK)
  let avatarObjectUrl: string | null = null
  function setAvatar(v: string) {
    if (avatarObjectUrl && v !== avatarObjectUrl) {
      URL.revokeObjectURL(avatarObjectUrl)
      avatarObjectUrl = null
    }
    avatarSrc.value = v
  }
  async function refreshAvatar() {
    const a = user.value?.avatar
    if (!a) return setAvatar(AVATAR_FALLBACK)
    if (a.startsWith('data:') || a.startsWith('blob:') || a.startsWith('http')) return setAvatar(a)
    try {
      // Authorization header is added by the axios interceptor — never the URL.
      const res = await client.get('/v1/users/avatar', { responseType: 'blob' })
      const url = URL.createObjectURL(res.data as Blob)
      setAvatar(url)
      avatarObjectUrl = url
    } catch {
      setAvatar(AVATAR_FALLBACK)
    }
  }
  // Re-resolve when the avatar path or the session changes (login/logout).
  watch(() => [user.value?.avatar, accessToken.value], refreshAvatar, { immediate: true })

  // Pull persisted tokens on first use. Idempotent — safe to call from
  // the router guard on every navigation, but the `hydrated` flag
  // short-circuits after the first successful load.
  function hydrate() {
    if (hydrated.value) return
    hydrated.value = true
    const a = localStorage.getItem(STORAGE_ACCESS)
    const r = localStorage.getItem(STORAGE_REFRESH)
    const u = localStorage.getItem(STORAGE_USER)
    if (a) accessToken.value = a
    if (r) refreshToken.value = r
    if (u) {
      try {
        user.value = JSON.parse(u) as AuthUser
      } catch {
        // Corrupt cached user — clear the slot so the next login re-seeds it.
        localStorage.removeItem(STORAGE_USER)
      }
    }
    // Wave 29: pull per-user UI prefs from the backend so reloads on a
    // new device or browser don't fall back to defaults. Fire-and-forget
    // — the call is debounced internally and stores read from the warm
    // localStorage cache while it's in flight.
    if (user.value?.id) {
      void hydrateFromBackend(user.value.id)
    }
  }

  function setSession(access: string, refresh: string, u: AuthUser) {
    accessToken.value = access
    refreshToken.value = refresh
    user.value = u
    localStorage.setItem(STORAGE_ACCESS, access)
    localStorage.setItem(STORAGE_REFRESH, refresh)
    localStorage.setItem(STORAGE_USER, JSON.stringify(u))
    // Re-arm the 401 interceptor's logout latch. Without this, a user
    // who was just booted to /login by a 401 and then signed back in
    // would hit a permanently-tripped latch, causing the next 401
    // during the new session to be swallowed without redirect.
    resetAuthClientLatch()
    // Wave 29: kick off the per-user UI-prefs hydrate as soon as login
    // succeeds so the dashboard renders with the user's saved layout
    // instead of the localStorage cache from a previous occupant.
    void hydrateFromBackend(u.id)
  }

  // Wave 36: swap in a rotated access/refresh pair after the 401
  // interceptor renews the session. Deliberately not setSession(): that
  // one re-fires the UI-prefs hydrate and needs a user row, and /v1/users/
  // refresh returns tokens only — the account behind them is unchanged.
  function setTokens(access: string, refresh: string) {
    accessToken.value = access
    refreshToken.value = refresh
    localStorage.setItem(STORAGE_ACCESS, access)
    localStorage.setItem(STORAGE_REFRESH, refresh)
  }

  // Wave 36: adopt an access/refresh pair another tab has already
  // rotated into localStorage, reporting whether there was one to take.
  //
  // Refresh is rotating and strict server-side: presenting a jti a
  // sibling tab has already spent reads as token theft and burns the
  // whole family, logging every tab out. Since every open tab polls,
  // they all hit the 3 h expiry within milliseconds of each other, so
  // whichever tab loses the race has to ride on the winner's tokens
  // instead of presenting its own.
  //
  // On its own this check does NOT make the loser safe: called before
  // the winner's POST comes back it truthfully reports "nothing new to
  // take", and the loser goes on to spend the same jti. It only works
  // as the second half of a pair — api/client.ts holds a cross-tab lock
  // (Web Locks, or a localStorage claim on the plain-http origins where
  // Web Locks is unavailable) and re-runs this the moment it gets in,
  // by which point the winner has already published.
  //
  // Only adopt while the persisted user row is still the same account —
  // otherwise a tab left open by a previous occupant would silently
  // start acting as whoever logged in after them.
  function adoptStoredTokens(): boolean {
    const a = localStorage.getItem(STORAGE_ACCESS)
    const r = localStorage.getItem(STORAGE_REFRESH)
    if (!a || !r || a === accessToken.value) return false
    const u = localStorage.getItem(STORAGE_USER)
    if (!u) return false
    try {
      if ((JSON.parse(u) as AuthUser).id !== user.value?.id) return false
    } catch {
      return false
    }
    accessToken.value = a
    refreshToken.value = r
    return true
  }

  function updateUser(fields: Partial<AuthUser>) {
    if (!user.value) return
    user.value = { ...user.value, ...fields }
    localStorage.setItem(STORAGE_USER, JSON.stringify(user.value))
  }

  function logout() {
    // Wave 29: best-effort server-side revocation. Without this call
    // the refresh token in localStorage stays valid until its TTL — a
    // stolen device could keep refreshing access tokens after a "log
    // out". We fire-and-forget so a backend hiccup doesn't block the
    // local clear; the local-side wipe happens regardless.
    const tokenToRevoke = refreshToken.value
    if (tokenToRevoke) {
      void revokeRefreshFamily(tokenToRevoke)
    }
    accessToken.value = ''
    refreshToken.value = ''
    user.value = null
    localStorage.removeItem(STORAGE_ACCESS)
    localStorage.removeItem(STORAGE_REFRESH)
    localStorage.removeItem(STORAGE_USER)

    // Wave 29: wipe the per-user UI prefs from the local cache so a
    // different user logging in on the same browser doesn't inherit
    // the previous occupant's theme, app order, or dashboard tweaks.
    // The backend copy is left intact — it'll be re-fetched on the
    // next login.
    clearLocalPrefs()

    // Signal all stores to stop polling and release resources.
    // Other stores (system, notifications, etc.) listen for this event
    // so they can tear down intervals and abort in-flight requests that
    // would otherwise continue using the now-stale token.
    window.dispatchEvent(new CustomEvent('auth-logout'))
  }

  return {
    accessToken,
    refreshToken,
    user,
    isAuthenticated,
    avatarSrc,
    hydrate,
    setSession,
    setTokens,
    adoptStoredTokens,
    updateUser,
    logout,
  }
})
