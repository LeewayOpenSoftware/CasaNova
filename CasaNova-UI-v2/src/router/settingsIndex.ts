// The bare /app/settings address: a list on a phone, a redirect elsewhere.
//
// Above `sm` (640px) Settings is nav + page side by side, and /app/settings
// has always redirected to General so the right column is never empty. On a
// phone the two columns are two screens (SettingsLayout.vue): /app/settings
// is the list, and redirecting it would make the list unreachable — every
// "back" would land on General again. So the redirect only happens when the
// viewport is wider than a phone.
//
// The breakpoint is Tailwind's `sm`, the same one the layout switches on;
// keep the two together.

import { SETTINGS_FALLBACK_PATH } from './settingsAccess'

export const SETTINGS_INDEX_PATH = '/app/settings'
export const PHONE_MEDIA_QUERY = '(max-width: 639.98px)'

export function isPhoneViewport(): boolean {
  try {
    return typeof window !== 'undefined'
      && typeof window.matchMedia === 'function'
      && window.matchMedia(PHONE_MEDIA_QUERY).matches
  } catch {
    return false
  }
}

// beforeEnter of the index route: stay on a phone, go to the fallback page
// anywhere else (and when the viewport cannot be measured, as before).
export function settingsIndexEnter(): true | string {
  return isPhoneViewport() ? true : SETTINGS_FALLBACK_PATH
}
