// @vitest-environment happy-dom
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { APIError } from '@/api/client'
import { Permissions, type PermissionCode } from '@/auth/generated-permissions'
import ExecutionLogsPanel from '@/components/ExecutionLogsPanel.vue'
import FollowsView from '@/views/FollowsView.vue'
import RuntimeLogsView from '@/views/RuntimeLogsView.vue'
import { executionLogPath, executionStatusLabel, filterReasonRows, logFieldLabel, logFieldText, type ExecutionLogPage } from '@/execution-logs'
import { useAuthStore } from '@/stores/auth'
import { useJobLiveRefresh } from '@/use-job-live-refresh'

const { apiMock } = vi.hoisted(() => ({ apiMock: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: apiMock }))
vi.mock('@/use-job-live-refresh', () => ({ useJobLiveRefresh: vi.fn() }))
const snapshot = '2026-10-19T08:00:00.123456789Z'
const record = (id: string, message = '【自动追更】站点 A 返回 12 个资源') => ({ event_id: id, timestamp: '2026-10-19T07:59:00Z', level: 'info', message, operation: 'follow_search', operation_label: '自动追更', stage: 'search', outcome: 'finished', fields: { site_name: '站点 A', raw_results: 12 } })
function result(overrides: Partial<ExecutionLogPage> = {}): ExecutionLogPage { return { list: [record('event-1')], snapshot_to: snapshot, partial: false, availability: 'available', attempt_number: 1, ...overrides } }
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }
const wrappers: VueWrapper[] = []
async function context(path = '/subscriptions/manage', permissions: PermissionCode[] = [Permissions.FollowsReadOwn]) {
  const pinia = createPinia(), auth = useAuthStore(pinia)
  auth.user = { id: 7, username: 'reader', display_name: '普通用户', status: 'active', is_owner: false, roles: ['normal_user'], permissions }
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }] })
  await router.push(path); await router.isReady()
  return { auth, router, plugins: [pinia, router] }
}
function findButton(wrapper: VueWrapper, name: string) { return wrapper.findAll('button').find(button => button.text() === name)! }
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); apiMock.mockReset(); vi.useRealTimers(); vi.restoreAllMocks() })

describe('readable scoped execution logs', () => {
  it('renders the specific coverage uncertainty counts and a metadata-only wait without blaming scans', async () => {
    const ctx = await context()
    const message = '9 集播出信息不完整，等待剧集播出日期更新；2 集的 TMDB 剧集信息暂缺，等待元数据更新；当前没有可独立搜索的明确缺集，将按计划重新检查'
    apiMock.mockResolvedValue(result({ summary: { title: '已播均在库', status: 'failed', error_code: 'follow_coverage_unknown', error_message: message, filter_summary: { coverage_unknown: 11, coverage_air_date_unknown: 9, coverage_metadata_unknown: 2 } } }))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'follow', followId: 'f-1', runId: 'r-1' } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    expect(wrapper.text()).toContain(message)
    expect(wrapper.text()).toContain('播出日期未确认的集数 9')
    expect(wrapper.text()).toContain('剧集元数据暂缺的集数 2')
    expect(wrapper.text()).not.toContain('等待媒体库扫描')
    expect(filterReasonRows({ coverage_library_unknown: 3, coverage_seasons_unavailable: 1 })).toEqual([{ code: 'coverage_library_unknown', label: '在库覆盖依据待核实的集数', count: 3 }, { code: 'coverage_seasons_unavailable', label: '剧集清单暂不可用的季数', count: 1 }])
  })
  it.each(['follow_coverage_unknown', 'follow_submit_failed'])('renders the exact coverage wait without hiding other failures (%s)', async code => {
    const ctx = await context()
    apiMock.mockResolvedValue(result({ summary: { title: '等待中的订阅', status: 'failed', error_code: code, error_message: '当前运行说明', filter_summary: { coverage_unknown: 9 } } }))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'follow', followId: 'f-1', runId: 'r-1' } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    const outcome = wrapper.get('.execution-summary__outcome')
    expect(outcome.text()).toBe(code === 'follow_coverage_unknown' ? '等待剧集状态确认' : '本轮执行失败')
    expect(outcome.classes().includes('semantic-danger-text')).toBe(code !== 'follow_coverage_unknown')
    expect(wrapper.text()).toContain('状态未确认、暂不下载的集数 9')
  })

  it('serializes only scoped filters and gives unknown enums a safe Chinese fallback', () => {
    const path = executionLogPath({ kind: 'follow', followId: 'f/1', runId: 'r?2' }, { limit: 999, attempt: 2, keyword: '  柯南  ', level: 'warn', cursor: 'cursor-1', snapshot_to: snapshot })
    const [base, query] = path.split('?')
    expect(base).toBe('/api/v1/follows/f%2F1/runs/r%3F2/execution-logs')
    expect(Object.fromEntries(new URLSearchParams(query))).toEqual({ limit: '200', attempt: '2', keyword: '柯南', level: 'warn', cursor: 'cursor-1', snapshot_to: snapshot })
    expect(filterReasonRows({ resolution: 3, submit_failed: 1, future_reason: 2, invalid: -1 })).toEqual([{ code: 'resolution', label: '分辨率不符合', count: 3 }, { code: 'submit_failed', label: '提交下载失败', count: 1 }, { code: 'future_reason', label: '其他原因', count: 2 }])
    expect(filterReasonRows({ episode_not_confirmed: 2, coverage_unknown: 9 })).toEqual([{ code: 'episode_not_confirmed', label: '资源包含未播或状态未确认的剧集', count: 2 }, { code: 'coverage_unknown', label: '状态未确认、暂不下载的集数', count: 9 }])
    expect(logFieldText('size_bytes', 1.25 * 1024 ** 3)).toBe('1.25 GiB')
    expect(logFieldText('reason_code', 'private-provider-failure')).toBe('其他原因')
    expect(executionStatusLabel('no_match')).toBe('本轮没有可提交资源')
    expect(logFieldLabel('raw_results')).toBe('累计返回条目（含重复）')
  })

  it('shows durable counts, readable facts and plain text without requiring global log or job permissions', async () => {
    const ctx = await context()
    apiMock.mockResolvedValue(result({ list: [record('event-1', '<img src=x onerror=alert(1)>'), record('event-1')], summary: { title: '龙珠超', status: 'submitted', searched_names_count: 4, candidates: 2, accepted_submissions: 1, filter_summary: { resolution: 3 } } }))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'follow', followId: 'f-1', runId: 'r-1' } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    expect(apiMock).toHaveBeenCalledOnce()
    expect(apiMock.mock.calls[0]![0]).toBe('/api/v1/follows/f-1/runs/r-1/execution-logs?limit=100')
    expect(wrapper.text()).toContain('已提交下载，等待后续处理')
    expect(wrapper.text()).toContain('筛选后候选')
    expect(wrapper.text()).toContain('分辨率不符合 3')
    expect(wrapper.text()).toContain('不等于新建下载或已入库数量')
    expect(wrapper.findAll('.execution-events > li')).toHaveLength(1)
    expect(wrapper.text()).toContain('<img src=x onerror=alert(1)>')
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('在日志中心查看')
  })

  it('pins chronological pages to the server snapshot, pauses history and resets only on deliberate refresh', async () => {
    vi.useFakeTimers()
    const ctx = await context()
    apiMock.mockImplementation((path: string) => Promise.resolve(path.includes('cursor=page-2') ? result({ list: [record('event-2')] }) : result({ next_cursor: 'page-2', summary: { title: '快照中的任务', status: 'running' } })))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'job', jobId: 'job-1' }, active: true, fallbackSummary: { title: '最新任务不属于旧快照', status: 'completed', candidates: 99 } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    await findButton(wrapper, '下一页过程').trigger('click'); await flushPromises()
    const continuation = new URL(apiMock.mock.lastCall![0], 'http://test').searchParams
    expect(continuation.get('snapshot_to')).toBe(snapshot)
    expect(continuation.get('cursor')).toBe('page-2')
    await wrapper.setProps({ fallbackSummary: { title: '快照后又更新的摘要', status: 'completed', candidates: 999 } })
    expect(wrapper.find('[aria-label="本轮结果"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('快照后又更新的摘要')
    expect(wrapper.text()).toContain('已暂停自动刷新')
    const count = apiMock.mock.calls.length
    await wrapper.setProps({ active: false }); await vi.advanceTimersByTimeAsync(10000)
    expect(apiMock).toHaveBeenCalledTimes(count)
    expect(wrapper.text()).toContain('第 2 页')
    await findButton(wrapper, '上一页过程').trigger('click'); await flushPromises()
    expect(new URL(apiMock.mock.lastCall![0], 'http://test').searchParams.get('snapshot_to')).toBe(snapshot)
    await findButton(wrapper, '刷新本轮过程').trigger('click'); await flushPromises()
    expect(new URL(apiMock.mock.lastCall![0], 'http://test').searchParams.has('snapshot_to')).toBe(false)
    expect(wrapper.text()).toContain('第 1 页')
  })

  it('cancels a switched scope and clears private records on logout even when an old request resolves late', async () => {
    const ctx = await context()
    const old = deferred<ExecutionLogPage>()
    apiMock.mockImplementation((path: string) => path.includes('/old/') ? old.promise : Promise.resolve(result({ list: [record('new-event', '新任务过程')] })))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'job', jobId: 'old' } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    const signal = apiMock.mock.calls[0]![1].signal as AbortSignal
    await wrapper.setProps({ scope: { kind: 'follow', followId: 'f-2', runId: 'r-2' } }); await flushPromises()
    expect(signal.aborted).toBe(true)
    old.resolve(result({ list: [record('old-event', '旧任务不可回写')] })); await flushPromises()
    expect(wrapper.text()).toContain('新任务过程')
    expect(wrapper.text()).not.toContain('旧任务不可回写')
    ctx.auth.user = null; await flushPromises()
    expect(wrapper.findAll('.execution-events > li')).toHaveLength(0)
    const count = apiMock.mock.calls.length
    await findButton(wrapper, '刷新本轮过程').trigger('click'); await flushPromises()
    expect(apiMock).toHaveBeenCalledTimes(count)
  })

  it('keeps attempt filters separate, reports missing/partial history honestly and hides raw request failures', async () => {
    const ctx = await context()
    apiMock.mockResolvedValueOnce(result({ list: [], availability: 'unavailable', summary: { title: '历史任务', status: 'completed' } }))
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'job', jobId: 'history' }, attemptCount: 3 }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    expect(wrapper.text()).toContain('可能未记录或已超出保留范围')
    expect(wrapper.text()).toContain('缺少日志不代表本轮未执行')
    apiMock.mockResolvedValueOnce(result({ partial: true, availability: 'degraded' }))
    await wrapper.get('select').setValue('2'); await flushPromises()
    expect(new URL(apiMock.mock.lastCall![0], 'http://test').searchParams.get('attempt')).toBe('2')
    expect(wrapper.text()).toContain('不能将这些记录当作完整的操作清单')
    expect(wrapper.text()).toContain('记录可能不完整')
    apiMock.mockRejectedValueOnce(new APIError(403, 'FORBIDDEN', 'raw credential-bearing upstream detail'))
    await findButton(wrapper, '刷新本轮过程').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('当前账号无权查看')
    expect(wrapper.text()).not.toContain('raw credential')
    expect(wrapper.findAll('.execution-events > li')).toHaveLength(0)
  })

  it('clears the previous attempt immediately and never gives a historical attempt the latest fallback result', async () => {
    const ctx = await context()
    const historical = deferred<ExecutionLogPage>()
    apiMock.mockResolvedValueOnce(result({ list: [record('latest', '最新尝试日志')], attempt_number: 3, summary: { title: '最新任务摘要', status: 'completed' } }))
      .mockImplementationOnce(() => historical.promise)
    const wrapper = mount(ExecutionLogsPanel, { props: { scope: { kind: 'job', jobId: 'retry-job' }, attemptCount: 3, fallbackSummary: { title: '最新任务摘要', status: 'completed' } }, global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    expect(wrapper.text()).toContain('最新任务摘要')
    await wrapper.get('select').setValue('2')
    expect(wrapper.text()).not.toContain('最新尝试日志')
    expect(wrapper.text()).not.toContain('最新任务摘要')
    historical.resolve(result({ list: [record('historical', '第二次尝试日志')], attempt_number: 2 }))
    await flushPromises()
    expect(wrapper.text()).toContain('第二次尝试日志')
    expect(wrapper.find('[aria-label="本轮结果"]').exists()).toBe(false)
  })
})

describe('existing domain log entry points', () => {
  it('shows an active uncertain follow as a scheduled wait in both run list and details', async () => {
    const ctx = await context()
    const follow = { id: 'f-wait', owner_id: 7, title: '等待中的订阅', tmdb_id: 1, media_type: 'tv', status: 'active', revision: 1, snapshot: { seasons: [1], site_ids: [2], schedule: { minutes: 60 } }, progress_target: 15, progress_present: 15, progress_missing: 0, last_error_code: 'follow_coverage_unknown', last_error_message: '当前没有明确缺集', next_run_at: '2026-10-22T15:00:00Z' }
    const run = { id: 'r-wait', job_id: 'job-wait', status: 'failed', trigger: 'scheduled', subscription_revision: 1, searched_names_count: 0, candidates: 0, selected: 0, filter_summary: { coverage_unknown: 9 }, created_at: '2026-10-22T14:00:00Z', error_code: 'follow_coverage_unknown', error_message: '等待信息完善' }
    apiMock.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/follows?')) return Promise.resolve({ list: [follow], total: 1 })
      if (path.endsWith('/runs')) return Promise.resolve({ list: [run] })
      if (path.includes('/execution-logs')) return Promise.resolve(result({ summary: { title: follow.title, status: run.status, error_code: run.error_code, error_message: run.error_message, filter_summary: run.filter_summary } }))
      throw new Error('unexpected route')
    })
    const wrapper = mount(FollowsView, { global: { plugins: ctx.plugins, stubs: { FollowEditorDialog: true, ClearHistoryButton: true } } }); wrappers.push(wrapper)
    await flushPromises(); await findButton(wrapper, '运行记录').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('等待剧集状态确认')
    expect(wrapper.text()).toContain('将按计划复查')
    expect(wrapper.text()).not.toContain('执行受阻')
    expect(wrapper.findAll('.semantic-danger-text')).toHaveLength(0)
    await findButton(wrapper, '查看本轮过程').trigger('click'); await flushPromises()
    const drawer = wrapper.get('[role="dialog"]')
    expect(drawer.text()).toContain('等待剧集状态确认')
    expect(drawer.text()).toContain('将按计划复查')
    expect(drawer.text()).toContain('状态未确认、暂不下载的集数 9')
    expect(drawer.findAll('.semantic-danger-text')).toHaveLength(0)
  })

  it.each([['list', 401], ['list', 403], ['runs', 401], ['runs', 403], ['runs', 404]] as const)('closes an open follow log when %s refresh is denied with %s', async (endpoint, status) => {
    const ctx = await context()
    const follow = { id: 'f-1', owner_id: 7, title: '已撤权订阅', tmdb_id: 1, media_type: 'tv', status: 'active', revision: 1, snapshot: { seasons: [1], site_ids: [2], schedule: { minutes: 60 } }, progress_target: 4, progress_present: 2, progress_missing: 2 }
    const run = { id: 'r-1', job_id: 'job-1', status: 'submitted', trigger: 'manual', subscription_revision: 1, searched_names_count: 4, candidates: 2, selected: 1, filter_summary: {}, created_at: '2026-10-19T07:00:00Z', error_code: '', error_message: '' }
    let denied = false
    apiMock.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/follows?')) return denied && endpoint === 'list' ? Promise.reject(new APIError(status, 'FORBIDDEN', '已不可访问')) : Promise.resolve({ list: [follow], total: 1 })
      if (path.endsWith('/runs')) return denied && endpoint === 'runs' ? Promise.reject(new APIError(status, 'FORBIDDEN', '已不可访问')) : Promise.resolve({ list: [run] })
      if (path.includes('/execution-logs')) return Promise.resolve(result({ list: [record('private-event', '撤权前执行日志')] }))
      throw new Error('unexpected route')
    })
    const wrapper = mount(FollowsView, { global: { plugins: ctx.plugins, stubs: { FollowEditorDialog: true, ClearHistoryButton: true } } }); wrappers.push(wrapper)
    await flushPromises(); await findButton(wrapper, '运行记录').trigger('click'); await flushPromises()
    await findButton(wrapper, '查看本轮过程').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('撤权前执行日志')
    denied = true
    await vi.mocked(useJobLiveRefresh).mock.lastCall![0]()
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('撤权前执行日志')
  })

  it('opens the exact follow run without jobs.read and displays existing reason counts', async () => {
    const ctx = await context()
    const follow = { id: 'f-1', owner_id: 7, title: '龙珠超', tmdb_id: 1, media_type: 'tv', status: 'active', revision: 1, snapshot: { seasons: [1], site_ids: [2], schedule: { minutes: 60 } }, progress_target: 4, progress_present: 2, progress_missing: 2 }
    const run = { id: 'r-1', job_id: 'job-1', status: 'submitted', trigger: 'manual', subscription_revision: 1, searched_names_count: 4, candidates: 2, selected: 1, filter_summary: { resolution: 3 }, created_at: '2026-10-19T07:00:00Z', error_code: '', error_message: '' }
    apiMock.mockImplementation((path: string) => { if (path.startsWith('/api/v1/follows?')) return Promise.resolve({ list: [follow], total: 1 }); if (path.endsWith('/runs')) return Promise.resolve({ list: [run] }); if (path.includes('/execution-logs')) return Promise.resolve(result()); throw new Error('unexpected route') })
    const wrapper = mount(FollowsView, { global: { plugins: ctx.plugins, stubs: { FollowEditorDialog: true, ClearHistoryButton: true } } }); wrappers.push(wrapper)
    await flushPromises(); await findButton(wrapper, '运行记录').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('筛选后候选 2')
    expect(wrapper.text()).toContain('分辨率不符合 3')
    expect(wrapper.text()).not.toContain('查看队列任务')
    await findButton(wrapper, '查看本轮过程').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('站点 A 返回 12 个资源')
    expect(apiMock.mock.calls.some(([path]) => path === '/api/v1/follows/f-1/runs/r-1/execution-logs?limit=100')).toBe(true)
    expect(apiMock.mock.calls.some(([path]) => path.startsWith('/api/v1/jobs') || path.startsWith('/api/v1/runtime-logs'))).toBe(false)
    await wrapper.get('button[aria-label="关闭本轮过程"]').trigger('click')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('deduplicates global business event IDs and freezes load-more queries while live refresh is paused', async () => {
    vi.useFakeTimers()
    const ctx = await context('/logs/runtime?job_id=job-1&follow_run_id=r-1&attempt=2', [Permissions.LogsRead])
    apiMock.mockImplementation((path: string) => {
      if (path.includes('/facets?')) return Promise.resolve({ levels: ['info'], modules: [], components: [], operations: ['follow_search'], plugin_ids: [] })
      if (path.endsWith('/settings')) return Promise.resolve({ health: { degraded: false } })
      const entry = (id: string) => ({ ...record(id), module: 'follow', component: 'worker', event_id: undefined, fields: { event_id: id, site_name: '站点 A' } })
      if (path.includes('cursor=older')) return Promise.resolve({ list: [entry('e1'), entry('e2')], snapshot_to: snapshot, partial: false, malformed: 0 })
      return Promise.resolve({ list: [entry('e1')], next_cursor: 'older', snapshot_to: snapshot, partial: false, malformed: 0 })
    })
    const wrapper = mount(RuntimeLogsView, { global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    const firstQuery = new URL(apiMock.mock.calls.find(([path]) => path.startsWith('/api/v1/runtime-logs?'))![0], 'http://test').searchParams
    expect(Object.fromEntries(firstQuery)).toMatchObject({ job_id: 'job-1', follow_run_id: 'r-1', attempt: '2' })
    await findButton(wrapper, '加载更多').trigger('click'); await flushPromises()
    expect(wrapper.findAll('article.runtime-entry')).toHaveLength(2)
    const query = new URL(apiMock.mock.lastCall![0], 'http://test').searchParams
    expect(query.get('snapshot_to')).toBe(snapshot)
    expect(wrapper.text()).toContain('新增日志不会打乱当前页面')
    const count = apiMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(20000)
    expect(apiMock).toHaveBeenCalledTimes(count)
    ctx.auth.user = null; await flushPromises()
    expect(wrapper.findAll('article.runtime-entry')).toHaveLength(0)
  })

  it.each(['true', 'false', null])('freezes an export before CSRF and reports only evidenced partial coverage (%s)', async partialHeader => {
    vi.useFakeTimers()
    const ctx = await context('/logs/runtime?job_id=job-1', [Permissions.LogsRead, Permissions.LogsExport])
    const refreshing = deferred<unknown>(), csrf = deferred<{ csrf_token: string }>()
    let listCalls = 0
    apiMock.mockImplementation((path: string) => {
      if (path.includes('/facets?')) return Promise.resolve({ levels: ['info'], modules: [], components: [], operations: [], plugin_ids: [] })
      if (path.endsWith('/settings')) return Promise.resolve({ health: { degraded: false } })
      if (path.endsWith('/csrf')) return csrf.promise
      if (++listCalls > 1) return refreshing.promise
      return Promise.resolve({ list: [{ ...record('export-visible', '导出快照记录'), module: 'follow', component: 'worker' }], snapshot_to: snapshot, partial: false, malformed: 0 })
    })
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('fixture export', { headers: partialHeader === null ? {} : { 'X-Log-Partial': partialHeader } }))
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:execution-export')
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const download = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(RuntimeLogsView, { global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    await findButton(wrapper, '刷新').trigger('click')
    const refreshSignal = apiMock.mock.calls.filter(([path]) => path.startsWith('/api/v1/runtime-logs?')).at(-1)![1].signal as AbortSignal
    await findButton(wrapper, '导出筛选结果').trigger('click')
    expect(refreshSignal.aborted).toBe(true)
    refreshing.resolve({ list: [{ ...record('late-refresh', '不得替换导出中的页面'), module: 'follow', component: 'worker' }], snapshot_to: '2026-10-19T09:00:00Z', partial: false, malformed: 0 })
    csrf.resolve({ csrf_token: 'fixture-csrf' }); await flushPromises()
    expect(wrapper.text()).toContain('导出快照记录')
    expect(wrapper.text()).not.toContain('不得替换导出中的页面')
    expect(JSON.parse(fetchMock.mock.lastCall![1]!.body as string).snapshot_to).toBe(snapshot)
    expect(download).toHaveBeenCalledOnce()
    expect(wrapper.text().includes('导出仅含部分可用记录')).toBe(partialHeader === 'true')
    const count = apiMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(20000)
    expect(apiMock).toHaveBeenCalledTimes(count)
    ctx.auth.user = null; await flushPromises()
    expect(wrapper.text()).not.toContain('导出仅含部分可用记录')
  })

  it.each([401, 403])('clears loaded log records if the raw export request returns %s', async status => {
    const ctx = await context('/logs/runtime', [Permissions.LogsRead, Permissions.LogsExport])
    apiMock.mockImplementation((path: string) => {
      if (path.includes('/facets?')) return Promise.resolve({ levels: [], modules: [], components: [], operations: [], plugin_ids: [] })
      if (path.endsWith('/settings')) return Promise.resolve({ health: { degraded: false } })
      if (path.endsWith('/csrf')) return Promise.resolve({ csrf_token: 'fixture-csrf' })
      return Promise.resolve({ list: [{ ...record('revoked', '只在撤权前可见'), module: 'follow', component: 'worker' }], snapshot_to: snapshot, partial: false, malformed: 0 })
    })
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('', { status }))
    const wrapper = mount(RuntimeLogsView, { global: { plugins: ctx.plugins } }); wrappers.push(wrapper)
    await flushPromises()
    expect(wrapper.text()).toContain('只在撤权前可见')
    await findButton(wrapper, '导出筛选结果').trigger('click'); await flushPromises()
    expect(wrapper.findAll('article.runtime-entry')).toHaveLength(0)
    expect(wrapper.get('[role="alert"]').text()).toContain(status === 401 ? '登录已失效' : '无权导出')
  })
})
