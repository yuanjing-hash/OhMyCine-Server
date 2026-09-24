// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UserMediaManager from './components/UserMediaManager.vue'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: request }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => false }) }))
afterEach(() => { request.mockReset(); vi.unstubAllGlobals() })
const item = (title: string) => ({ library_id: 1, work_id: title, title, kind: 'movie' })
const page = (list: unknown[], number = 1, total = list.length) => ({ list, total, page: number, page_size: 24, has_more: number * 24 < total, revision: 8 })

describe('same-page media management', () => {
  it('uses server pages beyond old limits and ignores stale tab responses', async () => {
    let old: (value: unknown) => void = () => {}
    request.mockImplementationOnce(() => new Promise(resolve => { old = resolve })).mockResolvedValue(page([{ id: 'c', name: 'New collection', source: 'manual', kind: 'collection', revision: 8 }]))
    const wrapper = mount(UserMediaManager, { props: { mode: 'favorites' } })
    await wrapper.setProps({ mode: 'manual' }); await flushPromises()
    old(page([item('Stale favorite')]))
    await flushPromises()
    expect(wrapper.text()).toContain('New collection')
    expect(wrapper.text()).not.toContain('Stale favorite')
    expect(request.mock.calls[1][0]).toContain('source=manual&page=1&page_size=24')
    wrapper.unmount()
  })

  it('paginates favorites and exposes cancellable own-state mutations', async () => {
    request.mockResolvedValueOnce(page([item('First')], 1, 1001)).mockResolvedValueOnce(page([item('Second')], 2, 1001)).mockResolvedValue({})
    vi.stubGlobal('confirm', () => true)
    const wrapper = mount(UserMediaManager, { props: { mode: 'favorites' } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '下一页')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('Second')
    expect(request.mock.calls[1][0]).toContain('page=2&page_size=24')
    await wrapper.findAll('button').find(button => button.text() === '取消收藏')!.trigger('click'); await flushPromises()
    expect(request.mock.calls[2]).toEqual(['/api/v1/media-libraries/favorites/1%3ASecond', { method: 'PUT', body: JSON.stringify({ favorite: false }) }])
    wrapper.unmount()
  })

  it('uses authoritative collection revision and preserves rename draft on conflict', async () => {
    request.mockResolvedValueOnce(page([item('One'), item('Two')])).mockRejectedValueOnce(new Error('合集已变化，请刷新后重试'))
    const wrapper = mount(UserMediaManager, { props: { mode: 'manual', initialCollection: { id: 'c', name: 'Original', kind: 'collection', source: 'manual', item_count: 2, revision: 1 } } })
    await flushPromises()
    await wrapper.get('input[aria-label="合集名称"]').setValue('Changed')
    await wrapper.findAll('form')[0].trigger('submit'); await flushPromises()
    expect(JSON.parse(request.mock.calls[1][1].body)).toEqual({ name: 'Changed', revision: 8 })
    expect(wrapper.get<HTMLInputElement>('input[aria-label="合集名称"]').element.value).toBe('Changed')
    expect(wrapper.text()).toContain('合集已变化')
    wrapper.unmount()
  })

  it('never offers manual mutations on an automatic collection and aborts teardown', async () => {
    request.mockResolvedValue(page([item('One')]))
    const wrapper = mount(UserMediaManager, { props: { mode: 'automatic', initialCollection: { id: 'a', name: 'Auto', kind: 'collection', source: 'tmdb', item_count: 1 } } })
    await flushPromises()
    expect(wrapper.text()).not.toContain('删除合集')
    expect(wrapper.text()).not.toContain('移出合集')
    expect(wrapper.text()).not.toContain('从媒体库添加作品')
    const signal = request.mock.calls[0][1].signal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })
})
