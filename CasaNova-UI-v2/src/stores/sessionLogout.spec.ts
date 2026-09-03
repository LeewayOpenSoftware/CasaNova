// Every store that holds session-bound state must drop it on 'auth-logout'.
//
// Why this test exists: `auth.ts` logout() does NOT reload the page — it
// clears tokens in place and emits a window 'auth-logout' event, and its own
// comment states the invariant ("a different user logging in on the same
// browser doesn't inherit the previous occupant's ..."). Ten modules honour
// that event; `virt` and `ysk` did not, so user A's VM inventory and YSK cards
// stayed on screen for user B until B's first fetch resolved.
//
// The test drives the real contract rather than asserting a listener was
// registered: it captures the handlers the store installs on window, fires
// 'auth-logout', and requires the session state to be back at its
// constructor default.
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

const handlers: Record<string, Array<() => void>> = {}

vi.stubGlobal('window', {
  addEventListener: (evt: string, fn: () => void) => {
    ;(handlers[evt] ??= []).push(fn)
  },
  removeEventListener: () => {},
})
vi.stubGlobal('document', {
  hidden: false,
  addEventListener: () => {},
  removeEventListener: () => {},
})

vi.mock('@/api/virt', () => ({
  getVirtInfo: vi.fn(async () => ({})),
  listDomains: vi.fn(async () => []),
  listPools: vi.fn(async () => []),
}))
vi.mock('@/api/ysk', () => ({
  listYskCards: vi.fn(async () => []),
  dismissYskCard: vi.fn(async () => {}),
}))

import { useVirtStore } from './virt'
import { useYskStore } from './ysk'

function fireLogout() {
  for (const fn of handlers['auth-logout'] ?? []) fn()
}

describe('session-bound stores release their state on auth-logout', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const k of Object.keys(handlers)) delete handlers[k]
  })

  it('virt: user A VM inventory is not readable by user B', () => {
    const virt = useVirtStore()
    // Stand in for "user A browsed the VMs view": the store is warm.
    virt.domains = [{ name: 'a-private-vm', status: 'running' }] as never
    virt.pools = [{ name: 'a-private-pool' }] as never
    virt.info = { hypervisor: 'qemu' } as never
    virt.available = true

    fireLogout()

    expect(virt.domains).toEqual([])
    expect(virt.pools).toEqual([])
    expect(virt.info).toBeNull()
    expect(virt.available).toBeNull()
  })

  it('ysk: user A cards are not readable by user B', () => {
    const ysk = useYskStore()
    ysk.cards = [{ id: 'card-of-user-a', title: 'private' }] as never

    fireLogout()

    expect(ysk.cards).toEqual([])
  })
})
