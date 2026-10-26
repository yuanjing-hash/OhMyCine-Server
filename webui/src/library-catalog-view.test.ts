// @vitest-environment happy-dom

import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import LibraryCatalogView from './views/LibraryCatalogView.vue'

const push = vi.fn()
const authState = reactive({ user: null as { id: number } | null, can: (): boolean => false })
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => authState }))

function envelope(data: unknown) {
  return { ok: true, status: 200, json: async () => ({ code: 0, message: 'success', data }) }
}

afterEach(() => { authState.user = null; authState.can = () => false; vi.unstubAllGlobals(); push.mockReset() })

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

  it('loads Server-only account history after the history tab is selected', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return envelope({ version: 'v1', sections: {
        continue_watching: { status: 'ok', list: [], has_more: false }, recently_added: { status: 'ok', list: [], has_more: false },
        favorites: { status: 'ok', list: [], has_more: false }, automatic_collections: { status: 'ok', list: [], has_more: false },
        manual_collections: { status: 'ok', list: [], has_more: false }, media_libraries: { status: 'ok', list: [], has_more: false },
      } })
      if (path === '/api/v1/media-libraries/history?page=1&page_size=24&group_by=work') return envelope({ list: [{ history_id: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', library_id: 1, work_id: 'movie-work', source_kind: 'server', source_name: 'OhMyCine Server', playable: true, title: '历史电影', position: 60, duration: 90, updated_at: 1 }], total: 1, page: 1, page_size: 24, has_more: false })
      throw new Error(`unexpected request ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[1].trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('历史电影')
    expect(wrapper.text()).toContain('OhMyCine Server')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('opens the exact episode history behind a grouped work and exposes an action button', async () => {
    const work = { history_id: 'b'.repeat(64), library_id: 1, work_id: 'series-work', media_type: 'tv', title: '同一部剧', subtitle: 'S01E02 · 第二集', position: 60, duration: 90, updated_at: 2, record_count: 2 }
    const grouped = { list: [work], total: 1, page: 1, page_size: 24, has_more: false }
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return envelope({ sections: {} })
      if (path.endsWith('&group_by=work')) return envelope(grouped)
      if (path === '/api/v1/media-libraries/history?page=1&page_size=24&library_id=1&work_id=series-work') return envelope({ ...grouped, list: [work, { ...work, history_id: 'a'.repeat(64), subtitle: 'S01E01 · 第一集', updated_at: 1 }], total: 2 })
      throw new Error(`unexpected request ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[1].trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.history-card')).toHaveLength(1)
    expect(wrapper.find('button[aria-label="同一部剧观看记录操作"]').exists()).toBe(true)
    await wrapper.findAll('button').find(button => button.text() === '分集记录 · 2')!.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.history-card')).toHaveLength(2)
    expect(wrapper.text()).toContain('S01E01 · 第一集')
    expect(wrapper.text()).toContain('S01E02 · 第二集')
    await wrapper.findAll('button').find(button => button.text() === '返回作品历史')!.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.history-card')).toHaveLength(1)
    wrapper.unmount()
  })

  it('does not publish late history results after the account changes', async () => {
    let finishHistory!: (value: ReturnType<typeof envelope>) => void
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return Promise.resolve(envelope({ sections: {} }))
      if (path.endsWith('&group_by=work')) return new Promise<ReturnType<typeof envelope>>(resolve => { finishHistory = resolve })
      throw new Error(`unexpected request ${path}`)
    })
    authState.user = { id: 1 }
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[1].trigger('click')
    authState.user = { id: 2 }
    await flushPromises()
    finishHistory(envelope({ list: [{ library_id: 1, work_id: 'old-work', title: '前一个账号的私有观看记录' }], total: 1, page: 1, page_size: 24, has_more: false }))
    await flushPromises()
    expect(wrapper.text()).not.toContain('前一个账号的私有观看记录')
    expect(wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[0].attributes('aria-selected')).toBe('true')
    wrapper.unmount()
  })

  it('clears private overview content and refuses its late response after logout', async () => {
    let finishOverview!: (value: ReturnType<typeof envelope>) => void
    authState.user = { id: 1 }
    vi.stubGlobal('fetch', vi.fn(() => new Promise<ReturnType<typeof envelope>>(resolve => { finishOverview = resolve })))
    const wrapper = mount(LibraryCatalogView)
    authState.user = null
    await flushPromises()
    finishOverview(envelope({ sections: { continue_watching: { list: [{ library_id: 1, work_id: 'private', title: '退出账号后不能出现的剧集', position: 10 }] } } }))
    await flushPromises()
    expect(wrapper.text()).not.toContain('退出账号后不能出现的剧集')
    expect(wrapper.findAll('.history-card')).toHaveLength(0)
    wrapper.unmount()
  })

  it('closes a private multi-library chooser when the account changes', async () => {
    authState.user = { id: 1 }
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return envelope({ sections: {
        media_libraries: { status: 'ok', list: [{ id: 1, name: '私有库一' }, { id: 2, name: '私有库二' }] },
      } })
      if (path === '/api/v1/media-libraries/catalog?page=1&page_size=20') return envelope({
        list: [{ id: 'private-work', title: '旧账号的私有作品', kind: 'movie', library_works: [
          { library_id: 1, work_id: 'first-work', library_name: '私有库一', file_count: 1 },
          { library_id: 2, work_id: 'second-work', library_name: '私有库二', file_count: 1 },
        ] }], total: 1, page: 1, page_size: 20, categories: [],
      })
      throw new Error(`unexpected request ${path}`)
    }))
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[5].trigger('click')
    await flushPromises()
    await wrapper.get('.discovery-poster').trigger('click')
    expect(wrapper.find('[aria-labelledby="library-selection-title"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('私有库一')

    authState.user = { id: 2 }
    await flushPromises()
    expect(wrapper.find('[aria-labelledby="library-selection-title"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('私有库一')
    expect(wrapper.text()).not.toContain('旧账号的私有作品')
    wrapper.unmount()
  })

  it('does not let the old account exclusion request replace a new account read', async () => {
    let finishOld!: (value: ReturnType<typeof envelope>) => void
    let finishCurrent!: (value: ReturnType<typeof envelope>) => void
    let excludedReads = 0
    authState.user = { id: 1 }
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return Promise.resolve(envelope({ sections: {} }))
      if (path === '/api/v1/media-libraries/exclusions?page=1&page_size=24') return new Promise<ReturnType<typeof envelope>>(resolve => {
        if (excludedReads++ === 0) finishOld = resolve
        else finishCurrent = resolve
      })
      throw new Error(`unexpected request ${path}`)
    }))
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    const tabs = () => wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')
    await tabs()[6].trigger('click')
    authState.user = { id: 2 }
    await flushPromises()
    await tabs()[6].trigger('click')
    finishOld(envelope({ list: [{ exclusion_id: 'old-private', library_id: 1, library_name: '旧账号私有库', title: '旧账号移出作品' }], total: 1, page: 1, page_size: 24, has_more: false }))
    await flushPromises()
    expect(wrapper.text()).not.toContain('旧账号移出作品')
    expect(wrapper.text()).toContain('正在读取已移出作品')
    finishCurrent(envelope({ list: [], total: 0, page: 1, page_size: 24, has_more: false }))
    await flushPromises()
    expect(wrapper.text()).toContain('暂无已移出的作品')
    wrapper.unmount()
  })

  it('does not dispatch an old account follow-up scan after restore finishes late', async () => {
    let finishRestore!: (value: ReturnType<typeof envelope>) => void
    let restoreSignal: AbortSignal | null = null
    authState.user = { id: 1 }
    authState.can = () => true
    const fetchMock = vi.fn((input: RequestInfo | URL, options?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/media-libraries/overview') return Promise.resolve(envelope({ sections: {} }))
      if (path === '/api/v1/auth/csrf') return Promise.resolve(envelope({ csrf_token: 'fixture-csrf' }))
      if (path === '/api/v1/media-libraries/exclusions?page=1&page_size=24') return Promise.resolve(envelope({ list: [{ exclusion_id: 'old-exclusion', library_id: 1, library_name: '旧账号的库', title: '旧账号的作品' }], total: 1, page: 1, page_size: 24, has_more: false }))
      if (path === '/api/v1/media-libraries/1/exclusions/old-exclusion/restore') {
        restoreSignal = options?.signal as AbortSignal
        return new Promise<ReturnType<typeof envelope>>(resolve => { finishRestore = resolve })
      }
      throw new Error(`unexpected request ${path}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(LibraryCatalogView)
    await flushPromises()
    await wrapper.get('nav[aria-label="媒体库视图"]').findAll('button')[6].trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '恢复并重新扫描')!.trigger('click')
    await flushPromises()
    authState.user = { id: 2 }
    await flushPromises()
    expect((restoreSignal as AbortSignal | null)?.aborted).toBe(true)
    finishRestore(envelope({}))
    await flushPromises()
    expect(fetchMock.mock.calls.some(([input]) => String(input) === '/api/v1/media-libraries/1/scan')).toBe(false)
    expect(wrapper.text()).not.toContain('旧账号的作品')
    wrapper.unmount()
  })
})
