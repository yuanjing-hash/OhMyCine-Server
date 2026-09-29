// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { effectScope, nextTick, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { api } from '@/api/client'
import { useSearchResultRecognition } from '@/search-result-recognition'
import { torrentRecognitionPath, saveTorrentSearchSession, type TorrentSearchResult, type TorrentRecognitionResult } from '@/sites'
import ExploreView from '@/views/ExploreView.vue'

vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: vi.fn() }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: { mode: 'resources' } }), useRouter: () => ({ replace: vi.fn(), push: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
const item = (token: string): TorrentSearchResult => ({ token, title: token, expires_at: new Date(Date.now() + 300000).toISOString() })
const matched = (id = 1): TorrentRecognitionResult => ({ engine_version: 'nextgen-domain-v12', status: 'matched', title: `作品${id}`, media_type: 'movie', tmdb_id: id, specifications: {} })
const present = { media_type: 'movie', tmdb_id: 1, status: 'present', movie: { present: true }, freshness: {}, libraries: [] }
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }
const cleanup: (() => void)[] = []
afterEach(() => { cleanup.splice(0).forEach(stop => stop()); sessionStorage.clear(); vi.clearAllMocks(); vi.useRealTimers() })
function setup(canRead = true) {
  const items = ref<TorrentSearchResult[]>([])
  const scope = effectScope()
  const queue = scope.run(() => useSearchResultRecognition(items, () => canRead))!
  cleanup.push(() => { queue.dispose(); scope.stop() })
  return { items, queue }
}

describe('automatic result recognition', () => {
  it('limits concurrency, deduplicates coverage, and keeps going after an individual failure', async () => {
    const pending = [deferred<TorrentRecognitionResult>(), deferred<TorrentRecognitionResult>()]
    let index = 0
    vi.mocked(api).mockImplementation((path) => {
      if (path === torrentRecognitionPath) return index < 2 ? pending[index++]!.promise : Promise.reject(new Error('无法识别这条结果'))
      return Promise.resolve(present)
    })
    const { items, queue } = setup()
    items.value = [item('a'), item('b'), item('c')]
    await nextTick()
    expect(api).toHaveBeenCalledTimes(2)
    pending[0]!.resolve(matched()); pending[1]!.resolve(matched())
    await flushPromises()
    expect(vi.mocked(api).mock.calls.filter(([url]) => url.endsWith('/coverage'))).toHaveLength(1)
    expect(queue.libraryStates.value.a?.label).toBe('已入库')
    expect(queue.libraryStates.value.b?.label).toBe('已入库')
    expect(queue.recognitionErrors.value.c).toBe('无法识别这条结果')
    items.value = [...items.value]
    await flushPromises()
    expect(api).toHaveBeenCalledTimes(4)
  })

  it('ignores previous searches and late automatic results after manual confirmation', async () => {
    const old = deferred<TorrentRecognitionResult>()
    const automatic = deferred<TorrentRecognitionResult>()
    vi.mocked(api).mockImplementation(path => path.endsWith('/coverage') ? Promise.resolve(present) : old.promise)
    const { items, queue } = setup()
    items.value = [item('old')]; await nextTick()
    const signal = vi.mocked(api).mock.calls[0]![1]!.signal!
    queue.reset(); items.value = [item('new')]
    vi.mocked(api).mockImplementation(path => path.endsWith('/coverage') ? Promise.resolve(present) : automatic.promise)
    await nextTick()
    queue.acceptManual('new', { ...matched(2), manual_override: true })
    old.resolve(matched(999)); automatic.resolve(matched(999)); await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(queue.recognitions.value.old).toBeUndefined()
    expect(queue.recognitions.value.new?.tmdb_id).toBe(2)
    expect(vi.mocked(api).mock.calls.some(([url]) => url.includes('/movie/2/coverage'))).toBe(true)
  })

  it('does not report absent for inaccessible, unrecognized or failed coverage', async () => {
    vi.mocked(api).mockImplementation(path => path === torrentRecognitionPath ? Promise.resolve(matched()) : Promise.reject(new Error('403')))
    const { items, queue } = setup()
    items.value = [item('failed')]; await flushPromises()
    expect(queue.libraryStates.value.failed?.label).toBe('库内状态暂不可用')
    const denied = setup(false)
    denied.items.value = [item('denied')]; await flushPromises()
    expect(denied.queue.libraryStates.value.denied?.label).toBe('无权查看库内状态')
    vi.mocked(api).mockResolvedValue({ ...matched(), status: 'unrecognized', tmdb_id: undefined })
    items.value = [item('unknown')]; await flushPromises()
    expect(queue.libraryStates.value.unknown?.label).toBe('库内状态待确认')
  })

  it('retries ahead of pending cards, ignores the old response, and refreshes coverage', async () => {
    const slowA = deferred<TorrentRecognitionResult>()
    const oldB = deferred<TorrentRecognitionResult>()
    const retryB = deferred<TorrentRecognitionResult>()
    let bCalls = 0
    let coverageCalls = 0
    vi.mocked(api).mockImplementation((path, options) => {
      if (path === torrentRecognitionPath) {
        const token = JSON.parse(String(options?.body)).result_token as string
        if (token === 'a') return slowA.promise
        if (token === 'b') return ++bCalls === 1 ? oldB.promise : bCalls === 2 ? retryB.promise : Promise.resolve(matched(2))
        return Promise.resolve(matched(3))
      }
      coverageCalls++
      return Promise.resolve(present)
    })
    const { items, queue } = setup()
    items.value = [item('a'), item('b'), item('c')]
    await nextTick()
    expect(queue.retry('b')).toBe(true)
    expect(bCalls).toBe(2)
    expect(queue.recognizingTokens.value).toContain('b')
    expect(vi.mocked(api).mock.calls.filter(([path]) => path === torrentRecognitionPath)).toHaveLength(3)
    oldB.resolve(matched(999))
    await nextTick()
    expect(queue.recognitions.value.b).toBeUndefined()
    retryB.resolve(matched(2))
    await flushPromises()
    expect(queue.recognitions.value.b?.tmdb_id).toBe(2)
    expect(queue.recognitions.value.c?.tmdb_id).toBe(3)
    expect(coverageCalls).toBe(2)
    expect(queue.retry('b')).toBe(true)
    await flushPromises()
    expect(coverageCalls).toBe(3)
  })

  it('releases both slots after the deadline even when recognition ignores abort', async () => {
    vi.useFakeTimers()
    const never = deferred<TorrentRecognitionResult>()
    vi.mocked(api).mockImplementation(() => never.promise)
    const { items, queue } = setup()
    items.value = [item('a'), item('b'), item('c')]
    await nextTick()
    expect(queue.recognizingTokens.value).toEqual(['a', 'b'])
    await vi.advanceTimersByTimeAsync(30000)
    expect(queue.recognitionErrors.value.a).toContain('超时')
    expect(queue.recognitionErrors.value.b).toContain('超时')
    expect(queue.recognizingTokens.value).toEqual(['c'])
    never.resolve(matched(999))
    await nextTick()
    expect(queue.recognitions.value.a).toBeUndefined()
    expect(queue.recognitions.value.b).toBeUndefined()
  })

  it('labels a stalled coverage query without discarding the recognized work', async () => {
    vi.useFakeTimers()
    const never = deferred<typeof present>()
    vi.mocked(api).mockImplementation(path => path === torrentRecognitionPath ? Promise.resolve(matched()) : never.promise)
    const { items, queue } = setup()
    items.value = [item('a')]
    await nextTick()
    await Promise.resolve()
    await Promise.resolve()
    expect(queue.recognitions.value.a?.tmdb_id).toBe(1)
    await vi.advanceTimersByTimeAsync(30000)
    expect(queue.recognitionErrors.value.a).toContain('库内状态查询超时')
    expect(queue.libraryStates.value.a?.label).toBe('库内状态暂不可用')
    expect(queue.recognizingTokens.value).toEqual([])
  })

  it('requires a new search for an expired result instead of retrying its claim', async () => {
    const { items, queue } = setup()
    items.value = [{ ...item('expired'), expires_at: new Date(Date.now() - 1000).toISOString() }]
    await nextTick()
    expect(queue.recognitionErrors.value.expired).toContain('请重新搜索')
    expect(queue.retry('expired')).toBe(false)
    expect(api).not.toHaveBeenCalled()
  })

  it('starts restored cards in visible sort order and offers retry on every card', async () => {
    const state = { input: { keyword: '作品', mediaType: '', siteIDs: [1], searchBy: 'title' as const }, groups: [{ site_id: 1, site_name: 'Jackett', site_type: 'bt' as const, status: 'success' as const, page: 1, has_next: false, skipped: 0, items: [{ ...item('low'), seeders: 1, published_at: '2026-08-30T00:00:00Z' }, { ...item('high'), seeders: 9, published_at: '2026-08-10T00:00:00Z' }, { ...item('middle'), seeders: 5, published_at: '2026-08-20T00:00:00Z' }] }], recognitions: {}, searched: true, savedAt: Date.now() }
    saveTorrentSearchSession(sessionStorage, state)
    const pending = deferred<TorrentRecognitionResult>()
    vi.mocked(api).mockImplementation(path => path === torrentRecognitionPath ? pending.promise : Promise.resolve(present))
    const wrapper = mount(ExploreView, { global: { stubs: { RouterLink: true } } })
    cleanup.push(() => wrapper.unmount())
    await flushPromises()
    const requested = () => vi.mocked(api).mock.calls.filter(([path]) => path === torrentRecognitionPath).map(([, options]) => JSON.parse(String(options?.body)).result_token as string)
    expect(requested()).toEqual(['high', 'middle'])
    const retryButtons = wrapper.findAll('button').filter(button => button.text() === '重新检测')
    expect(retryButtons).toHaveLength(3)
    await retryButtons[0]!.trigger('click')
    expect(requested()).toEqual(['high', 'middle', 'high'])
    pending.resolve(matched())
    await flushPromises()
    expect(requested()).toEqual(['high', 'middle', 'high', 'low'])
  })

  it('automatically enriches restored cards without a detect button or share requests', async () => {
    const state = { input: { keyword: '作品', mediaType: '', siteIDs: [1], searchBy: 'title' as const }, groups: [{ site_id: 1, site_name: '盘搜', site_type: 'cloud_share' as const, status: 'success' as const, page: 1, has_next: false, skipped: 0, items: [{ ...item('card'), source_kind: '115_share' as const }] }], recognitions: {}, searched: true, savedAt: Date.now() }
    saveTorrentSearchSession(sessionStorage, state)
    vi.mocked(api).mockImplementation(path => Promise.resolve(path === torrentRecognitionPath ? matched() : present))
    const wrapper = mount(ExploreView, { global: { stubs: { RouterLink: true } } })
    cleanup.push(() => wrapper.unmount())
    await flushPromises()
    expect(wrapper.text()).toContain('库内：已入库')
    expect(wrapper.findAll('button').some(button => button.text() === '检测')).toBe(false)
    expect(wrapper.findAll('button').some(button => button.text() === '手动检测')).toBe(true)
    expect(vi.mocked(api).mock.calls.map(([path]) => path)).toEqual([torrentRecognitionPath, '/api/v1/discovery/media/movie/1/coverage'])
  })
})
