// The per-account whole-filesystem SEARCH switch, on the store side: same
// unidirectional contract as the system-disk switch, its own endpoint.
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

vi.stubGlobal('window', {
  addEventListener: () => {},
  removeEventListener: () => {},
})
vi.stubGlobal('document', {
  hidden: false,
  addEventListener: () => {},
  removeEventListener: () => {},
})

const put = vi.fn()
const get = vi.fn()

vi.mock('@/api/client', () => ({
  default: {
    get: (...args: unknown[]) => get(...args),
    put: (...args: unknown[]) => put(...args),
  },
}))

vi.mock('@/api/safeCall', () => ({
  safeCall: async (call: () => Promise<unknown>) => {
    try {
      return { data: await call(), fixture: false, error: null, status: null }
    } catch (err) {
      return { data: undefined, fixture: false, error: String(err), status: 500 }
    }
  },
}))

import { useUsersStore } from './users'

describe('users store — whole-filesystem search', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    put.mockReset()
    get.mockReset()
  })

  it('sends the switch to its own endpoint, not the system-disk one', async () => {
    put.mockResolvedValue({ data: { success: 200 } })
    const users = useUsersStore()

    const ok = await users.setSystemSearchAccess({ id: '7', role: 'user' }, true)

    expect(ok).toBe(true)
    expect(put).toHaveBeenCalledWith('/v1/sys/system-search-access/7', { enabled: true, role: 'user' })
    expect(users.hasSystemSearchAccess('7')).toBe(true)
    expect(users.hasSystemDiskAccess('7')).toBe(false)
  })

  // The backend refuses ON without system-disk access; the switch must not
  // then claim it is on.
  it('does not record the switch when the backend refuses it', async () => {
    put.mockRejectedValue(new Error('400 needs system-disk access'))
    const users = useUsersStore()

    const ok = await users.setSystemSearchAccess({ id: '7', role: 'user' }, true)

    expect(ok).toBe(false)
    expect(users.hasSystemSearchAccess('7')).toBe(false)
  })

  it('keeps showing the switch on when turning it OFF fails', async () => {
    put.mockResolvedValue({ data: { success: 200 } })
    const users = useUsersStore()
    await users.setSystemSearchAccess({ id: '7', role: 'user' }, true)

    put.mockRejectedValue(new Error('network down'))
    const ok = await users.setSystemSearchAccess({ id: '7', role: 'user' }, false)

    expect(ok).toBe(false)
    expect(users.hasSystemSearchAccess('7')).toBe(true)
  })

  it('loads the list for an administrator and stays unloaded for anyone else', async () => {
    get.mockResolvedValue({ data: { success: 200, data: ['1'] } })
    const users = useUsersStore()
    await users.refreshSystemSearch()
    expect(get).toHaveBeenCalledWith('/v1/sys/system-search-access')
    expect(users.systemSearchLoaded).toBe(true)
    expect(users.hasSystemSearchAccess('1')).toBe(true)

    get.mockRejectedValue(new Error('403 forbidden'))
    await users.refreshSystemSearch()
    expect(users.systemSearchLoaded).toBe(false)
    expect(users.systemSearchIds).toEqual([])
  })
})
