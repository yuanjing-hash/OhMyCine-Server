import { describe, expect, it } from 'vitest'
import { parseSearchProgress, searchDuration, searchIssueLabel, searchPhaseLabel } from '@/search-progress'

const progress = { total: 1, pending: 0, running: 1, completed: 0, succeeded: 0, failed: 0, result_count: 7 }

describe('safe search progress boundary', () => {
  it('accepts a legacy snapshot and optional detailed stages with zero counters', () => {
    expect(parseSearchProgress(progress, 1)).toMatchObject(progress)
    const value = { ...progress, event_sequence: 1, site_detail: { site_id: 7, site_name: '盘搜', phase: 'queued', result_count: 0, elapsed_ms: 0, queries_total: 0, revision: 1 } }
    expect(parseSearchProgress(value, 1)?.site_detail).toEqual(value.site_detail)
    expect(parseSearchProgress({ total: 0, pending: 0, running: 0, completed: 0, succeeded: 0, failed: 0, result_count: 0, phase: 'preparing_media' }, 1)?.phase).toBe('preparing_media')
  })

  it.each([
    { ...progress, total: 2 }, { ...progress, completed: 1 }, { ...progress, result_count: -1 },
    { ...progress, running: Number.NaN }, { ...progress, event_sequence: Number.MAX_SAFE_INTEGER + 1 },
    { ...progress, site_detail: { site_id: 7, site_name: '盘搜', phase: 'queued', result_count: 0, elapsed_ms: -1 } },
  ])('rejects malformed progress instead of turning it into false completion', value => {
    expect(parseSearchProgress(value, 1)).toBeUndefined()
  })

  it('uses finite readable labels and real elapsed time', () => {
    expect(searchPhaseLabel('waiting_rate_limit')).toBe('等待站点限速')
    expect(searchIssueLabel('resource_browser_restore_timeout')).toContain('尚未确认登录失效')
    expect(searchIssueLabel('private-cookie-response')).not.toContain('private-cookie-response')
    expect(searchDuration(142_000)).toBe('2 分 22 秒')
  })
})
