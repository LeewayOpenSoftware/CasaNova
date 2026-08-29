// Embed probe client + the open-mode decision for the Apps section.
//
// Two separate questions decide how an app opens inside the panel, and they
// have different owners:
//
//   1. "Does the app consent to being framed?" — only the SERVER can answer.
//      X-Frame-Options and CSP on a cross-origin response are unreadable from
//      the browser (that is CORS working as intended), and a refused iframe
//      fails with no event the parent can see. POST /v1/sys/embed-probe reads
//      the headers on our behalf, loopback-only.
//
//   2. "Would THIS page be allowed to frame it?" — only the CLIENT can
//      answer, because it depends on how the dashboard was reached. Opened
//      through the tunnel, the page is HTTPS and an http:// iframe is mixed
//      content: the browser blocks it before any header is consulted.
//
// openMode() combines both. It is a pure function so the truth table is
// testable without a DOM or a network.

import client from './client'

export interface EmbedProbeTarget {
  port: number
  scheme?: 'http' | 'https'
}

export interface EmbedProbeResult {
  port: number
  scheme: string
  status: number
  frameOptions: string
  frameAncestors: string
  embeddable: boolean
  reason: string
}

export async function probeEmbed(
  targets: EmbedProbeTarget[],
): Promise<EmbedProbeResult[]> {
  const res = await client.post<{ success: number; data: EmbedProbeResult[] }>(
    '/v1/sys/embed-probe',
    { targets },
    // Worst case is dead ports: each costs the server its full 4s probe
    // timeout, 8 in flight at a time, so 30+ targets with many dead can
    // exceed the client's default 10s and this call would report failure
    // for a probe that was about to succeed — the exact apt-upgrade bug
    // shape. Long operations set their own budget.
    { timeout: 45_000 },
  )
  return res.data.data ?? []
}

export interface RemoteHost {
  hostname: string
  targetHost: string
  targetPort: number
  targetScheme: string
  source: 'cloudflared' | 'npm'
  onThisBox: boolean
}

/**
 * fetchRemoteHosts — the public HTTPS names this box already publishes
 * (tunnel ingress + NPM proxy hosts), read server-side from their own
 * sources. The UI matches them to apps by target port; only entries with
 * onThisBox are safe to match — a stale rule pointing at another machine
 * would embed a hostname that answers nothing.
 */
export async function fetchRemoteHosts(): Promise<RemoteHost[]> {
  const res = await client.get<{ success: number; data: RemoteHost[] }>(
    '/v1/sys/remote-hosts',
  )
  return res.data.data ?? []
}

export type OpenMode =
  | { mode: 'embed' }
  | { mode: 'newtab'; reason: string }
  | { mode: 'lan-only'; reason: string }

/**
 * openMode — how a given app opens from the current page.
 *
 * `pageIsSecure` is whether the DASHBOARD page is https (tunnel), not whether
 * the app is. The asymmetry that follows trips people up, so it is spelled
 * out here: an http app reached from the https tunnel cannot be embedded
 * (mixed content) AND cannot usefully be opened in a new tab either — the
 * visitor is not on the LAN, so http://192.168.x.x goes nowhere. That is
 * 'lan-only', a different answer from 'newtab', and the UI must say so
 * rather than hand out a link that dies.
 */
export function openMode(
  probe: Pick<EmbedProbeResult, 'embeddable' | 'reason'> | undefined,
  appScheme: 'http' | 'https',
  pageIsSecure: boolean,
  hasRemoteOrigin = false,
): OpenMode {
  // A secure page does NOT mean a remote visitor: the operator browses
  // casaos.<domain> from inside the LAN too, and a first version of this
  // rule demoted every domain-less app to 'lan-only' for them — apps that
  // had embedded fine the day before. The page's scheme only decides what
  // the BROWSER will refuse, so that is all it is used for here:
  //
  //   - an http app inside an https page is mixed content, blocked for
  //     everyone, LAN or not → without a published hostname it is
  //     'lan-only' (the UI offers it as a direct new-tab link, which IS
  //     allowed — mixed-content rules bind subresources, not top-level
  //     navigations — and works for anyone actually on the LAN);
  //   - an https app embeds either way; for a genuinely remote visitor
  //     without a published hostname the frame simply cannot connect,
  //     which is the network telling the truth, not us guessing wrong.
  if (pageIsSecure && !hasRemoteOrigin && appScheme === 'http') {
    return { mode: 'lan-only', reason: 'mixed-content' }
  }
  if (!probe) {
    // No probe result: the app was not reachable when asked. Embedding it
    // anyway would render a blank panel, the exact failure this module
    // exists to prevent.
    return { mode: 'newtab', reason: 'unreachable' }
  }
  if (!probe.embeddable) {
    return { mode: 'newtab', reason: probe.reason }
  }
  return { mode: 'embed' }
}
