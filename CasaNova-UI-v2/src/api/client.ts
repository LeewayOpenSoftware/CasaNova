// CasaNova API client.
//
// Single axios instance with two interceptors:
//
//   1. Request: pulls the current access token from the Pinia auth store
//      and attaches it as a Bearer header. The old UI stored the token in
//      Vuex and re-read it from localStorage on every request; we use
//      Pinia's in-memory ref (with localStorage persistence in the store)
//      so we pay exactly one storage read per page load.
//
//   2. Response: intercepts 401s and tries to renew the access token
//      through the refresh endpoint, retrying the failed request once.
//      Only if the renewal really fails does it clear the session and
//      bounce to /login. Skips the login endpoint itself so a
//      bad-password 401 doesn't nuke a valid session in another tab.
//
// Base URL is empty in dev so Vite's proxy handles the host/port; in
// production builds the frontend is served from the same origin as the
// backend so relative paths Just Work.

import axios, { type AxiosInstance, type InternalAxiosRequestConfig, AxiosError } from 'axios'
import { useAuthStore } from '@/stores/auth'
import router from '@/router'

const client: AxiosInstance = axios.create({
  baseURL: '',
  // 10 seconds is enough for any dashboard/settings API call. Long
  // operations (file uploads, compose installs) set their own timeout
  // per-request. The old 60s value meant the UI hung for a full minute
  // when the backend was slow or unreachable before the safeCall
  // fixture fallback could kick in.
  timeout: 10_000,
  // No default Content-Type — Axios auto-sets application/json for
  // plain objects and multipart/form-data (with boundary) for FormData.
  // A hardcoded default interferes with FormData uploads.
})

client.interceptors.request.use((config) => {
  const auth = useAuthStore()
  if (auth.accessToken) {
    config.headers.Authorization = `Bearer ${auth.accessToken}`
  }
  return config
})

// Endpoints whose 401 is expected (bad credentials, wrong SSH password,
// stale resource lookups) and must NOT trigger a session wipe / redirect.
//
// Wave 35-S: the SSH login dialog calls `/v1/sys/ssh-login` with the
// user's UNIX credentials. The backend returns 401 when the SSH password
// is wrong — that 401 means "wrong shell password", not "your CasaNova
// session expired". The previous list only exempted the auth endpoints,
// so a single typo in the SSH dialog tore the user out of the entire UI
// and dumped them on /login as if their CasaNova session had died. The
// websocket terminal endpoint `/v1/sys/wsssh` has the same property —
// any auth failure on the WS upgrade is local to that dialog and must
// not nuke the global session.
const AUTH_EXEMPT_URLS = [
  '/v1/users/login',
  '/v1/users/register',
  '/v1/users/refresh',
  '/v1/sys/ssh-login',
  '/v1/sys/wsssh',
  // The revocation endpoint auth.logout() fires on its way out. A 401
  // from it means "that session was already dead", which is exactly the
  // moment the session sweep must NOT run again: without this entry the
  // POST re-entered this interceptor, spent the (already dead) refresh
  // token on a renewal, then called auth.logout() a second time —
  // double 'auth-logout' event, double storage wipe, double redirect.
  '/v1/users/logout',
  // The message-bus (YSK cards, live event subscriptions) authenticates
  // service-to-service. A 401 from it means an inter-service auth hiccup
  // — NOT that the user's CasaNova session died. Before this, the YSK
  // card fetch on dashboard load returning 401 tore the user straight
  // back to /login ("enters for 5ms and bounces"). A bus 401 must never
  // nuke the global session.
  '/v2/message_bus/',
  '/message_bus/',
]

// Guard against multiple concurrent 401 responses each triggering
// separate logout + redirect cycles.
//
// The previous implementation used a boolean flag reset via a 2s
// setTimeout. That had two problems:
//   1. If the redirect took longer than 2s (slow machine, slow route
//      guard, heavy boot chunk), queued 401 responses landing after
//      the timeout would re-trigger `auth.logout()` + `router.push()`
//      and bounce the user through /login twice.
//   2. The timeout was racing the router transition, so a legitimate
//      concurrent 401 arriving during the transition could silently
//      slip past the guard because `isLoggingOut` had already been
//      cleared.
//
// Instead we hold a Promise latch. The first 401 creates the logout
// promise and every other concurrent 401 awaits the same promise —
// there is no magic timeout. The latch is cleared explicitly via
// `resetAuthClientLatch()`, which the auth store calls from its
// `setSession()` success path so a fresh login on the /login page
// re-arms the interceptor for the next session.
let logoutInFlight: Promise<void> | null = null

export function resetAuthClientLatch() {
  logoutInFlight = null
}

function performLogout(): Promise<void> {
  const auth = useAuthStore()
  auth.logout()
  // Only force-redirect when we weren't already on /login, else an
  // authentication-failed login attempt triggers an infinite loop.
  if (router.currentRoute.value.name !== 'login') {
    // router.push returns a Promise that resolves once the navigation
    // settles. We return it so all concurrent 401 handlers wait for
    // the same transition instead of each trying to push independently.
    return router.push({ name: 'login' }).then(() => undefined)
  }
  return Promise.resolve()
}

// Wave 36: access-token renewal.
//
// The access token lives 3 h and the refresh token 7 days, but nothing
// ever exchanged one for the other: the first request made past the 3 h
// mark got a 401, the session was wiped and the user was dumped on
// /login in the middle of whatever they were doing — an upload, a copy,
// an app's config form — while a perfectly valid refresh token sat in
// localStorage the whole time.
//
// POST /v1/users/refresh is deliberately mounted outside the JWT group
// (it validates the refresh token itself), so the dead access token the
// request interceptor stamps on it is harmless. What is NOT harmless is
// presenting the same refresh token twice: the server rotates the family
// on every call and reads a spent jti as theft, revoking the entire
// chain. Five requests 401-ing at once must therefore produce exactly
// ONE refresh — hence the single-flight promise every caller awaits.
//
// That promise only spans ONE tab, which is not enough. Every open tab
// polls, so two dashboards cross the 3 h expiry milliseconds apart:
// both 401, both look at localStorage and find the un-rotated pair
// still there (so adoptStoredTokens reports nothing to take), and both
// present the same jti. That is precisely the reuse the server reads as
// theft — it revokes the family and throws the user out everywhere. The
// per-tab latch therefore has to sit inside a CROSS-TAB lock; see
// withRenewalLock below.
let renewalInFlight: Promise<boolean> | null = null

// Cross-tab mutual exclusion for the renewal.
//
// Web Locks is the right primitive, but it is secure-context only and
// CasaNova is routinely reached at http://<lan-ip>, where
// navigator.locks is simply absent. The localStorage claim below is
// therefore the path most installs actually take, not a museum piece
// for ancient browsers.
const RENEWAL_LOCK_NAME = 'casanova.token-renewal'
// The claim is per-browser bookkeeping, never account data, so it lives
// under the `cn.` prefix syncedPrefs ignores. Under `casanova.` the
// logout-time prefs wipe (clearLocalPrefs) would yank a live holder's
// claim out from under it, and the whole point is that nobody else can.
const CLAIM_KEY = 'cn.auth.refreshClaim'
// The claim is written, then read back after a beat. Two tabs that both
// saw a free slot both write and only the last writer survives; whoever
// reads back a foreign owner id backs off. The pause is what makes the
// read-back mean anything — an immediate one races the sibling's write.
const CLAIM_SETTLE_MS = 40
const CLAIM_POLL_MS = 50
// A claim older than this counts as abandoned (tab closed or crashed
// mid-refresh). It must exceed the client timeout, or we would steal the
// claim from a holder whose POST is merely slow and double-spend the
// refresh token — the exact failure the lock exists to prevent.
const CLAIM_STALE_MS = 12_000
// How long a loser keeps looking for the winner's rotated pair. The
// normal case is one round trip and resolves in well under a second;
// the headroom past CLAIM_STALE_MS exists so a holder that dies without
// releasing is taken over rather than waited on forever. Giving up early
// is not the safe option here — it means logout(), which revokes the
// family and drags the winning tab down with us.
const CLAIM_WAIT_MS = CLAIM_STALE_MS + 2_000

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// lib.dom types `navigator.locks` as always present; it is not (plain
// http, and the node environment the specs run in has no navigator at
// all), so it is probed rather than trusted.
function lockManager(): LockManager | undefined {
  if (typeof navigator === 'undefined') return undefined
  return (navigator as Navigator & { locks?: LockManager }).locks
}

function readClaim(): { owner: string; at: number } | null {
  const raw = localStorage.getItem(CLAIM_KEY)
  if (!raw) return null
  const sep = raw.lastIndexOf(':')
  const at = sep < 0 ? NaN : Number(raw.slice(sep + 1))
  // A malformed value must not pin the lock shut forever — read it as
  // no claim at all.
  if (!Number.isFinite(at)) return null
  return { owner: raw.slice(0, sep), at }
}

async function tryClaim(owner: string): Promise<boolean> {
  const held = readClaim()
  if (held && held.owner !== owner && Date.now() - held.at < CLAIM_STALE_MS) return false
  try {
    localStorage.setItem(CLAIM_KEY, `${owner}:${Date.now()}`)
  } catch {
    // No storage to put a claim in (private mode, quota). There is no
    // lock to take, so proceed unserialized — that is the behaviour we
    // had before the lock existed, and it beats failing the renewal and
    // logging the user out for certain.
    return true
  }
  await sleep(CLAIM_SETTLE_MS)
  return readClaim()?.owner === owner
}

function releaseClaim(owner: string): void {
  // Only ever drop our own claim: past CLAIM_STALE_MS another tab may
  // have legitimately taken it over while we were still in flight.
  if (readClaim()?.owner === owner) localStorage.removeItem(CLAIM_KEY)
}

// Runs `renew` with at most one tab in it at a time, browser-wide.
async function withRenewalLock(renew: () => Promise<boolean>): Promise<boolean> {
  const locks = lockManager()
  if (locks) return locks.request(RENEWAL_LOCK_NAME, renew)

  const auth = useAuthStore()
  // Not a secret, just a tiebreaker — Math.random keeps it working on
  // the plain-http origins where crypto.randomUUID is unavailable.
  const owner = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  const giveUpAt = Date.now() + CLAIM_WAIT_MS
  for (;;) {
    if (await tryClaim(owner)) {
      try {
        return await renew()
      } finally {
        releaseClaim(owner)
      }
    }
    // Lost the race. The winner writes the rotated pair to localStorage
    // the moment its POST lands, so the way out is to keep watching for
    // it — never to present our own copy of the same jti.
    if (auth.adoptStoredTokens()) return true
    if (Date.now() >= giveUpAt) return auth.adoptStoredTokens()
    await sleep(CLAIM_POLL_MS)
  }
}

interface RefreshEnvelope {
  data?: { access_token?: string; refresh_token?: string }
}

// Trades the stored refresh token for a fresh pair and writes it back to
// the auth store. Resolves true only when the store holds a usable access
// token afterwards; every false path lands in the normal logout.
function renewSession(): Promise<boolean> {
  // The whole sequence — re-read storage, POST, write back — runs under
  // the cross-tab lock, and the storage re-read has to be INSIDE it. A
  // tab that lost the race gets in holding a spent refresh token with a
  // freshly rotated pair already sitting in localStorage; it must adopt
  // that pair, not ask for another one.
  return withRenewalLock(renewSessionLocked)
}

async function renewSessionLocked(): Promise<boolean> {
  const auth = useAuthStore()
  // A sibling tab may have rotated the pair already — every open tab
  // polls, so they all 401 within milliseconds of each other. Ride on
  // its tokens rather than presenting our spent one, which would burn
  // the family and log every tab out.
  if (auth.adoptStoredTokens()) return true
  const presented = auth.refreshToken
  if (!presented) return false
  try {
    const res = await client.post<RefreshEnvelope>('/v1/users/refresh', {
      refresh_token: presented,
    })
    const tokens = res.data?.data
    // Both halves or neither: keeping the old refresh token next to a
    // new access token would leave us presenting an already-rotated jti
    // at the next renewal, which is exactly the reuse signal that kills
    // the family.
    if (!tokens?.access_token || !tokens?.refresh_token) return false
    auth.setTokens(tokens.access_token, tokens.refresh_token)
    return true
  } catch {
    // Expired or revoked family, or the box is unreachable. One last
    // look at storage in case we merely lost the race to a sibling tab
    // between the read above and this POST.
    return auth.adoptStoredTokens()
  }
}

// The retry marker rides on the request config so it survives the trip
// back through the interceptor without a side table keyed by request.
type RetriableConfig = InternalAxiosRequestConfig & { _casanovaRetried?: boolean }

client.interceptors.response.use(
  (response) => response,
  async (error: AxiosError) => {
    if (error.response?.status === 401) {
      const url = error.config?.url ?? ''
      const isAuthEndpoint = AUTH_EXEMPT_URLS.some((exempt) => url.includes(exempt))
      if (!isAuthEndpoint) {
        const config = error.config as RetriableConfig | undefined
        // Exactly one renewal attempt per request. A 401 on the retry
        // means the freshly minted token was rejected too — that is a
        // genuinely dead session, so it falls through to the logout
        // below instead of looping.
        if (config && !config._casanovaRetried) {
          if (!renewalInFlight) {
            renewalInFlight = renewSession().finally(() => {
              renewalInFlight = null
            })
          }
          if (await renewalInFlight) {
            config._casanovaRetried = true
            // Replaying the config re-runs the request interceptor,
            // which stamps the new Bearer header — the retry never
            // re-sends the dead one.
            return client(config)
          }
        }
        if (!logoutInFlight) {
          logoutInFlight = performLogout()
        }
        // Concurrent 401s wait for the single in-flight logout to
        // settle, then reject with their own original error. No one
        // re-runs auth.logout() or router.push().
        return logoutInFlight.then(() => Promise.reject(error))
      }
    }
    return Promise.reject(error)
  },
)

export default client
