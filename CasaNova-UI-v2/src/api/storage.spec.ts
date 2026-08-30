// The disk operations must outwait the server, not the other way round.
//
// Both benchmark and resize shell out to a tool that works on a real disk, and
// both used to inherit the shared axios client's 10 second timeout. The dialog
// answered "timeout of 10000ms exceeded" — while the benchmark ran to
// completion on the server and its result was discarded. A quick run is about
// 14 seconds of honest work, so the feature could not succeed.
//
// The invariant is directional, and that is the whole point: the client must
// give up LATER than the server does. The server kills a wedged fio and
// explains why; if axios aborts first, that diagnosis is replaced by a generic
// timeout and the useful error is the one nobody ever sees. So these tests
// assert not just "a timeout is set" but that it clears the server's own
// ceiling, which is why the ceilings are restated here as constants — if
// someone lowers one in Go without lowering it here, that is a code review
// question, and if someone lowers it here below the server, this fails.

import { describe, it, expect, beforeEach, vi } from 'vitest'

type Call = { url: string; body: unknown; cfg?: { timeout?: number } }
const calls: Call[] = []

vi.mock('./client', () => ({
  default: {
    post: (url: string, body: unknown, cfg?: { timeout?: number }) => {
      calls.push({ url, body, cfg })
      return Promise.resolve({ data: { success: 200, data: {} } })
    },
    // listDisks fans out to three endpoints and destructures each answer, so
    // the stub has to return each one's real shape — an empty body would fail
    // for a reason that has nothing to do with timeouts.
    get: (url: string, cfg?: { timeout?: number }) => {
      calls.push({ url, body: undefined, cfg })
      const data =
        url === '/v1/disks'
          ? { disks: [], avail: [] }
          : url === '/v1/storage'
            ? []
            : url.includes('standby')
              ? { minutes: 0 }
              : {}
      return Promise.resolve({ data: { success: 200, data } })
    },
  },
}))

// Mirrors CasaNova-LocalStorage/route/v1/disk_ops.go.
const SERVER_BOUND_MS = {
  benchmarkQuick: 90_000, // ceiling for a quick run
  benchmarkDeep: 180_000, // ceiling for a deep run
  resize: 600_000, // context.WithTimeout(..., 10*time.Minute)
}

let storage: typeof import('./storage')

beforeEach(async () => {
  calls.length = 0
  storage = await import('./storage')
})

describe('disk operations outwait the server', () => {
  it('a quick benchmark waits longer than the server will run one', async () => {
    await storage.benchmarkDisk('/dev/sda', 'quick')
    const t = calls.at(-1)?.cfg?.timeout
    expect(t, 'benchmarkDisk sent no timeout — it inherits the 10s default').toBeDefined()
    expect(t!).toBeGreaterThan(SERVER_BOUND_MS.benchmarkQuick)
  })

  it('a deep benchmark waits longer still', async () => {
    // Deep is four times the work. A single constant covering both would have
    // to be sized for deep, which is the case that is easy to forget.
    await storage.benchmarkDisk('/dev/sda', 'deep')
    const t = calls.at(-1)?.cfg?.timeout
    expect(t).toBeDefined()
    expect(t!).toBeGreaterThan(SERVER_BOUND_MS.benchmarkDeep)
  })

  it('deep is given more room than quick', async () => {
    // Guards against a refactor that keeps a timeout but makes it a constant,
    // which would silently reintroduce the deep-mode failure alone.
    await storage.benchmarkDisk('/dev/sda', 'quick')
    const quick = calls.at(-1)!.cfg!.timeout!
    await storage.benchmarkDisk('/dev/sda', 'deep')
    const deep = calls.at(-1)!.cfg!.timeout!
    expect(deep).toBeGreaterThan(quick)
  })

  it('resize waits out a multi-terabyte grow', async () => {
    // resize2fs on a large filesystem is minutes of real work and the handler
    // allows ten. Aborting at ten seconds would leave a resize running with
    // nobody watching it.
    await storage.resizeDisk('/dev/sda')
    const t = calls.at(-1)?.cfg?.timeout
    expect(t, 'resizeDisk sent no timeout — it inherits the 10s default').toBeDefined()
    expect(t!).toBeGreaterThan(SERVER_BOUND_MS.resize)
  })

  it('the hardware enumerations outwait lsblk and smartctl', async () => {
    // listDisks used to be this file's example of a fast read. It is not one:
    // it walks real hardware, where lsblk is bounded at 30s and smartctl at 6s
    // per disk. Timing out threw away the degraded inventory the backend had
    // assembled — which it assembles precisely when a disk is sick.
    await storage.listDisks()
    const disksCall = calls.find((c) => c.url === '/v1/disks')
    expect(disksCall?.cfg?.timeout, '/v1/disks inherits the 10s default').toBeDefined()
    expect(disksCall!.cfg!.timeout!).toBeGreaterThan(30_000)
  })

  it('a genuinely fast read still uses the shared default', async () => {
    // Guards the guard. If every call in storage.ts carried a long timeout,
    // the tests above would pass while a hung request froze the page for
    // minutes. Opting out is deliberate and rare, not the house style.
    // GetStandby returns ctx.JSON from its own body — no exec, no disk.
    await storage.getStandbySettings()
    expect(calls.at(-1)?.cfg?.timeout).toBeUndefined()
  })
})
