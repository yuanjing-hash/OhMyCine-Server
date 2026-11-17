import type { DownloaderSummary, DownloadTaskSummary, SeedingTaskSummary, TransferNodeSummary } from '@/types/api'

export function isSourceCleanupTask(task: Pick<SeedingTaskSummary, 'purpose' | 'transfer_mode'>): boolean {
  return task.purpose === 'source_cleanup' || task.transfer_mode === 'move'
}

export type DownloadSourceMode = 'url' | 'torrent' | 'share'
export function isCloudShareSource(kind?: string): kind is '115_share' | 'quark_share' { return kind === '115_share' || kind === 'quark_share' }
export function isCloudDownloader(type?: string): boolean { return type === 'pan115_offline' || type === 'quark_share' }
export function cloudShareDownloaderType(source?: string): 'pan115_offline' | 'quark_share' { return source === 'quark_share' ? 'quark_share' : 'pan115_offline' }
export function downloadSourceKind(mode: DownloadSourceMode, downloaderType?: string): string { return mode === 'share' ? (downloaderType === 'quark_share' ? 'quark_share' : '115_share') : mode }
export function nodeSupportsDownloader(node: Pick<TransferNodeSummary, 'status' | 'capabilities'>, type: DownloaderSummary['type']): boolean {
  if (node.status !== 'online') return false
  const required: Partial<Record<DownloaderSummary['type'], string[]>> = {
    qbittorrent: ['qbittorrent_control', 'range_export'],
    pan115_offline: ['pan115_offline', 'pan115_read', 'pan115_share_receive', 'range_export'],
    quark_share: ['quark_tv_read_v2', 'quark_share_receive_v2', 'content_digest_v2', 'range_export'],
  }
  return required[type]?.every(code => node.capabilities.codes.includes(code)) ?? false
}
export type DownloadManagementSection = 'active' | 'history' | 'create' | 'seeding' | 'downloaders' | 'nodes'
export interface DownloaderTaskStats { active: number; total: number; downloadSpeed: number | null; uploadSpeed: number | null; averageProgress: number | null }
export interface DownloadRetryPresentationState { errorFingerprint: string; taskUpdatedAt: string; observedActive: boolean }
export type DownloadRetryPresentations = Record<string, DownloadRetryPresentationState>
export interface ParsedDownloadSources { sources: string[]; duplicateCount: number }

const activeProviderStatuses = new Set(['allocating', 'checkingdl', 'checkingresumedata', 'downloading', 'forceddl', 'metadl', 'queueddl', 'stalleddl'])

export function parseDownloadSourceLines(value: string, limit = 50): ParsedDownloadSources {
  const sources: string[] = []
  const seen = new Set<string>()
  let duplicateCount = 0
  for (const raw of value.split(/\r?\n/)) {
    const source = raw.trim()
    if (!source) continue
    if (seen.has(source)) {
      duplicateCount++
      continue
    }
    if (sources.length >= limit) throw new Error(`一次最多提交 ${limit} 个链接`)
    seen.add(source)
    sources.push(source)
  }
  return { sources, duplicateCount }
}

export function nodePan115UnsupportedSource(value: string): string {
  for (const raw of value.split(/\r?\n/)) {
    const source = raw.trim()
    if (!source) continue
    if (/^https?:\/\//i.test(source)) return source
  }
  return ''
}

export function formatBytes(value: number | null, suffix = ''): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '未知'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let amount = value; let index = 0
  while (amount >= 1024 && index < units.length - 1) { amount /= 1024; index++ }
  return `${amount.toFixed(index === 0 ? 0 : 1)} ${units[index]}${suffix}`
}

export function formatProgress(value: number | null): string {
  return value === null || !Number.isFinite(value) ? '未知' : `${Math.max(0, Math.min(100, value)).toFixed(1)}%`
}

export function formatETA(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '未知'
  if (value < 60) return `${Math.round(value)} 秒`
  if (value < 3600) return `${Math.ceil(value / 60)} 分钟`
  return `${Math.floor(value / 3600)} 小时 ${Math.ceil((value % 3600) / 60)} 分钟`
}

export function downloadStatusLabel(task: DownloadTaskSummary, retrying = false): string {
	if (retrying) return '正在重试…'
  if (task.control_pending) return '已取消 · 云端收尾中'
  if (task.scrape_status === 'completed_unverified' && task.job_status !== 'cancelled' && task.job_status !== 'completed') return task.job_status === 'failed' ? '文件核对失败' : '核对已下载文件'
  if (isDownloadSubmissionPending(task)) return task.job_status === 'failed' ? '提交结果待核对' : '核对原任务'
  if (!['failed', 'cancelled', 'paused', 'completed', 'waiting_user_action'].includes(task.job_status)) {
    const acquisitionLabel = acquisitionStateLabel(task.acquisition_state)
    if (acquisitionLabel) return acquisitionLabel
  }
	if (task.job_status === 'running' && normalizedProviderStatus(task.provider_status) === 'unavailable') return '等待下载器恢复'
	if (task.job_status === 'running' && normalizedProviderStatus(task.provider_status) === 'stalleddl') return '等待连接/暂无速度'
	const phases: Record<string, string> = { submitting: '提交下载器', metadata: '获取 metadata', classifying: '轻量刮削', waiting_user_action: '准备自动归入未识别', categorized: '已指派分类', downloading: '正式下载', verifying: '完成后复核' }
	if (task.job_status === 'running' && phases[task.phase]) return phases[task.phase]
  const labels: Record<string, string> = { queued: '排队中', running: '下载中', waiting_user_action: '准备自动处理', retry_wait: '等待重试', paused: '已暂停', completed: '已完成', failed: '失败', cancelled: '已取消' }
  return labels[task.job_status] ?? (task.job_status || '未知')
}

export function downloadStatusClass(task: DownloadTaskSummary, retrying = false): string {
  if (retrying) return 'status-chip status-chip--planned'
  if (task.acquisition_state === 'cancelled_stop_pending') return 'status-chip status-chip--error'
  if (task.job_status === 'failed' || task.job_status === 'cancelled') return isDownloadSubmissionPending(task) ? 'status-chip status-chip--warning' : 'status-chip status-chip--error'
  if (task.job_status === 'paused' || task.job_status === 'waiting_user_action') return 'status-chip status-chip--warning'
  if (task.job_status === 'completed') return 'status-chip status-chip--ready'
  if (['waiting_capacity', 'reconnecting', 'clock_unknown'].includes(task.acquisition_state ?? '')) return 'status-chip status-chip--warning'
  if (task.acquisition_state === 'cloud_ready') return 'status-chip status-chip--ready'
  if (task.acquisition_state === 'active' || task.acquisition_state === 'observing') return 'status-chip status-chip--planned'
  if (isDownloadSubmissionPending(task)) return 'status-chip status-chip--warning'
  if (task.job_status === 'completed') return 'status-chip status-chip--ready'
  if (task.job_status === 'failed' || task.job_status === 'cancelled') return 'status-chip status-chip--error'
  if (task.job_status === 'running' && normalizedProviderStatus(task.provider_status) === 'unavailable') return 'status-chip status-chip--warning'
  if (task.job_status === 'retry_wait' || task.job_status === 'waiting_user_action' || task.job_status === 'paused') return 'status-chip status-chip--warning'
  return 'status-chip status-chip--planned'
}

export function downloadErrorMessage(task: DownloadTaskSummary, retrying = false): string {
  if (retrying) return ''
  if (task.last_error_code === 'offline_deadline_exceeded') return task.control_pending ? '云端离线超过 30 分钟，已取消本次获取；正在结束原云端任务，已完成文件保留。' : '云端离线超过 30 分钟，已取消本次获取，已完成文件保留。'
  if (task.control_pending) return '后续入库已取消，正在核对并结束原云端任务。'
  if (task.job_status === 'cancelled') return ''
  // Preclassification deliberately keeps a safe TMDB warning visible while
  // the provider continues downloading. Only a stale terminal downloader
  // failure should be suppressed by authoritative active telemetry.
  if (task.scrape_status === 'fallback_unrecognized' && task.last_error_code.startsWith('tmdb_')) return task.last_error_message
  if (task.job_status === 'failed' || task.job_status === 'paused' || task.job_status === 'waiting_user_action') return task.last_error_message
  if (task.acquisition_state && ['active', 'observing', 'waiting_capacity', 'cloud_ready'].includes(task.acquisition_state) && (!task.last_error_code || task.last_error_code === 'download_observation_pending' || task.last_error_code === 'downloader_capacity')) return ''
  if ((task.job_status === 'queued' || task.job_status === 'running') && isActiveProviderStatus(task.provider_status)) return ''
  return task.last_error_message
}

function normalizedProviderStatus(value: string): string {
  return (value || '').trim().toLowerCase().replace(/[\s_-]+/g, '')
}

function isActiveProviderStatus(value: string): boolean {
  return activeProviderStatuses.has(normalizedProviderStatus(value))
}

export function downloadProviderStatusLabel(value: string): string {
  const labels: Record<string, string> = {
    allocating: '正在分配空间',
    checkingdl: '正在校验',
    checkingresumedata: '正在校验恢复数据',
    downloading: '正在下载',
    forceddl: '强制下载',
    metadl: '正在获取元数据',
    queueddl: '等待下载队列',
    stalleddl: '等待连接/暂无速度',
    unavailable: '等待下载器恢复',
  }
  const normalized = normalizedProviderStatus(value)
  return labels[normalized] ?? (value || '尚未采样')
}

function retryErrorFingerprint(task: DownloadTaskSummary): string {
  return `${task.last_error_code}\u0000${task.last_error_message}`
}

export function beginDownloadRetry(task: DownloadTaskSummary): DownloadRetryPresentationState {
  return { errorFingerprint: retryErrorFingerprint(task), taskUpdatedAt: task.updated_at, observedActive: false }
}

export function reconcileDownloadRetries(states: DownloadRetryPresentations, tasks: DownloadTaskSummary[]): DownloadRetryPresentations {
  const byID = new Map(tasks.map(task => [task.id, task]))
  const next: DownloadRetryPresentations = {}
  for (const [taskID, previous] of Object.entries(states)) {
    const task = byID.get(taskID)
    if (!task) continue
    const ordering = compareTaskUpdate(task.updated_at, previous.taskUpdatedAt)
    if (ordering < 0) {
      next[taskID] = previous
      continue
    }
    if (task.lifecycle_scope === 'history' || ['completed', 'cancelled'].includes(task.job_status)) continue
    if (task.job_status !== 'failed') {
      next[taskID] = { ...previous, taskUpdatedAt: ordering > 0 ? task.updated_at : previous.taskUpdatedAt, observedActive: true }
      continue
    }
    const newFailure = ordering > 0 || retryErrorFingerprint(task) !== previous.errorFingerprint
    if (!newFailure) next[taskID] = previous
  }
  return next
}

function compareTaskUpdate(value: string, baseline: string): number {
  const current = Date.parse(value)
  const previous = Date.parse(baseline)
  if (Number.isFinite(current) && Number.isFinite(previous)) return Math.sign(current - previous)
  return value === baseline ? 0 : value > baseline ? 1 : -1
}

export function torrentToBase64(bytes: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let offset = 0; offset < bytes.length; offset += chunk) binary += String.fromCharCode(...bytes.subarray(offset, offset + chunk))
  return btoa(binary)
}

export function summarizeDownloaderTasks(tasks: DownloadTaskSummary[], downloaderID: string): DownloaderTaskStats {
  const own = tasks.filter(task => task.downloader_id === downloaderID)
  const active = own.filter(task => {
    if (task.acquisition_state === 'cancelled_stop_pending') return true
    if (isDownloadHistoryTask(task) || ['failed', 'cancelled', 'paused', 'waiting_user_action'].includes(task.job_status)) return false
    return task.acquisition_state ? ['active', 'observing', 'reconnecting', 'cloud_ready', 'clock_unknown'].includes(task.acquisition_state) : task.job_status === 'running'
  })
  const knownProgress = active.flatMap(task => task.progress === null ? [] : [task.progress])
  const knownDownload = active.flatMap(task => task.download_speed === null ? [] : [task.download_speed])
  const knownUpload = active.flatMap(task => task.upload_speed === null ? [] : [task.upload_speed])
  return {
    active: active.length,
    total: own.length,
    downloadSpeed: knownDownload.length ? knownDownload.reduce((sum, value) => sum + value, 0) : null,
    uploadSpeed: knownUpload.length ? knownUpload.reduce((sum, value) => sum + value, 0) : null,
    averageProgress: knownProgress.length ? knownProgress.reduce((sum, value) => sum + value, 0) / knownProgress.length : null,
  }
}

export function isDownloadHistoryTask(task: DownloadTaskSummary): boolean {
  return task.lifecycle_scope === 'history'
}

export function canCancelDownloadPipeline(task: DownloadTaskSummary): boolean {
  return task.lifecycle_scope === 'active' && task.job_status !== 'cancelled' && !task.control_pending && task.phase !== 'cancelled'
}

export function isDownloadSubmissionPending(task: DownloadTaskSummary): boolean {
  return task.control_pending === true || ['downloader_submission_unconfirmed', 'downloader_submit_unknown', 'downloader_selection_result_unknown', 'downloader_share_result_unknown'].includes(task.last_error_code)
}

export function formatSampleTime(value: string | null): string {
  if (!value) return '尚未检查'
  const time = new Date(value)
  return Number.isNaN(time.getTime()) ? '时间未知' : time.toLocaleString('zh-CN', { hour12: false })
}

// These are persisted Server facts, never a timer derived from row creation or progress.
export function acquisitionStateLabel(state?: DownloadTaskSummary['acquisition_state']): string {
  return state ? ({ waiting_capacity: '等待下载器空位', active: '正在获取资源', observing: '正在获取 · 定期核对', reconnecting: '等待下载器恢复', cloud_ready: '云端所需文件已就绪', cancelled_stop_pending: '已取消 · 云端收尾中', clock_unknown: '等待核实云端起始时间' })[state] ?? '' : ''
}

export function offlineTimingLabel(task: Pick<DownloadTaskSummary, 'offline_started_at' | 'offline_deadline_at' | 'offline_ready_at' | 'offline_start_provenance'>): string {
  if (task.offline_ready_at && Number.isFinite(Date.parse(task.offline_ready_at))) return `云端就绪：${formatSampleTime(task.offline_ready_at)}；后续传输与入库继续处理。`
  if (!task.offline_started_at || !task.offline_deadline_at || !Number.isFinite(Date.parse(task.offline_started_at)) || !Number.isFinite(Date.parse(task.offline_deadline_at))) return ''
  const provenance = { dispatch: '云端提交', accepted: '云端受理', provider_added: '云端添加' }
  return `${task.offline_start_provenance ? provenance[task.offline_start_provenance] ?? '云端起始' : '云端起始'}：${formatSampleTime(task.offline_started_at)} · 离线截止：${formatSampleTime(task.offline_deadline_at)}`
}
