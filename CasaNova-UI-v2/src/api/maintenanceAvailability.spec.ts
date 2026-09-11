// The settings rail must not steer anyone into a section this build does not
// have. route/v1.go registers the whole maintenance group behind a
// compile-time constant that is currently false, so every path answers 404 and
// the page renders an honest "not available in this build" card. That card is
// the right answer for a bookmark; it is the wrong thing to advertise in a nav
// rail, because offering a section whose only content is an apology is still
// offering it.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it, vi } from 'vitest'

// api/maintenance imports the axios client, which imports the router, which
// calls createWebHashHistory() and needs a DOM `location` this project has no
// jsdom to provide. isModuleAbsent is a pure function; stub the transport.
vi.mock('./client', () => ({
  default: { get: async () => ({ data: {} }), post: async () => ({ data: {} }) },
}))

import { isModuleAbsent } from './maintenance'

describe('isModuleAbsent', () => {
  it('treats only an unregistered route as "this build lacks the module"', () => {
    expect(isModuleAbsent(404)).toBe(true)
    expect(isModuleAbsent(405)).toBe(true)
  })

  // POSITIVE CONTROL: a rule that answered true for everything would satisfy
  // the assertion above and would hide the section on any hiccup. These are the
  // statuses that mean the module IS there and something else went wrong —
  // 401/403 are a permission answer, which only a registered route can give.
  it('is not satisfied by a failure that says nothing about the build', () => {
    for (const s of [200, 401, 403, 409, 410, 500, 502, 504, null, undefined]) {
      expect(isModuleAbsent(s)).toBe(false)
    }
  })
})

function read(relative: string): string {
  const src = readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8')
  expect(src.length).toBeGreaterThan(200)
  return src
}

describe('the rule has exactly one definition, and both readers use it', () => {
  it('is applied by the settings rail to decide whether to offer the entry', () => {
    const src = read('../components/settings/SettingsNav.vue')
    // The CALL, not the identifier — an import line alone contains the name.
    expect(src).toContain('isModuleAbsent(result.status)')
    expect(src).toContain('showMaintenance.value = false')
    // Anti-vacuity: the entry is still there to be offered, and is still
    // conditional rather than deleted outright.
    expect(src).toContain("'/app/settings/maintenance'")
    expect(src).toContain('if (showMaintenance.value) {')
  })

  it('asks whether the principal may use it BEFORE asking whether it exists', () => {
    // The probe can only report on the build. Sending it for someone who may
    // not open the page buys nothing and costs a guaranteed 403 per settings
    // mount — GetMaintenanceScan requires PermSystemConfig — logged against
    // users with no connection to the feature. Today the module answers 404 to
    // everyone so the 403 never happens; the day the constant flips it does,
    // and the first sign would be noise in someone else's audit log.
    const src = read('../components/settings/SettingsNav.vue')
    const guard = src.indexOf('if (!canAccessSettingsPath(MAINTENANCE_PATH, role.value)) return')
    const probe = src.indexOf('safeCall(() => getMaintenanceScan())')
    expect(guard).toBeGreaterThan(-1)
    expect(probe).toBeGreaterThan(-1)
    // ORDER is the property, not mere presence: a permission check that runs
    // after the request has already gone out prevents nothing.
    expect(guard).toBeLessThan(probe)
  })

  it('is applied by the page itself, from the same definition', () => {
    const src = read('../views/settings/MaintenanceSettings.vue')
    expect(src).toContain('isModuleAbsent(res.status)')
    // ...and the page no longer carries its own copy of the two magic numbers.
    expect(src).not.toContain('res.status === 404')
  })
})
