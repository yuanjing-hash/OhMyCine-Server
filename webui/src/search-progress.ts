import type { TorrentSearchProgress, TorrentSearchSiteDetail } from '@/sites'

export type SearchOutcome = 'idle' | 'running' | 'complete' | 'partial' | 'cancelled' | 'failed' | 'retained'

export function searchPhaseLabel(phase: string): string {
  return ({
    preparing_media: '正在准备作品信息', queued: '等待搜索名额', preparing_site: '正在准备站点',
    restoring_session: '正在恢复登录会话', checking_session: '正在检查登录状态',
    waiting_rate_limit: '等待站点限速', querying: '正在请求站点', processing_results: '正在整理结果',
    finished: '本次查询已结束',
  } as Record<string, string>)[phase] ?? '正在搜索'
}

export function searchIssueLabel(code?: string): string {
  return ({
    request_timeout: '站点响应超时', site_request_timeout: '站点响应超时',
    site_budget_exceeded: '站点搜索时间已用完', search_budget_exceeded: '本轮搜索时间已用完',
    rate_limit_wait_exceeded: '等待站点限速超时', site_rate_limited: '站点暂时限制请求',
    resource_rate_limited: '站点暂时限制请求', resource_auth_required: '需要重新登录站点',
    resource_verification_required: '站点需要网页验证', resource_result_invalid: '站点返回的数据无法解析',
    resource_browser_verification_required: '站点需要网页验证',
    resource_browser_login_pending: '管理员正在完成站点登录，请稍后重试',
    resource_browser_restore_timeout: '恢复登录会话超时，尚未确认登录失效',
    result_limit: '结果已达到本次展示上限', cancelled: '搜索已取消',
    stream_interrupted: '搜索连接中断', invalid_response: '搜索进度或结果格式异常',
    site_unavailable: '站点暂时无法访问', retry_failed_previous_results: '重新搜索失败，保留上次结果',
  } as Record<string, string>)[code ?? ''] ?? '部分查询尚未完成'
}

export function searchDuration(milliseconds: number): string {
  const seconds = Math.max(0, Math.floor(milliseconds / 1000))
  return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`
}

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value)
const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0

export function parseSearchSiteDetail(value: unknown): TorrentSearchSiteDetail | undefined {
  if (!isRecord(value) || !isCount(value.site_id) || value.site_id === 0 || typeof value.site_name !== 'string'
    || typeof value.phase !== 'string' || !isCount(value.result_count) || !isCount(value.elapsed_ms)) return undefined
  const optionalCounts = ['query_index', 'queries_completed', 'queries_failed', 'queries_total', 'revision'] as const
  if (optionalCounts.some(key => value[key] !== undefined && !isCount(value[key]))) return undefined
  if (value.query_name !== undefined && typeof value.query_name !== 'string') return undefined
  return {
    site_id: value.site_id, site_name: value.site_name, phase: value.phase,
    result_count: value.result_count, elapsed_ms: value.elapsed_ms,
    query_name: value.query_name as string | undefined,
    query_index: value.query_index as number | undefined,
    queries_completed: value.queries_completed as number | undefined,
    queries_failed: value.queries_failed as number | undefined,
    queries_total: value.queries_total as number | undefined,
    revision: value.revision as number | undefined,
  }
}

export function parseSearchProgress(value: unknown, siteCount: number): TorrentSearchProgress | undefined {
  if (!isRecord(value)) return undefined
  const counts = ['total', 'pending', 'running', 'completed', 'succeeded', 'failed', 'result_count'] as const
  if (counts.some(key => !isCount(value[key]))) return undefined
  const progress = Object.fromEntries(counts.map(key => [key, value[key]])) as unknown as TorrentSearchProgress
  const preparing = value.phase === 'preparing_media' && counts.every(key => progress[key] === 0) && value.site_id === undefined && value.site_detail === undefined
  if ((!preparing && progress.total !== siteCount) || progress.completed !== progress.succeeded + progress.failed
    || progress.total !== progress.pending + progress.running + progress.completed) return undefined
  if (value.event_sequence !== undefined && !isCount(value.event_sequence)) return undefined
  if (value.site_id !== undefined && !isCount(value.site_id)) return undefined
  if (value.site_name !== undefined && typeof value.site_name !== 'string') return undefined
  if (value.site_status !== undefined && typeof value.site_status !== 'string') return undefined
  if (value.phase !== undefined && typeof value.phase !== 'string') return undefined
  progress.event_sequence = value.event_sequence as number | undefined
  progress.site_id = value.site_id as number | undefined
  progress.site_name = value.site_name as string | undefined
  progress.site_status = value.site_status as string | undefined
  progress.phase = value.phase as string | undefined
  if (typeof value.error_code === 'string') progress.error_code = value.error_code
  if (value.site_detail !== undefined) {
    progress.site_detail = parseSearchSiteDetail(value.site_detail)
    if (!progress.site_detail) return undefined
  }
  return progress
}
