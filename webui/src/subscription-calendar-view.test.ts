// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import SubscriptionCalendarView from './views/SubscriptionCalendarView.vue'
import type { CalendarPage } from './subscription-calendar'
import { clearCSRFToken } from './api/client'

const auth = reactive({ user: { id: 1, permissions: ['follows.read_own'] } as { id: number; permissions: string[] } | null,
  canAny: (permissions: readonly string[]): boolean => permissions.some(permission => auth.user?.permissions.includes(permission)) })
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/use-job-live-refresh', () => ({ useJobLiveRefresh: vi.fn() }))
function envelope(data: unknown) { return { ok: true, status: 200, json: async () => ({ code: 0, message: 'success', data }) } }
function page(title = '测试剧'): CalendarPage {
  return { events: [{ id: 'sub:1:1', subscription_id: 'sub', tmdb_id: 100, title, subscription_status: 'active', season_number: 1, episode_number: 1, episode_title: '第一集', air_date: '2026-10-26', library_status: 'present', activity: 'downloading', schedule_state: 'fresh' }],
    undated: [], subscriptions: [{ id: 'sub', title, status: 'active', seasons: [1] }], has_more: false, partial: false, reasons: [], missing_seasons: 0, stale_seasons: 0, refreshing_seasons: 0, undated_count: 0,
    date_basis: 'expected_air_date', scope: 'own', generated_at: '2026-10-26T00:00:00Z', timezone: 'UTC', start: '2026-09-28', end: '2026-11-09' }
}
beforeEach(() => { vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-10-26T12:00:00Z')); auth.user = { id: 1, permissions: ['follows.read_own'] } })
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); clearCSRFToken() })
const stubs = { RouterLink: { template: '<a><slot /></a>' } }

describe('subscription calendar mounted behavior', () => {
  it('renders month and agenda from the same facts and exposes a real subscription link', async () => {
    const fetchMock = vi.fn(async () => envelope(page()))
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    expect(wrapper.text()).toContain('播出日历'); expect(wrapper.text()).toContain('2026年10月')
    expect(wrapper.findAll('.calendar-day')).toHaveLength(42)
    expect(wrapper.text()).toContain('已入库'); expect(wrapper.text()).not.toContain('待接入')
    await wrapper.findAll('button').find(button => button.text() === '日程')!.trigger('click')
    expect(wrapper.findAll('.agenda-day')).toHaveLength(1)
    expect(wrapper.findAll('.agenda-day .calendar-event-card')).toHaveLength(1)
    expect(wrapper.text()).toContain('S01E01')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
  it('only requests schedule refresh after its explicit action and shows pending data honestly', async () => {
    const paths: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input); paths.push(path)
      if (path === '/api/v1/auth/csrf') return envelope({ csrf_token: 'test' })
      if (path === '/api/v1/follows/calendar/refresh') return envelope({ queued: 1, already_queued: 0, cooling_down: 0, partial: false })
      return envelope({ ...page(), events: [], missing_seasons: 1, partial: true, reasons: ['schedule_pending'] })
    }))
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    expect(paths.some(path => path.endsWith('/refresh'))).toBe(false)
    expect(wrapper.text()).toContain('1 个季的排期待更新')
    await wrapper.findAll('button').find(button => button.text() === '更新播出计划')!.trigger('click'); await flushPromises()
    expect(paths.filter(path => path.endsWith('/refresh'))).toHaveLength(1)
    expect(wrapper.text()).toContain('已安排更新 1 个季')
    wrapper.unmount()
  })
  it('clears old account facts and rejects a late response after account replacement', async () => {
    let resolveOld!: (value: ReturnType<typeof envelope>) => void
    let calls = 0
    vi.stubGlobal('fetch', vi.fn(async () => { if (++calls === 1) return new Promise<ReturnType<typeof envelope>>(resolve => { resolveOld = resolve }); return envelope({ ...page('新账号'), events: [], subscriptions: [] }) }))
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    auth.user = { id: 2, permissions: ['follows.read_own'] }; await flushPromises()
    resolveOld(envelope(page('旧账号秘密标题'))); await flushPromises()
    expect(wrapper.text()).not.toContain('旧账号秘密标题')
    expect(wrapper.text()).toContain('还没有可显示的订阅')
    wrapper.unmount()
  })
  it('drops the previous account subscription filter before reading the new account', async () => {
    const paths: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => { paths.push(String(input)); return envelope(page()) }))
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    await wrapper.findAll('select')[1]!.setValue('sub'); await flushPromises()
    expect(paths.at(-1)).toContain('subscription_id=sub')
    auth.user = { id: 2, permissions: ['follows.read_own'] }; await flushPromises()
    expect(paths.at(-1)).not.toContain('subscription_id=sub')
    wrapper.unmount()
  })
  it('ignores a refresh completion after its calendar range has changed', async () => {
    let finishRefresh!: (value: ReturnType<typeof envelope>) => void
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/csrf')) return envelope({ csrf_token: 'test' })
      if (path.endsWith('/calendar/refresh')) return new Promise<ReturnType<typeof envelope>>(resolve => { finishRefresh = resolve })
      return envelope(page())
    }))
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '更新播出计划')!.trigger('click'); await flushPromises()
    await wrapper.get('button[aria-label="下个月"]').trigger('click'); await flushPromises()
    finishRefresh(envelope({ queued: 9, already_queued: 0, cooling_down: 0, partial: false })); await flushPromises()
    expect(wrapper.text()).not.toContain('已安排更新 9 个季')
    expect(wrapper.text()).toContain('2026年11月')
    wrapper.unmount()
  })
  it('clears facts and disables both actions when calendar authority is revoked', async () => {
    const fetchMock = vi.fn(async () => envelope(page('撤权前可见标题')))
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    expect(wrapper.text()).toContain('撤权前可见标题')
    auth.user!.permissions = []; await flushPromises()
    expect(wrapper.text()).not.toContain('撤权前可见标题')
    expect(wrapper.text()).not.toContain('正在读取播出计划')
    const actions = wrapper.findAll('.calendar-actions button')
    expect(actions.every(button => button.attributes('disabled') !== undefined)).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
  it('does not call an empty refresh result queued or cooling down', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).endsWith('/csrf')) return envelope({ csrf_token: 'test' })
      if (String(input).endsWith('/calendar/refresh')) return envelope({ queued: 0, already_queued: 0, cooling_down: 0, partial: false })
      return envelope({ ...page(), events: [], subscriptions: [] })
    }))
    const wrapper = mount(SubscriptionCalendarView, { global: { stubs } }); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '更新播出计划')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('当前没有可更新的订阅排期')
    expect(wrapper.text()).not.toContain('刚刚已安排过更新')
    wrapper.unmount()
  })
})
