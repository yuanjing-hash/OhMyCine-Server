// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import JobRepairDetails from './components/JobRepairDetails.vue'
import LibraryReadiness from './components/LibraryReadiness.vue'
import { APIError } from './api/client'
const mocks = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/jobs', () => ({ getRepairDetails: mocks.get }))
afterEach(() => vi.clearAllMocks())
const result = (name: string) => ({ list: [{ ordinal: 1, action: 'move', source_path: name, target_path: 'TV/E01.mkv', status: 'succeeded', error_message: '' }], total: 101, page: 1, page_size: 50, phase: 'executing', counts: { succeeded: 1 }, current_batch_size: 0 })
describe('repair progress', () => {
 it('keeps the current page during updates and discards stale task results', async () => {
  mocks.get.mockResolvedValue(result('first'))
  const wrapper = mount(JobRepairDetails, { props: { jobId: 'one', revision: 1 } }); await flushPromises()
  await wrapper.findAll('button').find(b => b.text() === '下一页')!.trigger('click'); await flushPromises()
  await wrapper.setProps({ revision: 2 }); await flushPromises()
  expect(mocks.get.mock.calls.at(-1)?.slice(0,3)).toEqual(['one',2,''])
  let resolve!: (value: ReturnType<typeof result>) => void
  mocks.get.mockReturnValueOnce(new Promise(done => { resolve = done }))
  await wrapper.setProps({ revision: 3 })
  mocks.get.mockResolvedValueOnce(result('second'))
  await wrapper.setProps({ jobId: 'two' }); await flushPromises()
  resolve(result('stale')); await flushPromises()
  expect(wrapper.text()).toContain('second'); expect(wrapper.text()).not.toContain('stale')
  wrapper.unmount()
 })
 it('uses authoritative readiness and does not treat findings as a failure', async () => {
  const wrapper = mount(LibraryReadiness, { props: { library: { ready: true, readiness_status: 'ready' } } })
  expect(wrapper.text()).toBe('已准备好')
  await wrapper.setProps({ library: { ready: false, readiness_status: 'repairing' } })
  expect(wrapper.text()).toContain('整理修复中'); wrapper.unmount()
 })
 it('resumes automatic refresh after access is restored by a manual retry', async () => {
  mocks.get.mockRejectedValueOnce(new APIError(403, 'permission_denied', '权限已变化')).mockResolvedValue(result('restored'))
  const wrapper = mount(JobRepairDetails, { props: { jobId: 'one', revision: 1, active: true } }); await flushPromises()
  await wrapper.findAll('button').find(button => button.text() === '重试')!.trigger('click'); await flushPromises()
  await wrapper.setProps({ revision: 2 }); await flushPromises()
  expect(mocks.get).toHaveBeenCalledTimes(3)
  expect(wrapper.text()).toContain('restored')
  wrapper.unmount()
 })
})
