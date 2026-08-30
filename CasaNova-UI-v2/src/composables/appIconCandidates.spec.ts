// IT Tools showed an empty square in the Apps section while looking fine on
// the dashboard. Its declared icon is
// github.com/walkxcode/dashboard-icons/blob/main/png/it-tools.png?raw=true,
// which the page's CSP blocks: img-src allows *.githubusercontent.com but not
// github.com, and CSP judges the URL as written rather than where GitHub
// redirects it. Confirmed from the browser console on the box, not inferred.
//
// The dashboard survived that because AppCard walks a list of candidates and
// the second one — selfhst on cdn.jsdelivr.net, which IS allowed — exists.
// The Apps section had its own <img> whose letter fallback used v-else, so it
// only fired when the URL was ABSENT and never when it was present and failed.
// These pin the list both renderers now share.

import { describe, it, expect } from 'vitest'
import { appIconCandidates } from './appIconCandidates'

describe('appIconCandidates', () => {
  it('offers an allowed CDN URL after an icon the CSP will block', () => {
    const urls = appIconCandidates({
      iconUrl: 'https://github.com/walkxcode/dashboard-icons/blob/main/png/it-tools.png?raw=true',
      id: 'big-bear-it-tools',
      name: 'IT Tools',
    })
    expect(urls[0], 'the declared URL is still tried first').toContain('github.com')
    // it-tools.png was verified to exist on this CDN; the point of the test is
    // that SOMETHING allowed follows the blocked URL, not the exact filename.
    expect(urls.slice(1).some((u) => u.startsWith('https://cdn.jsdelivr.net/'))).toBe(true)
    expect(urls).toContain('https://cdn.jsdelivr.net/gh/selfhst/icons/png/it-tools.png')
  })

  it('derives its slug from the display name, not only the store id', () => {
    // The store id is "big-bear-it-tools" — a packager prefix that matches no
    // icon anywhere. The name is what resolves.
    const urls = appIconCandidates({ id: 'big-bear-it-tools', name: 'IT Tools' })
    expect(urls).toContain('https://cdn.jsdelivr.net/gh/selfhst/icons/png/it-tools.png')
  })

  it('tries the separator-less form after the kebab one', () => {
    // selfhst names a fair number of files without the separator.
    const urls = appIconCandidates({ name: 'Pi Hole' })
    const kebab = urls.indexOf('https://cdn.jsdelivr.net/gh/selfhst/icons/png/pi-hole.png')
    const compact = urls.indexOf('https://cdn.jsdelivr.net/gh/selfhst/icons/png/pihole.png')
    expect(kebab).toBeGreaterThanOrEqual(0)
    expect(compact).toBeGreaterThan(kebab)
  })

  it('never repeats a URL', () => {
    // A single-word app makes id, name, kebab and compact all identical.
    const urls = appIconCandidates({ id: 'jellyfin', name: 'Jellyfin' })
    expect(new Set(urls).size).toBe(urls.length)
  })

  it('still produces candidates when the app declares no icon at all', () => {
    // monitoring-stack and immich on the test box declare an empty icon.
    // Returning nothing would put them straight on a letter tile.
    expect(appIconCandidates({ id: 'monitoring-stack' }).length).toBeGreaterThan(0)
  })

  it('returns nothing to guess at when there is nothing to guess from', () => {
    // Guards the guard: if this returned a URL, the "no candidates" branch of
    // the tile — the letter fallback — would be unreachable.
    expect(appIconCandidates({})).toEqual([])
  })
})
