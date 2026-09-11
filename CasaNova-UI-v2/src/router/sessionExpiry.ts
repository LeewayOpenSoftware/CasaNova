import { decodeJwtPayload } from './jwtPayload'

/** Decode a JWT access token's expiry without verifying its signature. */
function isTokenExpired(token: string): boolean {
  const claims = decodeJwtPayload(token)
  if (!claims) return true
  const exp = claims.exp
  return typeof exp !== 'number' || exp * 1000 < Date.now()
}

// An expired access token is not a dead session while a refresh credential
// remains. Let the first protected request receive 401: the axios interceptor
// serializes renewal, retries it once, and logs out if renewal really fails.
export function shouldDiscardExpiredSession(accessToken: string, refreshToken: string): boolean {
  return isTokenExpired(accessToken) && refreshToken === ''
}
