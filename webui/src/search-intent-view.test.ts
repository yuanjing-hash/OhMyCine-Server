// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ExploreView from './views/ExploreView.vue'
import type { PTSearchGroup, PTSearchResult } from './sites'

const { request, notify } = vi.hoisted(() => ({ request: vi.fn(), notify: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('@/toast', () => ({ notify }))
vi.mock('@/search-result-recognition', async () => {
  const { ref } = await import('vue')
  return { useSearchResultRecognition: () => ({
    recognitions: ref({}), recognitionErrors: ref({}), recognizingTokens: ref([]), libraryStates: ref({}),
    reset: vi.fn(), dispose: vi.fn(), retry: vi.fn(), acceptManual: vi.fn(),
  }) }
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function result(token = 'magnet-a'): PTSearchResult {
  return { token, title: token, source_kind: 'bt', result_type: 'magnet', expires_at: '2099-01-01T00:00:00Z' }
}
function group(item = result()): PTSearchGroup {
  return { site_id: 7, site_name: '混合来源', site_type: 'cloud_share', status: 'success', page: 1, has_next: false, skipped: 0, items: [item] }
}
function recommendation() {
  return {
    source_kind: 'bt', preview_source_kind: 'url', recommended: { downloader_id: 'receiver', media_library_id: 1 },
    choices: [{ downloader_id: 'receiver', media_library_id: 1, downloader_name: '执行器', downloader_type: 'pan115_offline', library_name: '媒体库', enabled: true, reason_code: '', reason_message: '', route_kind: 'same_source_provider', route_label: '同源云端整理' }],
  }
}
function fixture(searchReply: (path: string, options?: RequestInit) => unknown = () => ({ groups: [group()] })) {
  request.mockImplementation((path: string, options?: RequestInit) => {
    if (path === '/api/v1/sites') return Promise.resolve({ list: [] })
    if (path === '/api/v1/discovery/search-options') return Promise.resolve({ list: [{ id: 7, name: '混合来源', site_type: 'cloud_share', searchable: true, health_status: 'online' }] })
    if (path.includes('/torrent-search?')) return Promise.resolve(searchReply(path, options))
    if (path === '/api/v1/downloaders') return Promise.resolve({ list: [{ id: 'receiver', name: '执行器', type: 'pan115_offline', enabled: true }] })
    if (path === '/api/v1/media-libraries') return Promise.resolve({ list: [{ id: 1, name: '媒体库', enabled: true }] })
    if (path === '/api/v1/sites/routes/recommend') return Promise.resolve(recommendation())
    if (path === '/api/v1/download-routes/preview') {
      const body = JSON.parse(String(options?.body))
      if (body.source_kind !== 'url') return Promise.reject(new Error('盘搜资源类型不支持入库'))
      return Promise.resolve({ downloader_id: 'receiver', source_kind: 'url', options: [{ media_library_id: 1, library_name: '媒体库', enabled: true, reason_code: '', reason_message: '' }] })
    }
    if (path === '/api/v1/discovery/downloads') return Promise.resolve({ id: 'accepted-once' })
    return Promise.reject(new Error(`Unexpected request ${path}`))
  })
}
const wrappers: VueWrapper[] = []
async function render(query: Record<string, string> = { mode: 'resources', site_id: '7', title: '旧作品', identity: 'tmdb', tmdb_id: '111', media_type: 'tv', year: '2026' }) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/discovery/explore', component: ExploreView }] })
  await router.push({ path: '/discovery/explore', query })
  await router.isReady()
  const wrapper = mount(ExploreView, { global: { plugins: [router], stubs: { RouterLink: true, SharePreviewDialog: { props: ['open', 'item'], template: '<div v-if="open || item" data-share-dialog />' } } } })
  wrappers.push(wrapper)
  await flushPromises()
  return { wrapper, router }
}
async function submit(wrapper: VueWrapper, keyword: string) {
  await wrapper.get('#discovery-keyword').setValue(keyword)
  await wrapper.get('#discovery-keyword').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}
function searches() { return request.mock.calls.filter(([path]) => String(path).includes('/torrent-search?')) }
function button(wrapper: VueWrapper, text: string) {
  const found = wrapper.findAll('button').find(value => value.text() === text)
  if (!found) throw new Error(`Missing button ${text}`)
  return found
}

beforeEach(() => { fixture(); sessionStorage.clear(); localStorage.clear(); vi.stubGlobal('EventSource', undefined) })
afterEach(() => { for (const wrapper of wrappers.splice(0)) wrapper.unmount(); request.mockReset(); notify.mockReset(); vi.unstubAllGlobals() })

it('drops a routed work identity and derived year when a different title is submitted', async () => {
  const { wrapper } = await render()
  expect(searches()[0]![0]).toContain('/media/tv/111/torrent-search?')
  await submit(wrapper, '迪迦')
  const next = new URL(String(searches().at(-1)![0]), 'https://server.test')
  expect(next.pathname).toBe('/api/v1/discovery/torrent-search')
  expect(next.searchParams.get('keyword')).toBe('迪迦')
  expect(next.searchParams.has('tmdb_id')).toBe(false)
  expect(next.searchParams.has('year')).toBe(false)
  expect(wrapper.text()).not.toContain('已确认作品身份')
})

it('allows a new draft to replace a running request and discards its late result', async () => {
  const old = deferred<unknown>()
  fixture(path => path.includes('/media/tv/111/') ? old.promise : { groups: [group(result('new-result'))] })
  const { wrapper } = await render()
  const signal = searches()[0]![1]!.signal as AbortSignal
  await wrapper.get('#discovery-keyword').setValue('迪迦')
  const submitButton = wrapper.get('#discovery-keyword').element.closest('form')!.querySelector('button.btn-primary')!
  expect(submitButton.hasAttribute('disabled')).toBe(false)
  await submit(wrapper, '迪迦')
  expect(signal.aborted).toBe(true)
  old.resolve({ groups: [group(result('stale-result'))] })
  await flushPromises()
  expect(wrapper.text()).toContain('new-result')
  expect(wrapper.text()).not.toContain('stale-result')
})

it('reinitializes a reused resource route and supersedes the previous identity', async () => {
  const { wrapper, router } = await render()
  await router.replace({ path: '/discovery/explore', query: { mode: 'resources', site_id: '7', title: '迪迦', identity: 'tmdb', tmdb_id: '222', media_type: 'tv', year: '1996' } })
  await flushPromises()
  expect(wrapper.get('#discovery-keyword').element).toHaveProperty('value', '迪迦')
  expect(wrapper.get('#discovery-year').element).toHaveProperty('value', '1996')
  expect(searches().at(-1)![0]).toContain('/media/tv/222/torrent-search?')
  expect(wrapper.text()).not.toContain('旧作品')
})

it('uses the same canonical preview kind for a magnet recommendation and final submission', async () => {
  const { wrapper } = await render({ mode: 'resources', site_id: '7', title: '磁力' })
  await wrapper.get('article footer .btn-primary').trigger('click')
  await flushPromises()
  const calls = request.mock.calls.filter(([path]) => path === '/api/v1/download-routes/preview')
  expect(calls).toHaveLength(1)
  expect(JSON.parse(String(calls[0]![1]!.body)).source_kind).toBe('url')
  expect(notify).not.toHaveBeenCalledWith('盘搜资源类型不支持入库', 'error')
  const confirmation = button(wrapper, '确认并入队')
  expect(confirmation.attributes('disabled'), JSON.stringify(notify.mock.calls)).toBeUndefined()
  // Happy DOM does not dispatch a form's native submit as a button-click default.
  await wrapper.get('form[role="dialog"]').trigger('submit')
  await flushPromises()
  const submits = request.mock.calls.filter(([path]) => path === '/api/v1/discovery/downloads')
  expect(submits).toHaveLength(1)
  expect(JSON.parse(String(submits[0]![1]!.body)).result_token).toBe('magnet-a')
})

it('filters mounted cards by actual resource type and combines selected types', async () => {
  fixture(() => ({ groups: [{ ...group(), items: [
    result('magnet-a'),
    { ...result('torrent-a'), result_type: undefined, payload_kind: 'torrent' },
    { ...result('115-a'), source_kind: '115_share', result_type: '115', payload_kind: 'share' },
    { ...result('quark-a'), source_kind: 'quark_share', result_type: 'quark', payload_kind: 'share' },
  ] }] }))
  const { wrapper } = await render({ mode: 'resources', site_id: '7', title: '类型' })
  const titles = () => wrapper.findAll('article h2').map(card => card.text()).sort()
  expect(titles()).toEqual(['115-a', 'magnet-a', 'quark-a', 'torrent-a'])
  const filters = wrapper.get('fieldset[aria-label="资源类型筛选"]')
  await filters.get('input[value="magnet"]').setValue(true)
  expect(titles()).toEqual(['magnet-a'])
  await filters.get('input[value="115"]').setValue(true)
  expect(titles()).toEqual(['115-a', 'magnet-a'])
  await filters.get('button').trigger('click')
  expect(titles()).toEqual(['115-a', 'magnet-a', 'quark-a', 'torrent-a'])
})

it.each(['resolve', 'reject'] as const)('retires an old-result route recommendation on new search, including late %s', async outcome => {
  const old = deferred<unknown>()
  const implementation = request.getMockImplementation()!
  request.mockImplementation((path: string, options?: RequestInit) => path === '/api/v1/sites/routes/recommend' ? old.promise : implementation(path, options))
  const { wrapper } = await render({ mode: 'resources', site_id: '7', title: 'A' })
  await wrapper.get('article footer .btn-primary').trigger('click')
  await flushPromises()
  const pending = request.mock.calls.find(([path]) => path === '/api/v1/sites/routes/recommend')![1]!.signal as AbortSignal
  await submit(wrapper, 'B')
  expect(pending.aborted).toBe(true)
  if (outcome === 'resolve') old.resolve(recommendation())
  else old.reject(new Error('obsolete recommendation failure'))
  await flushPromises()
  expect(wrapper.text()).not.toContain('创建下载任务')
  expect(notify).not.toHaveBeenCalledWith('obsolete recommendation failure', 'error')
})

it('does not reopen an old share preview after its delayed option reads finish', async () => {
  const old = deferred<unknown>()
  fixture(() => ({ groups: [group({ ...result('share-a'), source_kind: '115_share', result_type: '115' })] }))
  const implementation = request.getMockImplementation()!
  request.mockImplementation((path: string, options?: RequestInit) => path === '/api/v1/downloaders' ? old.promise : implementation(path, options))
  const { wrapper } = await render({ mode: 'resources', site_id: '7', title: 'A' })
  void button(wrapper, '预览分享链接内部内容').trigger('click')
  await flushPromises()
  await submit(wrapper, 'B')
  old.resolve({ list: [] })
  await flushPromises()
  expect(wrapper.find('[data-share-dialog]').exists()).toBe(false)
})

