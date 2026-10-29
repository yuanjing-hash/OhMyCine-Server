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
function button(wrapper: VueWrapper, text: string) {
  const found = wrapper.findAll('button').find(value => value.text() === text)
  if (!found) throw new Error(`Missing button ${text}`)
  return found
}

beforeEach(() => { fixture(); sessionStorage.clear(); localStorage.clear(); vi.stubGlobal('EventSource', undefined) })
afterEach(() => { for (const wrapper of wrappers.splice(0)) wrapper.unmount(); request.mockReset(); notify.mockReset(); vi.unstubAllGlobals() })


const upgradeFixture = (id = 'upgrade-a', title = 'Show A') => ({ id, state: 'prepared', stage: 'choose_version', library_id: 1, entry_id: 77, work_token: 'work-a', title, media_type: 'tv', tmdb_id: 111, season: 1, episode: 2, revision: 1, expires_at: '2099-01-01T00:00:00Z', recycle_supported: true })
function installUpgradeAPI(overrides: (path: string, options?: RequestInit) => unknown = () => undefined) {
  const normal = request.getMockImplementation()!
  request.mockImplementation((path: string, options?: RequestInit) => {
    const override = overrides(path, options)
    if (override !== undefined) return override
    if (path.startsWith('/api/v1/media-upgrades/') && !path.endsWith('/submit')) return Promise.resolve(upgradeFixture(path.split('/').at(-1)!))
    if (path.endsWith('/submit')) return Promise.resolve({ ...upgradeFixture(), state: 'submitted', stage: 'download_import', revision: 2, download_task_id: 'new-download' })
    return normal(path, options)
  })
}
const upgradeQuery = (id = 'upgrade-a', title = 'Show A') => ({ mode: 'resources', site_id: '7', title, identity: 'tmdb', tmdb_id: '111', media_type: 'tv', upgrade_id: id })
it('submits a replacement through the precise intent without a public target or source override', async () => {
  installUpgradeAPI()
  const { wrapper } = await render(upgradeQuery())
  expect(wrapper.get('[data-upgrade-context]').text()).toContain('第 1 季第 2 集')
  await button(wrapper, '入库').trigger('click'); await flushPromises()
  expect(button(wrapper, '确认并入队').attributes('disabled')).toBeUndefined()
  await wrapper.get('form[role="dialog"]').trigger('submit'); await flushPromises()
  const submitted = request.mock.calls.find(([path]) => path === '/api/v1/media-upgrades/upgrade-a/submit')!
  expect(submitted).toBeTruthy()
  expect(JSON.parse(String((submitted[1] as RequestInit).body))).toEqual({ result_token: 'magnet-a', downloader_id: 'receiver', priority: 0, revision: 1 })
  expect(request.mock.calls.some(([path]) => path === '/api/v1/discovery/downloads')).toBe(false)
  expect(wrapper.get('[data-upgrade-context]').text()).toContain('download_import')
  expect(sessionStorage.getItem('omc:site-search:session:v1') ?? '').not.toContain('upgrade-a')
})
it('never falls back to ordinary acquisition if the opaque replacement context cannot load', async () => {
  installUpgradeAPI(path => path === '/api/v1/media-upgrades/upgrade-a' ? Promise.reject(new Error('旧版本已变化')) : undefined)
  const { wrapper } = await render(upgradeQuery())
  expect(wrapper.get('[data-upgrade-context]').text()).toContain('旧版本已变化')
  await button(wrapper, '入库').trigger('click'); await flushPromises()
  expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  expect(request.mock.calls.some(([path]) => path === '/api/v1/discovery/downloads' || String(path).endsWith('/submit'))).toBe(false)
})
it('aborts and ignores a late old-episode context when the same search route changes target', async () => {
  const old = deferred<ReturnType<typeof upgradeFixture>>()
  installUpgradeAPI(path => path === '/api/v1/media-upgrades/upgrade-a' ? old.promise : path === '/api/v1/media-upgrades/upgrade-b' ? Promise.resolve({ ...upgradeFixture('upgrade-b', 'Show B'), episode: 15 }) : undefined)
  const { wrapper, router } = await render(upgradeQuery())
  const first = request.mock.calls.find(([path]) => path === '/api/v1/media-upgrades/upgrade-a')!
  await router.push({ path: '/discovery/explore', query: upgradeQuery('upgrade-b', 'Show B') }); await flushPromises()
  expect((first[1] as RequestInit).signal?.aborted).toBe(true)
  old.resolve(upgradeFixture()); await flushPromises()
  expect(wrapper.get('[data-upgrade-context]').text()).toContain('Show B')
  expect(wrapper.get('[data-upgrade-context]').text()).toContain('第 15 集')
  expect(wrapper.get('[data-upgrade-context]').text()).not.toContain('Show A')
})
