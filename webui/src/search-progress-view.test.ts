// @vitest-environment happy-dom

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ExploreView from '@/views/ExploreView.vue'
import { useAuthStore } from '@/stores/auth'
import { discoverySearchOptionsPath, readTorrentSearchSession, torrentRecognitionCandidatesPath, torrentSearchPath, type TorrentSearchGroup, type TorrentSearchProgress } from '@/sites'

const { apiMock, routeMock } = vi.hoisted(() => ({
  apiMock: vi.fn(),
  routeMock: { query: {} as Record<string, string> },
}))

vi.mock('@/api/client', async importOriginal => ({
  ...await importOriginal<typeof import('@/api/client')>(),
  api: (path: string, options?: RequestInit) => {
    // Availability is refreshed independently on mount and for each search.
    // Keep that request out of the deferred search-response queue below.
    if (path === '/api/v1/discovery/search-options' && options?.signal) {
      return Promise.resolve({ list: [7, 8].map(id => ({ id, name: `测试站点 ${id}`, searchable: true })) })
    }
    return apiMock(path, options)
  },
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
function render(fixedSite = true, permitted = false, actorID?: number) {
  routeMock.query = fixedSite ? { mode: 'resources', site_id: '7' } : { mode: 'resources' }
  const pinia = createPinia()
  if (actorID) useAuthStore(pinia).user = { id: actorID, username: `actor-${actorID}`, display_name: `账号 ${actorID}`, status: 'active', is_owner: false, roles: [], permissions: [] }
  if (permitted) vi.spyOn(useAuthStore(pinia), 'can').mockReturnValue(true)
  const wrapper = mount(ExploreView, {
    global: { plugins: [pinia], stubs: { RouterLink: true, SharePreviewDialog: true } },
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
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('fixed-site JSON search progress', () => {
  beforeEach(() => vi.stubGlobal('EventSource', undefined))
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

  it('accepts a legacy successful empty group whose items are null', async () => {
    apiMock.mockResolvedValue({ groups: [{ ...group(7, 'success'), items: null }] })
    const wrapper = render()
    await search(wrapper)
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 0 })
    expect(wrapper.text()).toContain('搜索完成')
    expect(wrapper.text()).not.toContain('搜索失败')
  })

  it('does not mistake an intermediate JSON group for the final-only contract', async () => {
    apiMock.mockResolvedValue({ groups: [{ ...group(7, 'success', 1), complete: false, revision: 1 }] })
    const wrapper = render()
    await search(wrapper)
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).toContain('站点搜索结果不完整，请重试')
    expect(wrapper.text()).not.toContain('搜索完成')
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
  beforeEach(() => vi.stubGlobal('EventSource', undefined))
  it('restores the retained site failure after cancelling its retry', async () => {
    const retry = deferred<{ groups: TorrentSearchGroup[] }>()
    apiMock.mockResolvedValueOnce({ groups: [group(7, 'error')] }).mockReturnValueOnce(retry.promise)
    const wrapper = render()
    await search(wrapper)
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    await button(wrapper, '取消搜索').trigger('click')
    expectProgress(wrapper, { total: 1, completed: 1, pending: 0, running: 0, succeeded: 0, failed: 1, result_count: 0 })
    expect(wrapper.text()).toContain('可重试站点：')
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
    expect(wrapper.text()).not.toContain('可重试站点：')
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
    const retryStream = SearchEventSource.instances[1]
    retryStream.emit('progress', { total: 1, completed: 0, pending: 0, running: 1, succeeded: 0, failed: 0, result_count: 0 })
    await flushPromises()
    expectProgress(wrapper, { ...first, result_count: 1 })
    expect(new URL(retryStream.url, 'https://server.test').searchParams.get('site_id')).toBe('8')
    retryStream.emit('site', group(8, 'success', 1))
    retryStream.emit('done', { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 1 })
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

  it('does not repeat a search after a stream error and ignores its late events', async () => {
    apiMock.mockImplementation(options)
    const wrapper = render(false)
    await aggregate(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.onerror?.()
    await flushPromises()
    expect(stream.closed).toBe(true)
    expect(apiMock.mock.calls.some(call => call[0].startsWith(torrentSearchPath))).toBe(false)
    expectProgress(wrapper, { total: 2, completed: 2, pending: 0, running: 0, succeeded: 0, failed: 2, result_count: 0 })
    stream.emit('site', group(7, 'success', 3))
    stream.emit('done', { total: 2, completed: 2, pending: 0, running: 0, succeeded: 2, failed: 0, result_count: 3 })
    await flushPromises()
    expectProgress(wrapper, { total: 2, completed: 2, pending: 0, running: 0, succeeded: 0, failed: 2, result_count: 0 })
    expect(wrapper.text()).not.toContain('取消搜索')
    expect(wrapper.text()).not.toContain('测试资源 7-0')
  })
})

describe('detailed incremental single-site search', () => {
  const finished = { total: 1, completed: 1, pending: 0, running: 0, succeeded: 1, failed: 0, result_count: 2 }
  const detail = (phase = 'querying', revision = 1) => ({
    site_id: 7, site_name: '测试站点 7', phase, query_name: '名侦探柯南', query_index: 2,
    queries_completed: 1, queries_failed: 0, queries_total: 3, result_count: 1, elapsed_ms: 12_000, revision,
  })

  it('shows real stage and cumulative batches before completion, ignores old revisions and retains partial results', async () => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    const url = new URL(stream.url, 'https://server.test')
    expect(url.searchParams.get('site_id')).toBe('7')
    expect(url.searchParams.get('progress_version')).toBe('2')
    stream.emit('progress', { ...running, event_sequence: 1, site_detail: detail() })
    stream.emit('site', { ...group(7, 'success', 1), complete: false, revision: 1 })
    await flushPromises()
    expect(wrapper.text()).toContain('正在查询：名侦探柯南')
    expect(wrapper.text()).toContain('已查询 1 / 3 个名称')
    expect(wrapper.text()).toContain('测试资源 7-0')
    expectProgress(wrapper, running)
    stream.emit('site', { ...group(7, 'success', 2), complete: false, revision: 2 })
    stream.emit('site', { ...group(7, 'success', 0), complete: false, revision: 1 })
    stream.emit('progress', { ...running, result_count: 2, event_sequence: 2, site_detail: { ...detail('waiting_rate_limit', 2), result_count: 2 } })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(2)
    expect(wrapper.text()).toContain('等待站点限速')
    stream.emit('site', { ...group(7, 'success', 2), complete: true, partial: true, incomplete_reason: 'request_timeout', revision: 3, queries_completed: 2, queries_failed: 1, queries_total: 3 })
    stream.emit('progress', { ...finished, event_sequence: 3 })
    stream.emit('done', { ...finished, event_sequence: 3 })
    await flushPromises()
    expectProgress(wrapper, finished)
    expect(wrapper.findAll('article')).toHaveLength(2)
    expect(wrapper.text()).toContain('搜索未完整完成')
    expect(wrapper.text()).toContain('已保留 2 条结果')
    expect(wrapper.text()).toContain('查询名称：名侦探柯南')
    expect(wrapper.text()).not.toContain('正在查询：')
    expect(stream.closed).toBe(true)
    expect(apiMock.mock.calls.some(([path]) => path.startsWith(torrentSearchPath))).toBe(false)
  })

  it('keeps a quiet stream beyond the old local deadlines without a duplicate JSON search', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('progress', { ...running, site_detail: detail() })
    await vi.advanceTimersByTimeAsync(130_000)
    expect(stream.closed).toBe(false)
    expect(SearchEventSource.instances).toHaveLength(1)
    expect(apiMock.mock.calls.some(([path]) => path.startsWith(torrentSearchPath))).toBe(false)
    expect(wrapper.text()).toContain('已用时 2 分')
    await button(wrapper, '取消搜索').trigger('click')
    expect(stream.closed).toBe(true)
  })

  it('preserves an arrived batch on disconnect and rejects late data from that stream', async () => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: false, revision: 1 })
    stream.onerror?.()
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).toContain('搜索连接中断')
    expect(wrapper.text()).not.toContain('当前渠道和筛选条件下没有结果')
    stream.emit('site', { ...group(7, 'success', 3), complete: true, revision: 2 })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(apiMock.mock.calls.some(([path]) => path.startsWith(torrentSearchPath))).toBe(false)
  })

  it('retains previous results while retrying and after the retry fails', async () => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: true, partial: true, revision: 1 })
    stream.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    expect(wrapper.text()).toContain('重新搜索中，暂时展示上次结果')
    expect(wrapper.findAll('article')).toHaveLength(1)
    SearchEventSource.instances[1].onerror?.()
    await flushPromises()
    expect(wrapper.text()).toContain('上次保留的结果')
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).not.toContain('搜索完成')
  })

  it('keeps submitted conditions for retry, paging and retained cache while the search form has a new draft', async () => {
    const wrapper = render()
    await search(wrapper, '原始作品')
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: true, partial: true, revision: 1, has_next: true })
    stream.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    await wrapper.get('#discovery-keyword').setValue('尚未提交的新作品')
    await wrapper.get('#discovery-kind').setValue('movie')
    await wrapper.get('#discovery-year').setValue(2025)
    await button(wrapper, '测试站点 7 · 重试').trigger('click')
    const retry = SearchEventSource.instances[1]
    const retryURL = new URL(retry.url, 'https://server.test')
    expect(retryURL.searchParams.get('keyword')).toBe('原始作品')
    expect(retryURL.searchParams.has('media_type')).toBe(false)
    expect(retryURL.searchParams.has('year')).toBe(false)
    retry.emit('site', { ...group(7, 'success', 1), complete: true, revision: 1, has_next: true })
    retry.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    expect(readTorrentSearchSession(sessionStorage)?.input.keyword).toBe('原始作品')
    expect(readTorrentSearchSession(sessionStorage)?.input.mediaType).toBe('')
    await button(wrapper, '下一页').trigger('click')
    const next = SearchEventSource.instances[2]
    const nextURL = new URL(next.url, 'https://server.test')
    expect(nextURL.searchParams.get('keyword')).toBe('原始作品')
    expect(nextURL.searchParams.get('page')).toBe('2')
    next.emit('site', { ...group(7, 'success', 1), page: 2, complete: true, revision: 1 })
    next.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    await search(wrapper, '尚未提交的新作品')
    const submitted = new URL(SearchEventSource.instances[3].url, 'https://server.test')
    expect(submitted.searchParams.get('keyword')).toBe('尚未提交的新作品')
    expect(submitted.searchParams.get('media_type')).toBe('movie')
    expect(submitted.searchParams.get('year')).toBe('2025')
  })

  it.each(['preview', 'download'] as const)('does not reopen an old actor %s after its options arrive late', async action => {
    const downloaders = deferred<{ list: unknown[] }>()
    const libraries = deferred<{ list: unknown[] }>()
    apiMock.mockImplementation((path: string) => {
      if (path === '/api/v1/sites') return Promise.resolve({ list: [] })
      if (path === '/api/v1/downloaders') return downloaders.promise
      if (path === '/api/v1/media-libraries') return libraries.promise
      throw new Error(`unexpected request ${path}`)
    })
    const wrapper = render(true, true)
    const auth = useAuthStore()
    auth.user = { id: 1, username: 'old-actor', display_name: '旧账号', status: 'active', is_owner: false, roles: [], permissions: [] }
    await flushPromises()
    await search(wrapper)
    const oldStream = SearchEventSource.instances[0]
    const sharing = group(7, 'success', 1)
    sharing.items[0].source_kind = '115_share'
    oldStream.emit('site', { ...sharing, complete: true, revision: 1 })
    oldStream.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    const label = action === 'preview' ? '预览分享链接内部内容' : '转存入库'
    await button(wrapper, label).trigger('click')
    auth.user = { ...auth.user, id: 2, username: 'new-actor', display_name: '新账号' }
    await flushPromises()
    for (const [path, options] of apiMock.mock.calls.filter(([path]) => path === '/api/v1/downloaders' || path === '/api/v1/media-libraries')) expect((options.signal as AbortSignal).aborted, `${path} option request must retire with its actor`).toBe(true)
    expect(wrapper.findAll('article')).toHaveLength(0)
    expect(wrapper.find('[aria-labelledby="pt-download-title"]').exists()).toBe(false)
    downloaders.resolve({ list: [{ id: 'old-private-downloader', name: '旧账号下载器', enabled: true }] })
    libraries.resolve({ list: [{ id: 88, name: '旧账号媒体库' }] })
    oldStream.emit('site', { ...sharing, complete: true, revision: 2 })
    await flushPromises()
    expect(wrapper.find('share-preview-dialog-stub').exists()).toBe(false)
    expect(wrapper.find('[aria-labelledby="pt-download-title"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('测试资源 7-0')
    expect(wrapper.text()).not.toContain('旧账号下载器')
    expect(wrapper.text()).not.toContain('旧账号媒体库')
    expect(apiMock.mock.calls.some(([path]) => path === '/api/v1/sites/routes/recommend')).toBe(false)
  })

  it('does not restore a different actor cache after the previous view has already unmounted', async () => {
    apiMock.mockResolvedValue({ list: [] })
    const previous = render(true, false, 1)
    await search(previous)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: true, revision: 1 })
    stream.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    expect(readTorrentSearchSession(sessionStorage)?.actorID).toBe(1)
    previous.unmount()
    const current = render(true, false, 2)
    await flushPromises()
    expect(current.findAll('article')).toHaveLength(0)
    expect(current.text()).not.toContain('测试资源 7-0')
    expect(readTorrentSearchSession(sessionStorage)).toBeNull()
  })

  it('aborts a manual candidate lookup on close and ignores its late response', async () => {
    const candidates = deferred<{ list: unknown[] }>()
    apiMock.mockImplementation((path: string) => path === '/api/v1/sites' ? Promise.resolve({ list: [] }) : candidates.promise)
    const wrapper = render(true, true)
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: true, revision: 1 })
    stream.emit('done', { ...finished, result_count: 1 })
    await flushPromises()
    await button(wrapper, '手动检测').trigger('click')
    const request = apiMock.mock.calls.find(([path]) => path === torrentRecognitionCandidatesPath)!
    const signal = request[1].signal as AbortSignal
    const dialog = wrapper.get('[aria-labelledby="manual-recognition-title"]')
    await dialog.findAll('button').find(item => item.text() === '关闭')!.trigger('click')
    expect(signal.aborted).toBe(true)
    candidates.resolve({ list: [{ id: 123, title: '迟到候选' }] })
    await flushPromises()
    expect(wrapper.find('[aria-labelledby="manual-recognition-title"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('迟到候选')
  })

  it('ignores foreign site and old page data without displaying them', async () => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(8, 'success', 1), complete: false, revision: 1 })
    await flushPromises()
    expect(wrapper.text()).not.toContain('测试资源 8-0')
    expect(wrapper.text()).toContain('搜索进度或结果格式异常')
    expect(stream.closed).toBe(true)
  })

  it.each([{ queries_completed: -1 }, { queries_total: 1.5 }, { complete: 'false' }])('retains arrived rows when a later snapshot has malformed fields: %j', async invalid => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: false, revision: 1 })
    stream.emit('site', { ...group(7, 'success', 0), complete: true, revision: 2, ...invalid })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).toContain('搜索进度或结果格式异常')
    expect(wrapper.text()).toContain('搜索未完整完成')
    expect(stream.closed).toBe(true)
  })

  it.each([{ event_sequence: 4 }, { event_sequence: 5, site_id: 8 }])('cannot report completion after rejecting the terminal progress envelope: %j', async invalid => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: true, revision: 1 })
    stream.emit('progress', { ...running, event_sequence: 5 })
    stream.emit('done', { ...finished, result_count: 1, ...invalid })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).toContain('搜索进度或结果格式异常')
    expect(wrapper.text()).toContain('搜索未完整完成')
    expect(wrapper.text()).not.toContain('搜索完成')
    expect(wrapper.text()).not.toContain('取消搜索')
    expectProgress(wrapper, { ...finished, result_count: 1 })
  })

  it('does not downgrade an established v2 attempt to unversioned progress or snapshots', async () => {
    const wrapper = render()
    await search(wrapper)
    const stream = SearchEventSource.instances[0]
    stream.emit('progress', { ...running, event_sequence: 1, site_detail: detail('querying', 1) })
    stream.emit('site', { ...group(7, 'success', 1), complete: false, revision: 1 })
    stream.emit('progress', finished)
    stream.emit('site', group(7, 'success', 0))
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(1)
    expect(wrapper.text()).toContain('取消搜索')
    expectProgress(wrapper, running)
    stream.emit('site', { ...group(7, 'success', 2), complete: true, revision: 2 })
    stream.emit('done', { ...finished, event_sequence: 2 })
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(2)
    expectProgress(wrapper, finished)
    expect(wrapper.text()).toContain('搜索完成')
  })

  it('keeps the selected claim, library and downloader while a newer batch replaces its card', async () => {
    const choices = [11, 22].flatMap(library => ['d1', 'd2'].map(downloader => ({ downloader_id: downloader, media_library_id: library, downloader_name: downloader, downloader_type: 'qbittorrent', library_name: `库${library}`, enabled: true, route_kind: 'same_source_local', route_label: '本地整理' })))
    apiMock.mockImplementation(async (path: string, options?: RequestInit) => {
      if (path === '/api/v1/sites') return { list: [] }
      if (path === '/api/v1/downloaders') return { list: ['d1', 'd2'].map(id => ({ id, name: id, type: 'qbittorrent', enabled: true })) }
      if (path === '/api/v1/media-libraries') return { list: [11, 22].map(id => ({ id, name: `库${id}`, profile_name: '自动分类', transfer_mode: 'move' })) }
      if (path === '/api/v1/sites/routes/recommend') {
        const input = JSON.parse(String(options?.body))
        return { source_kind: 'bt', recommended: { downloader_id: 'd1', media_library_id: input.media_library_id || 11 }, choices }
      }
      if (path === '/api/v1/download-routes/preview') return { options: [11, 22].map(id => ({ media_library_id: id, library_name: `库${id}`, enabled: true, route_label: '本地整理' })) }
      throw new Error(`unexpected request ${path}`)
    })
    const wrapper = render(true, true)
    await search(wrapper)
    expect(SearchEventSource.instances).toHaveLength(1)
    const stream = SearchEventSource.instances[0]
    stream.emit('site', { ...group(7, 'success', 1), complete: false, revision: 1 })
    await flushPromises()
    await wrapper.findAll('article')[0].findAll('button').find(button => button.text() === '入库')!.trigger('click')
    await flushPromises()
    const dialog = wrapper.get('[aria-labelledby="pt-download-title"]')
    await dialog.findAll('label').find(label => label.text().includes('库22'))!.get('input[type="radio"]').setValue(true)
    await flushPromises()
    await dialog.get('select').setValue('d2')
    await dialog.get('input[type="number"]').setValue(37)
    await flushPromises()
    const recommendations = apiMock.mock.calls.filter(([path]) => path === '/api/v1/sites/routes/recommend').length
    const incoming = { ...group(7, 'success', 2), complete: false, revision: 2 }
    incoming.items[0] = { ...incoming.items[0], token: 'newer-claim', title: '更新后的分享帖子' }
    stream.emit('site', incoming)
    await flushPromises()
    expect(dialog.get<HTMLSelectElement>('select').element.value).toBe('d2')
    expect(dialog.get<HTMLInputElement>('input[type="number"]').element.value).toBe('37')
    expect(dialog.get<HTMLInputElement>('input[type="radio"]:checked').element.closest('label')!.textContent).toContain('库22')
    expect(dialog.text()).toContain('测试资源 7-0')
    expect(dialog.text()).not.toContain('更新后的分享帖子')
    expect(apiMock.mock.calls.filter(([path]) => path === '/api/v1/sites/routes/recommend')).toHaveLength(recommendations)
    expect(wrapper.findAll('article')).toHaveLength(2)
  })
})
