// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ClearHistoryButton from './ClearHistoryButton.vue'

const mocks = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/api/client', () => ({ api: mocks.api }))

afterEach(() => {
  vi.restoreAllMocks()
  mocks.api.mockReset()
})

describe('ClearHistoryButton', () => {
  it('previews the exact management scope before confirmation and refreshes after purge', async () => {
    mocks.api
      .mockResolvedValueOnce({ scope: 'strm_runs', eligible: 3, deleted: 2, skipped: 1 })
      .mockResolvedValueOnce({ scope: 'strm_runs', eligible: 3, deleted: 2, skipped: 1 })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    vi.spyOn(window, 'alert').mockImplementation(() => undefined)
    const wrapper = mount(ClearHistoryButton, { props: { scope: 'strm_runs', resourceId: '9' } })

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(mocks.api).toHaveBeenNthCalledWith(1, '/api/v1/management-history/purge/preview', {
      method: 'POST',
      body: JSON.stringify({ scope: 'strm_runs', resource_id: '9' }),
    })
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('确认清除 2 条'))
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('1 条仍在执行、等待处理或自动核验文件结果'))
    expect(mocks.api).toHaveBeenNthCalledWith(2, '/api/v1/management-history/purge', {
      method: 'POST',
      body: JSON.stringify({ scope: 'strm_runs', resource_id: '9' }),
    })
    expect(wrapper.emitted('cleared')).toHaveLength(1)
    expect(window.alert).toHaveBeenCalledWith(expect.stringContaining('结束后可再次清除'))
  })

  it('does not submit when no terminal history is eligible', async () => {
    mocks.api.mockResolvedValueOnce({ scope: 'tasks', eligible: 0, deleted: 0, skipped: 0 })
    vi.spyOn(window, 'alert').mockImplementation(() => undefined)
    const wrapper = mount(ClearHistoryButton, { props: { scope: 'tasks' } })

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(mocks.api).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('cleared')).toBeUndefined()
  })

  it('explains every safe reason skipped history can remain without claiming user confirmation is required', async () => {
    mocks.api.mockResolvedValueOnce({ scope: 'tasks', eligible: 1, deleted: 0, skipped: 1 })
    vi.spyOn(window, 'alert').mockImplementation(() => undefined)
    const wrapper = mount(ClearHistoryButton, { props: { scope: 'tasks' } })

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(window.alert).toHaveBeenCalledWith(expect.stringContaining('仍在执行、等待处理或自动核验文件结果'))
    expect(window.alert).toHaveBeenCalledWith(expect.stringContaining('结束后可再次清除'))
    expect(mocks.api).toHaveBeenCalledTimes(1)
  })
})
