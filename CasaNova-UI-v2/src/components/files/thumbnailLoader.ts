// Shared plumbing for <FileThumbnail>: a visibility gate and a bounded
// request queue.
//
// Why this exists: FileThumbnail used to fire its GET straight from the
// setup watcher, so opening a folder with 500 photos issued 500 GETs in
// the same tick. The browser only opens 6 connections per origin, so 494
// of them sat queued in the network stack ahead of every other API call
// the page wanted to make (directory listings, the details pane, tab
// switches) — the whole file manager felt frozen until the thumbnail
// storm drained, and the server got hammered generating thumbnails for
// rows the user never scrolled to.
//
// Two guards, both needed:
//   1. observeVisible — nothing is requested until the tile is actually
//      on screen (plus a margin so it's already there by the time the
//      user reaches it). Scrolling past a folder costs zero requests.
//   2. requestThumbnail — of the tiles that DO become visible, at most
//      MAX_CONCURRENT fetches are in flight; the rest wait in a queue and
//      a tile that scrolls away / unmounts before its turn is dropped
//      from the queue without ever hitting the network.

import { filesApi } from '@/api'

// Four leaves headroom under the browser's 6-connections-per-origin cap
// for HTTP/1.1, so a burst of thumbnails can never starve the listing,
// upload or details-pane calls the same page is making.
const MAX_CONCURRENT = 4

// Resolved (or already-started) fetches, keyed on absolute path; value is
// the blob-URL Promise. Blob URLs live as long as the session: the map
// holds onto them until logout (see resetForLogout below), which is fine
// because a thumbnail is a 100x100 PNG (~5 KB) — a full directory of 1000
// images caps at ~5 MB of retained blobs. Rejections drop their entry so a
// later view of the same path can retry (e.g. after a server restart).
const cache = new Map<string, Promise<string>>()

interface PendingJob {
  path: string
  /** Live callers waiting on this job; the job is only droppable at 0. */
  waiters: number
  /**
   * Set the moment the job stops being cancellable — it left the queue
   * for the network, was cancelled, or was torn down on logout. release()
   * is a no-op afterwards; see the comment there for what breaks without
   * it.
   */
  settled: boolean
  promise: Promise<string>
  resolve: (url: string) => void
  reject: (err: unknown) => void
}

// Jobs that have been asked for but haven't left the queue yet, so a
// second tile asking for the same path joins the existing job instead of
// queueing a duplicate GET.
const pending = new Map<string, PendingJob>()
const queue: PendingJob[] = []
let inFlight = 0

export interface ThumbnailRequest {
  promise: Promise<string>
  /**
   * Drop this caller's interest in the thumbnail. Cancels the request
   * outright if it is still queued; a request that already left the queue
   * runs to completion (fetchImageThumbnail takes no AbortSignal), but
   * its result is simply ignored by the caller and kept in the cache.
   */
  cancel: () => void
}

const noop = () => {}

// Cancel handles are handed to component teardown paths that can run more
// than once (a watch-driven refresh followed by onBeforeUnmount), and a
// second release() on the same job would decrement `waiters` a second
// time for a caller that only ever registered once — dropping the job out
// from under the OTHER tiles still waiting on it. One call per handle.
function once(fn: () => void): () => void {
  let done = false
  return () => {
    if (done) return
    done = true
    fn()
  }
}

function finish() {
  inFlight--
  pump()
}

function pump() {
  while (inFlight < MAX_CONCURRENT && queue.length > 0) {
    const job = queue.shift()!
    pending.delete(job.path)
    job.settled = true
    inFlight++
    // Publish to the cache the moment the GET actually goes out, so any
    // later mount of the same path shares this one request.
    const fetching = filesApi.fetchImageThumbnail(job.path).catch((err) => {
      cache.delete(job.path)
      throw err
    })
    cache.set(job.path, fetching)
    // Hand the result over and free the slot in the same turn, so the
    // next queued thumbnail starts as soon as this one lands.
    fetching.then(
      (url) => {
        job.resolve(url)
        finish()
      },
      (err) => {
        job.reject(err)
        finish()
      },
    )
  }
}

function release(job: PendingJob) {
  // Already dequeued (or cancelled, or torn down) — the GET is in flight
  // and can't be taken back, so there is nothing to account for. Bailing
  // here also keeps `waiters` from going negative on a late cancel.
  if (job.settled) return
  job.waiters--
  if (job.waiters > 0) return
  // Identity, not presence: `pending` is keyed by path, and by the time a
  // late cancel arrives the entry under this path may be a DIFFERENT job
  // — the first one left the queue, its GET failed (which drops the cache
  // entry), and another tile re-requested the same path. Deleting on mere
  // presence evicted that live job from the dedupe map while it was still
  // queued, so the next tile asking for the path queued a duplicate GET.
  if (pending.get(job.path) !== job) return
  job.settled = true
  pending.delete(job.path)
  const i = queue.indexOf(job)
  if (i !== -1) queue.splice(i, 1)
  job.reject(new Error('thumbnail request cancelled'))
}

/**
 * requestThumbnail — queue a thumbnail fetch for `path`, respecting the
 * concurrency cap and de-duplicating against work already cached, in
 * flight, or waiting in the queue.
 */
export function requestThumbnail(path: string): ThumbnailRequest {
  const cached = cache.get(path)
  if (cached) return { promise: cached, cancel: noop }

  const queued = pending.get(path)
  if (queued) {
    queued.waiters++
    return { promise: queued.promise, cancel: once(() => release(queued)) }
  }

  let resolve!: (url: string) => void
  let reject!: (err: unknown) => void
  const promise = new Promise<string>((res, rej) => {
    resolve = res
    reject = rej
  })
  const job: PendingJob = { path, waiters: 1, settled: false, promise, resolve, reject }
  pending.set(path, job)
  queue.push(job)
  pump()
  return { promise, cancel: once(() => release(job)) }
}

// Everything above is module-level state that outlives a login session:
// the SPA does not reload on logout (performLogout routes to /login, it
// doesn't hit location.reload), so this module is still holding user A's
// thumbnails when user B signs in on the same tab. requestThumbnail()
// would then serve B a blob URL for /DATA/private/... straight out of the
// cache — no GET, so the backend never gets the chance to apply access
// control to it. Same class of cross-user leak syncedPrefs closed, so the
// same hook the stores use: clear on 'auth-logout'.
function resetForLogout() {
  // Revoke on the way out rather than just dropping the references —
  // an un-revoked blob URL pins its bytes for the life of the tab, which
  // is precisely the retention the cache comment above waves off as
  // bounded "for the session". Entries may still be in flight, so hang
  // the revoke off the promise instead of reading a value we don't have.
  for (const entry of cache.values()) {
    entry.then(
      (url) => {
        try {
          URL.revokeObjectURL(url)
        } catch {
          /* non-browser environment — nothing to revoke */
        }
      },
      () => {
        /* failed fetches never produced a URL */
      },
    )
  }
  cache.clear()
  // Drop the queue the same way a cancel does, so waiting tiles settle
  // instead of hanging forever on a promise nobody will ever fulfil.
  for (const job of queue) {
    job.settled = true
    job.reject(new Error('thumbnail request cancelled'))
  }
  queue.length = 0
  pending.clear()
  // `inFlight` is deliberately NOT reset: those GETs are still running and
  // will each call finish(). Zeroing it here would make those decrements
  // drive the counter negative and let the next directory blow past
  // MAX_CONCURRENT — the exact request storm this module exists to stop.
}

if (typeof window !== 'undefined') {
  window.addEventListener('auth-logout', resetForLogout)
}

// One observer per scroll root, so a directory of N tiles costs one
// observer instead of N. Keyed weakly on the root element: when the file
// browser is torn down the root goes away and its observer with it.
const rootObservers = new WeakMap<Element, IntersectionObserver>()
let viewportObserver: IntersectionObserver | null = null
const callbacks = new WeakMap<Element, () => void>()

// Start the work a screenful early so a tile is already painted by the
// time the user scrolls it into view rather than popping in afterwards.
const ROOT_MARGIN = '400px 0px'

function handle(observer: IntersectionObserver, entries: IntersectionObserverEntry[]) {
  for (const entry of entries) {
    if (!entry.isIntersecting) continue
    const cb = callbacks.get(entry.target)
    // One-shot: unobserve before running the callback so a callback that
    // re-arms (the "render more rows" sentinel) isn't fighting this loop.
    observer.unobserve(entry.target)
    callbacks.delete(entry.target)
    cb?.()
  }
}

// The file listing lives inside FileBrowserView's own overflow-y-auto
// pane, not the document scroller. An observer rooted at the viewport
// would still report tiles clipped by that pane as non-intersecting, but
// rootMargin is applied to the root rect only — it can't see past an
// ancestor's clip — so the pre-load margin would be dead. Rooting the
// observer at the nearest scrollable ancestor keeps the margin useful.
function scrollRoot(el: Element): Element | null {
  let node = el.parentElement
  while (node) {
    const overflowY = getComputedStyle(node).overflowY
    if (overflowY === 'auto' || overflowY === 'scroll') return node
    node = node.parentElement
  }
  return null
}

function observerFor(root: Element | null): IntersectionObserver {
  if (!root) {
    viewportObserver ??= new IntersectionObserver(
      (entries) => handle(viewportObserver!, entries),
      { rootMargin: ROOT_MARGIN },
    )
    return viewportObserver
  }
  let observer = rootObservers.get(root)
  if (!observer) {
    const created: IntersectionObserver = new IntersectionObserver(
      (entries) => handle(created, entries),
      { root, rootMargin: ROOT_MARGIN },
    )
    rootObservers.set(root, created)
    observer = created
  }
  return observer
}

/**
 * observeVisible — run `onVisible` once, the first time `el` comes within
 * a screenful of the scroll viewport. Returns a disarm function; callers
 * MUST call it on unmount, otherwise the observer keeps a strong
 * reference to a detached element and its callback closure.
 */
export function observeVisible(el: Element, onVisible: () => void): () => void {
  // Non-browser / no-observer environments (unit tests, ancient engines)
  // fall back to the previous behavior: load straight away.
  if (typeof IntersectionObserver === 'undefined') {
    onVisible()
    return noop
  }
  const observer = observerFor(scrollRoot(el))
  callbacks.set(el, onVisible)
  observer.observe(el)
  return () => {
    observer.unobserve(el)
    callbacks.delete(el)
  }
}
