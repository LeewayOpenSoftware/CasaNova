// Update-eligibility classification — one place decides how each app
// source updates, so the UI never silently hides an update or offers one
// that can't work.
import { describe, it, expect } from 'vitest'
import { updateEligibility } from './containers'

describe('updateEligibility — update model by source', () => {
  it('compose apps update via the compose path', () => {
    expect(updateEligibility({ source: 'compose', managed: true })).toEqual({
      method: 'compose',
      supported: true,
    })
  })

  it('standalone containers update via re-pull + recreate', () => {
    const e = updateEligibility({ source: 'standalone', managed: false })
    expect(e.method).toBe('recreate')
    expect(e.supported).toBe(true)
  })

  it('external compose stacks are not updatable as a unit — with a reason, not silence', () => {
    const e = updateEligibility({ source: 'external-stack', managed: false })
    expect(e.method).toBe('none')
    expect(e.supported).toBe(false)
  })

  it('falls back from legacy managed flag when source is absent', () => {
    expect(updateEligibility({ managed: false }).method).toBe('recreate') // unmanaged → standalone
    expect(updateEligibility({ managed: true }).method).toBe('compose')
    expect(updateEligibility({}).method).toBe('compose') // default
  })
})
