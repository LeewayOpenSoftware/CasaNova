// The calls that must outwait their handler.
//
// Every request in this app shares one axios instance with `timeout: 10_000`.
// That is right for the reads that make up most of the API and wrong for the
// handlers that shell out to docker, upsc or a store download. When it is
// wrong, axios aborts and replaces the server's answer — result or honest
// error — with "timeout of 10000ms exceeded", while the operation completes on
// the server with nobody listening. That is why a retry appears to fix it.
//
// The bounds asserted here were read out of the Go handlers, not assumed:
//
//   /v2/app_management/compose           15s   compose_app.go:59
//   /v1/container/all                    15s   CasaNova/route/v1/containers.go:175
//   /v1/container/stats                  10s   containers.go:347
//   /v1/container/:id/{stop,restart}     30s   container_ctl.go:62 (docker … -t 10)
//   /v1/sys/host/ups[/config]            15s+5s WaitDelay, more than once
//   /v2/app_management/stores/:id/sync   5min  downloadHelper/getter.go:68
//
// The direction is what matters and what these assert: the client must give up
// LATER than the server does, so the server's diagnosis survives. Asserting the
// exact constants instead would fail every time someone tuned one by a second
// while telling us nothing about whether the invariant still holds.

import { describe, it, expect, beforeEach, vi } from 'vitest'

type Call = { url: string; cfg?: { timeout?: number } }
const calls: Call[] = []

// Each endpoint gets its real response shape. A blanket {} makes the callers
// throw while parsing, which fails the test for a reason that has nothing to
// do with the timeout being asserted.
const shapes: Array<[RegExp, unknown]> = [
  [/\/container\/stats/, []],
  [/\/container\/all/, { containers: [] }],
]

const record = (url: string, cfg?: { timeout?: number }) => {
  calls.push({ url, cfg })
  const data = shapes.find(([re]) => re.test(url))?.[1] ?? {}
  return Promise.resolve({ data: { success: 200, data } })
}

vi.mock('./client', () => ({
  default: {
    get: (url: string, cfg?: { timeout?: number }) => record(url, cfg),
    // post/put take (url, body, cfg) — the config is the THIRD argument, which
    // is the mistake that makes a timeout silently do nothing.
    post: (url: string, _b?: unknown, cfg?: { timeout?: number }) => record(url, cfg),
    put: (url: string, _b?: unknown, cfg?: { timeout?: number }) => record(url, cfg),
  },
}))

const last = () => calls.at(-1)?.cfg?.timeout

beforeEach(() => {
  calls.length = 0
})

describe('container calls outwait the docker handlers', () => {
  it('stop and restart clear the 30s handler bound', async () => {
    // `docker stop -t 10` spends ten seconds waiting for SIGTERM before it
    // sends SIGKILL — the whole old budget, before the CLI fork, the daemon
    // round trip and the gateway hop. Any container that ignores SIGTERM
    // (databases, anything flushing state) aborted deterministically, and the
    // store swallowed it, so the UI reported failure and kept rendering a
    // container that had already stopped.
    const c = await import('./containers')
    await c.stopContainer('abc')
    expect(last(), 'stopContainer inherits the 10s default').toBeGreaterThan(30_000)
    await c.restartContainer('abc')
    expect(last(), 'restartContainer inherits the 10s default').toBeGreaterThan(30_000)
  })

  it('the stats poll clears its handler bound, which was exactly the old default', async () => {
    // containerStatsTimeout is 10s — the same number the client used. The
    // client aborted at precisely the moment the server was due to answer, so
    // the CPU and RAM tiles went blank with no error anyone could act on.
    const c = await import('./containers')
    await c.getContainerStats()
    expect(last()).toBeGreaterThan(10_000)
  })

  it('the two list calls clear their 15s bounds', async () => {
    const c = await import('./containers')
    await c.listApps()
    expect(last(), 'listApps inherits the 10s default').toBeGreaterThan(15_000)
    await c.getAllContainers()
    expect(last(), 'getAllContainers inherits the 10s default').toBeGreaterThan(15_000)
  })
})

describe('host and library calls outwait their handlers', () => {
  it('the UPS read and write allow for more than one bounded exec', async () => {
    // Each upsc/apcaccess call is 15s plus a 5s WaitDelay, and a read makes
    // several. Below that the card errored on every poll instead of saying it
    // could not reach upsd.
    const h = await import('./host')
    await h.getUPS()
    expect(last()).toBeGreaterThan(40_000)
    await h.setUPSConfig('192.168.1.10', 'ups')
    expect(last()).toBeGreaterThan(40_000)
  })

  it('a store sync waits out the whole download', async () => {
    // StoreSync calls SyncRemoteStore inline — no goroutine — and the download
    // is bounded at five minutes. "Sync" reported failure while the catalog
    // downloaded and installed anyway.
    const l = await import('./library')
    await l.syncStore('default')
    expect(last()).toBeGreaterThan(5 * 60_000)
  })
})
