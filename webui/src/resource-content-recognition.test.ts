// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, effectScope, h, nextTick, onBeforeUnmount, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { api } from '@/api/client'
import { useSearchResultRecognition } from '@/search-result-recognition'
import { resourceContentPreviewPath, type ResourceContentPreview } from '@/resource-content'
import { torrentRecognitionPath, type TorrentRecognitionResult, type TorrentSearchResult } from '@/sites'
import { createOwnedReadQueue, runResourceRead } from '@/owned-read-queue'

vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: vi.fn() }))
const cleanup: (() => void)[] = []
function pending<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }
const item = (token: string, extra: Partial<TorrentSearchResult> = {}): TorrentSearchResult => ({ token, title: token, expires_at: new Date(Date.now() + 300000).toISOString(), content_inspection: true, ...extra })
const title = (id: number): TorrentRecognitionResult => ({ engine_version: 'fixture', status: 'matched', title: `作品${id}`, media_type: 'tv', tmdb_id: id, specifications: {} })
function preview(token: string): ResourceContentPreview {
  return { version: 1, token: `content-${token}`, status: 'complete', complete: true, files: [{ token: `file-${token}`, path: 'Actual Show/Season 01/13.mkv', name: '13.mkv', size: 100, is_dir: false, file_kind: 'video', season: 1, episodes: [13], work_status: 'unrecognized', unresolved_reason: 'work_context_missing' }], total_count: 1, displayed_count: 1, total_size: 100, checked_at: new Date().toISOString(), expires_at: new Date(Date.now() + 300000).toISOString(), message: 'actual file facts' }
}
function setup() {
  const items = ref<TorrentSearchResult[]>([])
  const owner = effectScope()
  const queue = owner.run(() => useSearchResultRecognition(items, () => true))!
  cleanup.push(() => { queue.dispose(); owner.stop() })
  return { items, queue }
}
afterEach(() => { cleanup.splice(0).forEach(stop => stop()); vi.clearAllMocks(); vi.useRealTimers() })

describe('independent title, cloud, torrent, magnet and coverage lanes', () => {
  it('two slow cloud reads do not block quick titles, local torrent files or magnet metadata', async () => {
    const slow = pending<unknown>()
    vi.mocked(api).mockImplementation((path, options) => {
      const token = options?.body ? JSON.parse(String(options.body)).result_token as string : ''
      if (path === torrentRecognitionPath) return Promise.resolve(title(1))
      if (path === resourceContentPreviewPath) return token.startsWith('cloud') ? slow.promise : Promise.resolve(preview(token))
      return Promise.resolve({ status: 'present', media_type: 'tv', tmdb_id: 1, libraries: [] })
    })
    const { items, queue } = setup()
    items.value = [item('cloud-a', { source_kind: '115_share' }), item('cloud-b', { source_kind: 'quark_share' }), item('torrent', { payload_kind: 'torrent' }), item('magnet', { payload_kind: 'magnet' })]
    await flushPromises()
    expect(Object.keys(queue.recognitions.value)).toHaveLength(4)
    expect(queue.contentPreviews.value.torrent?.files[0]?.name).toBe('13.mkv')
    expect(queue.contentPreviews.value.magnet?.files[0]?.episodes).toEqual([13])
    expect(queue.inspectingTokens.value.sort()).toEqual(['cloud-a', 'cloud-b'])
    const titles = vi.mocked(api).mock.calls.filter(([path]) => path === torrentRecognitionPath)
    expect(titles).toHaveLength(4)
    expect(titles.every(([, options]) => JSON.parse(String(options?.body)).title_only === true)).toBe(true)
    expect(vi.mocked(api).mock.calls.some(([path]) => path.includes('/downloaders') || path.endsWith('/downloads'))).toBe(false)
    slow.resolve(preview('cloud')); await flushPromises()
  })

  it('slow coverage has its own limit and never holds the title lane', async () => {
    const slow = pending<unknown>()
    let index = 0
    vi.mocked(api).mockImplementation((path, options) => path === torrentRecognitionPath ? Promise.resolve(title(++index)) : path === resourceContentPreviewPath ? Promise.resolve(preview(JSON.parse(String(options?.body)).result_token)) : slow.promise)
    const { items, queue } = setup()
    items.value = Array.from({ length: 9 }, (_, index) => item(`title-${index}`, { payload_kind: 'torrent' }))
    await flushPromises()
    expect(Object.keys(queue.recognitions.value)).toHaveLength(9)
    expect(vi.mocked(api).mock.calls.filter(([path]) => path.endsWith('/coverage'))).toHaveLength(4)
    expect(queue.recognizingTokens.value).toEqual([])
    slow.resolve({ status: 'missing', media_type: 'tv', tmdb_id: 1, libraries: [] }); await flushPromises()
  })

  it('preserves filename and episode facts when title identification fails, and retries content independently', async () => {
    let contentReads = 0
    vi.mocked(api).mockImplementation(path => path === torrentRecognitionPath ? Promise.reject(new Error('TMDB unavailable')) : Promise.resolve(preview(`read-${++contentReads}`)))
    const { items, queue } = setup()
    items.value = [item('one')]
    await flushPromises()
    expect(queue.recognitionErrors.value.one).toBe('TMDB unavailable')
    expect(queue.contentPreviews.value.one?.files[0]?.path).toBe('Actual Show/Season 01/13.mkv')
    expect(queue.contentPreviews.value.one?.files[0]?.episodes).toEqual([13])
    queue.retryContent('one')
    await flushPromises()
    expect(contentReads).toBe(2)
    expect(vi.mocked(api).mock.calls.filter(([path]) => path === torrentRecognitionPath)).toHaveLength(1)
  })

  it('fences old content success/error/finally after a retry or new query', async () => {
    const old = pending<unknown>()
    let reads = 0
    vi.mocked(api).mockImplementation((path, options) => path === torrentRecognitionPath ? Promise.resolve(title(1)) : path === resourceContentPreviewPath ? ++reads === 1 ? old.promise : Promise.resolve(preview(JSON.parse(String(options?.body)).result_token)) : Promise.resolve({ status: 'missing', media_type: 'tv', tmdb_id: 1, libraries: [] }))
    const { items, queue } = setup()
    items.value = [item('old')]
    await flushPromises()
    const signal = vi.mocked(api).mock.calls.find(([path]) => path === resourceContentPreviewPath)![1]!.signal!
    queue.reset(); items.value = [item('new')]
    await flushPromises()
    expect(signal.aborted).toBe(true)
    old.resolve(preview('stale'))
    await flushPromises()
    expect(queue.contentPreviews.value.old).toBeUndefined()
    expect(queue.contentPreviews.value.new?.token).toBe('content-new')
    expect(queue.inspectingTokens.value).toEqual([])
  })

  it('retains physical metadata slots after timeout and repeated query resets until transport settles', async () => {
    vi.useFakeTimers()
    const old = [pending<unknown>(), pending<unknown>()]
    let physicalReads = 0
    vi.mocked(api).mockImplementation(path => path === torrentRecognitionPath ? Promise.resolve({ ...title(1), status: 'unrecognized', tmdb_id: undefined }) : old[physicalReads++]?.promise ?? Promise.resolve(preview('new')))
    const { items, queue } = setup()
    items.value = [item('a', { payload_kind: 'magnet' }), item('b', { payload_kind: 'magnet' }), item('c', { payload_kind: 'magnet' })]
    await nextTick(); await Promise.resolve(); await Promise.resolve()
    expect(queue.inspectingTokens.value.sort()).toEqual(['a', 'b'])
    await vi.advanceTimersByTimeAsync(65000)
    expect(queue.contentErrors.value.a).toContain('超时')
    expect(physicalReads).toBe(2)
    for (const token of ['reset-one', 'reset-two']) {
      queue.reset(); items.value = [item(token, { payload_kind: 'magnet' })]
      await nextTick(); await Promise.resolve(); await Promise.resolve()
    }
    expect(physicalReads).toBe(2)
    old[0]!.resolve(preview('stale'))
    await vi.advanceTimersByTimeAsync(1)
    expect(physicalReads).toBe(3)
    expect(queue.contentPreviews.value.a).toBeUndefined()
    expect(queue.contentPreviews.value['reset-one']).toBeUndefined()
    expect(queue.contentPreviews.value['reset-two']?.token).toBe('content-new')
    old[1]!.resolve(preview('stale-two'))
    await vi.advanceTimersByTimeAsync(1)
  })

  it('keeps unsettled permits across real component unmount/remount without blocking another lane', async () => {
    const old = [pending<unknown>(), pending<unknown>()]
    let contentReads = 0
    vi.mocked(api).mockImplementation((path, options) => {
      const token = options?.body ? JSON.parse(String(options.body)).result_token as string : ''
      if (path === torrentRecognitionPath) return Promise.resolve({ ...title(1), status: 'unrecognized', tmdb_id: undefined })
      return old[contentReads++]?.promise ?? Promise.resolve(preview(token))
    })
    const owned: ReturnType<typeof useSearchResultRecognition>[] = []
    function component(prefix: string) {
      return defineComponent({ setup() {
        const items = ref([item(`${prefix}-one`, { payload_kind: 'magnet' }), item(`${prefix}-two`, { payload_kind: 'magnet' })])
        const queue = useSearchResultRecognition(items, () => true); owned.push(queue)
        onBeforeUnmount(() => queue.dispose())
        // The real composable watches results arriving after mount.
        void nextTick(() => { items.value = [...items.value] })
        return () => h('div')
      } })
    }
    const first = mount(component('old')); await flushPromises()
    expect(contentReads).toBe(2)
    first.unmount()
    const second = mount(component('new')); cleanup.push(() => second.unmount()); await flushPromises()
    expect(contentReads).toBe(2)
    expect(Object.keys(owned[1]!.recognitions.value)).toEqual(['new-one', 'new-two'])
    old[0]!.resolve(preview('stale')); await flushPromises()
    expect(contentReads).toBe(4)
    expect(owned[0]!.contentPreviews.value).toEqual({})
    expect(Object.keys(owned[1]!.contentPreviews.value)).toEqual(['new-one', 'new-two'])
    old[1]!.resolve(preview('stale-two')); await flushPromises()
  })

  it('admits an already waiting manual selection before replenishing automatic content work', async () => {
    const first = pending<number>(); const second = pending<number>(); const manual = pending<number>()
    const started: string[] = []
    const queue = createOwnedReadQueue<number>(2, 65000, 'torrent-content')
    cleanup.push(() => queue.dispose())
    queue.update(['first', 'second', 'third', 'fourth', 'fifth'].map(key => ({ key, run: () => {
      started.push(key)
      return key === 'first' ? first.promise : key === 'second' ? second.promise : Promise.resolve(1)
    }, resolve: () => {}, reject: () => {} })))
    await flushPromises()
    const controller = new AbortController()
    const selection = runResourceRead('torrent-content', controller.signal, () => { started.push('manual'); return manual.promise })
    first.resolve(1); await flushPromises()
    expect(started.slice(0, 3)).toEqual(['first', 'second', 'manual'])
    manual.resolve(1); second.resolve(1); await selection; await flushPromises()
    expect(started).toEqual(['first', 'second', 'manual', 'third', 'fourth', 'fifth'])
  })
})
