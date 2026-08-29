// safeCall — unified axios wrapper that every store uses to reach the
// backend with a fixture-fallback escape hatch baked in. The dev
// scaffold runs against the real CasaNova services via the Vite proxy
// when they're up, and silently falls back to the in-memory fixture
// when they aren't, so the UI stays usable end-to-end without a
// running backend but never silently hides a real failure in prod.
//
// Usage pattern from a store:
//
//   const result = await safeCall(
//     () => api.getSystemUtilization(),
//     () => ({ cpu: 14, mem: 38, ...fixture }),
//   )
//   if (result.fixture) store.fixtureMode = true
//   store.stats = result.data
//
// Production deploys set VITE_STRICT_API=1 which disables the
// fallback entirely — any failure becomes an error the store can
// surface to the user via the toast queue.

import { AxiosError } from 'axios'

// Compile-time gate. When true, fallbacks never fire and every error
// bubbles up for the store to handle. Production builds should ship
// this flag ON; dev + preview builds leave it off so the UI is alive
// without a backend.
const STRICT_API =
  typeof import.meta !== 'undefined' && import.meta.env?.VITE_STRICT_API === '1'

export interface SafeCallResult<T> {
  data: T  // undefined when error is non-null — always check error first
  // True when the returned data came from the fixture fallback — stores
  // use this to flip a `fixtureMode` flag and skip mutation writes
  // that would otherwise hit a dead backend.
  fixture: boolean
  // Present when the API call failed AND no fallback was provided
  // (or STRICT_API is on). Stores surface this to the UI.
  error: string | null
  // The raw HTTP status code when the failure came from an HTTP
  // response (as opposed to a network-level failure).
  status: number | null
}

/**
 * Detect whether an axios error should fall through to the fixture.
 * Network errors (no response, ECONNREFUSED) always count. 5xx from
 * the Vite proxy is treated as "backend down" because the proxy
 * returns 500/502/504 when the upstream CasaNova server is unreachable.
 * 4xx errors (client errors) never fall back — those are real API
 * mistakes and the caller needs to see them.
 */
function shouldFallback(err: unknown): boolean {
  // Non-Axios errors are programming bugs (TypeError, SyntaxError) —
  // they must surface, not silently fall back to fixtures.
  if (!(err instanceof AxiosError)) return false
  if (err.code === 'ERR_NETWORK') return true
  if (err.message?.includes('Network Error')) return true
  const status = err.response?.status
  if (typeof status !== 'number') return true
  // Vite proxy bubbles backend-down as 5xx; a real 5xx from the real
  // backend also deserves the fallback in dev so the user sees data.
  // In strict mode neither falls back.
  return status >= 500
}

/**
 * safeCall — run the real API call, fall back to the fixture factory
 * on network/proxy failures, and return a discriminated result the
 * store can reason about without try/catch boilerplate at every site.
 */
export async function safeCall<T>(
  apiCall: () => Promise<T>,
  fixture?: () => T | Promise<T>,
): Promise<SafeCallResult<T>> {
  try {
    const data = await apiCall()
    return { data, fixture: false, error: null, status: null }
  } catch (err) {
    const axiosErr = err instanceof AxiosError ? err : null
    const status = axiosErr?.response?.status ?? null
    const errorMsg =
      axiosErr?.response?.data &&
      typeof axiosErr.response.data === 'object' &&
      'message' in (axiosErr.response.data as Record<string, unknown>)
        ? String((axiosErr.response.data as { message: unknown }).message)
        : axiosErr?.message ?? String(err)

    if (!STRICT_API && fixture && shouldFallback(err)) {
      try {
        const data = await fixture()
        return { data, fixture: true, error: null, status }
      } catch {
        // Fixture itself threw — fall through to the error path.
      }
    }

    return {
      // Casting to T because callers always check `error` first in
      // strict mode, and in dev mode they fall through to their own
      // zero-value when data is needed.
      data: undefined as unknown as T,
      fixture: false,
      error: errorMsg,
      status,
    }
  }
}

/**
 * isReachable — cheap health check against /ping so components can
 * show a "backend offline" banner without waiting for a real API call
 * to fail. Times out after 2 seconds so a hung backend doesn't stall
 * the UI.
 */
export async function isReachable(client: { get: (url: string, config?: unknown) => Promise<unknown> }): Promise<boolean> {
  try {
    await client.get('/ping', { timeout: 2000 })
    return true
  } catch {
    return false
  }
}
