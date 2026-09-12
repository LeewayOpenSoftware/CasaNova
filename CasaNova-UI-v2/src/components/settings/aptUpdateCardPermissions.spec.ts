import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// There is no jsdom and no @vue/test-utils here, so the card cannot be mounted
// and clicked. What can be pinned is that every control which calls a
// PermSystemConfig endpoint is behind the admin check — the card used to offer
// "Upgrade all" to any authenticated viewer of /app/settings/updates, a route
// that is deliberately NOT admin-only, and the click earned them a 403 toast.
// Each assertion is paired with an anti-vacuity check so a renamed file or a
// rewritten template fails loudly instead of passing by reading nothing.
function read(relative: string): string {
  const src = readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8')
  expect(src.length).toBeGreaterThan(2000)
  return src
}

describe('AptUpdateCard does not offer what a non-admin cannot do', () => {
  it('derives the role from the same source the server will judge', () => {
    const src = read('./AptUpdateCard.vue')
    expect(src).toContain("from '@/router/settingsAccess'")
    expect(src).toContain("effectiveRole(auth.accessToken, auth.user?.role) === 'admin'")
  })

  it.each([
    ['check', '@click="checkApt"'],
    ['upgrade-all', '@click="upgradeConfirm = true"'],
    ['security-now', '@click="runSecurityNow"'],
  ])('hides the %s button from a non-admin', (_name, handler) => {
    const src = read('./AptUpdateCard.vue')
    // Anti-vacuity: the control still exists and still calls its handler.
    expect(src).toContain(handler)
    const tag = src
      .split('<')
      .find((chunk) => chunk.includes(handler))
    expect(tag, `no element carries ${handler}`).toBeTruthy()
    expect(tag).toMatch(/v-if="isAdmin(\s|&|")/)
  })

  it('leaves the unattended-upgrades toggle visible but inert', () => {
    const src = read('./AptUpdateCard.vue')
    // GetAptAuto is PermSystemRead: the STATE is information a non-admin is
    // entitled to, so this one is disabled rather than hidden. A hidden switch
    // would hide a fact; a disabled button would carry none.
    const tag = src.split('<').find((chunk) => chunk.includes('ToggleSwitch :model-value'))
    expect(tag, 'the toggle is gone entirely').toBeTruthy()
    expect(tag).toContain(':disabled="!isAdmin"')
    expect(tag).toContain('@update:model-value="toggleAutoUpgrade"')
  })

  it('still shows a non-admin the counts and the reboot alarm', () => {
    const src = read('./AptUpdateCard.vue')
    // The positive control for the whole file: gating the READS too would pass
    // every assertion above and leave a non-admin an empty card. These three
    // renders must not be behind isAdmin.
    for (const render of ['apt.rebootRequired', "t('settings.host.apt_count'", 'apt.security > 0" severity="danger"']) {
      const tag = src.split('<').find((chunk) => chunk.includes(render))
      expect(tag, `the card no longer renders ${render}`).toBeTruthy()
      expect(tag).not.toContain('isAdmin')
    }
  })
})
