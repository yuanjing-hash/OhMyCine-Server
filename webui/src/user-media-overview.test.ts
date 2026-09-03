import { describe, expect, it } from 'vitest'
import {
  historyProgress, normalizeUserHistoryPage, normalizeUserMediaOverview, userMediaEndpoints,
} from './user-media-overview'

describe('Server media-library overview contract', () => {
  it('uses cookie-session management routes instead of Player device routes', () => {
    expect(userMediaEndpoints.overview).toBe('/api/v1/media-libraries/overview')
    expect(userMediaEndpoints.history(2, 24)).toBe('/api/v1/media-libraries/history?page=2&page_size=24')
    expect(userMediaEndpoints.collectionItems('collection/a')).toBe('/api/v1/media-libraries/collections/collection%2Fa/items')
    expect(Object.values(userMediaEndpoints).join(' ')).not.toContain('/api/v1/player/')
  })

  it('normalizes nullable and malformed section lists without reviving a recent-history section', () => {
    const result = normalizeUserMediaOverview({ version: 'v1', sections: {
      continue_watching: { status: 'ok', list: [{ library_id: 0, work_id: '', title: '无效目标' }] },
      recently_added: { status: 'unavailable', list: [], error_code: 'CATALOG_BUSY' },
      favorites: {}, automatic_collections: {}, manual_collections: {}, media_libraries: {},
      recent_history: { status: 'ok', list: [{ title: '不应进入总览' }] },
    } })
    expect(result.sections.continue_watching.list).toEqual([])
    expect(result.sections.recently_added).toMatchObject({ status: 'unavailable', error_code: 'CATALOG_BUSY' })
    expect(result.sections).not.toHaveProperty('recent_history')
  })

  it('drops history rows without a current Server catalog target and bounds progress', () => {
    const page = normalizeUserHistoryPage({ list: [
      { library_id: 2, work_id: 'safe-work', title: '剧名', subtitle: 'S01E02 · 第二集', position: 50, duration: 100, updated_at: 1 },
      { library_id: 0, work_id: '', title: '旧记录' },
    ], total: 2, page: 1, page_size: 24, has_more: false })
    expect(page.list).toHaveLength(1)
    expect(page.list[0].title).toBe('剧名')
    expect(historyProgress(page.list[0])).toBe(50)
    expect(historyProgress({ ...page.list[0], position: 200 })).toBe(100)
  })
})
