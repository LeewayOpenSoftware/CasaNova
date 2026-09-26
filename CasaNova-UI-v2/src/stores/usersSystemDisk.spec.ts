// The per-account "system disk access" switch, on the store side.
//
// Why this test exists: Wave 34-Y already learned this lesson once, on the
// alert-rule toggles. The old wiring mutated local state and THEN asked the
// backend to persist it, so a failed PUT left the switch showing the new
// position while the backend kept the old one. For an on/off preference that
// is a confusing view. For THIS switch it is worse in one specific direction:
// an administrator turns whole-disk access OFF, the PUT fails, the switch
// shows off, and the account can still read the whole disk. The UI would be
// asserting a revocation that never happened.
//
// So the store is unidirectional, and this drives that contract rather than
// asserting the shape of the call.
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

describe('users store — system disk access', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    put.mockReset()
    get.mockReset()
  })

  it('does not record access when the backend refuses the change', async () => {
    put.mockRejectedValue(new Error('403 forbidden'))
    const users = useUsersStore()

    const ok = await users.setSystemDiskAccess({ id: '7', role: 'user' }, true)

    expect(ok).toBe(false)
    expect(users.hasSystemDiskAccess('7')).toBe(false)
  })

  // The direction that matters most: a failed revoke must not look like a
  // successful one.
  it('keeps showing access when turning it OFF fails', async () => {
    put.mockResolvedValue({ data: { success: 200 } })
    const users = useUsersStore()
    await users.setSystemDiskAccess({ id: '7', role: 'user' }, true)
    expect(users.hasSystemDiskAccess('7')).toBe(true)

    put.mockRejectedValue(new Error('network down'))
    const ok = await users.setSystemDiskAccess({ id: '7', role: 'user' }, false)

    expect(ok).toBe(false)
    expect(users.hasSystemDiskAccess('7')).toBe(true)
  })

  it('records access only after the backend agrees', async () => {
    put.mockResolvedValue({ data: { success: 200 } })
    const users = useUsersStore()

    expect(users.hasSystemDiskAccess('7')).toBe(false)
    const ok = await users.setSystemDiskAccess({ id: '7', role: 'user' }, true)

    expect(ok).toBe(true)
    expect(users.hasSystemDiskAccess('7')).toBe(true)
  })

  // A non-administrator is refused both endpoints, and the column is hidden by
  // systemDiskLoaded staying false. "Nobody is switched on" and "you may not
  // know" must not render the same way.
  it('leaves the switch list unloaded when the backend refuses to list it', async () => {
    get.mockRejectedValue(new Error('403 forbidden'))
    const users = useUsersStore()

    await users.refreshSystemDisk()

    expect(users.systemDiskLoaded).toBe(false)
    expect(users.systemDiskIds).toEqual([])
  })

  it('loads the switch list for an administrator', async () => {
    get.mockResolvedValue({ data: { success: 200, data: ['1', '7'] } })
    const users = useUsersStore()

    await users.refreshSystemDisk()

    expect(users.systemDiskLoaded).toBe(true)
    expect(users.hasSystemDiskAccess('7')).toBe(true)
    expect(users.hasSystemDiskAccess('9')).toBe(false)
  })
})
