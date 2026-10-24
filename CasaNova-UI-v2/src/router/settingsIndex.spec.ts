import { afterEach, describe, expect, it, vi } from 'vitest'
import { PHONE_MEDIA_QUERY, settingsIndexEnter } from './settingsIndex'
import { SETTINGS_FALLBACK_PATH } from './settingsAccess'

function stubViewport(phone: boolean) {
  vi.stubGlobal('window', { matchMedia: (q: string) => ({ matches: phone && q === PHONE_MEDIA_QUERY }) })
}

describe('settingsIndexEnter', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps /app/settings as the list on a phone', () => {
    stubViewport(true)
    expect(settingsIndexEnter()).toBe(true)
  })

  it('redirects to the fallback page on a wider viewport, as before', () => {
    stubViewport(false)
    expect(settingsIndexEnter()).toBe(SETTINGS_FALLBACK_PATH)
  })

  it('redirects when the viewport cannot be measured', () => {
    vi.stubGlobal('window', {})
    expect(settingsIndexEnter()).toBe(SETTINGS_FALLBACK_PATH)
  })
})
