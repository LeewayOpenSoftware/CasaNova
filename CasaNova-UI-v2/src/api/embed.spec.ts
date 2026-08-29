// Truth table for openMode — the function that decides whether an app opens
// inside the panel, in a new tab, or not at all from here.
//
// The row worth the file: an http app seen from the https tunnel. Embedding
// is blocked by the browser (mixed content) before any header is read, and a
// new-tab link to http://192.168.x.x is equally dead because the visitor is
// not on the LAN. Both "just embed it" and "just link out" produce a failure
// the user cannot diagnose; the only honest answer is to say "LAN only".

import { describe, it, expect, vi } from 'vitest'

// embed.ts imports the axios client, whose module graph reaches vue-router's
// createWebHashHistory — which dereferences `location` at import time and
// dies in this DOM-less runner. openMode itself is pure; the mock cuts the
// graph at the first edge.
vi.mock('./client', () => ({ default: {} }))

import { openMode } from './embed'

const consents = { embeddable: true, reason: '' }
const refuses = { embeddable: false, reason: 'x-frame-options: deny' }

describe('openMode', () => {
  it('embeds a consenting app from the LAN', () => {
    expect(openMode(consents, 'http', false)).toEqual({ mode: 'embed' })
  })

  it('opens a refusing app in a new tab, carrying the reason', () => {
    expect(openMode(refuses, 'http', false)).toEqual({
      mode: 'newtab',
      reason: 'x-frame-options: deny',
    })
  })

  it('marks an http app LAN-only when the page came through the tunnel', () => {
    // Mixed content: the browser blocks the frame before headers matter, so
    // even a consenting app cannot be embedded from the https page.
    expect(openMode(consents, 'http', true)).toEqual({
      mode: 'lan-only',
      reason: 'mixed-content',
    })
  })

  it('lan-only wins over the probe refusal, because the newtab link would be dead too', () => {
    const m = openMode(refuses, 'http', true)
    expect(m.mode).toBe('lan-only')
  })

  it('keeps embedding an https app from an https page even without a public hostname', () => {
    // Regression pin. A secure page does not mean a remote visitor — the
    // operator browses casaos.<domain> from inside the LAN, and a version of
    // this rule that demoted domain-less https apps to lan-only disabled
    // crafty for them one day after it had worked. The https iframe is not
    // mixed content; whether the address answers is the network's call.
    expect(openMode(consents, 'https', true)).toEqual({ mode: 'embed' })
  })

  it('embeds a consenting app from the tunnel through its published hostname', () => {
    expect(openMode(consents, 'http', true, true)).toEqual({ mode: 'embed' })
  })

  it('still honours the app\'s refusal when reached through its hostname', () => {
    // The tunnel does not strip the app's own framing headers; a refusal on
    // the LAN is a refusal through the domain too.
    expect(openMode(refuses, 'http', true, true)).toEqual({
      mode: 'newtab',
      reason: 'x-frame-options: deny',
    })
  })

  it('ignores the remote origin on the LAN — local URLs are faster and need no tunnel', () => {
    expect(openMode(consents, 'http', false, true)).toEqual({ mode: 'embed' })
  })

  it('treats a missing probe as unreachable, never as consent', () => {
    // Embedding an unprobed app is how blank panels happen.
    expect(openMode(undefined, 'http', false)).toEqual({
      mode: 'newtab',
      reason: 'unreachable',
    })
  })
})
