// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import TasksView from './views/TasksView.vue'
import FollowsView from './views/FollowsView.vue'
import type { Job } from './jobs'
import { APIError } from './api/client'

const mocks = vi.hoisted(() => ({ list: vi.fn(), detail: vi.fn(), attempts: vi.fn(), timeline: vi.fn(), api: vi.fn(), followAction: vi.fn() }))
vi.mock('@/follows', async original => ({ ...await original<object>(), followAction: mocks.followAction }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 1 }, can: () => true, canAny: () => true }) }))
vi.mock('@/jobs', async original => ({ ...await original<object>(), listJobs: mocks.list, getJob: mocks.detail, getAttempts: mocks.attempts, getTimeline: mocks.timeline }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: mocks.api }))

class Socket {
  static instances: Socket[] = []
  onmessage: (() => void) | null = null
  onclose: (() => void) | null = null
  onopen: (() => void) | null = null
  close = vi.fn()
  constructor() { Socket.instances.push(this) }
}
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}
function job(id: string, status: Job['status'] = 'running'): Job {
  return { id, status, display_name: id, job_type: 'download', priority: 10, provider: '', resource_key: '', revision: 1, lane_position: 1, lane_rank: 1, owner_id: 1, created_by_kind: 'user', progress: null, processed_items: null, total_items: null, speed: null, eta_seconds: null, last_error_code: '', last_error_message: '', next_attempt_at: null, cancellation_requested: false, interrupt_pending: '', attempt_count: 1, created_at: '2026-09-05T00:00:00Z', updated_at: '2026-09-05T00:00:00Z', started_at: null, finished_at: null, action_request: null }
}
const page = (list: Job[]) => ({ list, total: list.length, page: 1, page_size: 50 })
const wrappers: VueWrapper[] = []
async function open(path = '/automation/tasks') {
  const router = createRouter({ history: createMemoryHistory(), routes: [
    { path: '/automation/tasks', component: TasksView }, { path: '/subscriptions/manage', component: FollowsView },
  ] })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(path.startsWith('/subscriptions') ? FollowsView : TasksView, { global: { plugins: [router] } })
  wrappers.push(wrapper)
  await flushPromises()
  return { wrapper, router }
}
beforeEach(() => {
  vi.clearAllMocks()
  mocks.api.mockReset()
  mocks.followAction.mockResolvedValue(undefined)
  Socket.instances = []
  vi.stubGlobal('WebSocket', Socket)
  mocks.list.mockResolvedValue(page([job('initial')]))
  mocks.detail.mockImplementation(async (id: string) => job(id))
  mocks.attempts.mockResolvedValue({ list: [] })
  mocks.timeline.mockResolvedValue({ list: [] })
})
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); vi.useRealTimers(); vi.unstubAllGlobals() })

describe('task interaction lifecycle', () => {
  it('shows a current wait reason separately in the row and drawer without hiding the last error', async () => {
    const waiting = { ...job('waiting', 'queued'), wait_reason: { code: 'library_busy', message: '先前文件操作的结果仍待核验' }, last_error_message: '上次执行失败' }
    mocks.list.mockResolvedValue(page([waiting]))
    mocks.detail.mockResolvedValue(waiting)
    const { wrapper } = await open('/automation/tasks?job_id=waiting')
    expect(wrapper.get('tbody').text()).toContain('等待原因：先前文件操作的结果仍待核验')
    expect(wrapper.get('tbody').text()).toContain('上次执行失败')
    expect(wrapper.get('[role="dialog"]').text()).toContain('等待原因：先前文件操作的结果仍待核验')
  })

  it('does not display an obsolete wait reason on a terminal job', async () => {
    const cancelled = { ...job('cancelled', 'cancelled'), wait_reason: { code: 'library_busy', message: 'obsolete wait' } }
    mocks.list.mockResolvedValue(page([cancelled]))
    mocks.detail.mockResolvedValue(cancelled)
    const { wrapper } = await open('/automation/tasks?job_id=cancelled')
    expect(wrapper.text()).not.toContain('obsolete wait')
  })

  it('ignores stale errors and restores filters when returning from a different page', async () => {
    const { wrapper, router } = await open('/automation/tasks?status=running&page=3')
    const pending = deferred<ReturnType<typeof page>>()
    mocks.list.mockReturnValueOnce(pending.promise)
    await wrapper.get('select').setValue('failed')
    await flushPromises()
    await wrapper.get('select').setValue('completed')
    await flushPromises()
    pending.reject(new APIError(403, 'permission_denied', 'stale forbidden'))
    await flushPromises()
    expect(wrapper.text()).not.toContain('stale forbidden')
    expect(wrapper.text()).toContain('initial')
    await router.push('/subscriptions/manage')
    router.back()
    await flushPromises()
    expect(wrapper.get('select').element.value).toBe('completed')
    expect(router.currentRoute.value.query.page).toBe('1')
  })

  it('does not reopen a closed drawer after delayed detail completion', async () => {
    const pending = deferred<Job>()
    mocks.detail.mockReturnValueOnce(pending.promise)
    const { wrapper } = await open('/automation/tasks?job_id=slow')
    await wrapper.get('[aria-label="关闭详情"]').trigger('click')
    await flushPromises()
    pending.resolve(job('slow'))
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect((mocks.detail.mock.calls[0][1] as AbortSignal).aborted).toBe(true)
  })

  it('clears a detail after access is denied instead of retaining sensitive content', async () => {
    vi.useFakeTimers()
    const { wrapper } = await open('/automation/tasks?job_id=private-job')
    mocks.detail.mockRejectedValueOnce(new APIError(403, 'permission_denied', '权限已变化'))
    Socket.instances[0].onmessage?.()
    await vi.advanceTimersByTimeAsync(500)
    await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('private-job')
    expect(wrapper.get('[role="dialog"]').text()).toContain('权限已变化')
  })

  it('reconnects boundedly and stops all refreshes on logout', async () => {
    vi.useFakeTimers()
    await open()
    Socket.instances[0].onclose?.()
    await vi.advanceTimersByTimeAsync(1500)
    expect(Socket.instances).toHaveLength(2)
    window.dispatchEvent(new CustomEvent('omc:unauthorized'))
    const calls = mocks.list.mock.calls.length
    await vi.advanceTimersByTimeAsync(60_000)
    expect(mocks.list).toHaveBeenCalledTimes(calls)
    expect(Socket.instances[1].close).toHaveBeenCalled()
  })
  it('ignores an old list response after a newer filter response', async () => {
    const { wrapper } = await open()
    const old = deferred<ReturnType<typeof page>>(), latest = deferred<ReturnType<typeof page>>()
    mocks.list.mockReturnValueOnce(old.promise).mockReturnValueOnce(latest.promise)
    await wrapper.get('select').setValue('running')
    await flushPromises()
    await wrapper.get('select').setValue('failed')
    await flushPromises()
    latest.resolve(page([job('new-failed', 'failed')]))
    await flushPromises()
    old.resolve(page([job('old-running')]))
    await flushPromises()
    expect(wrapper.text()).toContain('new-failed')
    expect(wrapper.text()).not.toContain('old-running')
  })

  it('opens the exact deep-linked job even outside the current page', async () => {
    const { wrapper, router } = await open('/automation/tasks?job_type=follow-search&job_id=outside-page&page=3')
    expect(mocks.detail).toHaveBeenCalled()
    expect(wrapper.get('[role="dialog"]').text()).toContain('outside-page')
    expect(mocks.list.mock.calls[0][0].get('page')).toBe('3')
    expect(mocks.list.mock.calls[0][0].get('job_type')).toBe('follow-search')
    await wrapper.get('[aria-label="关闭详情"]').trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.query.job_id).toBeUndefined()
    expect(router.currentRoute.value.query.page).toBe('3')
  })

  it('keeps the table during background refresh and refreshes open details', async () => {
    vi.useFakeTimers()
    const { wrapper } = await open('/automation/tasks?job_id=initial')
    const pending = deferred<ReturnType<typeof page>>()
    mocks.list.mockReturnValueOnce(pending.promise)
    mocks.detail.mockResolvedValue(job('initial', 'completed'))
    Socket.instances[0].onmessage?.()
    await vi.advanceTimersByTimeAsync(500)
    await flushPromises()
    expect(wrapper.find('table').exists()).toBe(true)
    expect(wrapper.get('[role="dialog"]').text()).toContain('已完成')
    pending.resolve(page([job('initial', 'completed')]))
    await flushPromises()
  })

  it('pages timeline and attempts independently when their totals differ', async () => {
    mocks.timeline.mockImplementation(async (_id: string, _signal: AbortSignal, current = 1) => ({ list: [{ id: current, event_type: 'status_changed', from_status: 'queued', to_status: 'running', code: '', created_at: '2026-09-05T00:00:00Z' }], total: 101, page: current, page_size: 50 }))
    mocks.attempts.mockImplementation(async (_id: string, _signal: AbortSignal, current = 1) => ({ list: current === 1 ? [{ id: 1, attempt_number: 1, status: 'running', error_code: '', error_message: '', started_at: '2026-09-05T00:00:00Z', finished_at: null }] : [], total: 1, page: current, page_size: 50 }))
    const { wrapper } = await open('/automation/tasks?job_id=initial')
    await wrapper.findAll('button').find(button => button.text() === '下一页时间线')!.trigger('click')
    await flushPromises()
    expect(mocks.timeline.mock.calls.at(-1)?.[2]).toBe(2)
    expect(mocks.attempts.mock.calls.at(-1)?.[2]).toBe(1)
    expect(wrapper.get('[role="dialog"]').text()).toContain('第 1 次')
  })

  it('aborts pending reads and closes the socket on unmount', async () => {
    const { wrapper } = await open()
    const signal = mocks.list.mock.calls[0][1] as AbortSignal | undefined
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    expect(signal?.aborted).toBe(true)
    expect(Socket.instances[0].close).toHaveBeenCalled()
  })
})

describe('subscription paging', () => {
  it('refreshes expanded runs after search and shows failed reads as errors, not empty history', async () => {
    const follow = { id: 'follow-1', title: '测试剧', owner_id: 1, status: 'active', revision: 1, snapshot: { seasons: [1], site_ids: [1], schedule: { minutes: 60 } } }
    let runCount = 0
    mocks.api.mockImplementation(async (url: string) => {
      if (url.endsWith('/runs')) return { list: [{ id: String(++runCount), status: runCount === 1 ? 'running' : 'completed', job_id: `job-${runCount}` }] }
      return { list: [follow], total: 1 }
    })
    const { wrapper } = await open('/subscriptions/manage')
    await wrapper.findAll('button').find(button => button.text() === '运行记录')!.trigger('click')
    await flushPromises()
    expect(runCount).toBe(1)
    await wrapper.findAll('button').find(button => button.text() === '立即搜索')!.trigger('click')
    await flushPromises()
    expect(runCount).toBe(2)
    expect(wrapper.find('a').attributes('href')).toContain('job-2')
    await wrapper.findAll('button').find(button => button.text() === '运行记录')!.trigger('click')
    mocks.api.mockRejectedValueOnce(new Error('读取运行失败'))
    await wrapper.findAll('button').find(button => button.text() === '运行记录')!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('读取运行失败')
    expect(wrapper.text()).not.toContain('暂无运行记录')
  })
  it('provides a route-backed next page beyond the first 100 subscriptions', async () => {
    mocks.api.mockImplementation(async (path: string) => {
      const current = Number(new URL(path, 'https://example.test').searchParams.get('page') || 1)
      return { list: [{ id: `follow-${current}`, title: `订阅第${current}页`, owner_id: 1, status: 'active', revision: 1, snapshot: { seasons: [1], site_ids: [1], schedule: { minutes: 60 } } }], total: 101, page: current, page_size: 100 }
    })
    const { wrapper, router } = await open('/subscriptions/manage')
    const next = wrapper.findAll('button').find(button => button.text() === '下一页')
    expect(next).toBeDefined()
    await next!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('订阅第2页')
    expect(router.currentRoute.value.query.page).toBe('2')
  })
})
