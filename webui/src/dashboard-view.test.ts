// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import DashboardView from './views/DashboardView.vue'
import { Permissions, type PermissionCode } from '@/auth/generated-permissions'

interface TestAuth { user: { id: number; display_name: string; permissions: PermissionCode[] } | null; can: (code: PermissionCode) => boolean; canAny: (codes: readonly PermissionCode[]) => boolean }
let auth: TestAuth
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/use-job-live-refresh', () => ({ useJobLiveRefresh: () => undefined }))
const reply = (data: unknown) => new Response(JSON.stringify({ code: 0, message: 'success', data }), { headers: { 'Content-Type': 'application/json' } })
const section = <T,>(list: T[] = []) => ({ status: 'ok', list, has_more: false })
function task(name: string) { return { id: name, display_name: name, job_type: 'transfer', status: 'running', progress: 0.5, processed_items: 1, total_items: 2 } }
function activity(name = '') { return { generated_at: '2026-10-26T08:00:00Z', active_tasks: section(name ? [task(name)] : []), waiting_tasks: section(), attention_tasks: section(), recently_added: section([{ library_id: 1, work_id: 'show', title: '最近入库作品', kind: 'series', added_at: '2026-10-25T08:00:00Z', season_count: 1, episode_count: 8 }]), capacities: section([{ id: 1, name: '媒体空间', enabled: true, status: 'not_sampled', free_bytes: null, total_bytes: null, sampled_at: null, checked_at: null }]) } }
function calendar() { return { events: [{ id: 'event', subscription_id: 'follow', title: '订阅剧集', season_number: 1, episode_number: 3, air_date: '2026-10-26', library_status: 'present', activity: 'none', schedule_state: 'fresh' }], undated: [], subscriptions: [{ id: 'follow', title: '订阅剧集' }], has_more: false, partial: false, missing_seasons: 0, stale_seasons: 0 } }
function setup(permissions: PermissionCode[] = Object.values(Permissions)) {
  auth = reactive({ user: { id: 1, display_name: '测试用户', permissions }, can: (code: PermissionCode): boolean => auth.user?.permissions.includes(code) ?? false, canAny: (codes: readonly PermissionCode[]): boolean => codes.some(code => auth.can(code)) })
}
function serve(view = activity('入库：测试剧 S01E08'), options: { calendarFailure?: boolean } = {}) {
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    void init
    const path = String(input)
    if (path === '/api/v1/dashboard/activity') return reply(view)
    if (path === '/api/v1/dashboard/operations') return reply({ sections: { 'media-summary': section([{ label: '可见媒体库', value: 2, link: '/discovery/library' }]), 'active-tasks': section([{ label: 'completed', value: 474 }]) } })
    if (path === '/api/v1/dashboard') return reply({ initialized: true, recovery_required: false, users: null, active_users: null, roles: null, audit_events: null })
    if (path.startsWith('/api/v1/follows/calendar?')) {
      if (options.calendarFailure) throw new Error('排期暂时不可用')
      return reply(calendar())
    }
    if (path === '/api/v1/auth/csrf') return reply({ csrf_token: 'fixture' })
    if (path === '/api/v1/storages/1/capacity') return reply({ id: 1, name: '媒体空间', enabled: true, status: 'ok', free_bytes: 1024 ** 4, total_bytes: 2 * 1024 ** 4, sampled_at: '2026-10-26T08:05:00Z', checked_at: '2026-10-26T08:05:00Z' })
    throw new Error(`unexpected ${path}`)
  })
  vi.stubGlobal('fetch', fetcher)
  return fetcher
}
function render() { return mount(DashboardView, { global: { stubs: { RouterLink: { props: ['to'], template: '<a><slot /></a>' } } } }) }
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('dashboard actual operations and calendar', () => {
  it('shows named running tasks, real recent posters and shared calendar without placeholder cards or history counts', async () => {
    setup(); const fetcher = serve(); const wrapper = render(); await flushPromises()
    expect(wrapper.text()).toContain('入库：测试剧 S01E08')
    expect(wrapper.text()).toContain('50%')
    expect(wrapper.text()).toContain('最近入库作品')
    expect(wrapper.text()).toContain('订阅剧集')
    expect(wrapper.text()).toContain('已入库')
    expect(wrapper.text()).not.toContain('474')
    expect(wrapper.text()).not.toContain('待接入')
    expect(wrapper.text()).not.toContain('data owner')
    expect(fetcher.mock.calls.some(([, options]) => (options as RequestInit | undefined)?.method === 'POST')).toBe(false)
    wrapper.unmount()
  })
  it('renders calm zero-running state and only samples capacity after an explicit action', async () => {
    setup(); const fetcher = serve(activity()); const wrapper = render(); await flushPromises()
    expect(wrapper.text()).toContain('暂无正在执行的任务')
    expect(wrapper.text()).toContain('尚未检测')
    expect(fetcher.mock.calls.some(([url]) => String(url).endsWith('/capacity'))).toBe(false)
    await wrapper.findAll('button').find(button => button.text() === '更新容量')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('剩余 1.0 TiB / 总计 2.0 TiB')
    expect(wrapper.text()).toContain('成功采样')
    wrapper.unmount()
  })
  it('isolates calendar failure and permission-hides unavailable domains', async () => {
    setup(); serve(activity('继续执行的任务'), { calendarFailure: true }); const wrapper = render(); await flushPromises()
    expect(wrapper.text()).toContain('排期暂时不可用')
    expect(wrapper.text()).toContain('继续执行的任务')
    expect(wrapper.text()).toContain('最近入库作品')
    auth.user = { id: 2, display_name: '只读首页', permissions: [Permissions.DashboardRead] }; await flushPromises()
    expect(wrapper.text()).not.toContain('继续执行的任务')
    expect(wrapper.text()).not.toContain('最近入库作品')
    expect(wrapper.text()).not.toContain('未来七天订阅更新')
    expect(wrapper.text()).not.toContain('更新容量')
    wrapper.unmount()
  })
  it('rejects a previous actor response that arrives after the new actor', async () => {
    setup(); let release: (value: Response) => void = () => undefined
    const oldResponse = new Promise<Response>(resolve => { release = resolve })
    const normal = serve(activity('新账号任务'))
    let first = true
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === '/api/v1/dashboard/activity' && first) { first = false; return oldResponse }
      return normal(input, init)
    }))
    const wrapper = render(); await flushPromises()
    auth.user = { id: 2, display_name: '新账号', permissions: Object.values(Permissions) }; await flushPromises()
    expect(wrapper.text()).toContain('新账号任务')
    release(reply(activity('旧账号私有任务'))); await flushPromises()
    expect(wrapper.text()).not.toContain('旧账号私有任务')
    wrapper.unmount()
  })
  it('preserves a completed capacity sample when an older dashboard read arrives last', async () => {
    setup()
    let release: (value: Response) => void = () => undefined
    const oldResponse = new Promise<Response>(resolve => { release = resolve })
    const normal = serve(activity())
    let activityReads = 0
    let pendingSignal: AbortSignal | null | undefined
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === '/api/v1/dashboard/activity' && ++activityReads === 2) {
        pendingSignal = init?.signal
        return oldResponse
      }
      return normal(input, init)
    }))
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click'); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '更新容量')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('剩余 1.0 TiB / 总计 2.0 TiB')
    release(reply(activity())); await flushPromises()
    expect(wrapper.text()).toContain('剩余 1.0 TiB / 总计 2.0 TiB')
    expect(pendingSignal?.aborted).toBe(true)
    expect(wrapper.findAll('button').find(button => button.text() === '刷新')!.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
  it('uses the registered task types for subscription and STRM stages', async () => {
    setup()
    const view = activity()
    view.active_tasks.list = [
      { ...task('搜索订阅：测试剧'), job_type: 'follow-search' },
      { ...task('生成媒体文件：测试剧'), job_type: 'strm_reconcile' },
    ]
    serve(view)
    const wrapper = render(); await flushPromises()
    expect(wrapper.text()).toContain('订阅搜索')
    expect(wrapper.text()).toContain('同步 STRM')
    wrapper.unmount()
  })
})
