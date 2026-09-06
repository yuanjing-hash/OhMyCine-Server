// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { useJobLiveRefresh } from './use-job-live-refresh'
import NotificationCenter from './components/NotificationCenter.vue'
import { APIError } from './api/client'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
class Socket {
  static instances: Socket[] = []
  onmessage: (() => void) | null = null
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  close = vi.fn()
  constructor() { Socket.instances.push(this) }
}
const wrappers: VueWrapper[] = []
beforeEach(() => { vi.useFakeTimers(); vi.stubGlobal('WebSocket', Socket); Socket.instances = []; request.mockReset() })
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()); vi.useRealTimers(); vi.unstubAllGlobals() })
const page = (read = false, resolved = false) => ({ list: [{ id: 'job-1', title: '需要处理', status: resolved ? 'completed' : 'failed', occurrence: 8, read, resolved, link: '/automation/tasks?job_id=job-1' }], total: 1, page: 1, has_more: false })
function liveProbe(refresh: () => Promise<void>, socketAllowed = true) {
  return defineComponent({ setup() { useJobLiveRefresh(refresh, () => true, () => socketAllowed); return () => null } })
}

describe('live administration', () => {
  it('retains polling for dashboard readers without job stream permission', async () => {
    const refresh = vi.fn().mockResolvedValue(undefined)
    const component = liveProbe(refresh, false)
    wrappers.push(mount(component))
    await vi.advanceTimersByTimeAsync(15_500)
    expect(refresh).toHaveBeenCalledTimes(1)
    expect(Socket.instances).toHaveLength(0)
  })

  it('replays invalidations received during a pending refresh', async () => {
    let release!: () => void
    const refresh = vi.fn().mockImplementationOnce(() => new Promise<void>(resolve => { release = resolve })).mockResolvedValue(undefined)
    wrappers.push(mount(liveProbe(refresh)))
    Socket.instances[0].onmessage?.()
    await vi.advanceTimersByTimeAsync(300)
    Socket.instances[0].onmessage?.()
    await vi.advanceTimersByTimeAsync(300)
    expect(refresh).toHaveBeenCalledTimes(1)
    release(); await flushPromises()
    await vi.advanceTimersByTimeAsync(300)
    expect(refresh).toHaveBeenCalledTimes(2)
  })

  it('keeps read acknowledgement separate from recovery and navigates to the exact task', async () => {
    request.mockResolvedValueOnce(page()).mockResolvedValueOnce({}).mockResolvedValueOnce(page(true))
    const wrapper = mount(NotificationCenter); wrappers.push(wrapper)
    await flushPromises()
    await wrapper.findAll('button').find(b => b.text() === '标为已读')!.trigger('click'); await flushPromises()
    expect(request.mock.calls[1]).toEqual(['/api/v1/notifications/job-1/read', { method: 'POST', body: JSON.stringify({ occurrence: 8 }) }])
    expect(wrapper.text()).toContain('任务失败 · 已读')
    expect(wrapper.text()).not.toContain('已恢复 /')
    request.mockResolvedValue(page(true, true))
    Socket.instances[0].onmessage?.(); await vi.advanceTimersByTimeAsync(300); await flushPromises()
    expect(wrapper.text()).toContain('已恢复 / 不再待处理')
    await wrapper.findAll('button').find(b => b.text() === '查看任务')!.trigger('click')
    expect(wrapper.emitted('navigate')).toEqual([['/automation/tasks?job_id=job-1']])
  })

  it('clears notifications after denied access and stops requests after teardown', async () => {
    request.mockResolvedValueOnce(page()).mockRejectedValue(new APIError(403, 'permission_denied', '权限已变化'))
    const wrapper = mount(NotificationCenter); wrappers.push(wrapper)
    await flushPromises()
    Socket.instances[0].onmessage?.(); await vi.advanceTimersByTimeAsync(300); await flushPromises()
    expect(wrapper.text()).toContain('权限已变化')
    expect(wrapper.find('article').exists()).toBe(false)
    wrapper.unmount(); wrappers.pop()
    const count = request.mock.calls.length
    await vi.advanceTimersByTimeAsync(60_000)
    expect(request).toHaveBeenCalledTimes(count)
  })
})
