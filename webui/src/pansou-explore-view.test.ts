// @vitest-environment happy-dom
import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ExploreView from './views/ExploreView.vue'
import { saveTorrentSearchSession, type PTSearchGroup } from './sites'

const request = vi.hoisted(() => vi.fn())
const routeQuery = vi.hoisted(() => ({ mode: 'resources', site_id: '7' as string | undefined, title: 'Show' }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: routeQuery }), useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock('@/search-result-recognition', async () => {
  const { ref } = await import('vue')
  return { useSearchResultRecognition: () => ({ recognitions: ref({}), recognitionErrors: ref({}), recognizingTokens: ref([]), libraryStates: ref({}), reset: vi.fn(), dispose: vi.fn(), retry: vi.fn() }) }
})
function group(page = 1): PTSearchGroup { return { site_id: 7, site_name: '盘搜', site_type: 'cloud_share', status: 'success', page, has_next: page === 1, skipped: 0, snapshot_id: 'snapshot-owned', completion: 'received', items: [{ token: `magnet-${page}`, title: `磁力第 ${page} 页`, source_kind: 'bt', result_type: 'magnet', acquisition_available: true, expires_at: new Date(Date.now() + 60_000).toISOString(), origins: [{ kind: 'plugin', name: 'gying' }] }] } }
function fixture() {
  const paths: string[] = []
  request.mockImplementation(async (path: string) => {
    if (path === '/api/v1/sites') return { list: [] }
    if (path === '/api/v1/discovery/search-options') return { list: [{ id: 7, name: '盘搜', site_type: 'cloud_share', searchable: true, health_status: 'online' }] }
    if (path.startsWith('/api/v1/discovery/torrent-search?')) {
      paths.push(path)
      return { groups: [group(Number(new URL(path, 'http://local.test').searchParams.get('page')))] }
    }
    throw new Error(`Unexpected request: ${path}`)
  })
  return paths
}
function render() { return mount(ExploreView, { global: { stubs: { RouterLink: true } } }) }
afterEach(() => { request.mockReset(); sessionStorage.clear(); localStorage.clear(); routeQuery.site_id = '7'; vi.unstubAllGlobals() })

it('uses a single-site snapshot for paging, freezes the applied type filter and updates real response progress', async () => {
  const paths = fixture()
  const wrapper = render()
  try {
    await flushPromises()
    expect(wrapper.text()).toContain('站点搜索进度 1 / 1')
    expect(wrapper.text()).toContain('PanSou · 插件 · gying')
    const controls = wrapper.get('[aria-label="PanSou 结果类型"]')
    await controls.get('select').setValue('all')
    await wrapper.findAll('button').find(button => button.text() === '下一页')!.trigger('click')
    await flushPromises()
    const page = new URL(paths.at(-1)!, 'http://local.test')
    expect(page.searchParams.get('snapshot_id')).toBe('snapshot-owned')
    expect(page.searchParams.get('type_policy')).toBeNull()
    expect(page.searchParams.get('refresh')).toBeNull()
    await wrapper.findAll('button').find(button => button.text() === '上一页')!.trigger('click')
    await flushPromises()
    const previous = new URL(paths.at(-1)!, 'http://local.test')
    expect(previous.searchParams.get('page')).toBe('1')
    expect(previous.searchParams.get('snapshot_id')).toBe('snapshot-owned')
    expect(previous.searchParams.get('type_policy')).toBeNull()
    expect(previous.searchParams.get('refresh')).toBeNull()
    await controls.findAll('button').find(button => button.text() === '按此类型重新搜索')!.trigger('click')
    await flushPromises()
    const refreshed = new URL(paths.at(-1)!, 'http://local.test')
    expect(refreshed.searchParams.get('type_policy')).toBe('all')
    expect(refreshed.searchParams.get('refresh')).toBe('true')
    expect(refreshed.searchParams.get('snapshot_id')).toBeNull()
  } finally { wrapper.unmount() }
})

it('retains cached results from a disabled source but prevents new resource actions', async () => {
  saveTorrentSearchSession(sessionStorage, { input: { keyword: 'Show', mediaType: '', searchBy: 'title', siteID: 7 }, groups: [group()], recognitions: {}, searched: true, savedAt: Date.now() })
  request.mockImplementation(async (path: string) => {
    if (path === '/api/v1/sites' || path === '/api/v1/discovery/search-options') return { list: [] }
    throw new Error(`Unexpected request: ${path}`)
  })
  const wrapper = render()
  try {
    await flushPromises()
    expect(wrapper.text()).toContain('磁力第 1 页')
    expect(wrapper.text()).toContain('来源已停用或不再可访问')
    expect(wrapper.findAll('article footer button').filter(button => button.text() === '入库').every(button => button.attributes('disabled') !== undefined)).toBe(true)
    expect(request.mock.calls.some(([path]) => path.startsWith('/api/v1/discovery/torrent-search?'))).toBe(false)
  } finally { wrapper.unmount() }
})

it('keeps explicit aggregate search refresh when the browser uses the JSON fallback', async () => {
  vi.stubGlobal('EventSource', undefined)
  routeQuery.site_id = undefined
  const paths = fixture()
  const wrapper = render()
  try {
    await flushPromises()
    await wrapper.get('[aria-labelledby="search-site-selector-title"]').findAll('button').find(button => button.text() === '搜索')!.trigger('click')
    await flushPromises()
    expect(paths).toHaveLength(1)
    const query = new URL(paths[0], 'http://local.test').searchParams
    expect(query.get('refresh')).toBe('true')
    expect(query.get('snapshot_id')).toBeNull()
    expect(query.getAll('site_ids')).toEqual(['7'])
  } finally { wrapper.unmount() }
})

it('cancels the in-flight single-site JSON request and ignores a late result', async () => {
  fixture()
  const implementation = request.getMockImplementation()!
  let finish!: (value: unknown) => void
  let signal!: AbortSignal
  request.mockImplementation((path: string, options?: RequestInit) => {
    if (path.startsWith('/api/v1/discovery/torrent-search?')) {
      signal = options!.signal as AbortSignal
      return new Promise(resolve => { finish = resolve })
    }
    return implementation(path, options)
  })
  const wrapper = render()
  try {
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '取消搜索')!.trigger('click')
    expect(signal.aborted).toBe(true)
    finish({ groups: [group()] })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(0)
  } finally { wrapper.unmount() }
})
