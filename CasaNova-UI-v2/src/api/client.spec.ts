// Focused tests for the 401 → refresh → retry path in the axios client.
//
// Regression context: the access token expires after 3 h and nothing ever
// renewed it, so the first request past that mark wiped the session and
// dropped the user on /login mid-action. The renewal that fixes it has
// two failure modes that only appear under concurrency and both are
// silent:
//   - one refresh per failed request rotates the family more than once,
//     which the server reads as token reuse and revokes the whole chain
//     — the fix would log the user out harder than the bug did;
//   - retrying a request that 401s again loops forever.
// These pin both invariants, plus the fallback to the old logout path
// when the refresh token is genuinely dead.
//
// The single-flight promise only covers one tab, so the renewal also
// runs under a cross-tab lock. There is no navigator in this runner's
// node environment, which is exactly the branch that matters: Web Locks
// is secure-context only and absent on the plain-http LAN origins most
// CasaNova boxes are reached at, so these specs exercise the
// localStorage-claim fallback that carries those installs.

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

const { routerPush } = vi.hoisted(() => ({ routerPush: vi.fn(async () => undefined) }))

vi.mock('@/router', () => ({
  default: {
    currentRoute: { value: { name: 'dashboard' } },
    push: routerPush,
  },
}))

vi.mock('@/composables/syncedPrefs', () => ({
  hydrateFromBackend: vi.fn(),
  clearLocalPrefs: vi.fn(),
}))

import {
  AxiosError,
  type AxiosResponse,
  type InternalAxiosRequestConfig,
} from 'axios'
import client, { resetAuthClientLatch } from './client'
import { useAuthStore } from '@/stores/auth'

// These specs run in the default node environment (there is no jsdom in
// devDependencies), so the two browser globals the auth store touches —
// token persistence and the 'auth-logout' broadcast — get stand-ins.
const cells = new Map<string, string>()
globalThis.localStorage = {
  getItem: (k: string) => (cells.has(k) ? (cells.get(k) as string) : null),
  setItem: (k: string, v: string) => {
    cells.set(k, String(v))
  },
  removeItem: (k: string) => {
    cells.delete(k)
  },
  clear: () => cells.clear(),
  key: (i: number) => [...cells.keys()][i] ?? null,
  get length() {
    return cells.size
  },
} as Storage
const dispatchEvent = vi.fn(() => true)
globalThis.window = {
  addEventListener: () => undefined,
  removeEventListener: () => undefined,
  dispatchEvent,
} as unknown as Window & typeof globalThis

// Mirrors CLAIM_KEY in client.ts. The cross-tab lock is a localStorage
// protocol, so a rename that broke it would be invisible to the type
// checker — pinning the literal here is the only thing that catches it.
const CLAIM_KEY = 'cn.auth.refreshClaim'

function ok(config: InternalAxiosRequestConfig, data: unknown): AxiosResponse {
  return { data, status: 200, statusText: 'OK', headers: {}, config } as AxiosResponse
}

function unauthorized(config: InternalAxiosRequestConfig): AxiosError {
  const response = {
    data: { message: 'unauthorized' },
    status: 401,
    statusText: 'Unauthorized',
    headers: {},
    config,
  } as AxiosResponse
  return new AxiosError('unauthorized', 'ERR_BAD_REQUEST', config, undefined, response)
}

// Counters for the fake backend installed as the axios adapter.
let refreshCalls = 0
let protectedCalls = 0
let logoutCalls = 0

// `renewable` mints A2/R2 on refresh; `dead` rejects the refresh token.
// `protectedAlways401` keeps rejecting even the renewed token, which is
// what a session that really is over looks like. `logout401` is the
// revocation endpoint answering "that session was already gone".
function installAdapter(opts: {
  renewable: boolean
  protectedAlways401?: boolean
  logout401?: boolean
}) {
  client.defaults.adapter = async (config: InternalAxiosRequestConfig) => {
    const url = config.url ?? ''
    if (url === '/v1/users/refresh') {
      refreshCalls++
      if (!opts.renewable) throw unauthorized(config)
      return ok(config, {
        success: 200,
        message: 'ok',
        data: { access_token: 'A2', refresh_token: 'R2', expires_at: 0 },
      })
    }
    // The real /v1/users/logout is mounted outside the JWT group, so it
    // answers 200 even with a dead access token.
    if (url === '/v1/users/logout') {
      logoutCalls++
      if (opts.logout401) throw unauthorized(config)
      return ok(config, { success: 200 })
    }
    protectedCalls++
    const bearer = String(config.headers?.Authorization ?? '')
    if (bearer === 'Bearer A2' && !opts.protectedAlways401) return ok(config, { fine: true })
    throw unauthorized(config)
  }
}

beforeEach(() => {
  setActivePinia(createPinia())
  cells.clear()
  refreshCalls = 0
  protectedCalls = 0
  logoutCalls = 0
  routerPush.mockClear()
  dispatchEvent.mockClear()
  resetAuthClientLatch()
  useAuthStore().setSession('A1', 'R1', { id: 1, username: 'tester', role: 'admin' })
})

describe('access-token renewal', () => {
  it('refreshes once for a burst of concurrent 401s and retries each request', async () => {
    installAdapter({ renewable: true })

    const results = await Promise.all(
      ['a', 'b', 'c', 'd', 'e'].map((n) => client.get(`/v1/things/${n}`)),
    )

    // The invariant that matters: one rotation, not five. A second
    // rotation with the same token burns the family server-side.
    expect(refreshCalls).toBe(1)
    expect(results.every((r) => r.status === 200)).toBe(true)
    // 5 initial failures + 5 retries.
    expect(protectedCalls).toBe(10)
    const auth = useAuthStore()
    expect(auth.accessToken).toBe('A2')
    expect(auth.refreshToken).toBe('R2')
    expect(routerPush).not.toHaveBeenCalled()
  })

  it('retries a request exactly once, then logs out instead of looping', async () => {
    installAdapter({ renewable: true, protectedAlways401: true })

    await expect(client.get('/v1/things/a')).rejects.toMatchObject({
      response: { status: 401 },
    })

    expect(protectedCalls).toBe(2)
    expect(refreshCalls).toBe(1)
    expect(useAuthStore().accessToken).toBe('')
    expect(routerPush).toHaveBeenCalledWith({ name: 'login' })
  })

  it('falls back to the logout + /login path when the refresh token is dead', async () => {
    installAdapter({ renewable: false })

    await expect(client.get('/v1/things/a')).rejects.toMatchObject({
      response: { status: 401 },
    })

    expect(refreshCalls).toBe(1)
    expect(protectedCalls).toBe(1)
    const auth = useAuthStore()
    expect(auth.accessToken).toBe('')
    expect(auth.refreshToken).toBe('')
    expect(routerPush).toHaveBeenCalledWith({ name: 'login' })
  })

  it('adopts the pair a sibling tab rotated instead of spending its own', async () => {
    installAdapter({ renewable: true })
    // What another tab's successful refresh leaves behind: newer tokens
    // in storage for the same account, while this tab still holds A1/R1
    // in memory.
    localStorage.setItem('casanova.access_token', 'A2')
    localStorage.setItem('casanova.refresh_token', 'R2')

    const res = await client.get('/v1/things/a')

    expect(res.status).toBe(200)
    // No POST /v1/users/refresh at all — presenting R1 here would be the
    // reuse that revokes the family for every tab.
    expect(refreshCalls).toBe(0)
    expect(useAuthStore().accessToken).toBe('A2')
  })
})

describe('cross-tab renewal lock', () => {
  it('waits out the tab holding the claim instead of spending the same jti', async () => {
    installAdapter({ renewable: true })
    // What the losing tab actually sees at the expiry boundary: a
    // sibling grabbed the claim microseconds ago and its POST is still
    // in flight, so storage still holds the un-rotated pair and
    // adoptStoredTokens has nothing to report. Without the claim this
    // tab would go ahead and present R1 a second time — the reuse the
    // server reads as theft.
    localStorage.setItem(CLAIM_KEY, `sibling:${Date.now()}`)
    setTimeout(() => {
      localStorage.setItem('casanova.access_token', 'A2')
      localStorage.setItem('casanova.refresh_token', 'R2')
      localStorage.removeItem(CLAIM_KEY)
    }, 200)

    const res = await client.get('/v1/things/a')

    expect(res.status).toBe(200)
    expect(refreshCalls).toBe(0)
    expect(useAuthStore().accessToken).toBe('A2')
    expect(routerPush).not.toHaveBeenCalled()
  })

  it('takes over a claim left behind by a tab that died mid-refresh', async () => {
    installAdapter({ renewable: true })
    // Old enough that its holder cannot still be in flight — the tab was
    // closed or crashed. Honouring it would strand every other tab until
    // the refresh token expired on its own.
    localStorage.setItem(CLAIM_KEY, `ghost:${Date.now() - 60_000}`)

    const res = await client.get('/v1/things/a')

    expect(res.status).toBe(200)
    expect(refreshCalls).toBe(1)
    expect(useAuthStore().accessToken).toBe('A2')
    // Released, not leaked: a claim left behind here would cost the next
    // renewal the whole staleness window.
    expect(localStorage.getItem(CLAIM_KEY)).toBeNull()
  })
})

describe('logout revocation', () => {
  it('does not re-enter the session sweep when the revocation POST 401s', async () => {
    installAdapter({ renewable: true, logout401: true })

    useAuthStore().logout()
    // logout() fires the revocation fire-and-forget; give the rejected
    // POST time to travel back through the response interceptor.
    await new Promise((resolve) => setTimeout(resolve, 300))

    expect(logoutCalls).toBe(1)
    // A 401 from the revocation endpoint means the session was already
    // dead — the one moment the sweep must NOT run again. Before the
    // exemption this bounced back through the interceptor and called
    // logout() a second time: two 'auth-logout' events and a redirect
    // fired from inside the teardown that was already under way.
    expect(dispatchEvent).toHaveBeenCalledTimes(1)
    expect(routerPush).not.toHaveBeenCalled()
    expect(refreshCalls).toBe(0)
  })
})
