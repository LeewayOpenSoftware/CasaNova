// Decoding a JWT payload in the browser, in ONE place.
//
// Two router-side modules need the access token's claims: sessionExpiry.ts
// reads `exp`, settingsAccess.ts reads `role`. They must agree byte for byte
// about what a token says — a decoder that succeeds for one and fails for the
// other would let the router keep a session it believes is live while hiding
// every page from the principal behind it. So the decode lives here and both
// import it, rather than each carrying its own copy that can drift.
//
// JWT (RFC 7519) uses base64**url**: `-` for `+`, `_` for `/`, and no padding.
// `atob` accepts neither substitution nor a missing pad, and almost every real
// payload contains at least one `-` or `_`, so the substitution is not an edge
// case — it is the common path. (Router history: the original inline decoder
// called `atob` on the raw segment, threw on nearly every token, and force-
// logged users out on every navigation.)
//
// NOTHING HERE IS VERIFICATION. The signature is not checked and cannot be:
// the browser has no public key and no reason to be trusted with one. These
// claims are usable only for presentation decisions the backend re-decides on
// its own — never as authorization.
export function decodeJwtPayload(token: string): Record<string, unknown> | null {
  try {
    const parts = token.split('.')
    if (parts.length !== 3) return null
    let payload = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    // base64 encodes 3 bytes per 4 chars, so a payload's length mod 4 is
    // 0, 2 or 3 — never 1. A remainder of 1 is a corrupt segment.
    const pad = payload.length % 4
    if (pad === 2) payload += '=='
    else if (pad === 3) payload += '='
    else if (pad === 1) return null
    const claims: unknown = JSON.parse(atob(payload))
    if (typeof claims !== 'object' || claims === null || Array.isArray(claims)) return null
    return claims as Record<string, unknown>
  } catch {
    return null
  }
}
