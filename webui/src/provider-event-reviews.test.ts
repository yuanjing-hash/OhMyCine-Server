// @vitest-environment happy-dom
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, expect, it, vi } from 'vitest'
import ProviderEventReviews from '@/components/ProviderEventReviews.vue'
import { api } from '@/api/client'
vi.mock('@/api/client', () => ({ api: vi.fn() }))
beforeEach(() => { vi.mocked(api).mockReset() })
it('shows paused reasons and pages without mutations', async () => {
  vi.mocked(api).mockResolvedValue({ list: [{ name: 'episode.mkv', reason: '缺少本地归属', created_at: '2026-09-15T00:00:00Z' }], total: 51, page: 1, page_size: 50 })
  const wrapper = mount(ProviderEventReviews, { props: { libraryId: 2 } }); await flushPromises()
  expect(wrapper.text()).toContain('待检查事件（51）')
  expect(wrapper.text()).toContain('不代表系统已完成对应的库内记录或 STRM 清理')
  await wrapper.findAll('button').find(button => button.text() === '下一页')!.trigger('click'); await flushPromises()
  expect(api).toHaveBeenLastCalledWith('/api/v1/media-libraries/2/provider-events?page=2', expect.objectContaining({ signal: expect.any(AbortSignal) }))
  wrapper.unmount()
})
it('cancels old library request and ignores late results', async () => {
  let resolve!: (value: unknown) => void
  vi.mocked(api).mockImplementationOnce(() => new Promise(done => { resolve = done })).mockResolvedValue({ list: [], total: 0, page: 1, page_size: 50 })
  const wrapper = mount(ProviderEventReviews, { props: { libraryId: 2 } }); await flushPromises()
  const signal = vi.mocked(api).mock.calls[0]![1]!.signal!
  await wrapper.setProps({ libraryId: 3 }); await flushPromises()
  expect(signal.aborted).toBe(true)
  resolve({ list: [{ name: 'stale', reason: 'stale' }], total: 1, page: 1, page_size: 50 }); await flushPromises()
  expect(wrapper.text()).not.toContain('stale')
  expect(wrapper.text()).toContain('没有待检查事件')
  wrapper.unmount()
})
it('timeout restores refresh instead of leaving a permanent spinner', async () => {
  vi.useFakeTimers()
  try {
    vi.mocked(api).mockImplementation(() => new Promise(() => {}))
    const wrapper = mount(ProviderEventReviews, { props: { libraryId: 2 } })
    await vi.advanceTimersByTimeAsync(15000)
    expect(wrapper.text()).toContain('读取超时')
    expect(wrapper.get('button').attributes('disabled')).toBeUndefined()
    expect(vi.mocked(api).mock.calls[0]![1]!.signal!.aborted).toBe(true)
    wrapper.unmount()
  } finally { vi.useRealTimers() }
})
