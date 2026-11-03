// @vitest-environment happy-dom
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, expect, it, vi } from 'vitest'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import FollowsView from '@/views/FollowsView.vue'
import type { FollowSummary } from '@/follows'

const { request } = vi.hoisted(() => ({ request: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/use-job-live-refresh', () => ({ useJobLiveRefresh: vi.fn() }))
const wrappers: VueWrapper[] = []
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); request.mockReset(); vi.useRealTimers(); vi.restoreAllMocks() })
const wait = { not_before: '2026-11-03T00:01:05Z', server_time: '2026-11-03T00:00:00Z', recorded_at: '2026-11-02T23:59:50Z', reason: 'provider_cooldown' as const }
const item: FollowSummary = { id: 'f1', owner_id: 1, media_type: 'tv', tmdb_id: 1, title: 'Example', status: 'active', revision: 1, snapshot: { version: 2, routing_policy: 'source_priority', seasons: [1], site_ids: [1], media_library_id: 1, schedule: { kind: 'interval', minutes: 360 }, filters: { resolutions: [], video_codecs: [], qualities: [], include_keywords: [], exclude_keywords: [], release_groups: [], exclude_release_groups: [], min_seeders: 0, max_age_hours: null, min_size_bytes: null, max_size_bytes: null }, max_resources_per_run: 3, download_priority: 0 }, progress_target: 4, progress_present: 3, progress_missing: 1, last_run_id: 'r1', last_error_code: 'follow_submit_partial', last_error_message: '请求受到限制', next_run_at: '2026-11-03T06:00:00Z', created_at: '', updated_at: '', wait }

async function render(rows: FollowSummary[], canExecute = false) {
  const pinia = createPinia(), auth = useAuthStore(pinia)
  auth.user = { id: 1, username: 'reader', display_name: 'Reader', status: 'active', is_owner: false, roles: [], permissions: [Permissions.FollowsReadOwn, ...(canExecute ? [Permissions.FollowsExecuteOwn] : [])] }
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }] })
  await router.push('/subscriptions/manage'); await router.isReady()
  request.mockImplementation((url: string) => Promise.resolve(url.endsWith('/runs') ? { list: [{ id: 'old-run', job_id: 'job', status: 'failed', subscription_revision: 1, error_code: 'follow_submit_failed', error_message: '旧记录', filter_summary: {}, wait, searched_names_count: 1, candidates: 1, selected: 0, created_at: '' }] } : { list: rows, total: rows.length }))
  const wrapper = mount(FollowsView, { global: { plugins: [pinia, router], stubs: { ClearHistoryButton: true } } }); wrappers.push(wrapper)
  await flushPromises(); return wrapper
}

it('uses one shared corrected clock, reaches zero without retrying, and disposes its timer', async () => {
  vi.useFakeTimers(); vi.setSystemTime(new Date('2026-01-01T00:00:00Z')) // deliberately skewed browser clock
  const startInterval = vi.spyOn(globalThis, 'setInterval'), stopInterval = vi.spyOn(globalThis, 'clearInterval')
  const wrapper = await render([item, { ...item, id: 'f2' }])
  expect(wrapper.text()).toContain('还需 1 分 5 秒')
  expect(wrapper.text()).toContain('下次检查')
  expect(startInterval).toHaveBeenCalledTimes(1)
  const calls = request.mock.calls.length
  await vi.advanceTimersByTimeAsync(1000)
  expect(wrapper.text()).toContain('还需 1 分 4 秒')
  await vi.advanceTimersByTimeAsync(64000)
  expect(wrapper.text()).toContain('本轮记录的冷却时间已结束')
  expect(wrapper.text()).not.toContain('还需 -')
  expect(request.mock.calls.length).toBe(calls)
  wrapper.unmount(); wrappers.splice(wrappers.indexOf(wrapper), 1)
  expect(stopInterval).toHaveBeenCalledWith(startInterval.mock.results[0]!.value)
})

it('does not invent a wait from errors or run history, and retains recorded historical facts', async () => {
  const wrapper = await render([{ ...item, wait: undefined }])
  expect(wrapper.text()).not.toContain('冷却截止时间')
  await wrapper.findAll('button').find(button => button.text() === '运行记录')!.trigger('click'); await flushPromises()
  expect(wrapper.text()).toContain('本轮记录的冷却截止时间')
  expect(wrapper.text()).toContain('此记录不代表上游当前状态')
  expect(wrapper.text()).not.toContain('还需')
})

it('refreshes the current wait and run history together after the child confirms history was cleared', async () => {
  const wrapper = await render([item], true)
  await wrapper.findAll('button').find(button => button.text() === '运行记录')!.trigger('click')
  await flushPromises()
  expect(wrapper.text()).toContain('本轮记录的冷却截止时间')
  const clearHistory = wrapper.findComponent({ name: 'ClearHistoryButton' })
  expect(clearHistory.exists()).toBe(true)

  request.mockImplementation((url: string) => Promise.resolve(url.endsWith('/runs')
    ? { list: [] }
    : { list: [{ ...item, wait: undefined }], total: 1 }))
  clearHistory.vm.$emit('cleared')
  await flushPromises()

  expect(wrapper.text()).not.toContain('冷却截止时间')
  expect(wrapper.text()).toContain('暂无运行记录')
  const paths = request.mock.calls.map(([url]) => String(url))
  expect(paths.filter(path => path.includes('/api/v1/follows?'))).toHaveLength(2)
  expect(paths.filter(path => path.endsWith('/runs'))).toHaveLength(2)
  expect(paths.every(path => path.startsWith('/api/v1/follows'))).toBe(true)
})
