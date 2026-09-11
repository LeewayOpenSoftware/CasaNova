import { describe, expect, it } from 'vitest'
import { decodeJwtPayload } from './jwtPayload'

describe('decodeJwtPayload', () => {
  it('decodes a base64url payload containing the substituted characters', () => {
    // `?` and `~` in the value force `+` and `/` into standard base64, which
    // a JWT then substitutes for `-` and `_`. A decoder that called atob on
    // the raw segment would throw on exactly this, which is the common case.
    const claims = { username: 'a?b~c', role: 'admin', exp: 1757500000 }
    const raw = btoa(JSON.stringify(claims))
    expect(raw).toMatch(/[+/]/)
    const payload = raw.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
    expect(decodeJwtPayload(`header.${payload}.signature`)).toEqual(claims)
  })

  it('returns null for anything that is not a decodable three-part token', () => {
    expect(decodeJwtPayload('')).toBeNull()
    expect(decodeJwtPayload('a.b')).toBeNull()
    expect(decodeJwtPayload('a.b.c.d')).toBeNull()
    expect(decodeJwtPayload('header.!!!.signature')).toBeNull()
    expect(decodeJwtPayload(`header.${btoa('[1,2,3]')}.signature`)).toBeNull()
    expect(decodeJwtPayload(`header.${btoa('"a string"')}.signature`)).toBeNull()
  })
})
