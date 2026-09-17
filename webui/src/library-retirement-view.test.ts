// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import MediaLibrariesView from './views/MediaLibrariesView.vue'

const mocks = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
const library = () => ({ id: 1, name: '移除测试库', enabled: true, status: 'listening', structure_status: 'issues', storage_id: 1, profile_id: 1, video_extensions: [], ignore_patterns: [], strm_asset_extra_extensions: [] })
const retirement = { status: 'deleting', job_id: 'retirement-job' }
let wrapper: VueWrapper | undefined
async function open() {
  wrapper = mount(MediaLibrariesView, { attachTo: document.body, global: { stubs: { RouterLink: { template: '<a><slot /></a>' }, DirectoryPickerDialog: true, MediaReorganizationDialog: true, MediaLibrarySettingsFields: true } } })
  await flushPromises()
  return wrapper
}
beforeEach(() => { vi.useFakeTimers(); mocks.api.mockReset() })
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.restoreAllMocks(); vi.useRealTimers() })

describe('library retirement view', () => {
  it('retains deleting state, avoids catalog/diagnosis and keeps polling after transient failure', async () => {
    let reads = 0
    mocks.api.mockImplementation(async (path: string) => {
      if (path === '/api/v1/media-libraries') {
        reads++
        if (reads === 2) throw Error('temporary offline')
        return { list: reads < 4 ? [{ ...library(), enabled: false, retirement }] : [] }
      }
      return { list: [] }
    })
    const view = await open()
    expect(view.text()).toContain('正在移除索引')
    expect(view.text()).toContain('尚未完成')
    expect(view.find('[role="dialog"]').exists()).toBe(false)
    expect(mocks.api.mock.calls.some(([path]) => path.includes('/structure') || path.includes('/catalog') || path.includes('/runs'))).toBe(false)
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(view.text()).toContain('正在移除索引')
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(reads).toBe(3)
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(reads).toBe(4)
    expect(view.text()).not.toContain('正在移除索引')
  })

  it('closes an open diagnosis when another session retires the selected library', async () => {
    let retired = false
    mocks.api.mockImplementation(async (path: string) => {
      if (path === '/api/v1/media-libraries') return { list: [{ ...library(), ...(retired ? { enabled: false, retirement } : {}) }] }
      if (path.endsWith('/structure')) return { library_id: 1, generation: 1, revision: 'rev', status: 'issues', issue_count: 0, repairable_count: 0, classifications: {}, issues: [] }
      return { list: [], total: 0 }
    })
    const view = await open()
    await view.findAll('button').find(button => button.text() === '查看诊断与处理入口')!.trigger('click'); await flushPromises()
    expect(view.find('[role="dialog"]').exists()).toBe(true)
    retired = true
    await view.findAll('button').find(button => button.text() === '刷新')!.trigger('click'); await flushPromises()
    expect(view.find('[role="dialog"]').exists()).toBe(false)
    expect(view.text()).toContain('查看移除任务')
    const before = mocks.api.mock.calls.filter(([path]) => path.includes('/structure')).length
    await vi.advanceTimersByTimeAsync(4000); await flushPromises()
    expect(mocks.api.mock.calls.filter(([path]) => path.includes('/structure'))).toHaveLength(before)
  })
})
