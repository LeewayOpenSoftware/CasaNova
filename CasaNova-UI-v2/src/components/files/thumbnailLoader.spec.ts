// Tests for the thumbnail request queue.
//
// Regression context: FileThumbnail used to call the thumbnail endpoint
// straight from its setup watcher, so a directory of 500 photos issued
// 500 GETs in one tick — past the browser's 6-connections-per-origin
// limit, which stalled every other request the page wanted to make. The
// queue is what bounds that, so these pin its contract:
//   - never more than MAX_CONCURRENT requests in flight;
//   - a request cancelled while still queued never reaches the network
//     (the tile scrolled away / unmounted before its turn);
//   - the same path is fetched once no matter how many tiles ask;
//   - a cancel that arrives after its job already left the queue touches
//     nothing (it used to evict a *different* job from the dedupe map);
//   - logout forgets everything, so the next user on this tab can't be
//     served the previous one's thumbnails out of the cache.

import { describe, it, expect, beforeEach, vi } from 'vitest'

const started: string[] = []
const settle = new Map<string, (url: string) => void>()
const fail = new Map<string, (err: unknown) => void>()

vi.mock('@/api', () => ({
  filesApi: {
    fetchImageThumbnail: (path: string) =>
      new Promise<string>((resolve, reject) => {
        started.push(path)
        settle.set(path, resolve)
        fail.set(path, reject)
      }),
  },
}))

// Vitest runs in the default node environment, so there is no `window`
// and no blob-URL registry. The loader registers its logout teardown on
// `window` (guarded) and revokes through `URL`, so stand in for both:
// a minimal event target we can fire 'auth-logout' on the way auth.ts
// does, and a revoke spy so the test can see the blobs being released.
const logoutHandlers: Array<() => void> = []
vi.stubGlobal('window', {
  addEventListener: (type: string, fn: () => void) => {
    if (type === 'auth-logout') logoutHandlers.push(fn)
  },
  removeEventListener: () => {},
})

const revoked: string[] = []
vi.spyOn(URL, 'revokeObjectURL').mockImplementation((url: string) => {
  revoked.push(url)
})

function fireLogout() {
  for (const fn of [...logoutHandlers]) fn()
}

// Let the promise chain inside pump() (fetch → cache hook → resolve →
// finish → pump) run to quiescence before asserting on `started`.
async function drainMicrotasks() {
  for (let i = 0; i < 5; i++) await Promise.resolve()
}

// The queue is module-level state, so each test gets a fresh copy.
async function freshLoader() {
  vi.resetModules()
  started.length = 0
  settle.clear()
  fail.clear()
  revoked.length = 0
  logoutHandlers.length = 0
  return import('./thumbnailLoader')
}

describe('requestThumbnail', () => {
  beforeEach(() => {
    started.length = 0
    settle.clear()
    fail.clear()
    revoked.length = 0
  })

  it('caps how many fetches run at once and drains the queue', async () => {
    const { requestThumbnail } = await freshLoader()

    const paths = Array.from({ length: 10 }, (_, i) => `/pics/${i}.jpg`)
    const requests = paths.map((p) => requestThumbnail(p))

    expect(started).toEqual(paths.slice(0, 4))

    // Completing one request lets exactly one more start.
    settle.get(paths[0])!('blob:0')
    await requests[0].promise
    expect(started).toEqual(paths.slice(0, 5))

    for (const p of paths.slice(1)) {
      settle.get(p)?.(`blob:${p}`)
      await Promise.resolve()
    }
    await Promise.all(requests.map((r) => r.promise))
    expect(started).toEqual(paths)
  })

  it('never fetches a request cancelled while still queued', async () => {
    const { requestThumbnail } = await freshLoader()

    const paths = Array.from({ length: 6 }, (_, i) => `/pics/${i}.jpg`)
    const requests = paths.map((p) => requestThumbnail(p))
    // Callers must handle the cancellation rejection.
    requests.forEach((r) => r.promise.catch(() => {}))

    expect(started).toHaveLength(4)

    // The two still waiting scroll out of view before their turn.
    requests[4].cancel()
    requests[5].cancel()

    settle.get(paths[0])!('blob:0')
    settle.get(paths[1])!('blob:1')
    await requests[0].promise
    await requests[1].promise
    await Promise.resolve()

    expect(started).toEqual(paths.slice(0, 4))
  })

  it('fetches a path once however many tiles ask for it', async () => {
    const { requestThumbnail } = await freshLoader()

    const a = requestThumbnail('/pics/same.jpg')
    const b = requestThumbnail('/pics/same.jpg')
    expect(started).toEqual(['/pics/same.jpg'])

    // One tile going away must not cancel the other's thumbnail.
    a.cancel()
    settle.get('/pics/same.jpg')!('blob:same')
    await expect(b.promise).resolves.toBe('blob:same')

    // Already resolved → served from cache, no second GET.
    const c = requestThumbnail('/pics/same.jpg')
    await expect(c.promise).resolves.toBe('blob:same')
    expect(started).toEqual(['/pics/same.jpg'])
  })

  // release() used to look up `pending` by path and delete whatever it
  // found. This is the sequence where the thing it found was somebody
  // else's job: the retry for the same path lost its dedupe entry and the
  // next tile queued a second GET for a thumbnail already on its way.
  it('ignores a cancel that arrives after its job left the queue', async () => {
    const { requestThumbnail } = await freshLoader()

    const X = '/pics/x.jpg'
    const fillers = ['/pics/f1.jpg', '/pics/f2.jpg', '/pics/f3.jpg', '/pics/f4.jpg']

    const x = requestThumbnail(X)
    const fillerReqs = fillers.map((p) => requestThumbnail(p))
    ;[x, ...fillerReqs].forEach((r) => r.promise.catch(() => {}))
    // x + the first three fillers are in flight; f4 waits in the queue.
    expect(started).toEqual([X, ...fillers.slice(0, 3)])

    // x's GET fails, which drops its cache entry, and f4 takes the slot —
    // so the four in-flight slots are full again.
    fail.get(X)!(new Error('thumbnail 500'))
    await drainMicrotasks()
    expect(started).toEqual([X, ...fillers])

    // Another tile asks for the same path. Slots are full, so this new
    // job is the live queued entry for X in the dedupe map.
    const b = requestThumbnail(X)
    b.promise.catch(() => {})
    expect(started.filter((p) => p === X)).toHaveLength(1)

    // Now the original tile finally unmounts and cancels. Before the
    // identity check this deleted `b` from `pending`.
    x.cancel()

    // A third tile must join `b` rather than queue a duplicate GET.
    const c = requestThumbnail(X)
    c.promise.catch(() => {})

    for (const p of fillers) {
      settle.get(p)!(`blob:${p}`)
      await drainMicrotasks()
    }
    await Promise.all(fillerReqs.map((r) => r.promise))

    // Exactly one retry went out, shared by b and c.
    expect(started.filter((p) => p === X)).toHaveLength(2)
    settle.get(X)!('blob:x-retry')
    await expect(b.promise).resolves.toBe('blob:x-retry')
    await expect(c.promise).resolves.toBe('blob:x-retry')
    expect(started.filter((p) => p === X)).toHaveLength(2)
  })

  // The SPA does not reload on logout, so without this hook the cache
  // survives the session change and hands the next user blob URLs the
  // backend was never asked to authorize.
  it('forgets cached and queued thumbnails when the session ends', async () => {
    const { requestThumbnail } = await freshLoader()

    const PRIVATE = '/DATA/fotos-privadas/1.jpg'
    const a = requestThumbnail(PRIVATE)
    settle.get(PRIVATE)!('blob:private-1')
    await expect(a.promise).resolves.toBe('blob:private-1')

    // Fill the four slots so one more request stays queued.
    const fillers = ['/pics/f1.jpg', '/pics/f2.jpg', '/pics/f3.jpg', '/pics/f4.jpg']
    const fillerReqs = fillers.map((p) => requestThumbnail(p))
    const queued = requestThumbnail('/DATA/fotos-privadas/2.jpg')
    ;[...fillerReqs, queued].forEach((r) => r.promise.catch(() => {}))
    expect(started).not.toContain('/DATA/fotos-privadas/2.jpg')

    fireLogout()

    // The tile still waiting is settled instead of hanging forever, and
    // its GET never goes out.
    await expect(queued.promise).rejects.toThrow(/cancelled/)
    await drainMicrotasks()
    // The blob the previous user's tile was painted from is released
    // rather than pinned for the life of the tab.
    expect(revoked).toEqual(['blob:private-1'])

    // Draining the GETs that were already on the wire must not start the
    // dropped job, and must not push inFlight negative either.
    for (const p of fillers) {
      settle.get(p)!(`blob:${p}`)
      await drainMicrotasks()
    }
    expect(started).not.toContain('/DATA/fotos-privadas/2.jpg')

    // The next user opening the same folder gets a real GET — the
    // backend, not this cache, decides whether they may see it.
    const next = requestThumbnail(PRIVATE)
    next.promise.catch(() => {})
    expect(started.filter((p) => p === PRIVATE)).toHaveLength(2)
  })
})
