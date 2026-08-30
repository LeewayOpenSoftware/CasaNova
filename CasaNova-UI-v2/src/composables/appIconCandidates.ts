// The list of URLs to try for an app's icon, in order.
//
// WHY A LIST. An app's declared icon URL comes from a third-party app store
// and can point anywhere. Some of those hosts are not in the page's CSP
// img-src allowlist, and that allowlist can never be complete — the store
// decides the URL, we do not. When one is blocked or 404s, the tile has to
// have somewhere else to go.
//
// WHY IT LIVES HERE. AppCard walks this list on <img @error> and lands on a
// letter tile only once every candidate has failed. The Apps section rendered
// its own <img> with a v-else letter fallback that only fires when the URL is
// ABSENT — never when it is present and fails. So a blocked icon left an empty
// square there while the same app looked fine on the dashboard. One list, both
// callers, no second copy to drift.
//
// Measured on the test box: big-bear-it-tools declares
// github.com/walkxcode/dashboard-icons/blob/…?raw=true, which the CSP blocks
// (github.com is not in the list; *.githubusercontent.com is, but CSP judges
// the URL as written, not where it redirects). The second candidate,
// cdn.jsdelivr.net/gh/selfhst/icons/png/it-tools.png, is allowed and exists.

const ICON_CDN = 'https://cdn.jsdelivr.net/gh/selfhst/icons/png'
const ICON_CDN_SVG = 'https://cdn.jsdelivr.net/gh/selfhst/icons/svg'

export interface AppIconSource {
  iconUrl?: string
  id?: string
  name?: string
}

const slugify = (raw: string | undefined) =>
  (raw ?? '')
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')

export function appIconCandidates(app: AppIconSource): string[] {
  const urls: string[] = []
  const seen = new Set<string>()
  const add = (u: string | undefined) => {
    if (!u || seen.has(u)) return
    seen.add(u)
    urls.push(u)
  }

  // 1. The backend-resolved URL first — it may point at a CasaOS or custom
  //    CDN we have no way to guess.
  add(app.iconUrl)

  // 2. Kebab slugs from the app id and name. These are the short identifiers
  //    that actually match selfhst filenames — "IT Tools" → "it-tools".
  const slugs = new Set([slugify(app.id), slugify(app.name)])

  // 3. png before svg: <img> renders png without wrangling and it is the
  //    most commonly populated format in that repo.
  for (const slug of slugs) {
    if (!slug) continue
    add(`${ICON_CDN}/${slug}.png`)
    add(`${ICON_CDN_SVG}/${slug}.svg`)
  }

  // 4. The separator-less form last. selfhst names a fair number of files
  //    without it ("pihole", not "pi-hole"), and CasaOS-imported apps — most
  //    of this box's library — are exactly the ones whose id carries a
  //    separator the CDN does not. `add` dedupes, so single-word apps pay
  //    nothing for this.
  for (const slug of slugs) {
    const compact = slug.replace(/-/g, '')
    if (!compact) continue
    add(`${ICON_CDN}/${compact}.png`)
    add(`${ICON_CDN_SVG}/${compact}.svg`)
  }

  return urls
}
