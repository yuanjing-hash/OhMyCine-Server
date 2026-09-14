import { describe, expect, it } from 'vitest'
import { progressPercent } from './jobs'

describe('progressPercent', () => {
  it('supports ratio progress and legacy percentage progress', () => {
    expect(progressPercent(1)).toBe('100%')
    expect(progressPercent(0.5)).toBe('50%')
    expect(progressPercent(20)).toBe('20%')
  })
})
