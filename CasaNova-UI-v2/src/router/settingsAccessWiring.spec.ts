import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { ADMIN_ONLY_SETTINGS_PATHS } from './settingsAccess'

// There is no jsdom and no @vue/test-utils in this project, so the guard and
// the nav cannot be mounted and driven. What can still be pinned is that they
// ASK — a correct settingsAccess.ts that nothing calls is the exact shape this
// regression already took once (a gate nobody reaches). Each assertion below
// is paired with an anti-vacuity check so a renamed or moved file fails loudly
// instead of passing by reading nothing.
function read(relative: string): string {
  const src = readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8')
  expect(src.length).toBeGreaterThan(200)
  return src
}

describe('the access rule is actually wired in', () => {
  it('is consulted by the router guard, which redirects rather than renders', () => {
    const src = read('./index.ts')
    expect(src).toContain("from './settingsAccess'")
    expect(src).toContain('canAccessSettingsPath(to.path')
    expect(src).toContain('SETTINGS_FALLBACK_PATH')
    // Anti-vacuity: this is the guard file, and it still guards auth.
    expect(src).toContain('router.beforeEach')
    expect(src).toContain('requiresAuth')
  })

  it('is consulted by the settings nav, so the rail cannot offer a bounced page', () => {
    const src = read('../components/settings/SettingsNav.vue')
    // The CALL, not the identifier: an import line alone contains the name,
    // so `toContain('canAccessSettingsPath')` stays green with every call
    // site deleted. That is not hypothetical — this assertion was written
    // that way first and a probe that removed the filter did not turn it red.
    expect(src).toContain('canAccessSettingsPath(e.to, role.value)')
    expect(src).toContain('effectiveRole(auth.accessToken')
    // Anti-vacuity: the rail still lists the pages it is supposed to filter.
    for (const path of ADMIN_ONLY_SETTINGS_PATHS) {
      expect(src).toContain(path)
    }
  })

  it('leaves the token decoder shared, not copied', () => {
    // sessionExpiry and settingsAccess must not disagree about what a token
    // says. One decoder, imported twice.
    expect(read('./sessionExpiry.ts')).toContain("from './jwtPayload'")
    expect(read('./settingsAccess.ts')).toContain("from './jwtPayload'")
    expect(read('./sessionExpiry.ts')).not.toContain('atob(')
    expect(read('./settingsAccess.ts')).not.toContain('atob(')
  })
})
