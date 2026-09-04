// @vitest-environment happy-dom

import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import LibraryCatalogView from './views/LibraryCatalogView.vue'

const push = vi.fn()
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }))

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, message: 'success', data }) }
}

afterEach(() => { vi.unstubAllGlobals(); push.mockReset() })

describe('LibraryCatalogView overview', () => {
  it('opens as the Server-wide overview and does not duplicate history as a second summary row', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return envelope({ version: 'v1', sections: {
        continue_watching: { status: 'ok', list: [{ library_id: 1, work_id: 'series-work', title: '测试剧', subtitle: 'S01E02 · 第二集', position: 20, duration: 100 }], has_more: false },
        recently_added: { status: 'ok', list: [{ library_id: 1, work_id: 'movie-work', title: '测试电影', kind: 'movie' }], has_more: false },
        favorites: { status: 'ok', list: [{ library_id: 1, work_id: 'favorite-work', title: '收藏电影', kind: 'movie' }], has_more: false },
        automatic_collections: { status: 'ok', list: [{ id: 'auto', name: '系列电影合集', source: 'tmdb', kind: 'collection', item_count: 3 }], has_more: false },
        manual_collections: { status: 'ok', list: [{ id: 'manual', name: '周末片单', source: 'manual', kind: 'collection', item_count: 2 }], has_more: false },
        media_libraries: { status: 'ok', list: [{ id: 1, name: '家庭影视库', status: 'listening', entry_count: 9, work_count: 6 }], has_more: false },
      } })
      throw new Error(`unexpected request ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()

    expect(wrapper.text()).toContain('继续观看')
    expect(wrapper.text()).toContain('我的收藏')
    expect(wrapper.text()).toContain('自动合集')
    expect(wrapper.text()).toContain('我的合集')
    expect(wrapper.text()).toContain('最近入库')
    expect(wrapper.text()).toContain('家庭影视库')
    expect(wrapper.text()).not.toContain('最近历史')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/media-libraries/overview')
    wrapper.unmount()
  })

  it('loads unified account history with external source labels after the history tab is selected', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return envelope({ version: 'v1', sections: {
        continue_watching: { status: 'ok', list: [], has_more: false }, recently_added: { status: 'ok', list: [], has_more: false },
        favorites: { status: 'ok', list: [], has_more: false }, automatic_collections: { status: 'ok', list: [], has_more: false },
        manual_collections: { status: 'ok', list: [], has_more: false }, media_libraries: { status: 'ok', list: [], has_more: false },
      } })
      if (path === '/api/v1/media-libraries/history?page=1&page_size=24') return envelope({ list: [{ library_id: 0, work_id: 'emby:item:42', source_kind: 'emby', source_name: '客厅 Emby', playable: false, title: '历史电影', poster_url: 'https://image.example.test/poster.jpg', position: 60, duration: 90, updated_at: 1 }], total: 1, page: 1, page_size: 24, has_more: false })
      throw new Error(`unexpected request ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[1].trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('历史电影')
    expect(wrapper.text()).toContain('客厅 Emby')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
