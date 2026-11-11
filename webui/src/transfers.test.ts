// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { APIError, api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import OrganizationView from '@/views/OrganizationView.vue'
import { canDeleteTransferRecord, formatTransferProgress, remoteTransferPhaseLabel, shouldRefreshTransferEvent, transferDeletionLabels, transferExecutionLabel, transferIdentityLabel, transferPhaseDescription, transferRouteLabel, transferStatusClass, transferStatusLabel, transferTargetCheckSummary, transferUploadMetrics, type TransferSummary } from '@/transfers'

const route = vi.hoisted(() => ({ query: {} as Record<string, string> }))
const actor = vi.hoisted(() => ({ canRespond: true }))
vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: vi.fn() }))
vi.mock('vue-router', () => ({ useRoute: () => route, useRouter: () => ({ replace: async ({ query }: { query: Record<string, string> }) => { route.query = query } }) }))
vi.mock('@/toast', () => ({ notify: vi.fn() }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 7 }, can: (code: string) => (code === Permissions.JobsRespond && actor.canRespond) || [Permissions.TransfersReadOwn, Permissions.JobsReadOwn, Permissions.JobsControlOwn].includes(code as typeof Permissions.TransfersReadOwn), canAny: () => true }) }))

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  route.query = {}
  actor.canRespond = true
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

const summary = { phase: 'planning', job_status: 'running', processed_files: 0, total_files: 2 } as TransferSummary

describe('media organization presentation', () => {
  it('uses exact transfer and waiting stages', () => {
    expect(transferStatusLabel(summary)).toBe('规划目录')
    expect(transferStatusLabel({ ...summary, job_status: 'waiting_user_action' })).toBe('等待冲突处理')
    expect(transferStatusLabel({ ...summary, phase: 'failed', job_status: 'failed' })).toBe('整理失败')
    expect(transferStatusLabel({ ...summary, phase: 'failed', job_status: 'queued' })).toBe('等待重试')
    expect(transferStatusLabel({ ...summary, phase: 'transferring', job_status: 'cancelled' })).toBe('已取消')
    expect(transferStatusLabel({ ...summary, phase: 'checking_directories' })).toBe('检查目标目录')
    expect(transferStatusLabel({ ...summary, phase: 'materializing' })).toBe('拉取到 Server 暂存')
    expect(transferStatusLabel({ ...summary, phase: 'verifying' })).toBe('校验完整性')
    expect(transferStatusLabel({ ...summary, phase: 'uploading' })).toBe('上传到目标网盘')
    expect(transferStatusLabel({ ...summary, phase: 'reconciling' })).toBe('媒体库对账')
    expect(transferPhaseDescription({ ...summary, phase: 'risk_backoff', retry_at: null })).toContain('等待安全重试')
  })

  it('does not invent progress before a plan exists', () => {
    expect(formatTransferProgress({ ...summary, total_files: 0 })).toBe('尚未生成计划')
    expect(formatTransferProgress({ ...summary, processed_files: 1 })).toBe('1 / 2')
  })

  it('exposes non-color status classes', () => {
    expect(transferStatusClass({ ...summary, phase: 'completed', job_status: 'completed' })).toContain('ready')
    expect(transferStatusClass({ ...summary, phase: 'failed', job_status: 'failed' })).toContain('error')
  })

  it('distinguishes locked, AI and provisional identities', () => {
    expect(transferIdentityLabel({ ...summary, identity_locked: true, identity_source: 'manual', identity_status: 'verified', identity_revision: 3 })).toBe('人工锁定 · r3')
    expect(transferIdentityLabel({ ...summary, identity_locked: true, identity_source: 'direct_id', identity_status: 'verified', identity_revision: 1 })).toBe('身份待核实 · r1')
    expect(transferIdentityLabel({ ...summary, identity_locked: false, identity_source: 'ai', identity_status: 'provisional', identity_revision: 2 })).toBe('AI 辅助 · r2')
    expect(transferIdentityLabel({ ...summary, identity_locked: false, identity_source: 'automatic', identity_status: 'provisional', identity_revision: 1 })).toBe('自动暂定 · r1')
  })

  it('renders only the frozen server route instead of inferring from provider types', () => {
    expect(transferRouteLabel('same_source_local')).toBe('同源本地')
    expect(transferRouteLabel('same_source_provider')).toBe('同源云端')
    expect(transferRouteLabel('cross_source')).toBe('跨数据源暂存')
    expect(transferRouteLabel('')).toBe('旧任务路线未知')
  })

  it('shows the frozen Node and its latest remote stage', () => {
    const remote = { ...summary, execution_location: 'node', node_name: '香港节点', remote_phase: 'uploading_target' } as TransferSummary
    expect(transferExecutionLabel(remote)).toBe('传输节点 · 香港节点')
    expect(remoteTransferPhaseLabel(remote)).toBe('节点上传目标网盘')
    expect(remoteTransferPhaseLabel({ ...remote, remote_phase: 'verifying_target' })).toBe('节点核验目标文件')
    const verified = { ...remote, remote_phase: 'verifying_target', remote_target_check: { version: 1, reason: 'target_verified', complete: true } }
    expect(remoteTransferPhaseLabel(verified)).toBe('节点核验目标文件')
    expect(transferTargetCheckSummary(verified)?.reason).toBe('目标文件核对通过')
    expect(transferTargetCheckSummary({ ...remote, remote_target_check: { version: 2, reason: 'target_missing', complete: true } })).toBeNull()
    expect(remoteTransferPhaseLabel({ ...remote, remote_phase: '' })).toBe('节点等待调度')
    expect(transferExecutionLabel({ ...summary, execution_location: 'server' } as TransferSummary)).toBe('主 Server')
  })

  it('refreshes only transfer or visible job events', () => {
    const visible = new Set(['visible-job'])
    expect(shouldRefreshTransferEvent(JSON.stringify({ data: { job_id: 'other', job_type: 'transfer' } }), visible)).toBe(true)
    expect(shouldRefreshTransferEvent(JSON.stringify({ data: { job_id: 'visible-job', job_type: 'download' } }), visible)).toBe(true)
    expect(shouldRefreshTransferEvent(JSON.stringify({ data: { job_id: 'other', job_type: 'download' } }), visible)).toBe(false)
    expect(shouldRefreshTransferEvent('invalid', visible)).toBe(false)
  })

  it('only offers record deletion for terminal tasks', () => {
    for (const job_status of ['failed', 'cancelled', 'completed'] as const) {
      expect(canDeleteTransferRecord({ ...summary, job_status })).toBe(true)
    }
    for (const job_status of ['queued', 'running', 'waiting_user_action', 'retry_wait', 'paused'] as const) {
      expect(canDeleteTransferRecord({ ...summary, job_status })).toBe(false)
    }
    const recovering = { ...summary, job_status: 'completed', recovery_pending: true } as TransferSummary
    expect(canDeleteTransferRecord(recovering)).toBe(false)
    expect(transferStatusLabel(recovering)).toBe('等待核对原上传')
    expect(remoteTransferPhaseLabel({ ...recovering, execution_location: 'node', remote_phase: 'uploading_target' })).toContain('请管理员核对原任务与节点状态')
  })

  it('exposes four explicit deletion scopes without collapsing destructive choices', () => {
    expect(Object.keys(transferDeletionLabels)).toEqual(['record_only', 'record_and_source', 'record_and_library', 'record_source_and_library'])
    expect(transferDeletionLabels.record_only).toBe('仅删除转移记录')
    expect(transferDeletionLabels.record_source_and_library).toContain('源文件和媒体库文件')
  })

  it('keeps measured single-file upload, old-operation recovery and Server history consistent across table and detail', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-11-11T02:00:00Z'))
    vi.stubGlobal('WebSocket', class { close() {} })
    const upload = {
      ...summary, id: 'upload', job_id: 'upload-job', owner_id: 7, display_name: '正在上传的一集',
      library_name: '媒体库', phase: 'transferring', execution_location: 'node', node_name: '传输节点',
      remote_phase: 'uploading_target', remote_progress: 0, processed_files: 0, total_files: 1,
      remote_transferred_bytes: 512 * 1024 * 1024, remote_total_bytes: 1024 * 1024 * 1024,
      remote_upload_speed_bps: 1024 * 1024, remote_sampled_at: '2026-11-11T02:00:00Z',
    } as TransferSummary
    const recovering = {
      ...upload, id: 'recovering', display_name: '原上传等待恢复', job_id: 'original-job', job_status: 'retry_wait',
      remote_phase: 'verifying_target', remote_error_code: 'node_reconciliation_required',
      remote_target_check: { version: 1, reason: 'target_missing', complete: true, name_matches: 0, size_matches: 0, identical_matches: 0 },
      retry_at: '2026-11-11T02:01:00Z', remote_transferred_bytes: undefined, remote_total_bytes: undefined,
      remote_upload_speed_bps: undefined, remote_sampled_at: undefined,
    } as TransferSummary
    const completed = { ...summary, id: 'finished', display_name: '真正完成的入库', phase: 'completed', job_status: 'completed', execution_location: 'server', total_files: 1, processed_files: 1 } as TransferSummary
    let pendingRead: Promise<void> | undefined
    let pendingDetail: Promise<void> | undefined
    let pendingRetry: Promise<void> | undefined
    let retryConflict = false
    let retryAction: { version: number; action_type: string; prompt: string; options: string[]; preview: Record<string, string> } | null = null
    let detailUnavailable = false
    vi.mocked(api).mockImplementation(async path => {
      if (path.startsWith('/api/v1/transfers?')) {
        await pendingRead
        const list = path.includes('scope=history') ? [completed] : [upload, recovering]
        return { list, total: list.length, stats: { processing: 2, waiting_action: 0, failed: 0, completed_today: 1 }, filter_options: { libraries: [], categories: [] } }
      }
      if (path === '/api/v1/transfers/upload') return { ...upload, job: { status: upload.job_status, action_request: null }, plan_summary: null, timeline: [], attempts: [] }
      if (path === '/api/v1/transfers/recovering') {
        await pendingDetail
        if (detailUnavailable) throw new APIError(503, 'TEMPORARILY_UNAVAILABLE', '详情读取暂时不可用')
        return { ...recovering, job: { status: recovering.job_status, action_request: retryAction }, plan_summary: null, timeline: [], attempts: [] }
      }
      if (path.startsWith('/api/v1/jobs/original-job/actions/') && path.endsWith('/respond')) {
        await pendingRetry
        if (retryConflict) {
          retryConflict = false
          recovering.upload_retry_version = 5
          retryAction = { ...retryAction!, version: 5 }
          throw new APIError(409, 'queue_action_stale', '操作已变化')
        }
        recovering.job_status = 'queued'
        recovering.upload_retry_allowed = false
        retryAction = null
        return { status: 'queued' }
      }
      throw new Error(`unexpected offline request ${path}`)
    })
    wrapper = mount(OrganizationView, { global: { stubs: { RouterLink: true, MediaReorganizationDialog: true, TransferDeletionDialog: true, ClearHistoryButton: true } } })
    await flushPromises()
    const table = wrapper.get('.organization-table')
    expect(table.text()).toContain('512.0 MiB / 1.0 GiB · 50.0%')
    expect(table.text()).toContain('上传速度 1.0 MiB/s')
    expect(table.text()).toContain('等待核对旧上传结果')
    const recoveryRow = table.findAll('tbody tr')[1]!
    expect(recoveryRow.text()).toContain('尚未发现符合原计划的目标文件')
    expect(recoveryRow.text()).toContain('同名 0 项')
    expect(recoveryRow.text()).toContain('下次检查')
    expect(recoveryRow.text()).toContain('上传数据 未知 / 未知')
    expect(recoveryRow.find('progress').exists()).toBe(false)
    expect(table.findAll('button').some(button => button.text() === '删除记录')).toBe(false)
    expect(wrapper.get('.organization-mobile-list').text()).toContain('50.0%')
    await table.findAll('button').find(button => button.text() === '查看详情')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-drawer').text()).toContain('50.0%')
    expect(wrapper.get('.organization-drawer').text()).toContain('文件 0 / 1')

    await vi.advanceTimersByTimeAsync(16_000)
    expect(wrapper.get('.organization-table').text()).toContain('速度采样已过期')
    expect(wrapper.get('.organization-drawer').text()).toContain('上传速度 未知')
    upload.remote_phase = 'hashing_source'
    upload.remote_sampled_at = new Date().toISOString()
    await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-drawer').text()).toContain('节点校验源文件')
    expect(wrapper.get('.organization-drawer').text()).toContain('上传速度 未知')
    expect(transferUploadMetrics({ ...upload, remote_phase: 'uploading_target', remote_sampled_at: new Date(Date.now() + 1_000).toISOString() }, Date.now()).speed).toBeNull()
    expect(transferUploadMetrics({ ...upload, remote_transferred_bytes: undefined, remote_total_bytes: undefined, remote_sampled_at: undefined }, Date.now()).percentage).toBeNull()

    let finishRead!: () => void
    pendingRead = new Promise(resolve => { finishRead = resolve })
    const beforePoll = vi.mocked(api).mock.calls.filter(([path]) => path.startsWith('/api/v1/transfers?')).length
    await vi.advanceTimersByTimeAsync(14_000)
    expect(vi.mocked(api).mock.calls.filter(([path]) => path.startsWith('/api/v1/transfers?'))).toHaveLength(beforePoll + 1)
    let finishDetail!: () => void
    pendingDetail = new Promise(resolve => { finishDetail = resolve })
    await wrapper.get('.organization-table').findAll('button').filter(button => button.text() === '查看详情')[1]!.trigger('click')
    await flushPromises()
    finishRead()
    pendingRead = undefined
    await flushPromises()
    finishDetail()
    pendingDetail = undefined
    await flushPromises()
    expect(wrapper.get('.organization-drawer h2').text()).toBe('原上传等待恢复')
    expect(route.query.task).toBe('recovering')
    detailUnavailable = true
    await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-drawer h2').text()).toBe('原上传等待恢复')
    expect(wrapper.get('.organization-drawer').text()).toContain('详情读取暂时不可用')
    expect(route.query.task).toBe('recovering')

    detailUnavailable = false
    recovering.job_status = 'waiting_user_action'
    recovering.upload_retry_allowed = true
    recovering.upload_retry_version = 4
    retryAction = { version: 4, action_type: 'upload_retry', prompt: '旧上传尚未确认，可按原计划重新尝试', options: ['retry_upload'], preview: {} }
    actor.canRespond = false
    await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-table').findAll('button').some(button => button.text() === '重新尝试上传')).toBe(false)
    expect(wrapper.get('.organization-drawer').findAll('button').find(button => button.text() === '重新尝试上传')!.attributes('disabled')).toBeDefined()
    actor.canRespond = true
    await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-mobile-list').findAll('button').some(button => button.text() === '重新尝试上传')).toBe(true)
    const confirmation = vi.spyOn(window, 'confirm').mockReturnValue(true)
    let finishRetry!: () => void
    pendingRetry = new Promise(resolve => { finishRetry = resolve })
    retryConflict = true
    const retryButton = wrapper.get('.organization-drawer').findAll('button').find(button => button.text() === '重新尝试上传')!
    await retryButton.trigger('click')
    await retryButton.trigger('click')
    await flushPromises()
    const retryCalls = () => vi.mocked(api).mock.calls.filter(([path]) => path.endsWith('/respond'))
    expect(retryCalls()).toHaveLength(1)
    expect(retryCalls()[0]).toEqual(['/api/v1/jobs/original-job/actions/4/respond', { method: 'POST', body: JSON.stringify({ response: 'retry_upload' }) }])
    expect(retryButton.attributes('disabled')).toBeDefined()
    expect(confirmation).not.toHaveBeenCalled()
    finishRetry()
    pendingRetry = undefined
    await flushPromises()
    expect(retryCalls()).toHaveLength(1)
    expect(wrapper.get('.organization-drawer h2').text()).toBe('原上传等待恢复')
    await wrapper.get('.organization-table').findAll('button').find(button => button.text() === '重新尝试上传')!.trigger('click')
    await flushPromises()
    expect(retryCalls()).toHaveLength(2)
    expect(retryCalls()[1]![0]).toBe('/api/v1/jobs/original-job/actions/5/respond')
    expect(wrapper.get('.organization-drawer h2').text()).toBe('原上传等待恢复')
    expect(wrapper.get('.organization-drawer').text()).not.toContain('整理完成')
    expect(wrapper.get('.organization-table').findAll('button').some(button => button.text() === '重新尝试上传')).toBe(false)

    await wrapper.get('[aria-label="关闭详情"]').trigger('click')
    await wrapper.get('#organization-tab-history').trigger('click')
    await flushPromises()
    expect(wrapper.get('.organization-table').text()).toContain('真正完成的入库')
    expect(wrapper.get('.organization-table').text()).not.toContain('正在上传的一集')
    expect(vi.mocked(api).mock.calls.some(([path]) => path.startsWith('/api/v1/transfers?scope=history'))).toBe(true)
    await vi.advanceTimersByTimeAsync(0)
    wrapper.unmount()
    wrapper = undefined
    expect(vi.getTimerCount()).toBe(0)
  })
})
