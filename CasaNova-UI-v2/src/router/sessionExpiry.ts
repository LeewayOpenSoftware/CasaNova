/** Decode a JWT access token's expiry without verifying its signature. */
function isTokenExpired(token: string): boolean {
  try {
    const parts = token.split('.')
    if (parts.length !== 3) return true
    let payload = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const pad = payload.length % 4
    if (pad === 2) payload += '=='
    else if (pad === 3) payload += '='
    else if (pad === 1) return true
    const claims = JSON.parse(atob(payload)) as { exp?: unknown }
    return typeof claims.exp !== 'number' || claims.exp * 1000 < Date.now()
  } catch {
    return true
  }
}

// An expired access token is not a dead session while a refresh credential
// remains. Let the first protected request receive 401: the axios interceptor
// serializes renewal, retries it once, and logs out if renewal really fails.
export function shouldDiscardExpiredSession(accessToken: string, refreshToken: string): boolean {
  return isTokenExpired(accessToken) && refreshToken === ''
}
