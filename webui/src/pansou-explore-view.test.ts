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
function fixture(result: (page: number) => PTSearchGroup = group) {
  const paths: string[] = []
  request.mockImplementation(async (path: string) => {
    if (path === '/api/v1/sites') return { list: [] }
    if (path === '/api/v1/discovery/search-options') return { list: [{ id: 7, name: '盘搜', site_type: 'cloud_share', searchable: true, health_status: 'online' }] }
    if (path.startsWith('/api/v1/discovery/torrent-search?')) {
      paths.push(path)
      return { groups: [result(Number(new URL(path, 'http://local.test').searchParams.get('page')))] }
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
    expect(query.get('site_id') ? [query.get('site_id')] : query.getAll('site_ids')).toEqual(['7'])
    expect(query.has('site_id') && query.has('site_ids')).toBe(false)
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

it.each(['盘搜 · 115 分享', '我收藏的分享站'])('labels the Site name %s separately from the actual Quark provider and plugin origin', async name => {
  fixture(page => ({ ...group(page), site_name: name, items: [{ ...group(page).items[0]!, title: '嫁金钗', source_kind: 'quark_share', result_type: 'quark', origins: [{ kind: 'plugin', name: 'yunso' }], matched_name: '嫁金钗' }] }))
  const wrapper = render()
  try {
    await flushPromises()
    const labels = wrapper.get('article').findAll('.status-chip').map(chip => chip.text())
    expect(labels).toContain(`站点：${name}`)
    expect(labels).toContain('资源：夸克')
    expect(labels).toContain('PanSou · 插件 · yunso')
    expect(labels).toContain('命中 嫁金钗')
    expect(labels).not.toContain(name)
  } finally { wrapper.unmount() }
})

it.each(['all', 'manual'])('keeps %s results visible with distinct unsupported and receiver-unavailable states, without disabling magnets', async policy => {
  const noReceiver = '当前没有可用的夸克分享接收器，请先完成连接授权、目录验证并启用接收器。'
  const unsupported = '暂不支持此网盘的分享转存，可复制资源链接查看。'
  const paths = fixture(page => ({ ...group(page), items: [
    { ...group(page).items[0]!, token: 'quark', title: 'Quark result', source_kind: 'quark_share', result_type: 'quark', acquisition_available: false, acquisition_reason: noReceiver },
    { ...group(page).items[0]!, token: 'baidu', title: 'Baidu result', source_kind: 'unsupported', result_type: 'baidu', acquisition_available: false, acquisition_reason: unsupported },
    { ...group(page).items[0]!, token: 'magnet', title: 'Magnet result' },
  ] }))
  const wrapper = render()
  try {
    await flushPromises()
    const controls = wrapper.get('[aria-label="PanSou 结果类型"]')
    await controls.get('select').setValue(policy)
    await controls.findAll('button').find(button => button.text() === '按此类型重新搜索')!.trigger('click')
    await flushPromises()
    expect(new URL(paths.at(-1)!, 'http://local.test').searchParams.get('type_policy')).toBe(policy)
    const cards = wrapper.findAll('article')
    const quark = cards.find(card => card.text().includes('Quark result'))!
    const baidu = cards.find(card => card.text().includes('Baidu result'))!
    const magnet = cards.find(card => card.text().includes('Magnet result'))!
    expect(quark.text()).toContain(noReceiver)
    expect(quark.get('footer .btn-primary').text()).toBe('暂不可入库')
    expect(quark.get('footer .btn-primary').attributes('disabled')).toBeDefined()
    expect(quark.findAll('button').find(button => button.text() === '预览分享链接内部内容')!.attributes('disabled')).toBeDefined()
    expect(baidu.text()).toContain(unsupported)
    expect(baidu.get('footer .btn-primary').text()).toBe('仅供查看')
    expect(magnet.get('footer .btn-primary').text()).toBe('入库')
    expect(magnet.get('footer .btn-primary').attributes('disabled')).toBeUndefined()
    expect(quark.findAll('button').find(button => button.text() === '复制资源链接')!.attributes('disabled')).toBeUndefined()
  } finally { wrapper.unmount() }
})

it.each(['115_share', 'quark_share'])('keeps legacy %s results usable and opens the real dialog with freshly returned receiver capabilities', async source => {
  fixture(page => ({ ...group(page), items: [{ token: 'legacy', title: 'Legacy share', source_kind: source, expires_at: new Date(Date.now() + 60_000).toISOString() }] }))
  const implementation = request.getMockImplementation()!
  request.mockImplementation((path: string, options?: RequestInit) => {
    if (path === '/api/v1/downloaders') return Promise.resolve({ list: [
      { id: '115', name: '115 receiver', type: 'pan115_offline', enabled: true, capabilities: { share_receive: true } },
      { id: 'quark', name: 'Quark unfinished', type: 'quark_share', enabled: true, capabilities: { share_receive: false } },
    ] })
    if (path === '/api/v1/media-libraries') return Promise.resolve({ list: [] })
    if (path === '/api/v1/discovery/share-preview') return Promise.resolve({ token: 'preview', entries: [], total_size: 0, file_count: 0, expires_at: new Date(Date.now() + 60_000).toISOString() })
    return implementation(path, options)
  })
  const wrapper = render()
  try {
    await flushPromises()
    const card = wrapper.get('article')
    expect(card.get('footer .btn-primary').attributes('disabled')).toBeUndefined()
    expect(card.findAll('.status-chip').map(chip => chip.text())).toContain(`资源：${source === '115_share' ? '115' : '夸克'}`)
    await card.findAll('button').find(button => button.text() === '预览分享链接内部内容')!.trigger('click')
    await flushPromises()
    const dialog = wrapper.get('[aria-labelledby="share-preview-title"]')
    const calls = request.mock.calls.filter(([path]) => path === '/api/v1/discovery/share-preview')
    if (source === '115_share') {
      expect(dialog.get('select').element).toHaveProperty('value', '115')
      expect(JSON.parse(String(calls[0]![1]!.body))).toEqual({ result_token: 'legacy', downloader_id: '115' })
    } else {
      expect(dialog.text()).toContain('当前没有可用的夸克分享接收器')
      expect(dialog.get('select').findAll('option')).toHaveLength(1)
      expect(calls).toHaveLength(0)
    }
  } finally { wrapper.unmount() }
})
