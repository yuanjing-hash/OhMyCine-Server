// @vitest-environment happy-dom

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ExploreView from '@/views/ExploreView.vue'
import { discoverySearchOptionsPath, torrentSearchPath, type TorrentSearchGroup, type TorrentSearchProgress } from '@/sites'

const { apiMock, routeMock } = vi.hoisted(() => ({
  apiMock: vi.fn(),
  routeMock: { query: {} as Record<string, string> },
}))

vi.mock('@/api/client', async importOriginal => ({
  ...await importOriginal<typeof import('@/api/client')>(),
  api: apiMock,
}))
vi.mock('vue-router', () => ({ useRoute: () => routeMock, useRouter: () => ({ replace: vi.fn(), push: vi.fn() }) }))
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
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function group(siteID: number, status: 'success' | 'error', items = 0): TorrentSearchGroup {
  return {
    site_id: siteID, site_name: `测试站点 ${siteID}`, site_type: 'bt_resource', status,
    error_code: status === 'error' ? 'resource_result_invalid' : undefined,
    page: 1, has_next: false, skipped: 0,
    items: Array.from({ length: items }, (_, index) => ({
      token: `result-${siteID}-${index}`, title: `测试资源 ${siteID}-${index}`, expires_at: '2099-01-01T00:00:00Z',
    })),
  }
}

class SearchEventSource {
  static instances: SearchEventSource[] = []
  listeners = new Map<string, (event: MessageEvent<string>) => void>()
  onerror: (() => void) | null = null
  closed = false
  constructor(public url: string) { SearchEventSource.instances.push(this) }
  addEventListener(type: string, listener: (event: MessageEvent<string>) => void) { this.listeners.set(type, listener) }
  close() { this.closed = true }
  emit(type: string, value: unknown) { this.listeners.get(type)?.(new MessageEvent(type, { data: JSON.stringify(value) })) }
}

const wrappers: VueWrapper[] = []
function render(fixedSite = true) {
  routeMock.query = fixedSite ? { mode: 'resources', site_id: '7' } : { mode: 'resources' }
  const wrapper = mount(ExploreView, {
    global: { plugins: [createPinia()], stubs: { RouterLink: true, SharePreviewDialog: true } },
  })
  wrappers.push(wrapper)
  return wrapper
}

async function search(wrapper: VueWrapper, keyword = '名侦探柯南') {
  await wrapper.get('#discovery-keyword').setValue(keyword)
  await wrapper.get('#discovery-keyword').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}

function button(wrapper: VueWrapper, label: string) {
  const found = wrapper.findAll('button').find(candidate => candidate.text() === label)
  if (!found) throw new Error(`button missing: ${label}`)
  return found
}

function expectProgress(wrapper: VueWrapper, value: Pick<TorrentSearchProgress, 'total' | 'completed' | 'pending' | 'running' | 'succeeded' | 'failed' | 'result_count'>) {
  const panel = wrapper.get('section[aria-live="polite"]')
  expect(panel.text()).toContain(`站点搜索进度 ${value.completed} / ${value.total}`)
  expect(panel.text()).toContain(`等待 ${value.pending} · 正在搜索 ${value.running} · 成功 ${value.succeeded} · 失败 ${value.failed} · 已发现 ${value.result_count}`)
  expect(panel.get('[role="progressbar"]').attributes('aria-valuenow')).toBe(String(value.completed))
}

const running = { total: 1, completed: 0, pending: 0, running: 1, succeeded: 0, failed: 0, result_count: 0 }

beforeEach(() => {
  apiMock.mockReset()
  sessionStorage.clear()
  localStorage.clear()
  SearchEventSource.instances = []
  vi.stubGlobal('EventSource', SearchEventSource)
})
afterEach(() => {
  for (const wrapper of wrappers.splice(0)) wrapper.unmount()
  vi.unstubAllGlobals()
})

describe('fixed-site JSON search progress', () => {
  it.each([
    { status: 'success' as const, items: 2, succeeded: 1, failed: 0 },
    { status: 'success' as const, items: 0, succeeded: 1, failed: 0 },
    { status: 'error' as const, items: 0, succeeded: 0, failed: 1 },
  ])('settles a $status group with $items results', async ({ status, items, succeeded, failed }) => {
    const request = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockReturnValue(request.promise)
    const wrapper = render()
    await search(wrapper)
    expectProgress(wrapper, running)
    const url = new URL(apiMock.mock.calls[0][0], 'https://server.test')
    expect(url.pathname).toBe(torrentSearchPath)
    expect(url.searchParams.get('site_id')).toBe('7')
    expect(url.searchParams.has('site_ids')).toBe(false)
    expect(SearchEventSource.instances).toHaveLength(0)

    request.resolve({ groups: [group(7, status, items)] })
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded, failed, result_count: items })
    expect(wrapper.text()).not.toContain('取消搜索')
    if (status === 'error') expect(wrapper.text()).toContain('测试站点 7 · 重试')
  })

  it('settles a failed top-level request and permits a fresh fixed-site search', async () => {
    apiMock.mockRejectedValueOnce(new Error('搜索服务暂时不可用')).mockResolvedValueOnce({ groups: [group(7, 'success', 1)] })
    const wrapper = render()
    await search(wrapper)
    expect(wrapper.text()).toContain('搜索服务暂时不可用')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 0 })
    await button(wrapper, '重试此站').trigger('click')
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 1 })
    expect(wrapper.text()).not.toContain('搜索服务暂时不可用')
  })

  it.each(['resolve', 'reject'] as const)('cancels JSON requests and ignores a late $0', async outcome => {
    const request = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockReturnValue(request.promise)
    const wrapper = render()
    await search(wrapper)
    const signal = apiMock.mock.calls[0][1].signal as AbortSignal
    await button(wrapper, '取消搜索').trigger('click')
    expect(signal.aborted).toBe(true)
    expect(wrapper.text()).not.toContain('取消搜索')
    if (outcome === 'resolve') request.resolve({ groups: [group(7, 'success', 1)] })
    else request.reject(new Error('取消后的旧错误'))
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 0, pending: 0, running: 0, succeeded: 0, failed: 0, result_count: 0 })
    expect(wrapper.text()).not.toContain('测试资源 7-0')
    expect(wrapper.text()).not.toContain('取消后的旧错误')
  })

  it('counts a page change as the same site and replaces the result count', async () => {
    const first = { ...group(7, 'success', 2), has_next: true }
    const next = { ...group(7, 'success', 1), page: 2 }
    apiMock.mockResolvedValueOnce({ groups: [first] }).mockResolvedValueOnce({ groups: [next] })
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '下一页').trigger('click')
    await flushPromises()
    expect(new URL(apiMock.mock.calls[1][0], 'https://server.test').searchParams.get('page')).toBe('2')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 1 })
    expect(wrapper.text()).toContain('测试站点 7 · 第 2 页')
    expect(wrapper.text()).not.toContain('测试资源 7-1')
  })

  it('clears a failed page request when the same page is requested again successfully', async () => {
    const next = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockResolvedValueOnce({ groups: [{ ...group(7, 'success', 2), has_next: true }] })
      .mockRejectedValueOnce(new Error('分页请求暂时不可用'))
      .mockReturnValueOnce(next.promise)
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '下一页').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('分页请求暂时不可用')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 2 })
    await button(wrapper, '下一页').trigger('click')
    expect(wrapper.text()).not.toContain('分页请求暂时不可用')
    next.resolve({ groups: [{ ...group(7, 'success', 1), page: 2 }] })
    await flushPromises()
    expect(wrapper.text()).not.toContain('搜索失败')
    expect(wrapper.text()).not.toContain('分页请求暂时不可用')
    expect(wrapper.text()).toContain('测试站点 7 · 第 2 页')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 1 })
  })

  it.each([
    { label: 'missing', groups: [] },
    { label: 'duplicate', groups: [group(7, 'success'), group(7, 'success')] },
    { label: 'foreign', groups: [group(8, 'success')] },
  ])('does not claim success when the requested site group is $label', async ({ groups }) => {
    apiMock.mockResolvedValue({ groups })
    const wrapper = render()
    await search(wrapper)
    expect(wrapper.text()).toContain('站点搜索结果不完整，请重试')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 0 })
  })
})

describe('site retry request ownership', () => {
  it('restores the retained site failure after cancelling its retry', async () => {
    const retry = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockResolvedValueOnce({ groups: [group(7, 'error')] }).mockReturnValueOnce(retry.promise)
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    await button(wrapper, '取消搜索').trigger('click')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 0 })
    expect(wrapper.text()).toContain('失败站点：')
    retry.resolve({ groups: [group(7, 'success', 1)] })
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 0 })
    expect(wrapper.text()).not.toContain('测试资源 7-0')
  })

  it.each(['success', 'error', 'transport'] as const)('settles a retry with %s without adding another site', async outcome => {
    const retry = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockResolvedValueOnce({ groups: [group(7, 'error')] }).mockReturnValueOnce(retry.promise)
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    expectProgress(wrapper, running)
    expect(wrapper.text()).not.toContain('失败站点：')
    expect(wrapper.text()).not.toContain('暂时不可用（resource_result_invalid）')
    expect(new URL(apiMock.mock.calls[1][0], 'https://server.test').searchParams.get('site_id')).toBe('7')
    if (outcome === 'transport') retry.reject(new Error('重试暂时不可用'))
    else retry.resolve({ groups: [group(7, outcome, outcome === 'success' ? 1 : 0)] })
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: outcome === 'success' ? 1 : 0, failed: outcome === 'success' ? 0 : 1, result_count: outcome === 'success' ? 1 : 0 })
    expect(wrapper.text()).not.toContain('取消搜索')
  })

  it.each(['resolve', 'reject'] as const)('ignores an old retry that %s after a new search starts', async outcome => {
    const retry = deferred<{ groups: TorrentSearchGroup[] }>()
    const current = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockResolvedValueOnce({ groups: [group(7, 'error')] }).mockReturnValueOnce(retry.promise).mockReturnValueOnce(current.promise)
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    await button(wrapper, '取消搜索').trigger('click')
    await search(wrapper, '新的搜索')
    expectProgress(wrapper, running)
    const currentProgressText = wrapper.get('section[aria-live="polite"]').text()
    if (outcome === 'resolve') retry.resolve({ groups: [group(7, 'success', 2)] })
    else retry.reject(new Error('旧请求错误'))
    await flushPromises()
    expectProgress(wrapper, running)
    expect(wrapper.get('section[aria-live="polite"]').text()).toBe(currentProgressText)
    expect(wrapper.text()).not.toContain('旧请求错误')
    expect(wrapper.text()).not.toContain('测试资源 7-0')
    expect(wrapper.text()).toContain('取消搜索')
    current.resolve({ groups: [group(7, 'success', 1)] })
    await flushPromises()
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 1 })
  })
})

describe('aggregate search progress', () => {
  async function aggregate(wrapper: VueWrapper) {
    await search(wrapper)
    await wrapper.get('[role="dialog"] button.btn-primary').trigger('click')
    await flushPromises()
  }

  function options(path: string) {
    if (path === discoverySearchOptionsPath) return Promise.resolve({ list: [7, 8].map(id => ({ id, name: `测试站点 ${id}`, site_type: 'bt_resource', health_status: 'healthy', searchable: true })) })
    throw new Error(`unexpected path: ${path}`)
  }

  it('keeps complete SSE snapshots authoritative and preserves other sites during a retry', async () => {
    const retry = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockImplementation((path: string) => path.startsWith(torrentSearchPath) ? retry.promise : options(path))
    const wrapper = render(false)
    await aggregate(wrapper)
    const stream = SearchEventSource.instances[0]
    expect(new URL(stream.url, 'https://server.test').searchParams.getAll('site_ids')).toEqual(['7', '8'])
    const first = { total: 2, completed: 1, pending: 0, running: 1, succeeded: 1, failed: 0, result_count: 1, site_id: 8, site_name: '测试站点 8', site_status: 'running' }
    stream.emit('site', group(7, 'success', 1))
    stream.emit('progress', first)
    await flushPromises()
    expectProgress(wrapper, first)
    stream.emit('site', group(8, 'error'))
    await flushPromises()
    expectProgress(wrapper, first)
    const done = { total: 2, completed: 2, pending: 0, running: 0, succeeded: 1, failed: 1, result_count: 1 }
    stream.emit('done', done)
    await flushPromises()
    expectProgress(wrapper, done)
    expect(stream.closed).toBe(true)
    await button(wrapper, '测试站点 8 · 重试').trigger('click')
    expectProgress(wrapper, { ...first, result_count: 1 })
    retry.resolve({ groups: [group(8, 'success', 1)] })
    await flushPromises()
    expectProgress(wrapper, { total: 2, completed: 2, pending: 0, running: 0, succeeded: 2, failed: 0, result_count: 2 })
    expect(wrapper.text()).toContain('测试资源 7-0')
    expect(wrapper.text()).toContain('测试资源 8-0')
  })

  it('settles scoped JSON fallback from both successful and failed groups', async () => {
    vi.stubGlobal('EventSource', undefined)
    apiMock.mockImplementation((path: string) => path.startsWith(torrentSearchPath) ? Promise.resolve({ groups: [group(7, 'success', 1), group(8, 'error')] }) : options(path))
    const wrapper = render(false)
    await aggregate(wrapper)
    const path = apiMock.mock.calls.find(call => call[0].startsWith(torrentSearchPath))![0]
    expect(new URL(path, 'https://server.test').searchParams.getAll('site_ids')).toEqual(['7', '8'])
    expectProgress(wrapper, { total: 2, completed: 2, pending: 0, running: 0, succeeded: 1, failed: 1, result_count: 1 })
  })

  it('settles JSON after a pre-delivery stream error and ignores late events from the closed stream', async () => {
    const fallback = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockImplementation((path: string) => path.startsWith(torrentSearchPath) ? fallback.promise : options(path))
    const wrapper = render(false)
    await aggregate(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.onerror?.()
    await flushPromises()
    expect(stream.closed).toBe(true)
    const requestedURL = new URL(apiMock.mock.calls.find(call => call[0].startsWith(torrentSearchPath))![0], 'https://server.test')
    expect(requestedURL.searchParams.getAll('site_ids')).toEqual(['7', '8'])
    expectProgress(wrapper, { total: 2, completed: 0, pending: 0, running: 2, succeeded: 0, failed: 0, result_count: 0 })
    stream.emit('site', group(7, 'success', 3))
    stream.emit('done', { total: 2, completed: 2, pending: 0, running: 0, succeeded: 2, failed: 0, result_count: 3 })
    await flushPromises()
    expectProgress(wrapper, { total: 2, completed: 0, pending: 0, running: 2, succeeded: 0, failed: 0, result_count: 0 })
    expect(wrapper.text()).toContain('取消搜索')
    fallback.resolve({ groups: [group(7, 'success', 1), group(8, 'error')] })
    await flushPromises()
    expectProgress(wrapper, { total: 2, completed: 2, pending: 0, running: 0, succeeded: 1, failed: 1, result_count: 1 })
  })
})
