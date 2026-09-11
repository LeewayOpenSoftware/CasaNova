import { describe, expect, it, vi } from 'vitest'
import { shouldDiscardExpiredSession } from './sessionExpiry'

function token(exp: number): string {
  const payload = btoa(JSON.stringify({ exp }))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
  return `header.${payload}.signature`
}

describe('expired access-token routing', () => {
  it('keeps an expired session alive when it can still be refreshed', () => {
    vi.setSystemTime(new Date('2026-09-11T00:00:00Z'))
    expect(shouldDiscardExpiredSession(token(1), 'still-valid-refresh')).toBe(false)
  })

  it('discards an expired or malformed access token when no refresh exists', () => {
    vi.setSystemTime(new Date('2026-09-11T00:00:00Z'))
    expect(shouldDiscardExpiredSession(token(1), '')).toBe(true)
    expect(shouldDiscardExpiredSession('not-a-jwt', '')).toBe(true)
  })
})
