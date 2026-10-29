// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ExploreView from '@/views/ExploreView.vue'
import ResourceContentSummary from '@/components/ResourceContentSummary.vue'
import { resourceContentPreviewPath, resourceContentSelectionPath, type ResourceContentPreview } from '@/resource-content'

const { request, notify } = vi.hoisted(() => ({ request: vi.fn(), notify: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('@/toast', () => ({ notify }))

function preview(token = 'current'): ResourceContentPreview {
  return { version: 1, token: `content-${token}`, status: 'complete', complete: true, files: [
    { token: 'video-13', path: 'Actual Show/Season 01/13.mkv', name: '13.mkv', size: 100, is_dir: false, file_kind: 'video', season: 1, episodes: [13], work_status: 'matched' },
    { token: 'subtitle-13', path: 'Actual Show/Season 01/13.zh-Hans.srt', name: '13.zh-Hans.srt', size: 20, is_dir: false, file_kind: 'subtitle', episodes: [], work_status: 'not_applicable', subtitles: [{ language: 'zh', script: 'Hans', source: 'sidecar_filename' }] },
  ], total_count: 2, displayed_count: 2, total_size: 120, checked_at: new Date().toISOString(), expires_at: '2099-01-01T00:00:00Z', message: 'actual files' }
}
const wrappers: VueWrapper[] = []
function fixture(unavailable = false) {
  request.mockImplementation((path: string) => {
    if (path === '/api/v1/sites') return Promise.resolve({ list: [] })
    if (path === '/api/v1/discovery/search-options') return Promise.resolve({ list: [{ id: 7, name: 'Mixed', site_type: 'bt', searchable: true }] })
    if (path.includes('/torrent-search?')) return Promise.resolve({ groups: [{ site_id: 7, site_name: 'Mixed', site_type: 'bt', status: 'success', page: 1, has_next: false, skipped: 0, items: [{ token: 'result', title: 'Advertisement pack', source_kind: 'bt', result_type: 'magnet', payload_kind: 'magnet', content_inspection: true, expires_at: '2099-01-01T00:00:00Z' }] }] })
    if (path.endsWith('/torrent-results/recognize')) return Promise.resolve({ engine_version: 'fixture', status: 'unrecognized', title: 'Advertisement pack', specifications: {} })
    if (path === resourceContentPreviewPath) return unavailable ? Promise.reject(new Error('元数据暂不可得')) : Promise.resolve(preview())
    if (path === resourceContentSelectionPath) return Promise.resolve({ selection_token: 'selection-current', preview_token: 'content-current', selected_entry_tokens: ['video-13', 'subtitle-13'], file_count: 2, video_count: 1, selected_bytes: 120 })
    if (path === '/api/v1/downloaders') return Promise.resolve({ list: [{ id: 'executor', name: 'qB', type: 'qbittorrent', enabled: true }] })
    if (path === '/api/v1/media-libraries') return Promise.resolve({ list: [{ id: 1, name: 'Library', enabled: true }] })
    if (path === '/api/v1/sites/routes/recommend') return Promise.resolve({ source_kind: 'bt', preview_source_kind: 'url', recommended: { downloader_id: 'executor', media_library_id: 1 }, choices: [{ downloader_id: 'executor', downloader_type: 'qbittorrent', media_library_id: 1, enabled: true, library_name: 'Library', route_kind: 'same_source_local', route_label: 'Local' }] })
    if (path === '/api/v1/download-routes/preview') return Promise.resolve({ source_kind: 'url', downloader_id: 'executor', options: [{ media_library_id: 1, library_name: 'Library', enabled: true, route_kind: 'same_source_local' }] })
    if (path === '/api/v1/discovery/downloads') return Promise.resolve({ id: 'one-task' })
    if (path.startsWith('/api/v1/media-upgrades/')) return Promise.resolve({ id: 'upgrade', state: 'prepared', stage: 'choose_version', library_id: 1, entry_id: 1, work_token: 'work', title: 'Actual Show', media_type: 'tv', tmdb_id: 100, season: 1, episode: 13, revision: 1, expires_at: '2099-01-01T00:00:00Z', recycle_supported: true })
    return Promise.resolve({})
  })
}
async function render(upgrade = false) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/discovery/explore', component: ExploreView }] })
  await router.push({ path: '/discovery/explore', query: { mode: 'resources', site_id: '7', title: 'Pack', ...(upgrade ? { upgrade_id: 'upgrade' } : {}) } })
  await router.isReady()
  const wrapper = mount(ExploreView, { global: { plugins: [router], stubs: { RouterLink: true } } }); wrappers.push(wrapper)
  await flushPromises(); return wrapper
}
beforeEach(() => { fixture(); localStorage.clear(); sessionStorage.clear(); vi.stubGlobal('EventSource', undefined) })
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); request.mockReset(); notify.mockReset(); vi.unstubAllGlobals() })

it('shows actual file names directly and submits only the opaque selection with selected bytes', async () => {
  const wrapper = await render()
  const content = wrapper.get('[aria-label="实际文件内容"]')
  expect(content.text()).toContain('13.mkv')
  expect(content.text()).toContain('13.zh-Hans.srt')
  expect(content.text()).toContain('实际字幕文件的名称标注')
  expect(request.mock.calls.some(([path]) => path === '/api/v1/downloaders')).toBe(false)
  const checkboxes = content.findAll('input[type="checkbox"]')
  await checkboxes[0]!.setValue(true); await checkboxes[1]!.setValue(true)
  await content.findAll('button').find(button => button.text().startsWith('选择文件入库'))!.trigger('click')
  await flushPromises()
  const route = request.mock.calls.find(([path]) => path === '/api/v1/sites/routes/recommend')!
  expect(JSON.parse(String(route[1]!.body))).toMatchObject({ content_selection_token: 'selection-current', expected_bytes: 120 })
  await wrapper.get('form[role="dialog"]').trigger('submit'); await flushPromises()
  const submits = request.mock.calls.filter(([path]) => path === '/api/v1/discovery/downloads')
  expect(submits).toHaveLength(1)
  const body = JSON.parse(String(submits[0]![1]!.body))
  expect(body).toMatchObject({ content_selection_token: 'selection-current', result_token: 'result', downloader_id: 'executor', media_library_id: 1 })
  expect(body).not.toHaveProperty('indices')
})

it('keeps explicit whole-resource intake available after magnet metadata fails', async () => {
  fixture(true)
  const wrapper = await render()
  expect(wrapper.get('[aria-label="实际文件内容"]').text()).toContain('元数据暂不可得')
  await wrapper.get('article footer .btn-primary').trigger('click'); await flushPromises()
  const dialog = wrapper.get('form[role="dialog"]')
  expect(dialog.text()).toContain('获取整个资源')
  expect(dialog.find('button.btn-primary').attributes('disabled')).toBeDefined()
  await dialog.get('input[data-test="whole-resource-intent"]').setValue(true)
  expect(dialog.find('button.btn-primary').attributes('disabled')).toBeUndefined()
  await dialog.trigger('submit'); await flushPromises()
  const call = request.mock.calls.find(([path]) => path === '/api/v1/discovery/downloads')!
  expect(call).toBeDefined()
  expect(JSON.parse(String(call[1]!.body))).toMatchObject({ content_mode: 'all' })
  expect(JSON.parse(String(call[1]!.body))).not.toHaveProperty('content_selection_token')
})

it('does not allow washing to silently use the whole resource when metadata is unavailable', async () => {
  fixture(true)
  const wrapper = await render(true)
  await wrapper.get('article footer .btn-primary').trigger('click'); await flushPromises()
  const dialog = wrapper.get('form[role="dialog"]')
  expect(dialog.text()).toContain('洗版需要先读取实际文件')
  expect(dialog.find('button.btn-primary').attributes('disabled')).toBeDefined()
  await dialog.trigger('submit'); await flushPromises()
  expect(request.mock.calls.some(([path]) => path.endsWith('/submit') || path === '/api/v1/discovery/downloads')).toBe(false)
})

it('discards a late selected-file receipt after the card changes', async () => {
  let resolve!: (value: unknown) => void
  request.mockImplementation(() => new Promise(done => { resolve = done }))
  const wrapper = mount(ResourceContentSummary, { props: { resultToken: 'old', preview: preview('old'), canSelect: true } }); wrappers.push(wrapper)
  await wrapper.get('input[type="checkbox"]').setValue(true)
  await wrapper.findAll('button').find(button => button.text().startsWith('选择文件入库'))!.trigger('click')
  await flushPromises()
  const signal = request.mock.calls[0]![1]!.signal as AbortSignal
  await wrapper.setProps({ resultToken: 'new', preview: preview('new') })
  expect(signal.aborted).toBe(true)
  resolve({ selection_token: 'old-selection', selected_bytes: 100 })
  await flushPromises()
  expect(wrapper.emitted('select')).toBeUndefined()
})
