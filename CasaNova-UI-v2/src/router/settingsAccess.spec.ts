import { describe, expect, it } from 'vitest'
import {
  ADMIN_ONLY_SETTINGS_PATHS,
  SETTINGS_FALLBACK_PATH,
  canAccessSettingsPath,
  effectiveRole,
  isAdminOnlySettingsPath,
  normalizeRole,
  roleFromAccessToken,
} from './settingsAccess'

function tokenWith(claims: Record<string, unknown>): string {
  const payload = btoa(JSON.stringify(claims))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
  return `header.${payload}.signature`
}

describe('normalizeRole', () => {
  it('accepts the three real roles regardless of case or padding', () => {
    expect(normalizeRole('admin')).toBe('admin')
    expect(normalizeRole('  Admin ')).toBe('admin')
    expect(normalizeRole('USER')).toBe('user')
    expect(normalizeRole('guest')).toBe('guest')
  })

  it('fails closed to guest for anything else, matching rbac.Normalize', () => {
    // "administrator" is the legacy spelling PostUserLogin promotes to admin
    // when minting the TOKEN. It is not a role name, so on its own it is
    // guest here too — the promotion is the server's to make, not ours.
    for (const v of ['', '   ', 'administrator', 'owner', 'root', null, undefined, 7, {}]) {
      expect(normalizeRole(v)).toBe('guest')
    }
  })
})

describe('roleFromAccessToken', () => {
  it('reads the claim the backend reads', () => {
    expect(roleFromAccessToken(tokenWith({ role: 'admin' }))).toBe('admin')
    expect(roleFromAccessToken(tokenWith({ role: 'user' }))).toBe('user')
  })

  it('reports guest — not "unknown" — for a token with no role claim', () => {
    // `role` is omitempty in CasaNova-Common/utils/jwt: a pre-role-claim
    // token simply has no such key, and CasaNova normalizes that to guest.
    expect(roleFromAccessToken(tokenWith({ id: 1 }))).toBe('guest')
  })

  it('returns null only when the token is not a JWT at all', () => {
    expect(roleFromAccessToken('')).toBeNull()
    expect(roleFromAccessToken('dev-1757500000')).toBeNull()
    expect(roleFromAccessToken('a.b')).toBeNull()
    expect(roleFromAccessToken('header.!!!not-base64!!!.sig')).toBeNull()
  })
})

describe('effectiveRole precedence', () => {
  it('prefers the token over a cached row that disagrees', () => {
    // The real divergence this ordering exists for: a legacy account whose
    // database row has no role mints an ADMIN token, while the login
    // response still returns the bare row. Trusting the row hides the whole
    // settings section from the actual owner of the box.
    expect(effectiveRole(tokenWith({ role: 'admin' }), '')).toBe('admin')
    expect(effectiveRole(tokenWith({ role: 'admin' }), 'administrator')).toBe('admin')
    // ...and the other direction: a stale "admin" row cannot re-promote a
    // principal the server has already demoted in a freshly minted token.
    expect(effectiveRole(tokenWith({ role: 'user' }), 'admin')).toBe('user')
  })

  it('falls back to the cached row only when the token cannot be decoded', () => {
    expect(effectiveRole('dev-1757500000', 'admin')).toBe('admin')
  })

  it('fails closed to guest when neither source says anything', () => {
    expect(effectiveRole('', '')).toBe('guest')
    expect(effectiveRole('', undefined)).toBe('guest')
  })
})

describe('admin-only settings pages', () => {
  it('covers exactly the pages whose mount-time read is PermSystemConfig', () => {
    expect([...ADMIN_ONLY_SETTINGS_PATHS].sort()).toEqual([
      '/app/settings/audit',
      '/app/settings/logs',
      '/app/settings/scheduled',
    ])
  })

  it('matches a path with a trailing slash or a query string', () => {
    expect(isAdminOnlySettingsPath('/app/settings/scheduled/')).toBe(true)
    expect(isAdminOnlySettingsPath('/app/settings/scheduled?tab=1')).toBe(true)
  })

  it('hides them from every non-admin role', () => {
    for (const path of ADMIN_ONLY_SETTINGS_PATHS) {
      expect(canAccessSettingsPath(path, 'user')).toBe(false)
      expect(canAccessSettingsPath(path, 'guest')).toBe(false)
    }
  })

  // POSITIVE CONTROL for the assertion above: a rule that hid everything
  // would satisfy it just as well. These are the pages that must stay.
  it('leaves the admin, and every read-at-PermSystemRead page, alone', () => {
    for (const path of ADMIN_ONLY_SETTINGS_PATHS) {
      expect(canAccessSettingsPath(path, 'admin')).toBe(true)
    }
    const stillVisible = [
      '/app/settings/general',
      '/app/settings/appearance',
      '/app/settings/users',
      '/app/settings/storage',
      '/app/settings/apps',
      '/app/settings/containers',
      '/app/settings/network',
      '/app/settings/security',
      '/app/settings/backups',
      '/app/settings/alerts',
      '/app/settings/host',
      '/app/settings/server',
      '/app/settings/about',
      '/app/settings/updates',
      '/app/settings/diagnosis',
      '/app/settings/maintenance',
      '/app/settings/troubleshooter',
      '/app/settings/remote-access',
      '/app/settings/app-stores',
      '/app/settings/migration',
      '/app/files',
      '/app',
    ]
    for (const path of stillVisible) {
      expect(canAccessSettingsPath(path, 'guest')).toBe(true)
    }
  })

  it('sends a redirected non-admin somewhere they can actually load', () => {
    expect(canAccessSettingsPath(SETTINGS_FALLBACK_PATH, 'guest')).toBe(true)
  })
})
