import { describe, expect, it } from 'vitest'
import { jobTypeLabel, progressPercent } from './jobs'

describe('progressPercent', () => {
  it('supports ratio progress and legacy percentage progress', () => {
    expect(progressPercent(1)).toBe('100%')
    expect(progressPercent(0.5)).toBe('50%')
    expect(progressPercent(20)).toBe('20%')
    expect(jobTypeLabel({ job_type: 'seeding', purpose: 'source_cleanup' })).toBe('源文件清理')
    expect(jobTypeLabel({ job_type: 'seeding' })).toBe('做种管理')
    expect(jobTypeLabel({ job_type: 'transfer' })).toBe('整理入库')
  })
})
