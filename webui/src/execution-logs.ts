import { api, APIError } from '@/api/client'

export type ExecutionLogScope = { kind: 'job'; jobId: string } | { kind: 'follow'; followId: string; runId: string }
export interface ExecutionLogEntry {
  event_id: string; timestamp: string; level: string; message: string
  operation?: string; operation_label?: string; stage?: string; outcome?: string
  fields: Record<string, unknown>
}
export interface ExecutionSummary {
  title: string; status: string; trigger?: string; searched_names_count?: number; candidates?: number
  accepted_submissions?: number; filter_summary?: Record<string, number>; error_code?: string; error_message?: string
  started_at?: string; finished_at?: string
}
export interface ExecutionLogPage {
  list: ExecutionLogEntry[]; next_cursor?: string; snapshot_to: string; attempt_number?: number; partial: boolean
  availability: 'available' | 'unavailable' | 'restricted' | 'degraded'; summary?: ExecutionSummary
}
export interface ExecutionLogFilters { attempt?: number; level?: string; keyword?: string; limit?: number; cursor?: string; snapshot_to?: string }

export function executionScopeKey(scope: ExecutionLogScope) {
  return scope.kind === 'job' ? `job:${scope.jobId}` : `follow:${scope.followId}:${scope.runId}`
}
export function executionLogPath(scope: ExecutionLogScope, filters: ExecutionLogFilters = {}) {
  const base = scope.kind === 'job'
    ? `/api/v1/jobs/${encodeURIComponent(scope.jobId)}/execution-logs`
    : `/api/v1/follows/${encodeURIComponent(scope.followId)}/runs/${encodeURIComponent(scope.runId)}/execution-logs`
  const params = new URLSearchParams({ limit: String(Math.min(200, Math.max(1, Math.floor(filters.limit ?? 100)))) })
  if (filters.attempt && Number.isInteger(filters.attempt) && filters.attempt > 0) params.set('attempt', String(filters.attempt))
  if (['debug', 'info', 'warn', 'error'].includes(filters.level ?? '')) params.set('level', filters.level!)
  if (filters.keyword?.trim()) params.set('keyword', filters.keyword.trim().slice(0, 128))
  if (filters.cursor) params.set('cursor', filters.cursor)
  if (filters.snapshot_to) params.set('snapshot_to', filters.snapshot_to)
  return `${base}?${params}`
}
export const getExecutionLogs = (scope: ExecutionLogScope, filters: ExecutionLogFilters, signal?: AbortSignal) => api<ExecutionLogPage>(executionLogPath(scope, filters), { signal })

const statuses: Record<string, string> = {
  queued: '等待执行', running: '本轮正在执行', no_match: '本轮没有可提交资源', submitted: '已提交下载，等待后续处理',
  completed: '本轮执行结束', failed: '本轮执行失败', partial: '部分操作未完成', cancelled: '本轮已取消', stale: '本轮配置已失效',
  paused: '执行已暂停', retry_wait: '等待重试', waiting_user_action: '等待用户处理',
  enabled: '已启用', disabled: '已停用', online: '连接正常', offline: '连接不可用', available: '有可用版本', idle: '当前空闲',
  checking: '正在检查', preparing: '正在准备', ready: '已准备好', waiting_for_exit: '等待服务退出', rolled_back: '已回滚',
  catalog_ready: '基础目录已发布', superseded: '已被新版本替代', retained: '按策略保留', seeding: '正在做种', kept: '保持原样',
}
export const executionStatusLabel = (value: string) => statuses[value] ?? '其他执行状态'
export const executionTriggerLabel = (value: string) => ({ scheduled: '计划触发', manual: '手动触发', created: '新建订阅触发', event: '事件触发', retry: '重试触发' } as Record<string, string>)[value] ?? '其他触发方式'
export const logLevelLabel = (value: string) => ({ debug: '调试', info: '信息', warn: '警告', error: '错误' } as Record<string, string>)[value] ?? '其他级别'

const stages: Record<string, string> = {
  planning: '制定执行计划', coverage: '核对在库与缺集', search: '站点搜索', searching: '站点搜索', site_search: '站点搜索',
  filtering: '筛选资源', filter: '筛选资源', selection: '选择资源', routing: '选择下载路线', route: '选择下载路线',
  submission: '提交下载', submit: '提交下载', download: '下载', waiting: '等待后续处理', reconciliation: '核对执行结果',
  enumeration: '枚举媒体', enumerating: '从数据源枚举', enumeration_complete: '数据源枚举完成', processing: '本地并发处理',
  staging: '分批落库', publishing: '发布基础目录', scope_reconciliation: '增量范围对账', scope_fallback: '改用全量对账',
  catalog_ready: '基础目录可用', recognition: '媒体识别', recognition_queued: '等待后台识别', recognition_running: '后台识别',
  recognition_enqueue_failed: '识别待恢复', recognition_artifact_enqueue_failed: '元数据产物待恢复',
  artifact_scheduling: '安排 STRM/产物任务', artifact_running: '正在生成 STRM/产物', artifact_refresh_queued: '准备再次生成产物',
  artifact_completed: 'STRM/产物生成完成', artifact_failed: 'STRM/产物生成失败', organize: '媒体整理', transfer: '文件传输',
  move: '移动文件', copy: '复制文件', rename: '重命名文件', conflict: '处理文件冲突', publish: '发布目录',
  artifacts: '生成元数据产物', refresh: '发起刷新', cleanup: '清理', start: '开始执行', finish: '执行结束',
  completed: '执行结束', failed: '执行失败', superseded: '已被新执行替代', schedule: '计划调度',
  probe: '检测连接', authentication: '身份验证', update: '软件更新', plugin: '插件执行',
  trigger: '触发计划', execute: '执行计划动作', monitor: '检查运行状态', validate: '复核执行条件', persist: '保存执行结果',
  listen: '监听数据源事件', connect: '连接数据源', poll: '读取生活事件', dispatch: '分发生活事件', install: '安装组件',
  runtime: '管理组件运行', uninstall: '卸载组件', restore: '恢复组件', shutdown: '停止组件', check: '检查更新', configure: '保存配置',
  restart: '等待服务重启', reconcile: '核对实际结果', setup: '首次设置', authorize: '核对账号权限', session: '登录会话',
  artifact_write: '写入媒体产物', reserve: '保留季集占用', materialize: '接收媒体文件', upload: '上传媒体文件',
  enqueue: '任务入队', tracking: '跟踪下载', verify: '核验结果与条件', metadata: '读取文件清单', classification: '识别与分类',
  prepare: '检查执行条件', limit: '等待站点请求额度', query: '查询站点', identity: '核对作品身份',
  source: '解析下载来源', share_check: '校验分享内容', image_fetch: '获取媒体图片',
}
export const logStageLabel = (value: string) => stages[value] ?? '其他处理步骤'
const outcomes: Record<string, string> = {
  planned: '已计划', started: '开始执行', accepted: '已受理', confirmed: '已确认', skipped: '已跳过', waiting: '等待确认',
  failed: '失败', cancelled: '已取消', finished: '执行结束', succeeded: '成功', partial: '部分完成', retry: '等待重试',
  created: '任务已创建', reused: '已复用', rejected: '提交被拒绝', uncertain: '结果待核对',
}
export const logOutcomeLabel = (value: string) => outcomes[value] ?? '其他执行状态'

const reasons: Record<string, string> = {
  identity: '作品不匹配', identity_mismatch: '作品不匹配', media_mismatch: '作品不匹配', season_mismatch: '季不匹配', season: '季不匹配',
  episode_unproven: '无法确认季集范围', episode: '不覆盖缺集', no_missing_coverage: '不覆盖缺集', already_present: '已在媒体库中',
  already_in_library: '已在媒体库中', active_download: '已有下载任务', already_downloading: '已有下载任务', duplicate: '重复资源',
  include_keyword: '缺少要求的关键词', exclude_keyword: '命中排除关键词', resolution: '分辨率不符合', video_codec: '视频编码不符合',
  quality: '片源质量不符合', release_group: '发布组不符合', excluded_release_group: '命中排除发布组', seeders: '做种数不符合',
  size: '文件大小不符合或未知', age: '发布时间不符合或未知', submit_failed: '提交下载失败', search_failed: '站点搜索失败',
  site_failed: '站点搜索失败', site_unavailable: '站点不可用', no_route: '没有可用下载路线', route_unavailable: '没有可用下载路线',
  claim_conflict: '缺集已有任务占用', resource_claimed: '资源已有任务占用', no_episode_overlap: '不覆盖缺集', title_mismatch: '标题不匹配',
  result_expired: '搜索结果已过期', permission_denied: '当前权限不允许', disabled: '已停用', cancelled: '执行被取消',
  claim_unavailable: '搜索结果凭据不可用', identity_bind_failed: '无法绑定作品身份', site_error: '站点搜索失败',
  reservation_failed: '无法保留季集占用', content_reused: '现有内容已核验并复用', unmanaged: '不是系统受管文件',
  unmanaged_collision: '同名文件不受系统管理', snapshot_incomplete: '目录快照不完整', source_missing: '来源文件不存在',
  configuration_changed: '执行配置已发生变化', dependency_not_ready: '依赖资源尚未准备好', misfire: '错过计划执行时间', overlap: '已有执行中的任务',
  provider_missing: '下载器中已无此任务', cleanup_disabled: '未启用自动清理', threshold_not_reached: '尚未达到清理条件',
  login_rate_limited: '登录保护尚未结束', invalid_credentials: '账号、密码或状态不符合要求',
  cooldown: '账号处于冷却期', rate_limited: '触发限流', timeout: '请求超时', network: '网络连接失败',
}
export const logReasonLabel = (value: string) => reasons[value] ?? '其他原因'
export function filterReasonRows(summary?: Record<string, number>) {
  return Object.entries(summary ?? {}).filter(([, count]) => Number.isFinite(count) && count > 0)
    .map(([code, count]) => ({ code, label: logReasonLabel(code), count }))
}

const operations: Record<string, string> = {
  server_lifecycle: '服务生命周期', http_request: 'HTTP 请求', system_health: '系统健康', authentication: '认证与初始化', dashboard: '仪表盘',
  access_control: '用户与权限', audit_log: '审计日志', storage_management: '存储管理', directory_browsing: '目录浏览',
  media_rule_management: '分类规则管理', media_library_management: '媒体库管理', connection_management: '连接管理', downloader_management: '下载器管理',
  system_settings: '系统设置', metadata_configuration: '元数据设置', runtime_logging: '运行日志', connection_probe: '连接检测',
  provider_life_event: '115 生活事件', library_initial_scan: '媒体库首次扫描', library_incremental_scan: '媒体库增量扫描', library_full_scan: '媒体库全量扫描',
  library_event_scan: '媒体库事件增量', media_recognition: '媒体识别', metadata_snapshot: '元数据快照', media_artifact: '媒体产物',
  strm_generation: 'STRM 生成', incremental_strm_generation: '增量 STRM 生成', full_strm_generation: '全量 STRM 生成', signed_proxy: '302 代理',
  emby_proxy_gateway: 'Emby 302 网关', pan115_sidecar_upload: '115 旁挂上传', artifact_cleanup: '产物清理', download_task: '下载任务',
  download_classification: '下载预分类', pan115_offline_download: '115 离线下载', pan115_share_ingest: '115 分享摄取',
  pan115_cloud_transfer: '115 云端整理', media_transfer: '媒体整理', seeding_management: '做种管理', task_queue: '系统任务队列',
  follow_search: '自动追更', automatic_follow: '自动追更', media_server_refresh: '媒体服务器刷新', site_search: '站点搜索',
  media_reorganization: '修正识别与重新整理', download_staging_cleanup: '下载暂存清理', pan115_recycle_cleanup: '115 回收站清理',
  pan115_multi_device_playback: '115 多设备播放', pan115_playback_cleanup: '115 临时副本清理', plugin_repository: '插件仓库', plugin_runtime: '插件运行时',
  unified_schedule: '计划任务', plugin_execution: '插件执行', server_update: '软件更新',
}
export const logOperationLabel = (value: string) => operations[value] ?? '其他业务操作'

const fieldLabels: Record<string, string> = {
  stage: '当前步骤', phase: '当前步骤', action: '执行动作', outcome: '执行结果', reason: '原因', reason_code: '原因', status: '执行状态',
  title: '作品', media_title: '作品', media_display_name: '媒体', resource_name: '资源', resource_title: '资源', file_name: '文件名',
  work_title: '作品', relative_path: '相对位置',
  old_name: '原文件名', new_name: '新文件名', source_name: '来源', target_name: '目标', source_relative_path: '来源相对位置',
  target_relative_path: '目标相对位置', source_display_path: '来源相对位置', target_display_path: '目标相对位置',
  site_name: '站点', query_name: '查询名称', query: '查询名称', downloader_name: '下载器', library_name: '媒体库', connection_name: '连接',
  storage_name: '存储', size_bytes: '实际大小', mode: '操作方式', transfer_mode: '整理方式', conflict_policy: '冲突策略',
  raw_results: '累计返回条目（含重复）', result_count: '返回条目数', raw_count: '累计返回条目（含重复）', deduplicated_count: '去重后数量', candidates: '筛选后候选',
  selected: '已选择资源', accepted_submissions: '已接受提交', created_count: '新建数量', reused_count: '复用数量', failed_count: '失败数量',
  searched_sites: '已查询站点', site_count: '站点数量', searched_names_count: '查询名称数', total: '总数', count: '数量',
  missing: '仍缺集数', present: '已入库集数', active: '已有下载占用', seasons: '季', episodes: '集', episode_range: '季集范围',
  filter_summary: '筛选原因汇总', reason_counts: '原因汇总', samples: '资源名称样本', sample_names: '名称样本', omitted: '省略数量',
  truncated: '记录已截断', omitted_count: '省略数量', duration_ms: '耗时（毫秒）', sampled: '已展示样本', remaining: '其余数量',
  media_type: '媒体类型', scan_kind: '扫描类型', worker_count: '本地处理线程', batch: '批次', batch_items: '本批数量', enumerated: '已枚举',
  processed: '已处理', persisted: '已落库', discovered: '发现视频', deduplicated: '去重/冲突', added: '新增', updated: '更新', removed: '删除',
  recognition_total: '待识别作品', matched: '识别成功', unrecognized: '未识别', recognition_failed: '识别失败', cache_hits: '缓存命中',
  provider_call_count: '数据源请求次数', provider_call_ms: '数据源请求耗时（毫秒）', provider_wait_ms: '限流/排队耗时（毫秒）',
  provider_configured_concurrency: '配置的数据源并发', provider_concurrency: '实际数据源并发', provider_configured_rate_per_second: '配置的每秒请求上限',
  provider_rate_per_second: '实际每秒请求上限', complete_snapshot: '完整快照', persistence_stage: '失败步骤', database_error_class: '数据库错误分类',
  error_code: '错误分类', attempt_number: '执行尝试', generation: '执行代际', event_id: '事件标识', event_code: '事件类型', event_version: '事件版本',
  job_id: '队列任务 ID', task_id: '业务任务 ID', related_job_id: '关联队列任务 ID', related_task_id: '关联业务任务 ID',
  follow_id: '订阅 ID', follow_run_id: '订阅运行 ID', library_id: '媒体库 ID', scan_run_id: '扫描运行 ID',
  connection_id: '连接 ID', downloader_id: '下载器 ID', storage_id: '存储 ID', site_id: '站点 ID', request_id: '请求 ID',
  source_library_id: '来源媒体库 ID', target_library_id: '目标媒体库 ID', source_connection_id: '来源连接 ID', target_connection_id: '目标连接 ID',
  source_storage_id: '来源存储 ID', target_storage_id: '目标存储 ID', user_id: '用户 ID',
  filtered_count: '筛选后数量', selected_count: '选中数量', accepted_count: '已接受数量', rejected_count: '已拒绝数量',
  uncertain_count: '结果待核对数量', skipped_count: '跳过数量', success_count: '成功数量', total_count: '总数',
  completed_count: '已完成数量', processed_count: '已处理数量', sample_count: '展示样本数', searched_sites_count: '实际查询站点数',
  planned_sites_count: '计划查询站点数', request_count: '实际请求数', missing_count: '仍缺集数', present_count: '已入库集数',
  active_count: '已有下载占用', matched_count: '识别成功数量', unrecognized_count: '未识别数量', cache_hit_count: '缓存命中数量',
  enumerated_count: '已枚举数量', batch_count: '批次数量', queued_count: '已入队数量', published_count: '已发布数量',
  artifact_type: '产物类型', updated_count: '更新数量', generated_count: '生成数量', deleted_count: '删除数量', uploaded_count: '上传数量', conflict_count: '冲突数量',
  moved_count: '已移动数量', copied_count: '已复制数量', renamed_count: '已重命名数量', retained_count: '保留数量',
  bytes_processed: '已处理字节数', identity_kept: '身份核验后数量', rejected: '已筛除资源', chosen: '最终选中资源',
  created: '新建任务', reused: '复用任务', failed_sites: '失败站点数', alias_requests: '别名查询请求数', season: '季', episode_count: '集数',
  conflict_decision: '冲突处理', trigger: '触发方式', source_type: '来源类型', outcome_code: '结果分类', job_type: '任务类型',
}
export const logFieldLabel = (key: string) => fieldLabels[key] ?? '其他信息'
const technicalFields = new Set(['event_id', 'event_code', 'event_version', 'job_id', 'task_id', 'related_job_id', 'related_task_id', 'follow_id', 'follow_run_id', 'library_id', 'scan_run_id', 'connection_id', 'downloader_id', 'storage_id', 'site_id', 'request_id', 'attempt_number', 'generation', 'source_library_id', 'target_library_id', 'source_connection_id', 'target_connection_id', 'source_storage_id', 'target_storage_id', 'user_id', 'outcome_code', 'job_type', 'error_code'])
const actions: Record<string, string> = {
  recognition_matched: '识别成功', recognition_unrecognized: '未识别', recognition_queued: '识别已入队', discovered: '发现媒体', filtered: '已过滤',
  deduplicated: '已去重', unchanged: '媒体未变化', added: '新增目录条目', updated: '更新目录条目', removed: '删除失效条目',
  staging_started: '开始分批落库', staging_completed: '分批落库完成', catalog_publish_started: '开始发布基础目录', collections_reconciled: '识别与自动合集已入库',
  artifact_queued: 'STRM/产物已入队', library_artwork_queued: '分类封面已入队', move: '移动', copy: '复制', rename: '重命名',
  written: '已写入', reused: '已复用', created: '新建任务', partial: '部分完成', nfo: 'NFO 元数据', strm: 'STRM 播放入口',
  poster: '海报图片', fanart: '背景图片', thumb: '缩略图', image: '图片', source_poster: '来源海报', source_fanart: '来源背景', subtitle: '字幕',
  login: '网页登录', player_login: 'Player 登录', clear_recycle_bin: '永久清空回收站', beta: 'Beta 通道', stable: '稳定通道',
  symlink: '创建软链接', hardlink: '创建硬链接', skip: '跳过', overwrite: '覆盖', ask: '等待用户决定', recycle: '移入回收站',
  browser_login: '网页登录', 'auth.login': '网页登录', remove_task_and_data: '移除下载器任务并请求清理数据', remove_task_keep_data: '移除下载器任务并保留数据',
  prepare_install: '准备安装包', install: '安装插件', update: '更新插件', enable: '启用插件', disable: '停用插件', rollback: '回滚插件', uninstall: '卸载插件',
  media_library_scan: '全量核对媒体库', media_library_structure_diagnose: '诊断媒体目录', media_library_structure_repair: '修复媒体目录',
  strm_reconcile: '核对 STRM', follow_search: '执行追更', cookiecloud_sync: '同步 CookieCloud', pan115_recycle_cleanup: '清空 115 回收站', empty_directory: '清理空目录',
}
const persistenceStages: Record<string, string> = {
  configuration_revalidate: '配置复核', load_existing_entries: '读取已有目录', persist_source_assets: '提交伴随文件', persist_recognition: '提交识别结果',
  persist_entries: '提交媒体目录', prune_stale_entries: '清理已失效条目', reconcile_tmdb_collections: '更新自动合集', advance_library_generation: '发布目录代际',
  persist_scan_run: '保存扫描状态', record_media_change: '发布媒体变更',
}
export function logFieldText(key: string, value: unknown): string {
  if (value === null || value === undefined) return '未知'
  if (typeof value === 'boolean') return value ? '是' : '否'
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) return '未知'
    if (key === 'size_bytes') { const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']; let n = value, unit = 0; while (n >= 1024 && unit < units.length - 1) { n /= 1024; unit++ } return `${Number(n.toFixed(2))} ${units[unit]}` }
    return String(value)
  }
  if (key === 'filter_summary' || key === 'reason_counts') return filterReasonRows(typeof value === 'object' && !Array.isArray(value) ? value as Record<string, number> : undefined).map(row => `${row.label} ${row.count}`).join('；') || '没有可用汇总'
  if (Array.isArray(value)) return value.slice(0, 20).map(item => typeof item === 'string' || typeof item === 'number' ? String(item) : '其他条目').join('、') + (value.length > 20 ? '（其余已省略）' : '')
  if (typeof value !== 'string') return '其他结构化信息'
  if (key === 'phase' || key === 'stage') return logStageLabel(value)
  if (key === 'outcome') return logOutcomeLabel(value)
  if (key === 'status') return executionStatusLabel(value)
  if (key === 'reason' || key === 'reason_code') return logReasonLabel(value)
  if (['action', 'mode', 'transfer_mode', 'conflict_policy', 'conflict_decision', 'artifact_type'].includes(key)) return actions[value] ?? '其他操作'
  if (key === 'trigger') return executionTriggerLabel(value)
  if (key === 'source_type') return ({ pt: 'PT 站点', bt: '公开 BT', bt_resource: '插件资源站', cloud_share: '网盘分享站', '115_share': '115 分享', local: '本地文件', remote: '远程节点', pan115: '115 网盘' } as Record<string, string>)[value] ?? '其他来源'
  if (key === 'media_type') return ({ movie: '电影', tv: '剧集', unknown: '待识别' } as Record<string, string>)[value] ?? '其他类型'
  if (key === 'scan_kind') return ({ initial: '首次全量', catch_up: '监听交接对账', event: '文件事件', incremental: '增量扫描', full: '全量扫描', manual: '手工扫描' } as Record<string, string>)[value] ?? '其他扫描'
  if (key === 'persistence_stage') return persistenceStages[value] ?? '其他提交步骤'
  if (key === 'database_error_class') return ({ configuration_changed: '扫描期间配置发生变化', foreign_key: '关联数据不完整', unique: '发现重复数据', constraint: '数据不符合约束', busy: '数据库正忙，已完成有限重试', unknown: '未知数据库错误' } as Record<string, string>)[value] ?? '未知数据库错误'
  if (key === 'error_code') return ({ media_library_scan_failed: '扫描结果提交失败', media_library_recognition_enqueue_failed: '后台识别等待恢复', artifact_refresh_schedule_failed: '元数据产物等待恢复', follow_submit_failed: '下载提交失败', follow_submit_partial: '部分下载提交失败' } as Record<string, string>)[value] ?? value
  return value
}
export function logFactRows(fields: Record<string, unknown>, technical = false) {
  return Object.entries(fields).filter(([key]) => key in fieldLabels && technicalFields.has(key) === technical)
    .map(([key, value]) => ({ key, label: logFieldLabel(key), text: logFieldText(key, value) }))
}
export function executionLogError(reason: unknown) {
  if (reason instanceof APIError) {
    if (reason.status === 401) return '登录已失效，请重新登录后查看执行日志。'
    if (reason.status === 403) return '当前账号无权查看这次执行的详细记录。'
    if (reason.status === 404) return '这次执行不存在或已不可查看。'
    if ([400, 409, 422].includes(reason.status)) return '筛选或分页快照已失效，请刷新本轮过程。'
  }
  return '执行日志读取失败，请稍后重试。'
}
export function deduplicateLogEntries<T extends { event_id?: string; fields?: Record<string, unknown> }>(entries: T[]): T[] {
  const seen = new Set<string>()
  return entries.filter(entry => { const id = entry.event_id || (typeof entry.fields?.event_id === 'string' ? entry.fields.event_id : ''); if (!id) return true; if (seen.has(id)) return false; seen.add(id); return true })
}
