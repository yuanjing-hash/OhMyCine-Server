<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import DirectoryPickerDialog from '@/components/DirectoryPickerDialog.vue'
import MediaReorganizationDialog from '@/components/MediaReorganizationDialog.vue'
import MediaLibrarySettingsFields from '@/components/MediaLibrarySettingsFields.vue'
import { clearDefaultIngestLibrary, draftFromLibrary, emptyMediaLibraryDraft, isActiveLibraryStatus, isMediaLibraryDraftValid, mediaLibraryDraftFingerprint, mediaLibrarySourceDisplayPath, payloadFromDraft, presentLibraryStatus, setDefaultIngestLibrary, supportsSidecarUpload, supportsSTRM, type MediaLibraryDraft } from '@/media-libraries'
import { mediaCatalogDetailEndpoint, mediaCatalogEndpoint, mediaCatalogPageCount, mediaCatalogPageSizes, mediaCatalogVisibleRange, type MediaCatalogMatchFilter, type MediaCatalogPageSize, type MediaCatalogTypeFilter } from '@/media-catalog'
import { useAuthStore } from '@/stores/auth'
import type { ListResponse, MediaCatalogDetail, MediaCatalogItem, MediaCatalogManagedTransfer, MediaClassificationProfileSummary, MediaLibraryDetail, MediaLibraryScanRun, MediaLibraryStructureBulkSelection, MediaLibraryStructureDiagnostics, MediaLibraryStructureIssue, MediaLibraryStructureIssuePage, MediaLibraryStructureIssueSummary, MediaLibraryStructureRepair, MediaLibraryStructureSelection, MediaLibraryStructureSelectionPreview, MediaRecognitionSummary, PageResponse, StorageSummary, TMDBCandidate } from '@/types/api'

type DetailTab = 'status' | 'runs' | 'entries' | 'settings'
type PickerTarget = 'source' | 'strm'
type CatalogMatchView = MediaCatalogMatchFilter | 'review' | 'manual'

const auth = useAuthStore()
const libraries = ref<MediaLibraryDetail[]>([])
const storages = ref<StorageSummary[]>([])
const profiles = ref<MediaClassificationProfileSummary[]>([])
const selectedID = ref<number | null>(null)
const runs = ref<MediaLibraryScanRun[]>([])
const catalog = ref<MediaCatalogItem[]>([])
const catalogTotal = ref(0)
const catalogPage = ref(1)
const catalogPageSize = ref<MediaCatalogPageSize>(50)
const catalogSearch = ref('')
const catalogType = ref<MediaCatalogTypeFilter>('')
const catalogMatch = ref<CatalogMatchView>('')
const catalogLoading = ref(false)
const catalogError = ref('')
const recognitions = ref<MediaRecognitionSummary[]>([])
const recognitionLoading = ref(false)
const candidateToken = ref('')
const candidates = ref<TMDBCandidate[]>([])
const candidateLoading = ref(false)
const candidateSearched = ref(false)
const manualRecognitionError = ref('')
const manualRecognitionForm = ref<{ title: string; mediaType: 'movie' | 'tv'; year: string }>({ title: '', mediaType: 'movie', year: '' })
const expandedWorkIDs = ref<string[]>([])
const catalogDetails = ref<Record<string, MediaCatalogDetail>>({})
const detailLoadingIDs = ref<string[]>([])
const reorganizationTarget = ref<{ work: MediaCatalogItem; transfer: MediaCatalogManagedTransfer } | null>(null)
const createOpen = ref(false)
const createDraft = ref<MediaLibraryDraft>(emptyMediaLibraryDraft())
const editDraft = ref<MediaLibraryDraft | null>(null)
const activeTab = ref<DetailTab>('status')
const pickerOpen = ref(false)
const pickerTarget = ref<PickerTarget>('source')
const pickerMode = ref<'create' | 'edit'>('create')
const loading = ref(true)
const refreshing = ref(false)
const saving = ref(false)
const error = ref('')
const notice = ref('')
const editBaseline = ref('')
const editSaveFeedback = ref<{ state: 'idle' | 'saving' | 'success' | 'error'; message: string }>({ state: 'idle', message: '' })
const draggedLibraryID = ref<number | null>(null)
const structureOpen = ref(false)
const structureLoading = ref(false)
const structureDiagnostics = ref<MediaLibraryStructureDiagnostics | null>(null)
const structureIssueFilter = ref('all')
const structureIssues = ref<MediaLibraryStructureIssueSummary[]>([])
const structureIssueTotal = ref(0)
const structureIssuePage = ref(1)
const structureIssuePageSize = ref(50)
const structureIssuesLoading = ref(false)
const structureIssuesError = ref('')
const structureSelectionDraft = ref<Record<string, MediaLibraryStructureSelection>>({})
const structureSelectionCodes = ref<Record<string, string>>({})
const structureBulkConflictAction = ref<'' | 'keep_recommended' | 'skip'>('')
const structureSelectionLoading = ref(false)
const structureSelectionError = ref('')
const structureSelectionPreview = ref<MediaLibraryStructureSelectionPreview | null>(null)
const promptedStructureIDs = new Set<number>()
let pollTimer: number | undefined
let structurePollTimer: number | undefined
let runsRequest: AbortController | null = null
let catalogRequest: AbortController | null = null
const detailRequests = new Map<string, AbortController>()

const selected = computed(() => libraries.value.find(item => item.id === selectedID.value) ?? null)
const selectedStorage = computed(() => storages.value.find(item => item.id === selected.value?.storage_id))
const activeDraft = computed(() => pickerMode.value === 'create' ? createDraft.value : editDraft.value)
const createStorage = computed(() => storages.value.find(item => item.id === createDraft.value.storage_id))
const editStorage = computed(() => storages.value.find(item => item.id === editDraft.value?.storage_id))
const editFingerprint = computed(() => editDraft.value ? mediaLibraryDraftFingerprint(editDraft.value, editStorage.value) : '')
const editDirty = computed(() => Boolean(editDraft.value && editBaseline.value && editFingerprint.value !== editBaseline.value))
const editFormValid = computed(() => Boolean(editDraft.value && isMediaLibraryDraftValid(editDraft.value, editStorage.value)))
const selectedSourceDisplay = computed(() => selected.value ? mediaLibrarySourceDisplayPath(selected.value, storages.value.find(item => item.id === selected.value?.storage_id)) : '')
const shouldPoll = computed(() => activeTab.value !== 'settings' && !editDirty.value && (libraries.value.some(item => isActiveLibraryStatus(item.status) || (item.enabled && (item.status === 'initialization_failed' || item.structure_status === 'pending' || item.structure_status === 'queued' || item.structure_status === 'running' || item.structure_status === 'repairing'))) || runs.value.some(run => run.status === 'running' || run.status === 'catalog_ready')))
const catalogPages = computed(() => mediaCatalogPageCount(catalogTotal.value, catalogPageSize.value))
const catalogRange = computed(() => mediaCatalogVisibleRange(catalogPage.value, catalogPageSize.value, catalogTotal.value))
const structureAttentionCount = computed(() => {
  const value = structureDiagnostics.value?.classifications
  const summaryCount = value ? value.unrecognized + value.invalid_path + value.template_unavailable + value.duplicate_target + value.sidecar_target_conflict : 0
  return Math.max(structureIssueTotal.value, summaryCount)
})
const structureRecognitionInProgress = computed(() => {
  const diagnostics = structureDiagnostics.value
  if (!diagnostics) return false
  return runs.value.some(run => run.generation === diagnostics.generation && ['recognition_queued', 'recognition_running', 'recognition_failed', 'recognition_enqueue_failed'].includes(run.phase))
})
const structureIssuePages = computed(() => Math.max(1, Math.ceil(structureIssueTotal.value / structureIssuePageSize.value)))
const structureSelectedCount = computed(() => Object.keys(structureSelectionDraft.value).length)
const structureConflictCodes = new Set(['recognition_suspect_conflict', 'catalog_duplicate_conflict', 'duplicate_target', 'sidecar_target_conflict'])

function message(reason: unknown) { return reason instanceof Error ? reason.message : '请求失败' }
function dateTime(value: string | null) { return value ? new Date(value).toLocaleString() : '尚无记录' }
function bytes(value: number) { const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']; let amount = value; let index = 0; while (amount >= 1024 && index < units.length - 1) { amount /= 1024; index++ } return `${amount.toFixed(index === 0 ? 0 : 1)} ${units[index]}` }
function episodeLabel(entry: { season: number | null; episode: number | null }) { return entry.episode === null ? '' : `${entry.season === null ? '' : `S${String(entry.season).padStart(2, '0')}`}E${String(entry.episode).padStart(2, '0')}` }
function seasonLabel(number: number) { return number === 0 ? '未分季' : `第 ${number} 季` }
function scanKind(kind: string) { return ({ initial: '首次全量', catch_up: '监听交接对账', event: '文件事件', incremental: '立即/定时增量', full: '立即/周期全量', manual: '手动跟进' } as Record<string, string>)[kind] ?? kind }
function scanStatus(run: MediaLibraryScanRun) { if (run.status === 'failed') return { label: '失败', className: 'status-chip status-chip--error' }; if (run.status === 'catalog_ready') return { label: '目录可用 · 识别中', className: 'status-chip status-chip--warning' }; if (run.status === 'superseded') return { label: '已被新扫描替代', className: 'status-chip' }; if (run.status === 'running') return { label: '扫描中', className: 'status-chip status-chip--warning' }; if (run.status === 'success') return { label: run.partial ? '部分完成' : '成功', className: run.partial ? 'status-chip status-chip--warning' : 'status-chip status-chip--ready' }; return { label: '状态待确认', className: 'status-chip status-chip--warning' } }
function scanPhase(phase: string) { return ({ enumerating:'正在从 115 枚举', enumeration_complete:'数据源枚举完成', processing:'128 线程处理中', staging:'分批安全落库', publishing:'正在发布目录', scope_reconciliation:'正在增量对账', scope_fallback:'增量范围不足，改用全量对账', recognition_queued:'目录已发布，等待识别', recognition_running:'目录已发布，后台识别中', recognition_enqueue_failed:'目录可用，识别待恢复', recognition_artifact_enqueue_failed:'识别完成，元数据产物待恢复', recognition_failed:'目录可用，识别正在重试', completed:'全部完成', superseded:'已由更新一代替代', failed:'失败' } as Record<string,string>)[phase] ?? (phase ? '其他处理步骤' : '等待开始') }
const scanErrorLabels:Record<string,string>={media_library_scan_failed:'扫描结果提交失败',media_library_recognition_enqueue_failed:'基础目录可用，后台识别等待恢复',artifact_refresh_schedule_failed:'识别已完成，元数据产物等待恢复'}
const persistenceStageLabels:Record<string,string>={configuration_revalidate:'配置复核',load_existing_entries:'读取已有目录',persist_source_assets:'提交伴随文件',persist_recognition:'提交识别结果',persist_entries:'提交媒体目录',prune_stale_entries:'清理已失效条目',reconcile_tmdb_collections:'更新自动合集',advance_library_generation:'发布目录代际',persist_scan_run:'保存扫描状态',record_media_change:'发布媒体变更'}
const databaseErrorLabels:Record<string,string>={configuration_changed:'扫描期间配置发生变化',foreign_key:'关联数据不完整',unique:'发现重复数据',constraint:'数据不符合约束',busy:'数据库正忙，已完成有限重试',unknown:'未知数据库错误'}
function scanError(run: MediaLibraryScanRun) { if(!run.error_code)return '—';const summary=scanErrorLabels[run.error_code]??'扫描未完成';const detail=[run.persistence_stage?(persistenceStageLabels[run.persistence_stage]??'其他提交步骤'):'',run.database_error_class?(databaseErrorLabels[run.database_error_class]??'未知数据库错误'):''].filter(Boolean).join(' · ');return `${summary}${detail?`（${detail}）`:''}` }
function recognitionErrorLabel(code: string) { return ({ recognition_input_invalid: '无法从文件名推断标题，请手动整理', tmdb_invalid_request: '无法从文件名推断标题，请手动整理', tmdb_no_match: 'TMDB 没有自动匹配结果', tmdb_low_confidence: '自动匹配置信度不足', tmdb_candidate_conflict: '存在多个相似候选', tmdb_credential_unavailable: 'TMDB 未配置' } as Record<string,string>)[code] ?? code }
function structureIssueLabel(code: string) { return ({ media_unrecognized: '自动识别失败或无匹配', missing_season_episode: '缺少季号或集号', invalid_path: '路径不符合安全规则', template_unavailable: '当前命名模板无法应用', recognition_suspect_conflict: '多个不同作品疑似被识别成同一作品', catalog_duplicate_conflict: '同一来源事实在目录中重复', duplicate_target: '多个真实文件会得到同一目标', sidecar_target_conflict: '伴随文件目标冲突', path_mismatch: '目录或文件名与规则不一致', cloud_transfer_root_misplaced: '历史 115 入库文件位于网盘根目录' } as Record<string,string>)[code] ?? '其他目录问题' }
function structureIssueAction(issue: MediaLibraryStructureIssue) { if (issue.repairable) return '可生成安全整理预览'; return ({ media_unrecognized: '进入媒体清单手动识别', missing_season_episode: '无需处理，保持原文件', invalid_path: '在来源侧修正文件名后重新检查', template_unavailable: '调整分类与命名规则后重新检查', recognition_suspect_conflict: '先核对并修正作品识别；不要删除来源文件', catalog_duplicate_conflict: '先执行一次完整扫描；若仍存在再检查数据源重复事实', duplicate_target: '确认确为同一作品的多个版本后，再决定保留哪一份', sidecar_target_conflict: '请在来源侧改名或清理冲突的字幕、NFO、图片' } as Record<string,string>)[issue.code] ?? '检查来源文件后重新诊断' }
function structureStatusLabel(status: MediaLibraryStructureDiagnostics['status']) { return ({ pending: '等待首次诊断', queued: '已进入后台队列', running: '后台诊断中', healthy: '目录结构健康', issues: '发现目录结构问题', repairing: '修复任务执行中', failed: '目录结构诊断系统失败' } as Record<string,string>)[status] ?? '状态待确认' }
function structureIssueStateLabel(issue: MediaLibraryStructureIssueSummary) { return ({ manual_identity_resolved: '已人工识别 · 尚未整理文件', pending_repair: '待选择整理方式', unrecognized: '等待人工识别', needs_attention: '等待用户决定' } as Record<string,string>)[issue.state] ?? issue.state }
function structureSelectionLabel(action: MediaLibraryStructureSelection['action']) { return ({ repair: '整理此项', keep_recommended: '保留推荐来源', keep_member: '保留指定来源', keep_all_versions: '全部保留为版本', skip: '本次跳过' } as Record<MediaLibraryStructureSelection['action'], string>)[action] }
function isStructureConflict(issue: MediaLibraryStructureIssueSummary) { return structureConflictCodes.has(issue.code) && issue.members.length > 1 }

async function persistOrder(next: MediaLibraryDetail[]) {
  const previous = libraries.value
  libraries.value = next
  try {
    const data = await api<ListResponse<MediaLibraryDetail>>('/api/v1/media-libraries/order', { method: 'PUT', body: JSON.stringify({ ids: next.map(item => item.id) }) })
    libraries.value = data.list
    notice.value = '媒体库默认选择顺序已保存。'
  } catch (reason) {
    libraries.value = previous
    error.value = message(reason)
  }
}

function moveLibrary(id: number, direction: -1 | 1) {
  const index = libraries.value.findIndex(item => item.id === id)
  const target = index + direction
  if (index < 0 || target < 0 || target >= libraries.value.length) return
  const next = [...libraries.value]
  ;[next[index], next[target]] = [next[target]!, next[index]!]
  void persistOrder(next)
}

function dropLibrary(targetID: number) {
  const sourceID = draggedLibraryID.value
  draggedLibraryID.value = null
  if (sourceID === null || sourceID === targetID) return
  const sourceIndex = libraries.value.findIndex(item => item.id === sourceID)
  const targetIndex = libraries.value.findIndex(item => item.id === targetID)
  if (sourceIndex < 0 || targetIndex < 0) return
  const next = [...libraries.value]
  const [moved] = next.splice(sourceIndex, 1)
  next.splice(targetIndex, 0, moved!)
  void persistOrder(next)
}

async function load(options: { quiet?: boolean; preferred?: number } = {}) {
  if (refreshing.value) return
  refreshing.value = true
  if (!options.quiet) error.value = ''
  try {
    const [libraryData, storageData, profileData] = await Promise.all([
      api<ListResponse<MediaLibraryDetail>>('/api/v1/media-libraries'),
      auth.can(Permissions.StoragesRead) ? api<ListResponse<StorageSummary>>('/api/v1/storages') : Promise.resolve({ list: [], total: 0 }),
      auth.can(Permissions.MediaClassificationProfilesRead) ? api<ListResponse<MediaClassificationProfileSummary>>('/api/v1/media-classification-profiles') : Promise.resolve({ list: [], total: 0 }),
    ])
    libraries.value = libraryData.list
    storages.value = storageData.list.filter(item => item.enabled)
    profiles.value = profileData.list
    const preferred = options.preferred ?? selectedID.value
    selectedID.value = libraries.value.some(item => item.id === preferred) ? preferred : libraries.value[0]?.id ?? null
    if (selected.value) await loadActivity(selected.value.id)
    const selectedLibrary = selected.value
    if (selectedLibrary?.structure_status === 'issues' && !promptedStructureIDs.has(selectedLibrary.id)) {
      promptedStructureIDs.add(selectedLibrary.id)
      void viewStructureDiagnostics()
    }
    schedulePoll()
  } catch (reason) {
    if (!options.quiet) error.value = message(reason)
  } finally { refreshing.value = false; loading.value = false }
}

async function loadActivity(id: number) {
	await Promise.all([loadRuns(id), activeTab.value === 'entries' ? loadCatalog(id) : Promise.resolve()])
}

async function loadRuns(id: number) {
  runsRequest?.abort()
  const controller = new AbortController()
  runsRequest = controller
  try {
    const data = await api<ListResponse<MediaLibraryScanRun>>(`/api/v1/media-libraries/${id}/runs?limit=30`, { signal: controller.signal })
    if (!controller.signal.aborted && selectedID.value === id) runs.value = data.list
  } catch (reason) {
    if (!controller.signal.aborted) throw reason
  } finally {
    if (runsRequest === controller) runsRequest = null
  }
}

async function loadCatalog(id = selectedID.value) {
  if (!id) return
  catalogRequest?.abort()
  const controller = new AbortController()
  catalogRequest = controller
  catalogLoading.value = true
  catalogError.value = ''
  try {
    if (catalogMatch.value === 'unrecognized' || catalogMatch.value === 'review' || catalogMatch.value === 'manual') {
      await loadRecognitions(id, controller)
      return
    }
    const data = await api<PageResponse<MediaCatalogItem>>(mediaCatalogEndpoint(id, { page: catalogPage.value, pageSize: catalogPageSize.value, query: catalogSearch.value, mediaType: catalogType.value, matchStatus: catalogMatch.value === 'matched' ? 'matched' : '' }), { signal: controller.signal })
    if (controller.signal.aborted || selectedID.value !== id) return
    catalog.value = data.list
    catalogTotal.value = data.total
    catalogPage.value = data.page
    expandedWorkIDs.value = expandedWorkIDs.value.filter(workID => data.list.some(item => item.id === workID))
  } catch (reason) {
    if (!controller.signal.aborted) catalogError.value = message(reason)
  } finally {
    if (catalogRequest === controller) { catalogRequest = null; catalogLoading.value = false }
  }
}

async function loadRecognitions(id: number, controller = new AbortController()) {
  recognitionLoading.value = true
  const params = new URLSearchParams({ status: catalogMatch.value === 'manual' || catalogMatch.value === 'review' ? 'matched' : 'unrecognized', page: String(catalogPage.value), page_size: String(catalogPageSize.value) })
  if (catalogMatch.value === 'manual') params.set('manual_only', 'true')
  if (catalogSearch.value.trim()) params.set('query', catalogSearch.value.trim())
  if (catalogType.value) params.set('media_type', catalogType.value)
  try {
    const data = await api<PageResponse<MediaRecognitionSummary>>(`/api/v1/media-libraries/${id}/recognitions?${params}`, { signal: controller.signal })
    if (!controller.signal.aborted && selectedID.value === id) { recognitions.value = data.list; catalogTotal.value = data.total; catalogPage.value = data.page }
  } finally { recognitionLoading.value = false }
}

async function retryRecognition(item: MediaRecognitionSummary) {
  if (!selectedID.value) return
  await run(async () => {
    const result = await api<MediaRecognitionSummary>(`/api/v1/media-libraries/${selectedID.value}/recognitions/${encodeURIComponent(item.token)}/retry`, { method: 'POST', body: '{}' })
    notice.value = result.status === 'matched' ? '已重新识别该项目。' : '自动识别仍未匹配，可使用“手动整理”指定作品。'
    await loadCatalog()
  })
}

function openManualRecognition(item: MediaRecognitionSummary) {
  candidateToken.value = item.token
  candidates.value = []
  candidateSearched.value = false
  manualRecognitionError.value = ''
  const sourceTitle = item.source_directory !== '媒体库根目录' && item.source_directory !== '目录名不可显示'
    ? item.source_directory
    : item.source_summary.replace(/\.[^.]+$/, '')
  manualRecognitionForm.value = { title: (sourceTitle || item.title).trim(), mediaType: item.media_type || 'movie', year: item.release_year ? String(item.release_year) : '' }
}

function closeManualRecognition() {
  candidateToken.value = ''
  candidates.value = []
  candidateSearched.value = false
  manualRecognitionError.value = ''
}

async function findCandidates(item: MediaRecognitionSummary) {
  if (!selectedID.value) return
  const title = manualRecognitionForm.value.title.trim()
  const year = manualRecognitionForm.value.year.trim()
  manualRecognitionError.value = ''
  candidates.value = []
  candidateSearched.value = true
  if (!title) { manualRecognitionError.value = '请输入要搜索的作品标题。'; return }
  if (year && (!/^\d{4}$/.test(year) || Number(year) < 1888 || Number(year) > 2200)) { manualRecognitionError.value = '年份必须在 1888–2200 之间。'; return }
  const params = new URLSearchParams({ title, media_type: manualRecognitionForm.value.mediaType })
  if (year) params.set('year', year)
  candidateLoading.value = true
  try { const data = await api<ListResponse<TMDBCandidate>>(`/api/v1/media-libraries/${selectedID.value}/recognitions/${encodeURIComponent(item.token)}/tmdb-candidates?${params}`); candidates.value = data.list }
  catch (reason) { manualRecognitionError.value = message(reason) }
  finally { candidateLoading.value = false }
}

async function chooseCandidate(item: MediaRecognitionSummary, candidate: TMDBCandidate) {
  if (!selectedID.value) return
  await run(async () => {
    await api(`/api/v1/media-libraries/${selectedID.value}/recognitions/${encodeURIComponent(item.token)}/override`, { method: 'PUT', body: JSON.stringify({ tmdb_id: candidate.id, media_type: candidate.media_type }) })
    closeManualRecognition()
    notice.value = '作品身份已经 Server 复验并保存；正在生成只读目录整理预览。'
    await loadCatalog()
    await openStructureDiagnostics()
  })
}

async function clearRecognitionOverride(item: MediaRecognitionSummary) {
  if (!selectedID.value) return
  await run(async () => { await api(`/api/v1/media-libraries/${selectedID.value}/recognitions/${encodeURIComponent(item.token)}/override`, { method: 'DELETE', body: '{}' }); closeManualRecognition(); notice.value = '人工匹配已清除并恢复自动识别。'; await loadCatalog() })
}

async function applyCatalogFilters() {
  catalogPage.value = 1
  await loadCatalog()
}

async function changeCatalogPage(page: number) {
  if (page < 1 || page > catalogPages.value || page === catalogPage.value) return
  catalogPage.value = page
  await loadCatalog()
}

async function toggleCatalog(item: MediaCatalogItem) {
  if (expandedWorkIDs.value.includes(item.id)) {
    expandedWorkIDs.value = expandedWorkIDs.value.filter(id => id !== item.id)
    return
  }
  expandedWorkIDs.value = [...expandedWorkIDs.value, item.id]
  if (catalogDetails.value[item.id] || !selectedID.value) return
  const libraryID = selectedID.value
  detailRequests.get(item.id)?.abort()
  const controller = new AbortController()
  detailRequests.set(item.id, controller)
  detailLoadingIDs.value = [...detailLoadingIDs.value, item.id]
  try {
    const detail = await api<MediaCatalogDetail>(mediaCatalogDetailEndpoint(libraryID, item.id), { signal: controller.signal })
    if (!controller.signal.aborted && selectedID.value === libraryID) catalogDetails.value[item.id] = detail
  } catch (reason) {
    if (!controller.signal.aborted) catalogError.value = message(reason)
  } finally {
    if (detailRequests.get(item.id) === controller) detailRequests.delete(item.id)
    detailLoadingIDs.value = detailLoadingIDs.value.filter(id => id !== item.id)
  }
}

function openCatalogReorganization(work: MediaCatalogItem, transfer: MediaCatalogManagedTransfer) {
  reorganizationTarget.value = { work, transfer }
}

async function catalogReorganizationQueued() {
  if (!selectedID.value) return
  catalogDetails.value = {}
  expandedWorkIDs.value = []
  await loadCatalog(selectedID.value)
}

function resetCatalog() {
  catalogRequest?.abort()
  catalogRequest = null
  for (const controller of detailRequests.values()) controller.abort()
  detailRequests.clear()
  catalog.value = []
  catalogTotal.value = 0
  catalogPage.value = 1
  catalogSearch.value = ''
  catalogType.value = ''
  catalogMatch.value = ''
  catalogError.value = ''
  recognitions.value = []; closeManualRecognition()
  expandedWorkIDs.value = []
  catalogDetails.value = {}
  detailLoadingIDs.value = []
}

function schedulePoll() {
  window.clearTimeout(pollTimer)
  if (shouldPoll.value) pollTimer = window.setTimeout(() => void load({ quiet: true }), 2000)
}

function beginCreate() {
  createDraft.value = emptyMediaLibraryDraft(storages.value[0]?.id, profiles.value[0]?.id)
  resetSourceSelection(createDraft.value, storages.value[0])
  createOpen.value = true; error.value = ''; notice.value = ''
}

function openPicker(mode: 'create' | 'edit', target: PickerTarget) {
  pickerMode.value = mode; pickerTarget.value = target; pickerOpen.value = true
}

function directorySelected(value: { path: string; token: string }) {
  const draft = activeDraft.value
  if (!draft) return
  if (pickerTarget.value === 'source') { draft.source_path = value.path; draft.relative_root_token = value.token }
  else { draft.strm_local_path = value.path; draft.strm_local_root_token = value.token }
  pickerOpen.value = false
}

function resetSourceSelection(draft: MediaLibraryDraft, storage?: StorageSummary) {
  draft.relative_root_token = ''
  draft.relative_root = '/'
  draft.source_path = ''
  if (storage?.type === 'pan115' && draft.transfer_mode === 'symlink') draft.transfer_mode = 'move'
}
function normalizeSTRM(draft: MediaLibraryDraft, storage: StorageSummary | undefined) {
  if (!supportsSTRM(storage)) { draft.strm_enabled = false; draft.strm_local_path = ''; draft.strm_local_root_token = '' }
  if (storage?.type === 'local') draft.upload_sidecars = false
  if (draft.strm_enabled || !supportsSidecarUpload(storage)) draft.upload_sidecars = false
}

async function createLibrary() {
  await run(async () => {
    const created = await api<MediaLibraryDetail>('/api/v1/media-libraries', { method: 'POST', body: JSON.stringify(payloadFromDraft(createDraft.value, createStorage.value)) })
    createOpen.value = false
    notice.value = created.enabled ? '媒体库已创建，首次全量扫描会自动开始。' : '媒体库配置已保存；启用后会自动执行首次全量扫描。'
    await load({ preferred: created.id })
  })
}

async function saveLibrary() {
  if (!selected.value || !editDraft.value) return
  const id = selected.value.id
  saving.value = true
  error.value = ''
  notice.value = ''
  editSaveFeedback.value = { state: 'saving', message: '正在保存媒体库配置…' }
  try {
    const saved = await api<MediaLibraryDetail>(`/api/v1/media-libraries/${id}`, { method: 'PUT', body: JSON.stringify(payloadFromDraft(editDraft.value, editStorage.value)) })
    libraries.value = libraries.value.map(item => item.id === saved.id ? saved : item)
    replaceEditDraft(saved)
    editSaveFeedback.value = { state: 'success', message: '保存成功，新的媒体库配置已生效。' }
    notice.value = '媒体库配置已保存。启停、扫描计划和监听状态将按新配置生效。'
    try { await loadActivity(id) } catch (reason) { error.value = `配置已经保存，但最新状态读取失败：${message(reason)}` }
    schedulePoll()
  } catch (reason) {
    const failure = message(reason)
    error.value = failure
    editSaveFeedback.value = { state: 'error', message: `保存失败：${failure}` }
  } finally { saving.value = false }
}

async function scanNow(mode: 'incremental' | 'full') {
  if (!selected.value) return
  const id = selected.value.id
  const isFull = mode === 'full'
  if (isFull && !window.confirm('立即全量会重新核对整个媒体库，115 大库可能需要较长时间。继续吗？')) return
  await run(async () => { const scan = await api<MediaLibraryScanRun>(`/api/v1/media-libraries/${id}/scan`, { method: 'POST', body: JSON.stringify({ mode }) }); notice.value = scan.status === 'catalog_ready' ? '基础目录已经可用，元数据正在后台识别。' : (isFull ? '立即全量扫描已完成。' : '立即增量扫描已完成。'); await load({ preferred: id }) })
}

async function retryNow() {
  if (!selected.value) return
  const id = selected.value.id
  await run(async () => { await api(`/api/v1/media-libraries/${id}/retry`, { method: 'POST', body: '{}' }); notice.value = '已唤醒该媒体库的初始化重试。'; await load({ preferred: id }) })
}

async function removeLibrary() {
  if (!selected.value || !window.confirm(`确认删除媒体库“${selected.value.name}”？只会删除配置、索引和扫描记录，不会删除来源媒体文件。`)) return
  const id = selected.value.id
  await run(async () => { await api(`/api/v1/media-libraries/${id}`, { method: 'DELETE', body: '{}' }); selectedID.value = null; notice.value = '媒体库配置和索引已删除；来源媒体文件未改变。'; await load() })
}

async function setAutoListenDefault() {
  if (!selected.value) return
  await run(async () => {
    const result = await setDefaultIngestLibrary(selected.value!.id)
    notice.value = `已将“${result.media_library_name}”设为该 115 账号的自动监听默认入库库。`
    await load({ preferred: selected.value!.id })
  })
}

async function clearAutoListenDefault() {
  if (!selected.value?.connection_id || !window.confirm('确认取消该 115 账号的自动监听默认入库库？\n\n如果仍有下载器开启生活事件监听，Server 会拒绝此操作。')) return
  const id = selected.value.id
  await run(async () => {
    await clearDefaultIngestLibrary(selected.value!.connection_id!)
    notice.value = '已取消自动监听默认入库库。'
    await load({ preferred: id })
  })
}

async function openStructureDiagnostics() {
  await showStructureDiagnostics(true)
}

async function viewStructureDiagnostics() {
  await showStructureDiagnostics(false)
}

async function showStructureDiagnostics(enqueue: boolean) {
  if (!selected.value) return
  window.clearTimeout(structurePollTimer)
  const libraryID = selected.value.id
  structureOpen.value = true; structureLoading.value = true; structureDiagnostics.value = null; structureIssueFilter.value = 'all'; structureIssuePage.value = 1; structureIssues.value = []; structureIssueTotal.value = 0; structureIssuesError.value = ''; structureSelectionDraft.value = {}; structureSelectionCodes.value = {}; structureBulkConflictAction.value = ''; structureSelectionPreview.value = null; structureSelectionError.value = ''
  try {
    const diagnostics = enqueue
      ? await api<MediaLibraryStructureDiagnostics>(`/api/v1/media-libraries/${libraryID}/structure/diagnose`, { method: 'POST', body: '{}' })
      : await api<MediaLibraryStructureDiagnostics>(`/api/v1/media-libraries/${libraryID}/structure`)
    structureDiagnostics.value = diagnostics
    if (diagnostics.status === 'queued' || diagnostics.status === 'running') structurePollTimer = window.setTimeout(() => void pollStructureDiagnostics(libraryID), 750)
    else await loadStructureIssues(libraryID)
  }
  catch (reason) { error.value = message(reason); structureOpen.value = false }
  finally { structureLoading.value = false }
}

async function loadStructureIssues(libraryID = selected.value?.id, resetPage = false) {
  if (!libraryID) return
  if (resetPage) structureIssuePage.value = 1
  structureIssuesLoading.value = true
  structureIssuesError.value = ''
  try {
    const params = new URLSearchParams({ page: String(structureIssuePage.value), page_size: String(structureIssuePageSize.value), actionable: 'true' })
    if (structureIssueFilter.value !== 'all') params.set('code', structureIssueFilter.value)
    const result = await api<MediaLibraryStructureIssuePage>(`/api/v1/media-libraries/${libraryID}/structure/issues?${params}`)
    if (selected.value?.id !== libraryID) return
    structureIssues.value = result.list
    structureIssueTotal.value = result.total
    structureIssuePage.value = result.page
    structureIssuePageSize.value = result.page_size
  } catch (reason) {
    structureIssuesError.value = message(reason)
  } finally {
    structureIssuesLoading.value = false
  }
}

async function setStructureIssueFilter(code: string) {
  structureIssueFilter.value = code
  await loadStructureIssues(selected.value?.id, true)
}

async function changeStructureIssuePage(page: number) {
  if (page < 1 || page > structureIssuePages.value || page === structureIssuePage.value) return
  structureIssuePage.value = page
  await loadStructureIssues()
}

function setStructureSelection(issue: MediaLibraryStructureIssueSummary, action: MediaLibraryStructureSelection['action'], memberToken = '') {
  const next = { ...structureSelectionDraft.value }
  next[issue.token] = { issue_token: issue.token, action, ...(memberToken ? { member_token: memberToken } : {}) }
  structureSelectionDraft.value = next
  structureSelectionCodes.value = { ...structureSelectionCodes.value, [issue.token]: issue.code }
  structureSelectionPreview.value = null
  structureSelectionError.value = ''
}

function setStructureBulkConflictAction(action: 'keep_recommended' | 'skip') {
  structureBulkConflictAction.value = action
  structureSelectionPreview.value = null
  structureSelectionError.value = ''
}

async function previewStructureSelections(currentTypeOnly = false) {
  if (!selected.value || !structureDiagnostics.value) return
  const code = currentTypeOnly && structureIssueFilter.value !== 'all' ? structureIssueFilter.value : ''
  const selections = Object.values(structureSelectionDraft.value).filter(item => !code || structureSelectionCodes.value[item.issue_token] === code)
  const bulkActions: MediaLibraryStructureBulkSelection[] = []
  if (structureBulkConflictAction.value) {
    const codes = currentTypeOnly ? (code && structureConflictCodes.has(code) ? [code] : []) : [...structureConflictCodes]
    if (codes.length) bulkActions.push({ codes, action: structureBulkConflictAction.value })
  }
  if (selections.length === 0 && bulkActions.length === 0) {
    structureSelectionError.value = currentTypeOnly ? '请先为当前类型选择至少一个处理方式。' : '请先选择要整理、保留或跳过的问题。'
    return
  }
  structureSelectionLoading.value = true
  structureSelectionError.value = ''
  try {
    structureSelectionPreview.value = await api<MediaLibraryStructureSelectionPreview>(`/api/v1/media-libraries/${selected.value.id}/structure/selection-preview`, { method: 'POST', body: JSON.stringify({ revision: structureDiagnostics.value.revision, selections, bulk_actions: bulkActions }) })
  } catch (reason) {
    structureSelectionPreview.value = null
    structureSelectionError.value = message(reason)
  } finally {
    structureSelectionLoading.value = false
  }
}

async function repairStructureSelections() {
  if (!selected.value || !structureSelectionPreview.value) return
  const preview = structureSelectionPreview.value
  if (!window.confirm(`确认执行 ${preview.move_count} 个整理动作，并将 ${preview.recycle_count} 个落选文件移入可恢复回收站？本次还会跳过 ${preview.skipped_count} 项。`)) return
  const id = selected.value.id
  await run(async () => {
    const repair = await api<MediaLibraryStructureRepair>(`/api/v1/media-libraries/${id}/structure/selection-repair`, { method: 'POST', body: JSON.stringify({ confirmation_token: preview.confirmation_token }) })
    notice.value = `已提交 ${repair.total_items} 个目录处理动作；完成后会自动重新扫描。`
    structureOpen.value = false
    structureDiagnostics.value = null
    structureSelectionPreview.value = null
    await load({ quiet: true, preferred: id })
  })
}

async function openUnrecognizedIssues() {
  structureOpen.value = false
  activeTab.value = 'entries'
  catalogMatch.value = 'unrecognized'
  catalogPage.value = 1
  await loadCatalog()
}

async function openRecognitionReview() {
  structureOpen.value = false
  activeTab.value = 'entries'
  catalogMatch.value = 'review'
  catalogPage.value = 1
  await loadCatalog()
}

async function pollStructureDiagnostics(libraryID: number) {
  if (!structureOpen.value || selected.value?.id !== libraryID) return
  try {
    const diagnostics = await api<MediaLibraryStructureDiagnostics>(`/api/v1/media-libraries/${libraryID}/structure`)
    structureDiagnostics.value = diagnostics
    if (diagnostics.status === 'queued' || diagnostics.status === 'running') structurePollTimer = window.setTimeout(() => void pollStructureDiagnostics(libraryID), 750)
    else { await loadStructureIssues(libraryID); await load({ quiet: true, preferred: libraryID }) }
  } catch (reason) { error.value = message(reason) }
}

async function repairStructure(workID: string) {
  if (!selected.value) return
  const id = selected.value.id
  await run(async () => {
    await api<MediaLibraryStructureRepair>(`/api/v1/media-libraries/${id}/structure/repair`, { method: 'POST', body: JSON.stringify({ work_id: workID }) })
    notice.value = '该作品的目录修复已进入队列；完成后会自动重新扫描。'
    await load({ quiet: true, preferred: id })
  })
}

async function run(action: () => Promise<void>) { saving.value = true; error.value = ''; notice.value = ''; try { await action() } catch (reason) { error.value = message(reason) } finally { saving.value = false } }

watch(selectedID, async id => {
	runs.value = []; resetCatalog(); activeTab.value = 'status'
  editSaveFeedback.value = { state: 'idle', message: '' }
  if (id) { const library = libraries.value.find(item => item.id === id); if (library) replaceEditDraft(library); else clearEditDraft(); try { await loadActivity(id) } catch (reason) { error.value = message(reason) } }
  else clearEditDraft()
})
watch(selected, library => { if (library && !editDirty.value && !saving.value) replaceEditDraft(library) })
watch(editFingerprint, fingerprint => {
  if (fingerprint !== editBaseline.value && editSaveFeedback.value.state !== 'saving') editSaveFeedback.value = { state: 'idle', message: '' }
})
watch(activeTab, tab => { if (tab === 'entries' && selectedID.value) void loadCatalog(selectedID.value) })
watch(() => createDraft.value.storage_id, () => { resetSourceSelection(createDraft.value, createStorage.value); normalizeSTRM(createDraft.value, createStorage.value) })
watch(() => editDraft.value?.storage_id, () => { if (editDraft.value && editDraft.value.storage_id !== selected.value?.storage_id) resetSourceSelection(editDraft.value, editStorage.value); if (editDraft.value) normalizeSTRM(editDraft.value, editStorage.value) })
onMounted(() => void load())
onBeforeUnmount(() => { window.clearTimeout(pollTimer); window.clearTimeout(structurePollTimer); runsRequest?.abort(); resetCatalog() })

function replaceEditDraft(library: MediaLibraryDetail) {
  const draft = draftFromLibrary(library, storages.value.find(item => item.id === library.storage_id))
  editDraft.value = draft
  editBaseline.value = mediaLibraryDraftFingerprint(draft, storages.value.find(item => item.id === library.storage_id))
}

function clearEditDraft() {
  editDraft.value = null
  editBaseline.value = ''
}
</script>

<template>
  <section class="mx-auto max-w-7xl">
    <div class="flex flex-wrap items-end justify-between gap-4">
      <div><h1 class="m-0 text-3xl font-800">媒体库</h1><p class="page-description mt-2 max-w-3xl">从已注册 Storage 选择受控相对根，建立只读扫描索引，并配置下载完成后的分类、命名和入库策略。</p></div>
      <div class="flex flex-wrap gap-2"><button class="btn-secondary" :disabled="refreshing" @click="load()">刷新</button><button v-if="auth.can(Permissions.MediaLibrariesCreate)" class="btn-primary" :disabled="!storages.length || !profiles.length || !auth.can(Permissions.StoragesBrowse)" @click="beginCreate">创建媒体库</button></div>
    </div>
    <p v-if="error" role="alert" class="semantic-error mt-5 p-3 text-sm">{{ error }}</p><p v-if="notice" role="status" class="semantic-success mt-5 p-3 text-sm">{{ notice }}</p>
    <p v-if="auth.can(Permissions.MediaLibrariesCreate) && (!auth.can(Permissions.StoragesRead) || !auth.can(Permissions.StoragesBrowse) || !auth.can(Permissions.MediaClassificationProfilesRead))" class="semantic-warning mt-5 p-3 text-sm">创建媒体库还需要 storages.read、storages.browse 与 media_classification_profiles.read，以安全选择来源和分类 Profile。</p>

    <form v-if="createOpen" class="panel mt-6" @submit.prevent="createLibrary">
      <div class="flex items-start justify-between gap-4"><div><h2 class="m-0 text-xl">创建媒体库</h2><p class="text-subtle mb-0 mt-1 text-sm">启用后自动进入首次全量扫描；无需额外点击扫描。</p></div><button type="button" class="btn-secondary" @click="createOpen = false">取消</button></div>
      <div class="mt-5 grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <div><label class="label" for="library-create-name">名称</label><input id="library-create-name" v-model="createDraft.name" class="input" required maxlength="128" placeholder="本地媒体库" /></div>
        <div><label class="label" for="library-create-storage">来源 Storage</label><select id="library-create-storage" v-model.number="createDraft.storage_id" class="input" required><option v-for="storage in storages" :key="storage.id" :value="storage.id">{{ storage.name }}（{{ storage.type }}）</option></select></div>
        <div><label class="label" for="library-create-profile">分类 Profile</label><select id="library-create-profile" v-model.number="createDraft.profile_id" class="input" required><option v-for="profile in profiles" :key="profile.id" :value="profile.id">{{ profile.name }} · r{{ profile.revision }}</option></select></div>
        <div class="md:col-span-2 xl:col-span-3"><label class="label">来源相对根</label><div class="flex gap-2"><input class="input min-w-0 font-mono" :value="createDraft.source_path" readonly required placeholder="请从来源 Storage 内选择目录；Storage 根会保存为 /" /><button v-if="createStorage && auth.can(Permissions.StoragesBrowse)" type="button" class="btn-secondary shrink-0" @click="openPicker('create', 'source')">选择目录</button></div><p class="text-subtle mb-0 mt-2 text-xs">目录选择令牌绑定当前用户、Connection、Storage 和 Storage 根；115 扫描从所选目录的稳定 file_id 开始。</p></div>
        <label class="text-muted flex items-center gap-3 text-sm"><input v-model="createDraft.enabled" type="checkbox" />创建后启用并自动初始化</label><label class="text-muted flex items-center gap-3 text-sm"><input v-model="createDraft.recursive" type="checkbox" />递归扫描子目录</label>
        <label class="text-muted flex items-center gap-3 text-sm"><input v-model="createDraft.metadata_artifacts_enabled" type="checkbox" />生成 NFO / 图片元数据</label>
        <template v-if="supportsSTRM(createStorage)"><label class="text-muted flex items-center gap-3 text-sm"><input v-model="createDraft.strm_enabled" type="checkbox" @change="normalizeSTRM(createDraft, createStorage)" />启用 signed 302 / STRM</label><div v-if="createDraft.strm_enabled" class="md:col-span-2 xl:col-span-3"><label class="label">本地 STRM 输出目录</label><div class="flex gap-2"><input class="input" :value="createDraft.strm_local_path" readonly required /><button type="button" class="btn-secondary" @click="openPicker('create', 'strm')">选择目录</button></div><p class="text-subtle mb-0 mt-2 text-xs">视频生成 STRM；NFO、海报、字幕和图片按同一目录结构生成在这里，不上传到网盘。</p></div></template>
        <label v-if="supportsSidecarUpload(createStorage) && !createDraft.strm_enabled" class="text-muted flex items-center gap-3 text-sm"><input v-model="createDraft.upload_sidecars" type="checkbox" :disabled="!createDraft.metadata_artifacts_enabled" />将 NFO / JPG 上传到云端媒体旁</label>
      </div>
      <details class="semantic-inset mt-5 p-4"><summary class="cursor-pointer font-650">扫描、限速与匹配配置</summary><MediaLibrarySettingsFields v-model="createDraft" class="mt-4" :storage-type="createStorage?.type" /></details>
      <button class="btn-primary mt-5" :disabled="saving || !createStorage || !createDraft.relative_root_token || (createDraft.strm_enabled && !createDraft.strm_local_path)">创建媒体库</button>
    </form>

    <div v-if="loading" class="panel mt-7">正在加载媒体库…</div>
    <div v-else-if="libraries.length === 0" class="panel mt-7"><h2 class="m-0 text-lg">尚未创建媒体库</h2><p class="page-description mb-0 mt-2 text-sm">先注册可读 Storage 并准备分类 Profile，再选择来源相对根。扫描和监听始终只读；只有已确认目标的独立入库任务会按这里的策略写入。</p></div>
    <div v-else class="mt-7 grid gap-5 xl:grid-cols-[minmax(18rem,.68fr)_minmax(0,1.7fr)]">
      <aside class="panel p-2">
        <div
          v-for="(library, index) in libraries"
          :key="library.id"
          class="semantic-list-item mb-1 flex items-stretch gap-1 p-1"
          :class="{ 'semantic-list-item--selected': selectedID === library.id }"
          draggable="true"
          @dragstart="draggedLibraryID = library.id"
          @dragend="draggedLibraryID = null"
          @dragover.prevent
          @drop.prevent="dropLibrary(library.id)"
        >
          <button class="btn-quiet shrink-0 cursor-grab px-2" type="button" aria-label="拖动媒体库排序" title="拖动排序">⋮⋮</button>
          <button type="button" class="min-w-0 flex-1 p-2 text-left" @click="selectedID = library.id">
            <div class="flex items-center justify-between gap-3"><strong class="truncate">{{ library.name }}</strong><span :class="presentLibraryStatus(library.status).className">{{ presentLibraryStatus(library.status).label }}</span></div>
            <div class="text-subtle mt-2 text-xs">第 {{ index + 1 }} 顺位 · {{ library.storage_name }} · {{ mediaLibrarySourceDisplayPath(library, storages.find(item => item.id === library.storage_id)) }}（相对根 {{ library.relative_root }}） · {{ library.entry_count }} 条目</div>
            <div class="text-subtle mt-1 text-xs">{{ library.transfer_mode === 'move' ? '移动' : library.transfer_mode === 'copy' ? '复制' : '软链接' }} · {{ library.profile_name }}</div>
            <div v-if="library.reclassification_due" class="semantic-warning-text mt-2 text-xs">分类规则已更新，待重分类</div>
          </button>
          <div v-if="auth.can(Permissions.MediaLibrariesUpdate)" class="flex shrink-0 flex-col justify-center gap-1">
            <button class="btn-quiet px-2" type="button" :disabled="index === 0 || saving" aria-label="上移媒体库" @click="moveLibrary(library.id, -1)">↑</button>
            <button class="btn-quiet px-2" type="button" :disabled="index === libraries.length - 1 || saving" aria-label="下移媒体库" @click="moveLibrary(library.id, 1)">↓</button>
          </div>
        </div>
      </aside>
      <main v-if="selected" class="min-w-0">
        <section class="panel">
          <div class="flex flex-wrap items-start justify-between gap-4"><div><div class="flex flex-wrap items-center gap-2"><h2 class="m-0">{{ selected.name }}</h2><span :class="presentLibraryStatus(selected.status).className">{{ presentLibraryStatus(selected.status).label }}</span></div><p class="text-subtle mb-0 mt-2 text-sm">{{ selected.storage_name }} · {{ selectedSourceDisplay }}（相对根 {{ selected.relative_root }}） · Profile {{ selected.profile_name }} r{{ selected.profile_revision }}</p></div><div class="flex flex-wrap gap-2"><button v-if="auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-secondary" :disabled="saving || structureLoading" @click="openStructureDiagnostics">检查目录结构</button><button v-if="selected.status === 'initialization_failed' && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-primary" :disabled="saving" @click="retryNow">立即重试</button><button v-if="selected.enabled && selected.status !== 'initializing' && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-secondary" :disabled="saving" @click="scanNow('incremental')">立即增量</button><button v-if="selected.enabled && selected.status !== 'initializing' && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-secondary" :disabled="saving" @click="scanNow('full')">立即全量</button><button v-if="auth.can(Permissions.MediaLibrariesDelete)" type="button" class="btn-danger" :disabled="saving" @click="removeLibrary">删除配置</button></div></div>
          <p v-if="selected.status === 'initialization_failed'" class="semantic-error mt-4 p-3 text-sm">初始化失败：{{ selected.status_error_code || 'media_library_scan_failed' }}。失败库不会启动监听；下次自动重试：{{ dateTime(selected.next_retry_at) }}。</p>
          <p v-if="selected.reclassification_due" class="semantic-warning mt-4 p-3 text-sm">所选 Profile 已更新。下一次扫描会重新应用分类，但不会移动、重命名或写回来源文件。<RouterLink class="semantic-link ml-1" to="/system/media-rules">打开规则管理</RouterLink></p>
          <div v-if="selected.structure_status === 'queued' || selected.structure_status === 'running'" class="semantic-inset mt-4 flex flex-wrap items-center justify-between gap-3 p-3 text-sm"><span>目录结构诊断正在后台{{ selected.structure_status === 'queued' ? '排队' : '计算' }}；媒体目录已经可用，诊断不会移动任何文件。</span><button class="btn-secondary" type="button" @click="viewStructureDiagnostics">查看诊断进度</button></div>
          <div v-else-if="selected.structure_status === 'issues'" class="semantic-warning mt-4 flex flex-wrap items-center justify-between gap-3 p-3 text-sm"><span>目录诊断发现可选整理建议或需处理项目。Server 尚未移动任何文件；打开后可分别预览整理、手动识别、查看冲突或重新检查。</span><button class="btn-secondary" type="button" @click="viewStructureDiagnostics">查看诊断与处理入口</button></div>
          <div v-else-if="selected.structure_status === 'repairing'" class="semantic-inset mt-4 p-3 text-sm">正在按当前 Profile 修复电影 / 电视剧目录，完成后会自动重新扫描并更新页面。</div>
          <div v-else-if="selected.structure_status === 'failed'" class="semantic-error mt-4 flex flex-wrap items-center justify-between gap-3 p-3 text-sm"><span>目录结构诊断发生系统错误，但没有移动任何文件。可重新诊断。</span><button class="btn-secondary" type="button" @click="openStructureDiagnostics">重新诊断</button></div>
        </section>

        <section v-if="selectedStorage?.type === 'pan115'" class="semantic-inset mt-4 p-4">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div><div class="flex flex-wrap items-center gap-2"><strong>自动监听默认入库库</strong><span v-if="selected.auto_listen_default" class="status-chip status-chip--ready">当前默认</span></div><p class="text-subtle mb-0 mt-1 text-xs">同一 115 账号只能有一个。它只接收 115 App 手工放入下载目录的内容；离线下载、转存、站点下载和追更仍使用各自任务的目标。</p></div>
            <div v-if="auth.can(Permissions.MediaLibrariesUpdate)" class="flex gap-2"><button v-if="!selected.auto_listen_default" type="button" class="btn-secondary" :disabled="saving || !selected.enabled" @click="setAutoListenDefault">设为默认</button><button v-else type="button" class="btn-danger" :disabled="saving" @click="clearAutoListenDefault">取消默认</button></div>
          </div>
        </section>

        <div class="management-tabs mt-4" role="tablist" aria-label="媒体库详情"><button v-for="tab in ([['status','状态'],['runs','扫描记录'],['entries','媒体清单'],['settings','配置']] as const)" :id="`library-tab-${tab[0]}`" :key="tab[0]" type="button" class="management-tab" :class="activeTab === tab[0] ? 'management-tab--active' : ''" role="tab" :aria-selected="activeTab === tab[0]" :aria-controls="`library-panel-${tab[0]}`" @click="activeTab = tab[0]">{{ tab[1] }}</button></div>

        <section v-if="activeTab === 'status'" id="library-panel-status" class="panel mt-4" role="tabpanel" aria-labelledby="library-tab-status"><div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4"><div class="semantic-inset p-3"><span class="text-subtle text-xs">媒体条目</span><strong class="mt-1 block">{{ selected.entry_count }}</strong></div><div class="semantic-inset p-3"><span class="text-subtle text-xs">基线 / dirty generation</span><strong class="mt-1 block">{{ selected.baseline_generation }} / {{ selected.dirty_generation }}</strong></div><div class="semantic-inset p-3"><span class="text-subtle text-xs">最近成功扫描</span><strong class="mt-1 block text-sm">{{ dateTime(selected.last_successful_scan_at) }}</strong></div><div class="semantic-inset p-3"><span class="text-subtle text-xs">监听方式</span><strong class="mt-1 block">{{ selected.enabled ? 'Storage driver 自动选择' : '未运行' }}</strong></div></div><p class="text-subtle mb-0 mt-4 text-sm">首次全量建立基线后，Server 才挂接监听并立即执行交接增量对账。每个媒体库独立运行，不占用下载、传输或持久任务队列。</p></section>
        <section v-else-if="activeTab === 'runs'" id="library-panel-runs" class="panel mt-4 overflow-x-auto" role="tabpanel" aria-labelledby="library-tab-runs"><table class="semantic-table min-w-260 w-full text-left text-sm"><thead><tr><th>开始时间</th><th>类型</th><th>状态 / 当前步骤</th><th>枚举 / 处理 / 落库 / 去重</th><th>识别进度</th><th>新增 / 更新 / 删除</th><th>Generation</th><th>错误 / 日志</th></tr></thead><tbody><tr v-for="runItem in runs" :key="runItem.id"><td>{{ dateTime(runItem.started_at) }}</td><td>{{ scanKind(runItem.kind) }}</td><td><span :class="scanStatus(runItem).className">{{ scanStatus(runItem).label }}</span><small class="text-subtle mt-1 block">{{ scanPhase(runItem.phase) }}</small></td><td>{{ runItem.enumerated }} / {{ runItem.processed }} / {{ runItem.persisted }} / {{ runItem.deduplicated }}</td><td>{{ runItem.recognition_completed }} / {{ runItem.recognition_total }}<small class="text-subtle mt-1 block">成功 {{ runItem.matched }} · 未识别 {{ runItem.unrecognized }} · 缓存 {{ runItem.cache_hits }}<span v-if="runItem.recognition_failed" class="semantic-danger-text"> · 失败 {{ runItem.recognition_failed }}</span></small></td><td>{{ runItem.added }} / {{ runItem.updated }} / {{ runItem.removed }}</td><td>{{ runItem.generation }}</td><td><span class="semantic-danger-text">{{ scanError(runItem) }}</span><RouterLink class="semantic-link mt-1 block text-xs" :to="{ name: 'runtime-logs', query: { library_id: String(runItem.library_id), scan_run_id: String(runItem.id) } }">查看本次详细日志</RouterLink></td></tr><tr v-if="runs.length === 0"><td colspan="8" class="text-subtle py-8 text-center">尚无扫描记录</td></tr></tbody></table></section>
        <section v-else-if="activeTab === 'entries'" id="library-panel-entries" class="panel mt-4" role="tabpanel" aria-labelledby="library-tab-entries">
          <div class="management-tabs mb-4" role="tablist" aria-label="媒体识别状态">
            <button v-for="filter in ([['','全部'],['matched','已识别'],['unrecognized','未识别'],['review','核对识别'],['manual','人工匹配']] as const)" :key="filter[0]" type="button" class="management-tab" :class="catalogMatch === filter[0] ? 'management-tab--active' : ''" @click="catalogMatch = filter[0]; applyCatalogFilters()">{{ filter[1] }}</button>
          </div>
          <form class="flex flex-wrap items-end gap-3" @submit.prevent="applyCatalogFilters">
            <div class="min-w-52 flex-1"><label class="label" for="media-catalog-search">搜索标题</label><input id="media-catalog-search" v-model="catalogSearch" class="input" maxlength="200" placeholder="输入电影或剧集标题" /></div>
            <div><label class="label" for="media-catalog-type">类型</label><select id="media-catalog-type" v-model="catalogType" class="input" @change="applyCatalogFilters"><option value="">全部</option><option value="movie">电影</option><option value="series">剧集</option></select></div>
            <div><label class="label" for="media-catalog-page-size">每页</label><select id="media-catalog-page-size" v-model.number="catalogPageSize" class="input" @change="applyCatalogFilters"><option v-for="size in mediaCatalogPageSizes" :key="size" :value="size">{{ size }}</option></select></div>
            <button type="submit" class="btn-secondary" :disabled="catalogLoading">搜索</button>
          </form>
          <p v-if="catalogError" role="alert" class="semantic-error mt-4 p-3 text-sm">{{ catalogError }}</p>
          <div v-if="catalogMatch !== 'unrecognized' && catalogMatch !== 'review' && catalogMatch !== 'manual'" class="mt-4 overflow-x-auto">
            <table class="semantic-table min-w-180 w-full text-left text-sm">
              <thead><tr><th class="w-12"></th><th>作品</th><th>类型</th><th>季 / 集 / 文件</th><th>分类 / 匹配</th><th>大小</th><th>修改时间</th></tr></thead>
              <tbody>
                <template v-for="work in catalog" :key="work.id">
                  <tr>
                    <td><button type="button" class="btn-quiet px-2" :aria-label="expandedWorkIDs.includes(work.id) ? `收起${work.title}` : `展开${work.title}`" @click="toggleCatalog(work)">{{ expandedWorkIDs.includes(work.id) ? '−' : '+' }}</button></td>
                    <td><strong>{{ work.title }}</strong></td><td>{{ work.kind === 'movie' ? '电影' : '剧集' }}</td>
                    <td>{{ work.kind === 'series' ? `${work.season_count} 季 / ${work.episode_count} 集 / ${work.file_count} 文件` : `${work.file_count} 文件` }}</td>
                    <td>{{ work.category_name || '未分类' }} · {{ work.match_status }}</td><td>{{ bytes(work.size) }}</td><td>{{ dateTime(work.modified_at) }}</td>
                  </tr>
                  <tr v-if="expandedWorkIDs.includes(work.id)"><td colspan="7" class="p-0">
                    <div class="semantic-inset m-2 p-3">
                      <p v-if="detailLoadingIDs.includes(work.id)" class="text-subtle m-0 text-sm">正在读取季度与分集…</p>
                      <div class="mb-4 flex flex-wrap items-center gap-2"><button v-if="work.match_status === 'matched' && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-secondary" :disabled="saving" @click="repairStructure(work.id)">检查并修复此作品目录</button><template v-if="catalogDetails[work.id]?.reorganizable_transfers.length"><span class="text-subtle text-xs">OhMyCine 托管入库记录：</span><button v-for="transfer in catalogDetails[work.id].reorganizable_transfers" :key="transfer.transfer_task_id" type="button" class="btn-secondary" @click="openCatalogReorganization(work, transfer)">修正并整理 {{ transfer.file_count }} 个文件</button></template></div>
                      <div v-for="season in catalogDetails[work.id]?.seasons ?? []" :key="season.number" class="mb-3 last:mb-0">
                        <strong class="text-sm">{{ seasonLabel(season.number) }}</strong>
                        <div class="mt-2 grid gap-1">
                          <div v-for="episode in season.episodes" :key="episode.id" class="grid gap-2 rounded-1 px-2 py-1.5 text-xs md:grid-cols-[7rem_minmax(0,1fr)_auto]">
                            <span>{{ episodeLabel(episode) || '未识别集号' }}</span><span class="truncate font-mono" :title="episode.relative_path">{{ episode.relative_path }}</span><span>{{ bytes(episode.size) }}</span>
                          </div>
                        </div>
                      </div>
                      <div v-if="catalogDetails[work.id]?.files.length" class="grid gap-1"><div v-for="file in catalogDetails[work.id].files" :key="file.id" class="grid gap-2 rounded-1 px-2 py-1.5 text-xs md:grid-cols-[minmax(0,1fr)_auto]"><span class="truncate font-mono" :title="file.relative_path">{{ file.relative_path }}</span><span>{{ bytes(file.size) }}</span></div></div>
                    </div>
                  </td></tr>
                </template>
                <tr v-if="catalogLoading && catalog.length === 0"><td colspan="7" class="text-subtle py-8 text-center">正在加载作品目录…</td></tr>
                <tr v-else-if="catalog.length === 0"><td colspan="7" class="text-subtle py-8 text-center">当前筛选下没有作品</td></tr>
              </tbody>
            </table>
          </div>
          <div v-else class="mt-4 overflow-x-auto">
            <table class="semantic-table min-w-220 w-full text-left text-sm">
              <thead><tr><th>识别标题 / 来源</th><th>所在目录</th><th>类型</th><th>原因</th><th>文件</th><th>更新时间</th><th>操作</th></tr></thead>
              <tbody>
                <template v-for="item in recognitions" :key="item.token">
                  <tr><td><strong>{{ item.title || '标题未解析' }}</strong><small class="text-subtle mt-1 block font-mono">{{ item.source_summary }}</small></td><td><span class="font-mono">{{ item.source_directory }}</span></td><td>{{ item.media_type === 'tv' ? '剧集' : item.media_type === 'movie' ? '电影' : '未知' }}</td><td :class="item.status === 'unrecognized' ? 'semantic-warning-text' : ''">{{ item.status === 'matched' ? `TMDB ${item.tmdb_id}` : recognitionErrorLabel(item.error_code || 'tmdb_no_match') }}</td><td>{{ item.file_count }}</td><td>{{ dateTime(item.updated_at) }}</td><td><div class="flex flex-wrap gap-2"><button v-if="item.status === 'unrecognized' && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-secondary" :disabled="saving" @click="retryRecognition(item)">重试自动识别</button><button v-if="!item.manual_override && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-primary" :disabled="saving" @click="openManualRecognition(item)">{{ item.status === 'matched' ? '修正识别' : '手动整理' }}</button><button v-if="item.manual_override && auth.can(Permissions.MediaLibrariesScan)" type="button" class="btn-danger" :disabled="saving" @click="clearRecognitionOverride(item)">清除人工匹配</button></div></td></tr>
                  <tr v-if="candidateToken === item.token"><td colspan="7"><form class="semantic-inset grid gap-3 p-4" @submit.prevent="findCandidates(item)"><div><strong>手动整理作品身份</strong><p class="text-subtle mb-0 mt-1 text-xs">先修改搜索条件并选择经 Server 复验的 TMDB 作品；保存后只生成目录移动预览，不会直接移动文件。</p></div><div class="grid gap-3 md:grid-cols-[minmax(14rem,1fr)_10rem_9rem_auto_auto]"><div><label class="label" :for="`manual-title-${item.token}`">搜索标题</label><input :id="`manual-title-${item.token}`" v-model="manualRecognitionForm.title" class="input" required maxlength="256" autocomplete="off" /></div><div><label class="label" :for="`manual-type-${item.token}`">作品类型</label><select :id="`manual-type-${item.token}`" v-model="manualRecognitionForm.mediaType" class="input"><option value="movie">电影</option><option value="tv">剧集</option></select></div><div><label class="label" :for="`manual-year-${item.token}`">年份（可选）</label><input :id="`manual-year-${item.token}`" v-model="manualRecognitionForm.year" class="input" inputmode="numeric" maxlength="4" placeholder="例如 2024" /></div><button type="submit" class="btn-primary self-end" :disabled="candidateLoading || saving || !manualRecognitionForm.title.trim()">{{ candidateLoading ? '搜索中…' : '搜索 TMDB' }}</button><button type="button" class="btn-secondary self-end" :disabled="candidateLoading || saving" @click="closeManualRecognition">取消</button></div><p v-if="manualRecognitionError" role="alert" class="semantic-error m-0 p-3 text-sm">{{ manualRecognitionError }}</p><p v-else-if="candidateLoading" class="text-subtle m-0">正在搜索有限 TMDB 候选…</p><p v-else-if="candidateSearched && candidates.length === 0" class="text-subtle m-0">没有找到候选，请调整标题、类型或年份后重试。</p><div v-else-if="candidates.length" class="grid gap-2"><button v-for="candidate in candidates" :key="`${candidate.media_type}-${candidate.id}`" type="button" class="semantic-list-item flex items-center justify-between gap-3 p-3 text-left" :disabled="saving" @click="chooseCandidate(item, candidate)"><span><strong>{{ candidate.title }}</strong><small class="text-subtle ml-2">{{ candidate.media_type === 'tv' ? '剧集' : '电影' }} · {{ candidate.release_year || '年份未知' }} · TMDB {{ candidate.id }}</small></span><span>{{ Math.round(candidate.confidence * 100) }}%</span></button></div></form></td></tr>
                </template>
                <tr v-if="recognitionLoading && recognitions.length === 0"><td colspan="7" class="text-subtle py-8 text-center">正在加载识别项目…</td></tr><tr v-else-if="recognitions.length === 0"><td colspan="7" class="text-subtle py-8 text-center">当前筛选下没有项目</td></tr>
              </tbody>
            </table>
          </div>
          <footer class="semantic-divider mt-4 flex flex-wrap items-center justify-between gap-3 border-t pt-4 text-sm">
            <span class="text-subtle">显示 {{ catalogRange.start }}–{{ catalogRange.end }}，共 {{ catalogTotal }} 个作品</span>
            <div class="flex items-center gap-2"><button type="button" class="btn-secondary" :disabled="catalogLoading || catalogPage <= 1" @click="changeCatalogPage(catalogPage - 1)">上一页</button><span>第 {{ catalogPage }} / {{ catalogPages }} 页</span><button type="button" class="btn-secondary" :disabled="catalogLoading || catalogPage >= catalogPages" @click="changeCatalogPage(catalogPage + 1)">下一页</button></div>
          </footer>
        </section>
        <form v-else-if="editDraft" id="library-panel-settings" class="panel mt-4" role="tabpanel" aria-labelledby="library-tab-settings" @submit.prevent="saveLibrary"><div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3"><div><label class="label">名称</label><input v-model="editDraft.name" class="input" required maxlength="128" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" /></div><div><label class="label">来源 Storage</label><select v-model.number="editDraft.storage_id" class="input" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)"><option v-for="storage in storages" :key="storage.id" :value="storage.id">{{ storage.name }}</option></select></div><div><label class="label">分类 Profile</label><select v-model.number="editDraft.profile_id" class="input" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)"><option v-for="profile in profiles" :key="profile.id" :value="profile.id">{{ profile.name }} · r{{ profile.revision }}</option></select></div><div class="md:col-span-2 xl:col-span-3"><label class="label">来源目录</label><div class="flex gap-2"><input class="input font-mono" :value="editDraft.source_path" readonly /><button v-if="editStorage && auth.can(Permissions.MediaLibrariesUpdate) && auth.can(Permissions.StoragesBrowse)" type="button" class="btn-secondary" @click="openPicker('edit', 'source')">重新选择</button></div><p v-if="editDraft.source_path" class="text-subtle mb-0 mt-2 text-xs">实际可读位置如上；数据库保存 Storage 相对根 {{ editDraft.relative_root || '/' }}，其中 / 表示该 Storage 根目录。</p><p v-else class="semantic-warning-text mb-0 mt-2 text-xs">更换 Storage 后必须通过目录选择器重新选择其范围内的来源根。</p></div><label class="text-muted flex items-center gap-3 text-sm"><input v-model="editDraft.enabled" type="checkbox" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" />启用媒体库</label><label class="text-muted flex items-center gap-3 text-sm"><input v-model="editDraft.recursive" type="checkbox" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" />递归扫描</label><label class="text-muted flex items-center gap-3 text-sm"><input v-model="editDraft.metadata_artifacts_enabled" type="checkbox" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" />生成 NFO / 图片元数据</label><template v-if="supportsSTRM(editStorage)"><label class="text-muted flex items-center gap-3 text-sm"><input v-model="editDraft.strm_enabled" type="checkbox" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" @change="normalizeSTRM(editDraft!, editStorage)" />启用 signed 302 / STRM</label><div v-if="editDraft.strm_enabled" class="md:col-span-2 xl:col-span-3"><label class="label">本地 STRM 输出目录</label><div class="flex gap-2"><input class="input" :value="editDraft.strm_local_path" readonly required /><button type="button" class="btn-secondary" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" @click="openPicker('edit', 'strm')">重新选择</button></div></div></template><label v-if="supportsSidecarUpload(editStorage) && !editDraft.strm_enabled" class="text-muted flex items-center gap-3 text-sm"><input v-model="editDraft.upload_sidecars" type="checkbox" :disabled="!auth.can(Permissions.MediaLibrariesUpdate) || !editDraft.metadata_artifacts_enabled" />将 NFO / JPG 上传到云端媒体旁</label></div><MediaLibrarySettingsFields v-model="editDraft" class="mt-5" :disabled="!auth.can(Permissions.MediaLibrariesUpdate)" :storage-type="editStorage?.type" /><div class="mt-5 flex flex-wrap items-center gap-2"><button v-if="auth.can(Permissions.MediaLibrariesUpdate)" class="btn-primary" :disabled="saving || !editDirty || !editFormValid">{{ saving ? '正在保存…' : '保存配置' }}</button><RouterLink class="btn-secondary" to="/system/media-rules">管理分类规则</RouterLink><p v-if="editSaveFeedback.message" class="mb-0 basis-full text-sm" :class="editSaveFeedback.state === 'error' ? 'text-[var(--danger)]' : editSaveFeedback.state === 'success' ? 'semantic-success-text' : 'text-muted'" aria-live="polite">{{ editSaveFeedback.message }}</p></div></form>
      </main>
    </div>

    <DirectoryPickerDialog :open="pickerOpen" :storage-id="pickerTarget === 'source' ? activeDraft?.storage_id : null" :restrict-to-storage="pickerTarget === 'source'" @close="pickerOpen = false" @select="directorySelected" />
    <MediaReorganizationDialog v-if="reorganizationTarget" :open="true" :transfer-task-id="reorganizationTarget.transfer.transfer_task_id" :download-task-id="reorganizationTarget.transfer.download_task_id" :display-name="reorganizationTarget.work.title" :current-title="reorganizationTarget.work.title" :current-media-type="reorganizationTarget.work.kind === 'movie' ? 'movie' : 'tv'" @close="reorganizationTarget = null" @queued="catalogReorganizationQueued" />
    <div v-if="structureOpen" class="fixed inset-0 z-80 grid place-items-center bg-black/65 p-4" @click.self="!saving && (structureOpen = false)">
      <section class="panel max-h-[90vh] w-full max-w-6xl overflow-y-auto" role="dialog" aria-modal="true" aria-labelledby="structure-dialog-title">
        <header class="flex items-start justify-between gap-4"><div><h2 id="structure-dialog-title" class="m-0 text-xl">目录诊断与整理</h2><p class="page-description mt-1 text-sm">诊断全程只读，不会移动文件。这里只显示需要决定或可以整理的问题；正常文件和缺集提示不会混进列表。任何移动或回收都要预览后再次确认。</p></div><button class="btn-secondary" type="button" :disabled="saving" @click="structureOpen = false">关闭</button></header>
        <div v-if="structureLoading" class="py-12 text-center text-muted">正在安排后台目录结构诊断…</div>
        <template v-else-if="structureDiagnostics">
          <div class="semantic-inset mt-4 flex flex-wrap items-center justify-between gap-3 p-3 text-sm"><div><strong>{{ structureRecognitionInProgress ? '目录结构初步检查完成 · 等待识别结果' : structureStatusLabel(structureDiagnostics.status) }}</strong><span v-if="structureDiagnostics.status === 'queued' || structureDiagnostics.status === 'running'" class="text-subtle ml-2">{{ structureDiagnostics.processed_items }} / {{ structureDiagnostics.total_items }} 已检查</span><span v-if="structureDiagnostics.scan_run_id" class="text-subtle ml-2">扫描 #{{ structureDiagnostics.scan_run_id }} · Generation {{ structureDiagnostics.generation }}</span></div><button v-if="structureDiagnostics.status === 'healthy' || structureDiagnostics.status === 'issues'" type="button" class="btn-secondary" :disabled="saving || structureLoading" @click="openStructureDiagnostics">重新检查</button></div>

          <p v-if="structureRecognitionInProgress" class="semantic-inset mt-3 p-3 text-sm">后台元数据识别尚未完成；等待中的媒体不会计入“需要处理”，识别完成后会执行本次来源版本的收敛检查。</p>

          <div class="mt-4 grid gap-3 sm:grid-cols-3">
            <div class="semantic-inset p-3"><span class="text-subtle text-xs">可整理</span><strong class="mt-1 block">{{ structureDiagnostics.repairable_count }}</strong><small class="text-subtle">选择后生成安全预览</small></div>
            <div class="semantic-inset p-3"><span class="text-subtle text-xs">需要决定</span><strong class="mt-1 block">{{ structureAttentionCount }}</strong><small class="text-subtle">识别失败、规则或目标冲突</small></div>
            <div class="semantic-inset p-3"><span class="text-subtle text-xs">已检查</span><strong class="mt-1 block">{{ structureDiagnostics.processed_items }} / {{ structureDiagnostics.total_items }}</strong><small class="text-subtle">诊断没有移动文件</small></div>
          </div>

          <div v-if="structureDiagnostics.status === 'issues'" class="mt-4 flex flex-wrap items-center gap-2">
            <button type="button" class="btn-secondary" @click="setStructureIssueFilter('all')">全部问题</button>
            <button v-if="structureDiagnostics.classifications.unrecognized" type="button" class="btn-secondary" @click="openUnrecognizedIssues">识别失败或无匹配 {{ structureDiagnostics.classifications.unrecognized }} · 去手动整理</button>
            <button v-if="structureDiagnostics.classifications.invalid_path" type="button" class="btn-secondary" @click="setStructureIssueFilter('invalid_path')">非法路径 {{ structureDiagnostics.classifications.invalid_path }}</button>
            <RouterLink v-if="structureDiagnostics.classifications.template_unavailable" class="btn-secondary" to="/system/media-rules" @click="structureOpen = false">模板问题 {{ structureDiagnostics.classifications.template_unavailable }} · 去规则管理</RouterLink>
            <button v-if="structureDiagnostics.classifications.duplicate_target" type="button" class="btn-secondary" @click="setStructureIssueFilter('duplicate_target')">视频目标冲突 {{ structureDiagnostics.classifications.duplicate_target }}</button>
            <button v-if="structureDiagnostics.classifications.sidecar_target_conflict" type="button" class="btn-secondary" @click="setStructureIssueFilter('sidecar_target_conflict')">伴随文件冲突 {{ structureDiagnostics.classifications.sidecar_target_conflict }}</button>
          </div>

          <div v-if="structureDiagnostics.status === 'issues' && auth.can(Permissions.MediaLibrariesScan)" class="semantic-success mt-4 p-3 text-sm">
            <div class="flex flex-wrap items-center justify-between gap-3"><div><strong>处理草稿不会立即改文件</strong><p class="mb-0 mt-1 text-xs">跨页选择会一直保留；可以只预览并提交当前问题类型。</p></div><div class="flex flex-wrap gap-2"><button class="btn-secondary" type="button" @click="setStructureBulkConflictAction('keep_recommended')">全部冲突按推荐保留</button><button class="btn-secondary" type="button" @click="setStructureBulkConflictAction('skip')">全部冲突跳过</button></div></div>
            <p v-if="structureBulkConflictAction" class="mb-0 mt-2 text-xs">跨页批量草稿：{{ structureBulkConflictAction === 'keep_recommended' ? '全部冲突按唯一推荐来源保留；无法唯一推荐的自动跳过' : '全部冲突跳过' }}。单项选择优先于批量草稿。</p>
            <div class="mt-3 flex flex-wrap items-center gap-2"><button class="btn-secondary" type="button" :disabled="structureSelectionLoading || saving" @click="previewStructureSelections(false)">{{ structureSelectionLoading ? '正在生成预览…' : '预览全部已选操作' }}</button><button v-if="structureIssueFilter !== 'all'" class="btn-secondary" type="button" :disabled="structureSelectionLoading || saving" @click="previewStructureSelections(true)">只预览并提交当前类型</button><button v-if="structureSelectionPreview" class="btn-primary" type="button" :disabled="saving" @click="repairStructureSelections">确认执行预览</button></div>
            <p v-if="structureSelectionPreview" class="mb-0 mt-3 text-xs">预览已冻结：{{ structureSelectionPreview.issue_count }} 项，{{ structureSelectionPreview.move_count }} 个整理动作，{{ structureSelectionPreview.recycle_count }} 个文件进入回收站，{{ structureSelectionPreview.skipped_count }} 项跳过。若目录或来源版本变化，Server 会拒绝执行。</p>
          </div>
          <p v-if="structureSelectionError" role="alert" class="semantic-error mt-3 p-3 text-sm">处理草稿预览失败：{{ structureSelectionError }}</p>
          <p v-if="structureAttentionCount" class="semantic-warning mt-4 p-3 text-sm">冲突项不会自动执行。选择“保留推荐/指定来源”时，落选文件只会进入可恢复回收站或受管回收目录：115 使用网盘回收站并沿用该连接的定时清空配置，本地文件保留在媒体库内的受管回收目录；不支持可恢复回收的来源会被 Server 拒绝，绝不会永久删除。</p>
          <p v-if="structureDiagnostics.status === 'failed'" class="semantic-error mt-4 p-3 text-sm">目录结构诊断发生系统错误。媒体目录仍然可用，并且没有移动任何文件。</p>

          <div v-if="structureDiagnostics.status === 'healthy' || structureDiagnostics.status === 'issues'" class="mt-4">
            <div class="mb-2 flex flex-wrap items-center justify-between gap-2 text-xs"><div><strong>完整问题列表</strong><span class="text-subtle ml-2">{{ structureIssueTotal }} 项 · 已选择 {{ structureSelectedCount }} 项</span></div><label class="flex items-center gap-2">筛选<select class="input py-1" :value="structureIssueFilter" @change="setStructureIssueFilter(($event.target as HTMLSelectElement).value)"><option value="all">全部</option><option value="path_mismatch">目录命名</option><option value="cloud_transfer_root_misplaced">网盘根目录错位</option><option value="media_unrecognized">未识别</option><option value="recognition_suspect_conflict">识别冲突</option><option value="catalog_duplicate_conflict">目录事实重复</option><option value="duplicate_target">视频目标冲突</option><option value="sidecar_target_conflict">伴随文件冲突</option><option value="invalid_path">非法路径</option><option value="template_unavailable">模板问题</option></select></label></div>
            <p v-if="structureIssuesError" role="alert" class="semantic-error p-3 text-sm">问题列表加载失败：{{ structureIssuesError }}</p>
            <div class="max-h-[32rem] overflow-auto"><table class="semantic-table w-full text-left text-xs"><thead><tr><th>类型 / 状态</th><th>作品身份</th><th>问题</th><th>当前路径 / 冲突来源</th><th>期望路径</th><th>本次操作</th></tr></thead><tbody><tr v-for="issue in structureIssues" :key="issue.token"><td><div>{{ issue.kind === 'video' ? '视频' : '伴随文件' }}</div><small class="text-subtle">{{ structureIssueStateLabel(issue) }}</small></td><td><strong>{{ issue.title || '尚未识别' }}</strong><div v-if="issue.state === 'manual_identity_resolved'" class="mt-1 space-y-1"><span class="status-chip status-chip--ready">已人工识别</span><div>{{ issue.media_type === 'tv' ? '剧集' : '电影' }}<template v-if="issue.release_year"> · {{ issue.release_year }}</template><template v-if="issue.tmdb_id"> · TMDB {{ issue.tmdb_id }}</template></div><small v-if="issue.poster_path" class="text-subtle">已保存 TMDB 海报</small><small class="semantic-warning-text block">身份已保存，文件尚未整理</small></div></td><td>{{ structureIssueLabel(issue.code) }}</td><td class="min-w-64 break-all"><div v-if="issue.current_path" class="font-mono">{{ issue.current_path }}</div><div v-if="issue.members.length" class="semantic-inset mt-2 space-y-2 p-2"><strong class="font-sans">同一目标的全部来源（{{ issue.members.length }}）</strong><div v-for="member in issue.members" :key="member.token" class="flex items-center justify-between gap-2"><span class="font-mono">{{ member.source_path }}</span><button v-if="isStructureConflict(issue) && auth.can(Permissions.MediaLibrariesScan)" class="btn-secondary shrink-0" type="button" @click="setStructureSelection(issue, 'keep_member', member.token)">保留这一份</button><span v-if="member.recommended" class="status-chip status-chip--ready">推荐</span></div></div></td><td class="min-w-56 break-all font-mono">{{ issue.expected_path || '—' }}</td><td><div v-if="auth.can(Permissions.MediaLibrariesScan)" class="flex min-w-40 flex-col gap-2"><template v-if="isStructureConflict(issue)"><button v-if="issue.recommended_member_token" class="btn-secondary" type="button" @click="setStructureSelection(issue, 'keep_recommended')">按推荐保留</button><button class="btn-secondary" type="button" @click="setStructureSelection(issue, 'keep_all_versions')">全部保留为版本</button><button class="btn-secondary" type="button" @click="setStructureSelection(issue, 'skip')">本次跳过</button></template><template v-else-if="issue.repairable || issue.state === 'manual_identity_resolved'"><button class="btn-secondary" type="button" @click="setStructureSelection(issue, 'repair')">选择整理</button><button class="btn-secondary" type="button" @click="setStructureSelection(issue, 'skip')">本次跳过</button></template><button v-else-if="issue.code === 'media_unrecognized'" type="button" class="btn-secondary" @click="openUnrecognizedIssues">去手动识别</button><button v-else-if="issue.code === 'recognition_suspect_conflict'" type="button" class="btn-secondary" @click="openRecognitionReview">核对并修正识别</button><RouterLink v-else-if="issue.code === 'template_unavailable'" class="btn-secondary" to="/system/media-rules" @click="structureOpen = false">去规则管理</RouterLink></div><span v-else>{{ structureIssueAction(issue) }}</span><small v-if="structureSelectionDraft[issue.token]" class="semantic-success-text mt-2 block">已加入草稿：{{ structureSelectionLabel(structureSelectionDraft[issue.token]!.action) }}</small></td></tr><tr v-if="!structureIssuesLoading && structureIssues.length === 0"><td colspan="6" class="py-8 text-center text-muted">当前筛选没有需要处理的问题</td></tr><tr v-if="structureIssuesLoading"><td colspan="6" class="py-8 text-center text-muted">正在加载完整问题列表…</td></tr></tbody></table></div>
            <footer class="mt-3 flex flex-wrap items-center justify-between gap-3 text-xs"><label>每页 <select v-model.number="structureIssuePageSize" class="input py-1" @change="loadStructureIssues(selected?.id, true)"><option :value="50">50</option><option :value="100">100</option><option :value="200">200</option></select> 项</label><div class="flex items-center gap-2"><button type="button" class="btn-secondary" :disabled="structureIssuesLoading || structureIssuePage <= 1" @click="changeStructureIssuePage(structureIssuePage - 1)">上一页</button><span>第 {{ structureIssuePage }} / {{ structureIssuePages }} 页</span><button type="button" class="btn-secondary" :disabled="structureIssuesLoading || structureIssuePage >= structureIssuePages" @click="changeStructureIssuePage(structureIssuePage + 1)">下一页</button></div></footer>
          </div>
          <footer class="semantic-divider mt-4 flex flex-wrap justify-end gap-3 border-t pt-4"><button class="btn-secondary" type="button" :disabled="saving" @click="structureOpen = false">关闭</button><button v-if="structureSelectionPreview && auth.can(Permissions.MediaLibrariesScan)" class="btn-primary" type="button" :disabled="saving" @click="repairStructureSelections">确认执行：整理 {{ structureSelectionPreview.move_count }} · 回收 {{ structureSelectionPreview.recycle_count }}</button></footer>
        </template>
      </section>
    </div>
  </section>
</template>
