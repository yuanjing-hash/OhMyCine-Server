<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import SharePreviewDialog from '@/components/SharePreviewDialog.vue'
import { shareValidationLabel, type ShareSelection, type ShareValidation } from '@/share-preview'
import { previewDownloadRoutes, recommendDownloadRoute, routeTargetByID, type DownloadRouteChoice, type DownloadRoutePreview, type DownloadRouteRecommendation } from '@/download-routes'
import { formatBytes, isCloudShareSource, cloudShareDownloaderType } from '@/downloads'
import { useSearchResultRecognition } from '@/search-result-recognition'
import { useSiteSearch } from '@/use-site-search'
import { searchDuration, searchIssueLabel, searchPhaseLabel } from '@/search-progress'
import { useAuthStore } from '@/stores/auth'
import { notify } from '@/toast'
import { buildDiscoveryMediaSearchPath, discoveryDetailRoute, mediaIdentitySearchURL, type DiscoveryMediaSearch, type DiscoveryMediaSearchFilter, type DiscoveryMediaType, type DiscoverySearchName, type DiscoveryWork } from '@/discovery'
import { discoveryDownloadsPath, discoverySearchOptionsPath, filterAndSortTorrentResults, ptRecognitionEpisodeLabel, ptRecognitionErrorLabel, ptRecognitionSpecLabels, readTorrentSearchSession, readTorrentSearchSiteSelection, saveTorrentSearchSession, saveTorrentSearchSiteSelection, sitesPath, torrentRecognitionCandidatesPath, torrentRecognitionOverridePath, torrentSearchPath, torrentSearchStreamPath, torrentSearchURL, torrentSearchSessionKey, type PTRecognitionCandidate, type SearchSiteOption, type SiteSummary, type TorrentRecognitionResult, type TorrentResultDirection, type TorrentSearchGroup, type TorrentSearchProgress, type TorrentSearchResult, type TorrentSearchSession } from '@/sites'
import type { DownloaderSummary, ListResponse, MediaLibraryDetail } from '@/types/api'
import { discoveryOrigins, discoveryResultLinkPath, discoveryTypeOptions, resultTypeLabel, safeDiscoveryLink, type DiscoveryTypePolicy } from '@/sites'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const mode = ref<'media' | 'resources'>(route.query.mode === 'resources' || typeof route.query.site_id === 'string' ? 'resources' : 'media')
const mediaQuery = ref(typeof route.query.query === 'string' ? route.query.query : '')
const mediaFilter = ref<DiscoveryMediaSearchFilter>(route.query.media_type === 'movie' || route.query.media_type === 'tv' ? route.query.media_type : 'all')
const mediaResults = ref<DiscoveryWork[]>([])
const mediaPage = ref(1)
const mediaTotalPages = ref(1)
const mediaSearching = ref(false)
const mediaSearched = ref(false)
const mediaError = ref('')
const keyword = ref(typeof route.query.title === 'string' ? route.query.title : '')
const mediaType = ref(typeof route.query.media_type === 'string' ? route.query.media_type : '')
const year = ref<number | undefined>(typeof route.query.year === 'string' ? Number(route.query.year) || undefined : undefined)
const tmdbID = ref<number | undefined>(typeof route.query.tmdb_id === 'string' ? Number(route.query.tmdb_id) || undefined : undefined)
const searchBy = ref<'title' | 'tmdb_id'>(route.query.search_by === 'tmdb_id' ? 'tmdb_id' : 'title')
const identityNames = ref<DiscoverySearchName[]>([])
const selectedTitle = computed(() => typeof route.query.title === 'string' ? route.query.title : '')
const lockedSiteID = computed(() => {
  const value = typeof route.query.site_id === 'string' ? Number(route.query.site_id) : 0
  return Number.isInteger(value) && value > 0 ? value : undefined
})
const lockedSite = ref<SiteSummary | null>(null)
const groups = ref<TorrentSearchGroup[]>([])
const searching = ref(false)
const searchError = ref('')
const searched = ref(false)
const searchProgress = ref<TorrentSearchProgress | null>(null)
const siteSelectorOpen = ref(false)
const siteOptionsLoading = ref(false)
const siteOptionsError = ref('')
const siteOptions = ref<SearchSiteOption[]>([])
const sourceStatusReady = ref(false)
const restoredResults = ref(false)
const panSouTypePolicy = ref<'default' | DiscoveryTypePolicy>('default')
const panSouCloudTypes = ref<string[]>(['115', 'quark', 'magnet'])
const appliedTypePolicy = ref<DiscoveryTypePolicy>()
const appliedCloudTypes = ref<string[]>()
const copyingToken = ref('')
let availabilityRequest: AbortController | null = null
let copyRequest: AbortController | null = null
const selectedSiteIDs = ref<number[]>([])
const activeSearchSiteIDs = ref<number[]>([])
const downloaders = ref<DownloaderSummary[]>([])
const libraries = ref<MediaLibraryDetail[]>([])
const downloadDialog = ref<TorrentSearchResult | null>(null)
const sharePreviewDialog = ref<TorrentSearchResult | null>(null)
const shareValidations = ref<Record<string, ShareValidation>>({})
const validationClock = ref(Date.now())
let validationTimer: number | undefined
function rememberShareValidation(token: string, validation: ShareValidation) {
 const recent = Object.entries(shareValidations.value).filter(([, v]) => Date.parse(v.expires_at) > Date.now()).slice(-999)
 shareValidations.value = { ...Object.fromEntries(recent), [token]: validation }
}
function shareValidation(item: TorrentSearchResult) { return shareValidations.value[item.token] ?? item.share_validation }
const shareSelection = ref<ShareSelection | null>(null)
const downloadForm = ref({ downloaderID: '', mediaLibraryID: 0, priority: 0 })
const downloadSiteID = ref<number | undefined>()
const routePreview = ref<DownloadRoutePreview | null>(null)
const routePreviewLoading = ref(false)
let routePreviewRequest: AbortController | null = null
const routeRecommendation = ref<DownloadRouteRecommendation | null>(null)
const routeChoices = ref<DownloadRouteChoice[]>([])
const routeRecommendationLoading = ref(false)
let routeRecommendationRequest: AbortController | null = null
const submitting = ref(false)
const manualDialog = ref<TorrentSearchResult | null>(null)
const manualForm = ref<{ keyword: string; mediaType: '' | 'movie' | 'tv'; year?: number }>({ keyword: '', mediaType: '' })
const manualCandidates = ref<PTRecognitionCandidate[]>([])
const selectedManualCandidate = ref<PTRecognitionCandidate | null>(null)
const manualSearching = ref(false)
const manualSaving = ref(false)
let manualRequestGeneration = 0
let manualCandidatesRequest: AbortController | null = null
let interactionGeneration = 0
let previewOpenGeneration = 0
let downloadOpenGeneration = 0
const downloadOptionRequests = new Set<AbortController>()
let mediaSearchRequest: AbortController | null = null

const enabledDownloaders = computed(() => {
  const compatible = new Set(routeChoices.value.filter(choice => choice.media_library_id === downloadForm.value.mediaLibraryID && choice.enabled).map(choice => choice.downloader_id))
  return downloaders.value.filter(item => item.enabled && compatible.has(item.id))
})
function choiceAppliesToSource(choice: DownloadRouteChoice) {
  const source = routeRecommendation.value?.source_kind
  return source === 'pt' ? choice.downloader_type === 'qbittorrent' : isCloudShareSource(source) ? choice.downloader_type === cloudShareDownloaderType(source) : true
}
const targetChoices = computed(() => {
  const byLibrary = new Map<number, DownloadRouteChoice>()
  const relevant = routeChoices.value.filter(choiceAppliesToSource)
  for (const choice of relevant.length ? relevant : routeChoices.value) {
    const previous = byLibrary.get(choice.media_library_id)
    if (!previous || !previous.enabled && choice.enabled) byLibrary.set(choice.media_library_id, choice)
  }
  return [...byLibrary.values()]
})
const selectedRoute = computed(() => routeTargetByID(routePreview.value, downloadForm.value.mediaLibraryID))
const selectedChoice = computed(() => routeChoices.value.find(choice => choice.media_library_id === downloadForm.value.mediaLibraryID && choice.downloader_id === downloadForm.value.downloaderID))
const routeFailure = computed(() => {
  if (!routeRecommendation.value || selectedRoute.value?.enabled) return ''
  return selectedRoute.value?.reason_message || selectedChoice.value?.reason_message || routeRecommendation.value.choices.find(choice => choiceAppliesToSource(choice) && !choice.enabled)?.reason_message || '当前没有可执行的下载与入库路线'
})
const selectedLibrary = computed(() => libraries.value.find(item => item.id === selectedRoute.value?.media_library_id) ?? null)
const selectableSiteOptions = computed(() => siteOptions.value.filter(item => item.searchable))
const activeChannel = ref<'all' | number>('all')
const enabledSiteTypes = ref<Array<'pt' | 'bt' | 'bt_resource' | 'cloud_share'>>(['pt', 'bt', 'bt_resource', 'cloud_share'])
const resolutionFilter = ref('')
const promotionFilter = ref('')
const minimumSeeders = ref<number | undefined>()
const resultSort = ref<'seeders' | 'published' | 'size'>(lockedSiteID.value ? 'published' : 'seeders')
const resultDirection = ref<TorrentResultDirection>('desc')

const orderedGroups = computed(() => [...groups.value].sort((left, right) => left.site_id - right.site_id))
function siteIsRunning(siteID: number) { return searching.value && Boolean(searchActivities.value[siteID]) && searchActivities.value[siteID].phase !== 'finished' }
const failedGroups = computed(() => orderedGroups.value.filter(group => (group.status === 'error' || group.partial || group.complete === false) && !siteIsRunning(group.site_id)))
const searchProgressPercent = computed(() => {
  const progress = searchProgress.value
  if (!progress?.total) return 0
  return Math.min(100, Math.round(progress.completed / progress.total * 100))
})
const activeGroup = computed(() => typeof activeChannel.value === 'number' ? groups.value.find(group => group.site_id === activeChannel.value) ?? null : null)
const resolutionOptions = computed(() => [...new Set(groups.value.flatMap(group => group.items.map(item => item.specifications?.resolution || item.quality || '').filter(Boolean)))].sort())
const visibleResults = computed(() => filterAndSortTorrentResults(groups.value, {
  activeChannel: activeChannel.value,
  enabledSiteTypes: enabledSiteTypes.value,
  resolution: resolutionFilter.value,
  promotion: promotionFilter.value,
  minimumSeeders: minimumSeeders.value,
  sort: resultSort.value,
  direction: resultDirection.value,
}))
const recognitionItems = computed(() => {
  if (mode.value !== 'resources') return []
  const visible = visibleResults.value.map(entry => entry.item)
  const visibleTokens = new Set(visible.map(item => item.token))
  return [...visible, ...groups.value.flatMap(group => group.items).filter(item => !visibleTokens.has(item.token))]
})
const resultRecognition = useSearchResultRecognition(recognitionItems, () => auth.can(Permissions.MediaLibrariesRead))
const { recognitions, recognitionErrors, recognizingTokens, libraryStates } = resultRecognition
const trustedIdentity = computed(() => mode.value === 'resources' && route.query.identity === 'tmdb' && (mediaType.value === 'movie' || mediaType.value === 'tv') && tmdbID.value != null && tmdbID.value > 0
  ? { mediaType: mediaType.value as DiscoveryMediaType, tmdbID: tmdbID.value }
  : null)
const initialResourceSearch = Boolean(trustedIdentity.value || keyword.value.trim() || searchBy.value === 'tmdb_id' && tmdbID.value)
let submittedSearchInput: TorrentSearchSession['input'] | null = null
let submittedIdentity: { mediaType: DiscoveryMediaType; tmdbID: number } | null = null

const searchRunner = useSiteSearch({
  groups, progress: searchProgress, error: searchError, searching, searched,
  scope: () => lockedSiteID.value ? [lockedSiteID.value] : activeSearchSiteIDs.value,
  siteName: id => groups.value.find(group => group.site_id === id)?.site_name || lockedSite.value?.name || siteOptions.value.find(site => site.id === id)?.name || `站点 #${id}`,
  url: (stream, scope, page, refresh) => {
    const identity = submittedIdentity
    const siteID = scope.length === 1 ? scope[0] : undefined
    const input = { ...searchInput(siteID, page, refresh), siteID, siteIDs: siteID ? undefined : scope }
    if (identity) return mediaIdentitySearchURL(identity.mediaType, identity.tmdbID, input, stream)
    return torrentSearchURL(stream ? torrentSearchStreamPath : torrentSearchPath, input)
  },
  media: value => {
    if (typeof value !== 'object' || value === null) return
    const names: unknown = Reflect.get(value, 'query_names')
    if (Array.isArray(names)) identityNames.value = names.filter((name): name is DiscoverySearchName => typeof name === 'object' && name !== null && typeof Reflect.get(name, 'value') === 'string' && typeof Reflect.get(name, 'kind') === 'string')
  },
})
const { activities: searchActivities, retainedSiteIDs, requestedPages, outcome: searchOutcome } = searchRunner
const searchActivityRows = computed(() => Object.values(searchActivities.value).sort((left, right) => left.site_id - right.site_id))
const searchOutcomeLabel = computed(() => ({ idle: '', running: '正在搜索', complete: '搜索完成', partial: '搜索未完整完成', cancelled: '搜索已取消', failed: '搜索失败', retained: '上次保留的结果' })[searchOutcome.value])
function groupWarning(group: TorrentSearchGroup) {
  if (retainedSiteIDs.value.includes(group.site_id)) return `${siteIsRunning(group.site_id) ? '正在重新搜索，暂时展示上次结果' : '本次查询未完成，保留上次结果'}（${group.items.length} 条）`
  return `${searchIssueLabel(group.incomplete_reason || group.error_code)}；已保留 ${group.items.length} 条结果`
}
function isPreviousPage(group: TorrentSearchGroup) { return requestedPages.value[group.site_id] !== undefined && requestedPages.value[group.site_id] !== group.page }
function activityLabel(siteID: number, phase: string) {
  const group = groups.value.find(group => group.site_id === siteID)
  return phase === 'finished' ? group?.partial ? '部分查询未完成' : group?.status === 'error' ? '查询失败' : searchOutcome.value === 'cancelled' ? '已取消' : '查询结束' : searchPhaseLabel(phase)
}

async function searchMedia(page = 1) {
  const query = mediaQuery.value.trim()
  if (!query) { notify('请输入电影或剧集名称', 'warning'); return }
  mediaSearchRequest?.abort()
  const controller = new AbortController()
  mediaSearchRequest = controller
  mediaSearching.value = true
  mediaError.value = ''
  try {
    const result = await api<DiscoveryMediaSearch>(buildDiscoveryMediaSearchPath(query, mediaFilter.value, page), { signal: controller.signal })
    mediaResults.value = result.items
    mediaPage.value = result.page
    mediaTotalPages.value = result.total_pages
    mediaSearched.value = true
    await router.replace({ path: '/discovery/explore', query: { query, media_type: mediaFilter.value === 'all' ? undefined : mediaFilter.value } })
  } catch (reason) {
    if (!controller.signal.aborted) mediaError.value = message(reason)
  } finally {
    if (mediaSearchRequest === controller) {
      mediaSearchRequest = null
      mediaSearching.value = false
    }
  }
}

function openMedia(work: DiscoveryWork) { void router.push(discoveryDetailRoute(work)) }
function switchMode(value: 'media' | 'resources') {
  if (value === 'media') {
    searchRunner.reset()
    searching.value = false
  } else {
    mediaSearchRequest?.abort()
    mediaSearchRequest = null
    mediaSearching.value = false
  }
  mode.value = value
  if (value === 'media') void router.replace({ path: '/discovery/explore' })
  else void router.replace({ path: '/discovery/explore', query: { mode: 'resources' } })
}

function searchInput(siteID?: number, page = 1, refresh = false) {
  const fixedSiteID = lockedSiteID.value ?? siteID
  const input = submittedSearchInput ?? currentSearchInput()
  return { keyword: input.keyword, mediaType: input.mediaType || undefined, year: input.year, tmdbID: input.tmdbID, searchBy: input.searchBy, page, siteID: fixedSiteID, siteIDs: fixedSiteID ? undefined : activeSearchSiteIDs.value, typePolicy: input.typePolicy, cloudTypes: input.cloudTypes, snapshotID: !refresh && fixedSiteID ? groups.value.find(group => group.site_id === fixedSiteID)?.snapshot_id : undefined, refresh }
}

function cancelSearch() { searchRunner.cancel() }

async function openSiteSelector() {
  siteSelectorOpen.value = true
  siteOptionsLoading.value = true
  siteOptionsError.value = ''
  try {
    const response = await api<ListResponse<SearchSiteOption>>(discoverySearchOptionsPath)
    siteOptions.value = response.list
    selectedSiteIDs.value = readTorrentSearchSiteSelection(typeof localStorage === 'undefined' ? undefined : localStorage, response.list)
  } catch (reason) {
    siteOptions.value = []
    selectedSiteIDs.value = []
    siteOptionsError.value = message(reason)
  } finally { siteOptionsLoading.value = false }
}

function selectAllSites() { selectedSiteIDs.value = selectableSiteOptions.value.map(item => item.id) }
function clearSelectedSites() { selectedSiteIDs.value = [] }

function confirmSiteSelection() {
  const allowed = new Set(selectableSiteOptions.value.map(item => item.id))
  const selected = [...new Set(selectedSiteIDs.value.filter(id => allowed.has(id)))]
  if (selected.length === 0) { notify('请至少选择一个可搜索站点', 'warning'); return }
  saveTorrentSearchSiteSelection(typeof localStorage === 'undefined' ? undefined : localStorage, selected)
  siteSelectorOpen.value = false
  executeSearch(selected)
}

function search() {
  if (!trustedIdentity.value && searchBy.value === 'title' && !keyword.value.trim()) { notify('请输入作品或发行标题', 'warning'); return }
  if (!trustedIdentity.value && searchBy.value === 'tmdb_id' && (!tmdbID.value || !mediaType.value)) { notify('TMDB ID 搜索需要有效 ID 与媒体类型', 'warning'); return }
  if (!lockedSiteID.value) { void openSiteSelector(); return }
  executeSearch([lockedSiteID.value])
}

function executeSearch(siteIDs: number[]) {
  if (panSouTypePolicy.value === 'manual' && !panSouCloudTypes.value.length) { notify('请至少选择一种盘搜结果类型', 'warning'); return }
  appliedTypePolicy.value = panSouTypePolicy.value === 'default' ? undefined : panSouTypePolicy.value
  appliedCloudTypes.value = panSouTypePolicy.value === 'manual' ? [...panSouCloudTypes.value] : undefined
  searchRunner.reset()
  previewOpenGeneration++
  activeSearchSiteIDs.value = [...siteIDs]
  submittedSearchInput = currentSearchInput()
  submittedIdentity = trustedIdentity.value ? { ...trustedIdentity.value } : null
  groups.value = []
  restoredResults.value = false
  void refreshSourceAvailability()
  activeChannel.value = lockedSiteID.value ?? 'all'
  resultRecognition.reset()
  identityNames.value = []
  searchError.value = ''
  searched.value = false
  manualRequestGeneration++
  manualCandidatesRequest?.abort()
  manualDialog.value = null
  manualSearching.value = false
  manualSaving.value = false
  searchRunner.start(siteIDs, 1, true)
}

function retrySite(group: TorrentSearchGroup) { searchRunner.start([group.site_id], 1, true) }
function nextPage(group: TorrentSearchGroup) { if (group.page < 20) searchRunner.start([group.site_id], group.page + 1) }
function previousPage(group: TorrentSearchGroup) { if (group.page > 1) searchRunner.start([group.site_id], group.page - 1) }

async function loadDownloadOptions() {
  const generation = interactionGeneration
  const controller = new AbortController()
  downloadOptionRequests.add(controller)
  const requests: Promise<void>[] = []
  if (auth.can(Permissions.DownloadersRead)) requests.push(api<ListResponse<DownloaderSummary>>('/api/v1/downloaders', { signal: controller.signal }).then(response => { if (!controller.signal.aborted && generation === interactionGeneration) downloaders.value = response.list }))
  if (auth.can(Permissions.MediaLibrariesRead)) requests.push(api<ListResponse<MediaLibraryDetail>>('/api/v1/media-libraries', { signal: controller.signal }).then(response => { if (!controller.signal.aborted && generation === interactionGeneration) libraries.value = response.list }))
  try { await Promise.all(requests) } finally { downloadOptionRequests.delete(controller) }
}

function cancelDownloadOptionRequests() {
  for (const controller of downloadOptionRequests) controller.abort()
  downloadOptionRequests.clear()
}

async function openSharePreview(item: TorrentSearchResult) {
 if (resourceUnavailable(item)) return
 const generation = interactionGeneration
 const openGeneration = ++previewOpenGeneration
 try { await loadDownloadOptions(); if (generation === interactionGeneration && openGeneration === previewOpenGeneration) sharePreviewDialog.value = item }
 catch (reason) { if (generation === interactionGeneration && openGeneration === previewOpenGeneration) notify(message(reason), 'error') }
}
async function acceptShareSelection(selection: ShareSelection) {
 const item = sharePreviewDialog.value
 if (!item) return
 sharePreviewDialog.value = null
 await openDownload(item, selection)
}
async function openDownload(item: TorrentSearchResult, selection: ShareSelection | null = null) {
  if (resourceUnavailable(item)) return
  const generation = interactionGeneration
  const openGeneration = ++downloadOpenGeneration
  routeRecommendationRequest?.abort()
  routeRecommendationRequest = null
  routeRecommendationLoading.value = false
  shareSelection.value = selection
  downloadDialog.value = item
  downloadSiteID.value = groups.value.find(group => group.items.some(candidate => candidate.token === item.token))?.site_id
  downloadForm.value = { downloaderID: selection?.downloaderID ?? '', mediaLibraryID: 0, priority: 0 }
  routePreviewRequest?.abort()
  routePreviewRequest = null
  routePreview.value = null
  routePreviewLoading.value = false
  routeChoices.value = []
  routeRecommendation.value = null
  try { await loadDownloadOptions(); if (generation === interactionGeneration && openGeneration === downloadOpenGeneration && downloadDialog.value?.token === item.token) await loadRouteRecommendation() }
  catch (reason) { if (generation === interactionGeneration && openGeneration === downloadOpenGeneration && downloadDialog.value?.token === item.token) notify(message(reason), 'error') }
}

function expectedRouteBytes() {
  if (shareSelection.value) return shareSelection.value.totalSize > 0 ? shareSelection.value.totalSize : undefined
  const item = downloadDialog.value
  return !isCloudShareSource(item?.source_kind) && item?.size_bytes && item.size_bytes > 0 ? item.size_bytes : undefined
}

async function loadRouteRecommendation(mediaLibraryID?: number) {
  routeRecommendationRequest?.abort()
  routeRecommendationRequest = null
  routePreviewRequest?.abort()
  routePreview.value = null
  routePreviewLoading.value = false
  const item = downloadDialog.value
  if (!item) return
  const controller = new AbortController()
  routeRecommendationRequest = controller
  routeRecommendationLoading.value = true
  try {
    const recommendation = await recommendDownloadRoute(item.token, mediaLibraryID, expectedRouteBytes(), controller.signal)
    if (controller.signal.aborted) return
    routeRecommendation.value = recommendation
    if (!mediaLibraryID) routeChoices.value = recommendation.choices
    if (!mediaLibraryID) downloadForm.value.mediaLibraryID = recommendation.recommended?.media_library_id ?? 0
    downloadForm.value.downloaderID = shareSelection.value?.downloaderID ?? recommendation.recommended?.downloader_id ?? ''
    await loadRoutePreview()
  } catch (reason) {
    if (!controller.signal.aborted) notify(message(reason), 'error')
  } finally {
    if (routeRecommendationRequest === controller) { routeRecommendationRequest = null; routeRecommendationLoading.value = false }
  }
}

async function chooseTarget(mediaLibraryID: number) {
  downloadForm.value.mediaLibraryID = mediaLibraryID
  await loadRouteRecommendation(mediaLibraryID)
}

async function loadRoutePreview() {
  routePreviewRequest?.abort()
  routePreviewRequest = null
  routePreview.value = null
  routePreviewLoading.value = false
  if (!downloadForm.value.downloaderID || !downloadDialog.value) return
  const controller = new AbortController()
  routePreviewRequest = controller
  routePreviewLoading.value = true
  try {
    const preview = await previewDownloadRoutes({
      downloader_id: downloadForm.value.downloaderID,
      source_kind: isCloudShareSource(downloadDialog.value.source_kind) ? downloadDialog.value.source_kind : 'torrent',
      site_id: downloadSiteID.value,
      expected_bytes: expectedRouteBytes(),
    }, controller.signal)
    if (!controller.signal.aborted) routePreview.value = preview
  } catch (reason) {
    if (!controller.signal.aborted) notify(message(reason), 'error')
  } finally {
    if (routePreviewRequest === controller) { routePreviewRequest = null; routePreviewLoading.value = false }
  }
}

async function submitDownload() {
  const item = downloadDialog.value
  const generation = interactionGeneration
  if (!item || !downloadForm.value.downloaderID) { notify('请选择已启用的下载器', 'warning'); return }
  if (!selectedRoute.value?.enabled || !selectedLibrary.value) { notify('请选择一条 Server 已确认可执行的入库路线', 'warning'); return }
  if (shareSelection.value && (shareSelection.value.downloaderID !== downloadForm.value.downloaderID || Date.parse(shareSelection.value.expiresAt) <= Date.now())) { notify('预览已过期或下载器已变化，请重新预览分享', 'warning'); return }
  submitting.value = true
  try {
    const result = await api<{ id: string; selection_tasks?: { id: string }[]; selection_pending?: number; selection_error?: string; share_validation?: ShareValidation }>(discoveryDownloadsPath, { method: 'POST', body: JSON.stringify({ preview_token: shareSelection.value?.previewToken, selected_entry_tokens: shareSelection.value?.entryTokens, result_token: item.token, downloader_id: downloadForm.value.downloaderID, media_library_id: downloadForm.value.mediaLibraryID, priority: downloadForm.value.priority }) })
    if (generation !== interactionGeneration) return
    if (result.share_validation) rememberShareValidation(item.token, result.share_validation)
    if (result.selection_pending) { notify(result.selection_error ?? '部分任务未提交，请重试', 'warning'); return }
    notify(result.selection_tasks?.length ? `已创建 ${result.selection_tasks.length} 个入库任务，各文件独立识别和处理` : `下载任务已进入统一队列：${result.id}`, 'success')
    downloadDialog.value = null
  } catch (reason) {
    if (generation !== interactionGeneration) return
    if (reason instanceof APIError && reason.shareValidation) rememberShareValidation(item.token, reason.shareValidation)
    notify(message(reason), 'error')
  }
  finally { if (generation === interactionGeneration) submitting.value = false }
}

function formatTime(value?: string) { return value ? new Date(value).toLocaleString() : '未知' }
function count(value?: number) { return value == null ? '—' : String(value) }
function resultExpired(item: TorrentSearchResult) { return !(Date.parse(item.expires_at) > validationClock.value) }
function mediaTypeLabel(value?: string) { return value === 'movie' ? '电影' : value === 'tv' ? '剧集' : '类型待定' }
function message(reason: unknown) { return reason instanceof Error ? reason.message : '种子搜索暂时不可用' }

function currentSearchInput() {
  return { keyword: keyword.value, mediaType: mediaType.value, year: year.value, tmdbID: tmdbID.value, searchBy: searchBy.value, siteID: lockedSiteID.value, siteIDs: lockedSiteID.value || activeSearchSiteIDs.value.length === 0 ? undefined : activeSearchSiteIDs.value, typePolicy: appliedTypePolicy.value, cloudTypes: appliedCloudTypes.value }
}

async function refreshSourceAvailability() {
  availabilityRequest?.abort()
  const request = new AbortController()
  availabilityRequest = request
  sourceStatusReady.value = false
  try {
    const response = await api<ListResponse<SearchSiteOption>>(discoverySearchOptionsPath, { signal: request.signal })
    if (availabilityRequest !== request || request.signal.aborted) return
    if (!Array.isArray(response.list)) return
    siteOptions.value = response.list
    sourceStatusReady.value = true
  } catch { /* Cached results stay visible; mutations wait for a confirmed current source. */ }
}
function groupUnavailable(group: TorrentSearchGroup) {
  if (!sourceStatusReady.value) return restoredResults.value ? '历史结果的来源状态尚未确认，请重新搜索。' : ''
  const option = siteOptions.value.find(site => site.id === group.site_id)
  return option?.searchable ? '' : option?.reason || '来源已停用或不再可访问，保留历史结果。'
}
function resourceUnavailable(item: TorrentSearchResult) {
  const group = groups.value.find(group => group.items.some(value => value.token === item.token))
  return (group ? groupUnavailable(group) : '') || (resultExpired(item) ? '搜索结果已过期，请重新搜索' : '') || (item.acquisition_available === false ? item.acquisition_reason || '当前类型仅支持查看，尚无可用接收能力。' : '')
}
function resourceActionLabel(item: TorrentSearchResult) {
  if (item.acquisition_available === false) return isCloudShareSource(item.source_kind) || item.source_kind === 'bt' || item.source_kind === 'pt' ? '暂不可入库' : '仅供查看'
  return isCloudShareSource(item.source_kind) ? '转存入库' : '入库'
}
async function copyResourceLink(item: TorrentSearchResult) {
  const group = groups.value.find(group => group.items.some(value => value.token === item.token))
  if (resultExpired(item) || group && groupUnavailable(group)) return
  copyRequest?.abort()
  const request = new AbortController()
  copyRequest = request
  copyingToken.value = item.token
  try {
    const result = await api<{ url: string }>(discoveryResultLinkPath, { method: 'POST', signal: request.signal, body: JSON.stringify({ result_token: item.token }) })
    if (copyRequest !== request || request.signal.aborted) return
    const link = safeDiscoveryLink(result.url)
    if (!link) throw new Error('Server 返回的资源链接无效。')
    await navigator.clipboard.writeText(link)
    if (copyRequest === request) notify('资源链接已复制', 'success')
  } catch (reason) { if (!request.signal.aborted) notify(message(reason), 'error') }
  finally { if (copyRequest === request) { copyRequest = null; copyingToken.value = '' } }
}

async function openManualRecognition(item: TorrentSearchResult) {
  const recognized = recognitions.value[item.token]
  manualDialog.value = item
  manualSaving.value = false
  manualForm.value = {
    keyword: recognized?.title?.trim() || item.title,
    mediaType: recognized?.media_type || '',
    year: recognized?.year,
  }
  manualCandidates.value = []
  selectedManualCandidate.value = null
  await searchManualCandidates()
}

function closeManualRecognition() {
  manualRequestGeneration++
  manualCandidatesRequest?.abort()
  manualCandidatesRequest = null
  manualDialog.value = null
  manualCandidates.value = []
  selectedManualCandidate.value = null
  manualSearching.value = false
  manualSaving.value = false
}

async function searchManualCandidates() {
  const item = manualDialog.value
  if (!item || !manualForm.value.keyword.trim()) { notify('请输入用于 TMDB 搜索的作品名', 'warning'); return }
  manualCandidatesRequest?.abort()
  const controller = new AbortController()
  manualCandidatesRequest = controller
  const run = ++manualRequestGeneration
  const isCurrent = () => !controller.signal.aborted && run === manualRequestGeneration && manualDialog.value?.token === item.token
  manualSearching.value = true
  selectedManualCandidate.value = null
  try {
    const result = await api<ListResponse<PTRecognitionCandidate>>(torrentRecognitionCandidatesPath, {
      method: 'POST',
      signal: controller.signal,
      body: JSON.stringify({ result_token: item.token, title: manualForm.value.keyword, media_type: manualForm.value.mediaType, year: manualForm.value.year || undefined }),
    })
    if (isCurrent()) manualCandidates.value = result.list
  } catch (reason) {
    if (!isCurrent()) return
    manualCandidates.value = []
    notify(message(reason), 'error')
  } finally { if (isCurrent()) { manualSearching.value = false; manualCandidatesRequest = null } }
}

async function confirmManualRecognition() {
  const item = manualDialog.value
  const candidate = selectedManualCandidate.value
  if (!item || !candidate) { notify('请先选择一个 TMDB 作品', 'warning'); return }
  const run = manualRequestGeneration
  manualSaving.value = true
  try {
    const result = await api<TorrentRecognitionResult>(torrentRecognitionOverridePath, {
      method: 'PUT',
      body: JSON.stringify({ result_token: item.token, tmdb_id: candidate.id, media_type: candidate.media_type }),
    })
    if (run !== manualRequestGeneration || manualDialog.value?.token !== item.token) return
    resultRecognition.acceptManual(item.token, result)
    manualDialog.value = null
    notify(`已确认：${result.title}，创建下载任务时将沿用此身份`, 'success')
  } catch (reason) { if (run === manualRequestGeneration && manualDialog.value?.token === item.token) notify(message(reason), 'error') }
  finally { if (run === manualRequestGeneration) manualSaving.value = false }
}

function sameSearchInput(left: ReturnType<typeof currentSearchInput>, right: TorrentSearchSession['input']) {
  const sameScope = !left.siteIDs?.length || JSON.stringify(left.siteIDs) === JSON.stringify(right.siteIDs)
  return left.keyword.trim() === right.keyword.trim() && left.mediaType === right.mediaType && left.year === right.year && left.tmdbID === right.tmdbID && left.searchBy === right.searchBy && left.siteID === right.siteID && sameScope && left.typePolicy === right.typePolicy && JSON.stringify(left.cloudTypes) === JSON.stringify(right.cloudTypes)
}

function clearSearchForActorChange() {
  searchRunner.reset()
  interactionGeneration++
  previewOpenGeneration++; downloadOpenGeneration++
  cancelDownloadOptionRequests()
  submittedSearchInput = null; submittedIdentity = null
  groups.value = []; searchProgress.value = null; searching.value = false; searched.value = false
  activeSearchSiteIDs.value = []; identityNames.value = []; searchError.value = ''
  downloadDialog.value = null; sharePreviewDialog.value = null; manualDialog.value = null
  downloaders.value = []; libraries.value = []; routeChoices.value = []; routeRecommendation.value = null; routePreview.value = null
  downloadForm.value = { downloaderID: '', mediaLibraryID: 0, priority: 0 }; downloadSiteID.value = undefined
  submitting.value = false; routeRecommendationLoading.value = false; routePreviewLoading.value = false
  shareValidations.value = {}; shareSelection.value = null
  manualRequestGeneration++; manualCandidatesRequest?.abort(); manualSearching.value = false; manualSaving.value = false
  routeRecommendationRequest?.abort(); routePreviewRequest?.abort()
  resultRecognition.reset()
  try { sessionStorage.removeItem(torrentSearchSessionKey) } catch { /* Storage is optional. */ }
}
watch(() => auth.user?.id, (current, previous) => { if (current !== previous) clearSearchForActorChange() })
watch(() => auth.can(Permissions.DiscoveryRead), (allowed, previous) => { if (previous && !allowed) clearSearchForActorChange() })

watch([groups, recognitions, searched], () => {
  saveTorrentSearchSession(typeof sessionStorage === 'undefined' ? undefined : sessionStorage, {
    actorID: auth.user?.id, input: submittedSearchInput ?? currentSearchInput(), groups: groups.value, recognitions: recognitions.value, searched: searched.value, savedAt: Date.now(),
  })
}, { deep: true })

onMounted(async () => {
  validationTimer = window.setInterval(() => { validationClock.value = Date.now() }, 15000)
  if (mode.value === 'media') {
    if (mediaQuery.value.trim()) await searchMedia()
    return
  }
  if (lockedSiteID.value && auth.can(Permissions.SystemAdmin)) {
    try {
      const response = await api<ListResponse<SiteSummary>>(sitesPath)
      lockedSite.value = response.list.find(item => item.id === lockedSiteID.value) ?? null
    } catch { /* Search API remains the authority if the management summary cannot load. */ }
  }
  void refreshSourceAvailability()
  if (searchOutcome.value !== 'idle') return
  const cached = readTorrentSearchSession(typeof sessionStorage === 'undefined' ? undefined : sessionStorage)
  const hasRouteSearch = typeof route.query.title === 'string' || typeof route.query.tmdb_id === 'string'
  if (cached && cached.actorID !== auth.user?.id) {
    try { sessionStorage.removeItem(torrentSearchSessionKey) } catch { /* Storage is optional. */ }
  }
  if (cached && cached.actorID === auth.user?.id && (!hasRouteSearch || sameSearchInput(currentSearchInput(), cached.input))) {
    keyword.value = cached.input.keyword
    mediaType.value = cached.input.mediaType
    year.value = cached.input.year
    tmdbID.value = cached.input.tmdbID
    searchBy.value = cached.input.searchBy
    activeSearchSiteIDs.value = cached.input.siteIDs ?? []
    submittedSearchInput = cached.input
    submittedIdentity = trustedIdentity.value ? { ...trustedIdentity.value } : null
    groups.value = cached.groups
    searchRunner.restore()
    recognitions.value = cached.recognitions
    searched.value = cached.searched
    restoredResults.value = true
    panSouTypePolicy.value = cached.input.typePolicy ?? 'default'
    panSouCloudTypes.value = cached.input.cloudTypes ?? ['115', 'quark', 'magnet']
    appliedTypePolicy.value = cached.input.typePolicy
    appliedCloudTypes.value = cached.input.cloudTypes
    if (lockedSiteID.value) activeChannel.value = lockedSiteID.value
    return
  }
  if (initialResourceSearch) search()
})
onBeforeUnmount(() => {
  availabilityRequest?.abort()
  copyRequest?.abort()
  interactionGeneration++
  cancelDownloadOptionRequests()
  routeRecommendationRequest?.abort()
  searchRunner.dispose()
  manualRequestGeneration++
  manualCandidatesRequest?.abort()
  resultRecognition.dispose()
  window.clearInterval(validationTimer)
  mediaSearchRequest?.abort()
  routePreviewRequest?.abort()
})
</script>

<template>
  <section class="space-y-5">
    <header><p class="text-xs font-700 uppercase tracking-widest text-[var(--text-subtle)]">Search</p><h1 v-if="mode === 'media'" class="mt-1 text-2xl font-800">搜索</h1><h1 v-else class="mt-1 text-2xl font-800">直接搜索</h1><p class="page-description mt-1">{{ mode === 'media' ? '先从 TMDB 海报确认作品，再进入统一详情、媒体库覆盖率和多语言资源聚合。' : lockedSiteID ? '当前处于单站搜索模式；请求、重试和翻页不会回退到其他站点。' : '按原始关键词、标题或 TMDB ID 直接查询 PT/BT 资源。' }}</p></header>
    <nav class="management-tabs overflow-x-auto" role="tablist" aria-label="搜索模式"><button class="management-tab shrink-0" :class="mode === 'media' ? 'management-tab--active' : ''" type="button" role="tab" :aria-selected="mode === 'media'" @click="switchMode('media')">搜索</button><button class="management-tab shrink-0" :class="mode === 'resources' ? 'management-tab--active' : ''" type="button" role="tab" :aria-selected="mode === 'resources'" @click="switchMode('resources')">直接搜索</button></nav>

    <template v-if="mode === 'media'">
      <form class="panel" @submit.prevent="searchMedia(1)"><div class="grid gap-3 md:grid-cols-[minmax(0,1fr)_10rem_auto] md:items-end"><div><label class="label" for="media-search-query">电影或剧集名称</label><input id="media-search-query" v-model="mediaQuery" class="input" maxlength="256" placeholder="三体 / Three-Body" required /></div><div><label class="label" for="media-search-type">媒体类型</label><select id="media-search-type" v-model="mediaFilter" class="input"><option value="all">电影 + 剧集</option><option value="movie">电影</option><option value="tv">剧集</option></select></div><button class="btn-primary" :disabled="mediaSearching">{{ mediaSearching ? '搜索中…' : '搜索' }}</button></div></form>
      <div v-if="mediaError" class="semantic-error p-4"><strong>影视搜索失败</strong><p class="mt-1 text-sm">{{ mediaError }}</p><button class="btn-secondary mt-3" @click="searchMedia(mediaPage)">重试</button></div>
      <div v-if="mediaSearching" class="panel py-10 text-center text-muted">正在通过 TMDB 搜索电影和剧集海报…</div>
      <div v-else-if="mediaSearched && !mediaResults.length && !mediaError" class="panel py-10 text-center text-muted">没有找到匹配作品。可以尝试原名、英文名，或切换到直接搜索。</div>
      <div v-if="mediaResults.length" class="grid gap-4 sm:grid-cols-2 md:grid-cols-3 xl:grid-cols-5"><button v-for="work in mediaResults" :key="`${work.media_type}:${work.tmdb_id}`" class="discovery-poster text-left" @click="openMedia(work)"><div class="discovery-poster__image"><img v-if="work.poster_url" :src="work.poster_url" :alt="`${work.title} 海报`" loading="lazy" /><span v-else>暂无海报</span></div><strong :title="work.title">{{ work.title }}</strong><small>{{ work.media_type === 'tv' ? '剧集' : '电影' }} · {{ work.year || '年份未知' }}<template v-if="work.rating != null"> · {{ work.rating.toFixed(1) }}</template></small><small v-if="work.original_title && work.original_title !== work.title" :title="work.original_title">{{ work.original_title }}</small></button></div>
      <footer v-if="mediaResults.length" class="panel flex items-center justify-center gap-3"><button class="btn-secondary" :disabled="mediaSearching || mediaPage <= 1" @click="searchMedia(mediaPage - 1)">上一页</button><span class="text-sm">第 {{ mediaPage }} / {{ mediaTotalPages }} 页</span><button class="btn-secondary" :disabled="mediaSearching || mediaPage >= mediaTotalPages" @click="searchMedia(mediaPage + 1)">下一页</button></footer>
    </template>

    <template v-else>
      <div v-if="lockedSiteID" class="semantic-inset flex flex-wrap items-center justify-between gap-3 p-4"><div><span class="status-chip">仅搜索此站</span><strong class="ml-2">{{ lockedSite?.name || `站点 #${lockedSiteID}` }}</strong></div><RouterLink class="btn-secondary" to="/system/sites">返回站点管理</RouterLink></div>
      <form class="panel" @submit.prevent="search">
        <div class="mb-4 flex flex-wrap gap-2" role="group" aria-label="搜索方式"><button type="button" class="btn-secondary" :class="{ '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent)]': searchBy === 'title' }" @click="searchBy = 'title'">按标题</button><button type="button" class="btn-secondary" :class="{ '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent)]': searchBy === 'tmdb_id' }" @click="searchBy = 'tmdb_id'">按 TMDB ID</button></div>
        <div class="grid gap-3 md:grid-cols-[minmax(0,1fr)_10rem_8rem_auto] md:items-end">
          <div v-if="searchBy === 'title'"><label class="label" for="discovery-keyword">作品或发行标题</label><input id="discovery-keyword" v-model="keyword" class="input" maxlength="160" placeholder="七武士 / Seven Samurai" required /></div>
          <div v-else><label class="label" for="discovery-tmdb">TMDB ID</label><input id="discovery-tmdb" v-model.number="tmdbID" class="input font-mono" type="number" min="1" placeholder="346" required /></div>
          <div><label class="label" for="discovery-kind">媒体类型</label><select id="discovery-kind" v-model="mediaType" class="input"><option value="">自动</option><option value="movie">电影</option><option value="tv">剧集</option></select></div>
          <div><label class="label" for="discovery-year">年份</label><input id="discovery-year" v-model.number="year" class="input" type="number" min="1880" max="2200" placeholder="可选" /></div>
          <button class="btn-primary" :disabled="searching">{{ searching ? '搜索中…' : '搜索' }}</button>
        </div>
      </form>
      <section class="panel space-y-3" aria-label="PanSou 结果类型">
        <div class="flex flex-wrap items-end gap-3"><label class="min-w-64"><span class="label">PanSou 结果类型</span><select v-model="panSouTypePolicy" class="input" :disabled="searching"><option value="default">使用站点默认设置</option><option value="auto">自动：已接入网盘＋磁力</option><option value="all">全部类型</option><option value="manual">指定类型</option></select></label><button class="btn-secondary" type="button" :disabled="searching || panSouTypePolicy === 'manual' && !panSouCloudTypes.length" @click="activeSearchSiteIDs.length ? executeSearch(activeSearchSiteIDs) : search()">按此类型重新搜索</button></div>
        <fieldset v-if="panSouTypePolicy === 'manual'" class="flex flex-wrap gap-3"><legend class="label">搜索类型</legend><label v-for="option in discoveryTypeOptions" :key="option[0]" class="text-sm"><input v-model="panSouCloudTypes" type="checkbox" :value="option[0]" /> {{ option[1] }}</label></fieldset>
        <p class="text-subtle m-0 text-xs">来源包含所选 TG 频道和 PanSou 已启用插件；类型设置只影响盘搜。自动模式始终包含磁力。切换后点击重新搜索，其他网盘可能仅支持查看链接。</p>
      </section>
      <div v-if="selectedTitle" class="panel"><span class="status-chip">已确认作品身份</span><h2 class="mt-3 text-xl font-750">{{ selectedTitle }}</h2><p class="mt-1 text-sm text-muted">Server 会重新验证 TMDB 身份，并按受限的中文名、地区别名、原名和英文名聚合搜索。</p><div v-if="identityNames.length" class="mt-3 flex flex-wrap gap-2"><span v-for="name in identityNames" :key="`${name.kind}:${name.locale}:${name.value}`" class="status-chip">{{ name.value }}<template v-if="name.locale"> · {{ name.locale }}</template></span></div></div>
      <div v-if="searchError" class="semantic-error p-4"><strong>{{ groups.some(group => group.items.length) ? '搜索未完整完成' : '搜索失败' }}</strong><p class="mt-1 text-sm">{{ searchError }}</p><button class="btn-secondary mt-3" @click="search">{{ lockedSiteID ? '重试此站' : '重新选择站点' }}</button></div>
      <section v-if="searchProgress" class="panel" aria-live="polite">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div><strong>站点搜索进度 {{ searchProgress.completed }} / {{ searchProgress.total }}</strong><span class="status-chip ml-2">{{ searchOutcomeLabel }}</span><p class="text-subtle mb-0 mt-1 text-xs">等待 {{ searchProgress.pending }} · 正在搜索 {{ searchProgress.running }} · 成功 {{ searchProgress.succeeded }} · 失败 {{ searchProgress.failed }} · 已发现 {{ searchProgress.result_count }}</p></div>
          <button v-if="searching" class="btn-secondary" type="button" @click="cancelSearch">取消搜索</button>
        </div>
        <div class="mt-3 h-2 overflow-hidden rounded-full bg-[var(--surface-subtle)]" role="progressbar" :aria-valuemin="0" :aria-valuemax="searchProgress.total" :aria-valuenow="searchProgress.completed">
          <div class="h-full rounded-full bg-[var(--accent)] transition-[width] duration-200" :style="{ width: `${searchProgressPercent}%` }"></div>
        </div>
        <p v-if="searching && searchProgress.phase === 'preparing_media'" class="text-muted mb-0 mt-3 text-sm">正在准备作品信息；确认搜索范围后将开始查询所选站点。</p>
        <div v-if="searchActivityRows.length" class="mt-4 grid gap-2 md:grid-cols-2" aria-label="各站点搜索详情">
          <div v-for="detail in searchActivityRows" :key="detail.site_id" class="semantic-inset p-3 text-sm" :data-search-site="detail.site_id">
            <div class="flex flex-wrap items-center justify-between gap-2"><strong>{{ detail.site_name }}</strong><span>{{ activityLabel(detail.site_id, detail.phase) }}</span></div>
            <p v-if="detail.query_name" class="mb-0 mt-2 break-words">{{ detail.phase === 'finished' ? '查询名称' : '正在查询' }}：{{ detail.query_name }}<span v-if="detail.query_index && detail.queries_total">（第 {{ detail.query_index }} / {{ detail.queries_total }} 个名称）</span></p>
            <p class="text-subtle mb-0 mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs"><span v-if="detail.queries_total">已查询 {{ (detail.queries_completed ?? 0) + (detail.queries_failed ?? 0) }} / {{ detail.queries_total }} 个名称<span v-if="detail.queries_failed">（失败 {{ detail.queries_failed }}）</span></span><span>本次已返回 {{ detail.result_count }} 条</span><span aria-live="off">已用时 {{ searchDuration(searchRunner.elapsed(detail.site_id)) }}</span></p>
            <p v-if="retainedSiteIDs.includes(detail.site_id)" class="text-subtle mb-0 mt-2 text-xs">{{ siteIsRunning(detail.site_id) ? '重新搜索中，暂时展示上次结果' : '已保留上次结果' }}</p>
          </div>
        </div>
        <p v-else-if="searchProgress.site_name" class="text-subtle mb-0 mt-2 text-xs">{{ searchProgress.site_name }}：{{ searchProgress.site_status === 'running' ? '搜索中' : searchProgress.site_status === 'success' ? '完成' : searchProgress.site_status === 'error' ? '失败' : '等待搜索' }}</p>
        <p v-if="searching" class="text-subtle mb-0 mt-3 text-xs">结果收到一批就会展示；上游整批响应前会持续显示当前阶段和耗时。</p>
        <div v-if="failedGroups.length" class="mt-3 flex flex-wrap items-center gap-2"><span class="text-sm font-650">可重试站点：</span><button v-for="group in failedGroups" :key="group.site_id" class="btn-secondary" type="button" :disabled="searching" @click="retrySite(group)">{{ group.site_name }} · 重试</button></div>
      </section>
      <div v-if="searching && !groups.length" class="panel py-10 text-center text-muted">正在按站点限速并行搜索，结果会渐进出现…</div>
      <div v-else-if="searched && !groups.length && !searchError" class="panel py-10 text-center text-muted">{{ searchOutcome === 'cancelled' ? '搜索已取消，尚未完成的站点不能视为没有结果。' : '本次搜索尚无可展示结果，请检查搜索站点或重新搜索。' }}</div>

      <p v-if="searchOutcome === 'retained'" class="text-subtle text-sm">当前展示上次保留的结果；重新搜索可更新内容。</p>
      <template v-if="groups.length">
        <nav v-if="!lockedSiteID" class="management-tabs overflow-x-auto" role="tablist" aria-label="搜索渠道">
          <button class="management-tab shrink-0" :class="activeChannel === 'all' ? 'management-tab--active' : ''" type="button" role="tab" :aria-selected="activeChannel === 'all'" @click="activeChannel = 'all'">全部渠道</button>
          <button v-for="group in orderedGroups" :key="group.site_id" class="management-tab shrink-0" :class="activeChannel === group.site_id ? 'management-tab--active' : ''" type="button" role="tab" :aria-selected="activeChannel === group.site_id" @click="activeChannel = group.site_id">{{ group.site_name }} <small>{{ group.site_type.toUpperCase() }}</small></button>
        </nav>

        <form class="panel grid gap-3 md:grid-cols-2 xl:grid-cols-[auto_auto_11rem_11rem_9rem_10rem_9rem_auto] xl:items-end" @submit.prevent>
          <fieldset class="flex flex-wrap gap-3"><legend class="label">站点类型</legend><label class="text-sm"><input v-model="enabledSiteTypes" type="checkbox" value="pt" /> PT</label><label class="text-sm"><input v-model="enabledSiteTypes" type="checkbox" value="bt" /> 公共 BT</label><label class="text-sm"><input v-model="enabledSiteTypes" type="checkbox" value="bt_resource" /> 插件资源站</label><label class="text-sm"><input v-model="enabledSiteTypes" type="checkbox" value="cloud_share" /> 网盘分享站</label></fieldset>
          <label><span class="label">分辨率</span><select v-model="resolutionFilter" class="input"><option value="">全部</option><option v-for="value in resolutionOptions" :key="value" :value="value">{{ value }}</option></select></label>
          <label><span class="label">优惠</span><select v-model="promotionFilter" class="input"><option value="">全部</option><option value="free">FREE</option><option value="2xfree">2X FREE</option><option value="2x">2X</option></select></label>
          <label><span class="label">最低做种</span><input v-model.number="minimumSeeders" class="input" type="number" min="0" placeholder="不限" /></label>
          <label><span class="label">排序字段</span><select v-model="resultSort" class="input"><option value="seeders">做种数</option><option value="published">发布时间</option><option value="size">体积</option></select></label>
          <label><span class="label">排序方向</span><select v-model="resultDirection" class="input"><option value="desc">降序</option><option value="asc">升序</option></select></label>
          <p class="text-subtle mb-0 text-xs xl:col-span-2">不同筛选项之间为 AND；同类标签按任一匹配。排序只针对已经取回的当前页结果。</p>
        </form>

        <template v-for="group in orderedGroups" :key="`notice:${group.site_id}`"><div v-if="activeChannel === 'all' || activeChannel === group.site_id" class="space-y-2"><p v-if="groupUnavailable(group)" class="semantic-warning m-0 p-3 text-sm">{{ group.site_name }}：{{ groupUnavailable(group) }}</p><p v-for="warning in group.warnings || []" :key="warning.code" class="semantic-warning m-0 p-3 text-sm">{{ group.site_name }}：{{ warning.message }}</p><p v-if="group.truncated" class="semantic-warning m-0 p-3 text-sm">结果达到本次保存上限，尚未完整展示；请缩小范围重新搜索。</p><p v-if="group.completion === 'received' && !group.warnings?.length" class="text-subtle m-0 text-xs">{{ group.site_name }} 已返回 {{ group.items.length }} 条；上游未确认全部来源完成。</p></div></template>
        <div v-if="activeGroup && (activeGroup.partial || activeGroup.status === 'error' || retainedSiteIDs.includes(activeGroup.site_id))" class="semantic-warning flex flex-wrap items-center justify-between gap-3 p-4"><span>{{ activeGroup.site_name }}：{{ groupWarning(activeGroup) }}<template v-if="isPreviousPage(activeGroup)">；当前仍显示第 {{ activeGroup.page }} 页</template></span><button v-if="!siteIsRunning(activeGroup.site_id)" class="btn-secondary" :disabled="searching || !!groupUnavailable(activeGroup)" @click="retrySite(activeGroup)">重试此站</button></div>
        <div v-if="!visibleResults.length" class="panel py-10 text-center text-muted">{{ searching ? '当前筛选暂未显示结果，搜索仍在继续…' : ['partial', 'failed', 'cancelled'].includes(searchOutcome) ? '搜索未完整完成，暂不能确定是否存在匹配资源。可以重新查询。' : '当前渠道和筛选条件下没有结果。' }}</div>
        <div v-else class="grid gap-4 xl:grid-cols-2">
          <article v-for="entry in visibleResults" :key="entry.item.token" class="panel flex min-h-72 flex-col">
            <p v-if="retainedSiteIDs.includes(entry.group.site_id)" class="text-subtle mb-3 text-xs">上次保留的结果<template v-if="isPreviousPage(entry.group)"> · 第 {{ entry.group.page }} 页</template></p>
            <p v-else-if="entry.group.partial" class="text-subtle mb-3 text-xs">{{ groupWarning(entry.group) }}</p>
            <div class="flex min-w-0 flex-1 gap-4">
              <div class="flex h-32 w-22 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-[var(--surface-subtle)] text-center text-xs text-subtle">
                <img v-if="recognitions[entry.item.token]?.poster_url" :src="recognitions[entry.item.token].poster_url" :alt="`${recognitions[entry.item.token].title} 海报`" class="h-full w-full object-cover" loading="lazy" />
                <span v-else>站点：{{ entry.group.site_name }}<br />资源：{{ resultTypeLabel(entry.item, entry.group.site_type.toUpperCase()) }}</span>
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap gap-1.5"><span class="status-chip">站点：{{ entry.group.site_name }}</span><span class="status-chip">资源：{{ resultTypeLabel(entry.item, entry.group.site_type.toUpperCase()) }}</span><template v-for="origin in discoveryOrigins(entry.item)" :key="`${origin.kind}:${origin.name}`"><span class="status-chip">PanSou · {{ origin.kind === 'tg' ? 'TG' : '插件' }} · {{ origin.name }}</span><a v-if="origin.post_url" :href="origin.post_url" target="_blank" rel="noopener noreferrer" class="text-xs">原帖 ↗</a></template><span v-if="entry.item.matched_name" class="status-chip status-chip--ready">命中 {{ entry.item.matched_name }}</span><span v-if="entry.item.promotion" class="status-chip status-chip--ready">{{ entry.item.promotion.toUpperCase() }}</span><span v-for="label in ptRecognitionSpecLabels(entry.item.specifications || {})" :key="label" class="status-chip">{{ label }}</span><span v-if="entry.item.quality" class="status-chip">{{ entry.item.quality }}</span><span v-for="tag in entry.item.tags || []" :key="tag" class="status-chip">{{ tag }}</span></div>
                <h2 class="mt-3 break-words text-base font-750">{{ entry.item.title }}</h2>
                <p v-if="entry.item.subtitle" class="text-subtle mb-0 mt-1 line-clamp-2 text-xs">{{ entry.item.subtitle }}</p>
                <div class="text-subtle mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs"><span>{{ formatBytes(entry.item.size_bytes ?? null) }}</span><span>{{ formatTime(entry.item.published_at) }}</span><strong>做种 {{ count(entry.item.seeders) }}</strong><span>下载 {{ count(entry.item.leechers) }}</span><span>完成 {{ count(entry.item.completed) }}</span></div>
                <div v-if="recognitions[entry.item.token]" class="semantic-inset mt-3 p-3 text-sm"><div class="flex flex-wrap items-center gap-2"><strong>{{ recognitions[entry.item.token].title }}</strong><span :class="recognitions[entry.item.token].status === 'matched' ? 'status-chip status-chip--ready' : 'status-chip status-chip--warning'">{{ recognitions[entry.item.token].manual_override ? '已人工确认' : recognitions[entry.item.token].status === 'matched' ? '标题预识别成功' : '标题预识别未命中' }}</span><span class="status-chip">{{ mediaTypeLabel(recognitions[entry.item.token].media_type) }}</span><span v-if="recognitions[entry.item.token].year" class="status-chip">{{ recognitions[entry.item.token].year }}</span></div><p v-if="ptRecognitionEpisodeLabel(recognitions[entry.item.token])" class="mb-0 mt-2">{{ ptRecognitionEpisodeLabel(recognitions[entry.item.token]) }}</p><p v-if="recognitions[entry.item.token].error_code" class="text-subtle mb-0 mt-2 text-xs">{{ ptRecognitionErrorLabel(recognitions[entry.item.token].error_code) }}</p></div>
                <p v-if="!recognitions[entry.item.token] && !recognitionErrors[entry.item.token]" class="text-subtle mt-3 text-xs" role="status">{{ recognizingTokens.includes(entry.item.token) ? '正在自动识别作品…' : '等待自动识别…' }}</p>
                <p v-if="libraryStates[entry.item.token]" class="mt-3 text-xs" role="status" :title="libraryStates[entry.item.token].detail"><span class="status-chip" :class="libraryStates[entry.item.token].present ? 'status-chip--ready' : ''">库内：{{ libraryStates[entry.item.token].label }}</span><span class="text-subtle ml-2">当前可见媒体库 · 作品级</span></p>
                <p v-if="recognitionErrors[entry.item.token]" class="semantic-warning mb-0 mt-3 p-3 text-xs">{{ recognitionErrors[entry.item.token] }}</p>
                <p v-if="resultExpired(entry.item) && recognitionErrors[entry.item.token] !== '搜索结果已过期，请重新搜索'" class="semantic-warning mb-0 mt-3 p-3 text-xs">搜索结果已过期，请重新搜索</p>
              </div>
            </div>
            <p v-if="isCloudShareSource(entry.item.source_kind)" class="text-subtle mt-3 text-xs" role="status" :title="shareValidation(entry.item)?.message">
              {{ shareValidationLabel(shareValidation(entry.item), validationClock) }}
              <span v-if="shareValidation(entry.item)"> · {{ formatTime(shareValidation(entry.item)?.checked_at) }}</span>
              <span v-if="shareValidation(entry.item)?.status === 'unavailable'"> · {{ shareValidation(entry.item)?.message }}</span>
            </p>
            <p v-if="resourceUnavailable(entry.item)" class="semantic-warning mt-3 p-3 text-xs" role="status">{{ resourceUnavailable(entry.item) }}</p>
            <footer class="mt-4 flex flex-wrap justify-end gap-2 border-t border-[var(--border)] pt-4"><button v-if="entry.item.result_type" class="btn-secondary" :disabled="copyingToken === entry.item.token || resultExpired(entry.item) || !!groupUnavailable(entry.group)" @click="copyResourceLink(entry.item)">{{ copyingToken === entry.item.token ? '正在读取…' : '复制资源链接' }}</button><button v-if="isCloudShareSource(entry.item.source_kind)" class="btn-secondary" :disabled="!auth.can(Permissions.DownloadsCreate) || !!resourceUnavailable(entry.item) || isPreviousPage(entry.group) && searching" @click="openSharePreview(entry.item)">预览分享链接内部内容</button><button class="btn-secondary" :disabled="!auth.can(Permissions.DiscoveryRead) || resultExpired(entry.item)" :title="resultExpired(entry.item) ? '搜索结果已过期，请重新搜索' : '重新读取作品识别及库内状态'" @click="resultRecognition.retry(entry.item.token)">重新检测</button><button class="btn-secondary" :disabled="!auth.can(Permissions.DownloadsCreate) || !!resourceUnavailable(entry.item) || isPreviousPage(entry.group) && searching" @click="openManualRecognition(entry.item)">手动检测</button><button class="btn-primary" :disabled="!auth.can(Permissions.DownloadsCreate) || !!resourceUnavailable(entry.item) || isPreviousPage(entry.group) && searching" :title="resourceUnavailable(entry.item)" @click="openDownload(entry.item)">{{ resourceActionLabel(entry.item) }}</button></footer>
          </article>
        </div>
        <footer v-if="activeGroup && (activeGroup.status === 'success' || retainedSiteIDs.includes(activeGroup.site_id)) && activeGroup.complete !== false" class="panel flex items-center justify-center gap-3"><button class="btn-secondary" :disabled="searching || !!groupUnavailable(activeGroup) || activeGroup.page <= 1" @click="previousPage(activeGroup)">上一页</button><span class="text-sm">{{ activeGroup.site_name }} · 第 {{ activeGroup.page }} 页</span><button class="btn-secondary" :disabled="searching || !!groupUnavailable(activeGroup) || !activeGroup.has_next" @click="nextPage(activeGroup)">下一页</button></footer>
        <p v-else-if="activeChannel === 'all'" class="text-subtle text-center text-xs">“全部渠道”聚合显示各站点当前页；切换到单个站点后可独立翻页。</p>
      </template>

    </template>

    <div v-if="siteSelectorOpen" class="modal-backdrop fixed inset-0 z-60 flex items-center justify-center p-4" @click.self="siteSelectorOpen = false" @keydown.esc="siteSelectorOpen = false">
      <section class="panel max-h-[90vh] w-full max-w-2xl overflow-y-auto" role="dialog" aria-modal="true" aria-labelledby="search-site-selector-title">
        <div class="flex items-start justify-between gap-3"><div><h2 id="search-site-selector-title" class="m-0 text-xl">选择搜索站点</h2><p class="page-description mt-1 text-sm">本次搜索、流式回退和多语言聚合只会访问这里选中的站点。</p></div><button class="icon-button" type="button" aria-label="关闭站点选择" @click="siteSelectorOpen = false">×</button></div>
        <div class="mt-4 flex flex-wrap items-center justify-between gap-3"><span class="text-sm">已选 {{ selectedSiteIDs.length }} / {{ selectableSiteOptions.length }} 个可用站点</span><div class="flex gap-2"><button class="btn-secondary" type="button" :disabled="siteOptionsLoading || !selectableSiteOptions.length" @click="selectAllSites">全选</button><button class="btn-secondary" type="button" :disabled="siteOptionsLoading || !selectedSiteIDs.length" @click="clearSelectedSites">取消全选</button></div></div>
        <div v-if="siteOptionsLoading" class="semantic-inset mt-4 p-5 text-center text-sm text-muted">正在读取可搜索站点…</div>
        <div v-else-if="siteOptionsError" class="semantic-error mt-4 p-4"><strong>站点列表读取失败</strong><p class="mb-0 mt-1 text-sm">{{ siteOptionsError }}</p><button class="btn-secondary mt-3" type="button" @click="openSiteSelector">重试</button></div>
        <div v-else-if="!siteOptions.length" class="semantic-warning mt-4 p-4 text-sm">当前没有已启用且支持搜索的 PT/BT 站点，请先到站点管理添加并测试。</div>
        <div v-else class="mt-4 grid gap-2 sm:grid-cols-2">
          <label v-for="site in siteOptions" :key="site.id" class="semantic-list-item flex items-start gap-3 p-3" :class="{ 'opacity-60': !site.searchable }">
            <input v-model="selectedSiteIDs" type="checkbox" :value="site.id" :disabled="!site.searchable" />
            <span class="min-w-0"><strong class="block break-words">{{ site.name }}</strong><small class="text-subtle mt-1 block">{{ site.site_type === 'cloud_share' ? '网盘分享站' : site.site_type.toUpperCase() }} · {{ site.health_status || 'unknown' }}</small><small v-if="site.reason" class="semantic-danger-text mt-1 block">{{ site.reason }}</small></span>
          </label>
        </div>
        <p v-if="!siteOptionsLoading && !siteOptionsError && selectedSiteIDs.length === 0" class="semantic-warning mt-4 p-3 text-sm" role="alert">至少选择一个可搜索站点后才能开始搜索。</p>
        <div class="mt-5 flex justify-end gap-3"><button class="btn-secondary" type="button" @click="siteSelectorOpen = false">取消</button><button class="btn-primary" type="button" :disabled="siteOptionsLoading || !!siteOptionsError || selectedSiteIDs.length === 0" @click="confirmSiteSelection">搜索</button></div>
      </section>
    </div>

    <div v-if="manualDialog" class="modal-backdrop fixed inset-0 z-50 flex items-center justify-center p-4" @click.self="!manualSaving && closeManualRecognition()">
      <form class="panel max-h-[90vh] w-full max-w-3xl overflow-y-auto" role="dialog" aria-modal="true" aria-labelledby="manual-recognition-title" @submit.prevent="searchManualCandidates">
        <div class="flex items-start justify-between gap-3"><div><h2 id="manual-recognition-title" class="m-0 text-xl">手动识别作品</h2><p class="page-description mt-1 line-clamp-2 text-sm">{{ manualDialog.title }}</p></div><button class="btn-secondary" type="button" :disabled="manualSaving" @click="closeManualRecognition">关闭</button></div>
        <p class="semantic-inset mt-4 p-3 text-sm">自动识别失败也可以在这里修改关键词。候选只用于选择 TMDB 身份；Server 会重新读取 TMDB 详情并把验证结果绑定到当前短期搜索令牌。</p>
        <div class="mt-4 grid gap-3 md:grid-cols-[minmax(0,1fr)_9rem_8rem_auto] md:items-end">
          <div><label class="label" for="manual-recognition-keyword">作品关键词</label><input id="manual-recognition-keyword" v-model="manualForm.keyword" class="input" maxlength="256" placeholder="中文、英文、原名或拼音" required /></div>
          <div><label class="label" for="manual-recognition-type">媒体类型</label><select id="manual-recognition-type" v-model="manualForm.mediaType" class="input"><option value="">电影 + 剧集</option><option value="movie">电影</option><option value="tv">剧集</option></select></div>
          <div><label class="label" for="manual-recognition-year">年份</label><input id="manual-recognition-year" v-model.number="manualForm.year" class="input" type="number" min="1888" max="2200" placeholder="可选" /></div>
          <button class="btn-secondary" :disabled="manualSearching">{{ manualSearching ? '搜索中…' : '搜索 TMDB' }}</button>
        </div>
        <div v-if="manualCandidates.length" class="mt-4 grid gap-3 sm:grid-cols-2">
          <button v-for="candidate in manualCandidates" :key="`${candidate.media_type}-${candidate.id}`" class="semantic-list-item flex min-h-32 gap-3 p-3 text-left" :class="{ 'semantic-list-item--selected': selectedManualCandidate?.id === candidate.id && selectedManualCandidate?.media_type === candidate.media_type }" type="button" @click="selectedManualCandidate = candidate">
            <img v-if="candidate.poster_url" :src="candidate.poster_url" :alt="`${candidate.title} 海报`" class="h-28 w-19 shrink-0 rounded object-cover" loading="lazy" />
            <div class="min-w-0"><strong class="break-words">{{ candidate.title }}</strong><small v-if="candidate.original_title && candidate.original_title !== candidate.title" class="text-subtle mt-1 block break-words">{{ candidate.original_title }}</small><div class="mt-2 flex flex-wrap gap-1.5"><span class="status-chip">{{ mediaTypeLabel(candidate.media_type) }}</span><span v-if="candidate.release_year" class="status-chip">{{ candidate.release_year }}</span><span v-if="candidate.original_language" class="status-chip">{{ candidate.original_language.toUpperCase() }}</span></div><small class="text-subtle mt-2 block">TMDB {{ candidate.id }} · 搜索相似度 {{ Math.round(candidate.confidence * 100) }}%</small></div>
          </button>
        </div>
        <p v-else-if="!manualSearching" class="text-subtle mt-4 text-sm">修改关键词并搜索，然后选择正确作品。没有候选不会改变当前识别结果。</p>
        <div class="mt-5 flex justify-end gap-3"><button class="btn-secondary" type="button" :disabled="manualSaving" @click="closeManualRecognition">取消</button><button class="btn-primary" type="button" :disabled="manualSaving || !selectedManualCandidate" @click="confirmManualRecognition">{{ manualSaving ? '正在验证身份…' : '确认此身份' }}</button></div>
      </form>
    </div>

    <SharePreviewDialog v-if="sharePreviewDialog" :result-token="sharePreviewDialog.token" :title="sharePreviewDialog.title" :source-kind="sharePreviewDialog.source_kind" :downloaders="downloaders" @close="sharePreviewDialog = null" @select="acceptShareSelection" @validation="rememberShareValidation" />
    <div v-if="downloadDialog" class="modal-backdrop fixed inset-0 z-50 flex items-center justify-center p-4" @click.self="!submitting && (downloadDialog = null)">
      <form class="panel max-h-[90vh] w-full max-w-xl overflow-y-auto" role="dialog" aria-modal="true" aria-labelledby="pt-download-title" @submit.prevent="submitDownload">
        <div class="flex items-start justify-between gap-3"><div><h2 id="pt-download-title" class="m-0 text-xl">{{ isCloudShareSource(downloadDialog.source_kind) ? '转存到媒体库' : '创建下载任务' }}</h2><p class="page-description mt-1 line-clamp-2 text-sm">{{ downloadDialog.title }}</p></div><button class="btn-secondary" type="button" :disabled="submitting" @click="downloadDialog = null">关闭</button></div>
        <div class="mt-5 grid gap-4 sm:grid-cols-2">
          <div><label class="label">下载器</label><select v-model="downloadForm.downloaderID" class="input" required :disabled="!!shareSelection || routeRecommendationLoading" @change="loadRoutePreview"><option value="" disabled>请选择可执行的下载器</option><option v-for="item in enabledDownloaders" :key="item.id" :value="item.id">{{ item.name }} · {{ item.type === 'pan115_offline' ? '115 网盘' : item.type === 'quark_share' ? '夸克分享转存' : item.type }}</option></select><p class="text-subtle mb-0 mt-2 text-xs">按资源来源和全局下载器顺序推荐；可手动改选兼容的下载器。</p></div>
          <div><label class="label">目标媒体库</label><div v-if="routeRecommendationLoading && !targetChoices.length" class="semantic-inset p-3 text-sm text-muted">正在计算入库路线…</div><div v-else-if="!targetChoices.length" class="semantic-warning p-3 text-sm">当前没有可用媒体库。</div><div v-else class="grid max-h-48 gap-2 overflow-y-auto" role="radiogroup" aria-label="目标媒体库"><label v-for="choice in targetChoices" :key="choice.media_library_id" class="semantic-list-item flex items-start gap-2 p-2 text-sm" :class="{ 'semantic-list-item--selected': downloadForm.mediaLibraryID === choice.media_library_id, 'opacity-60': !choice.enabled }"><input type="radio" name="explore-download-target" :checked="downloadForm.mediaLibraryID === choice.media_library_id" :disabled="!choice.enabled || routeRecommendationLoading" @change="chooseTarget(choice.media_library_id)" /><span><strong>{{ choice.library_name }}</strong><small class="text-subtle mt-1 block">{{ choice.route_label || choice.reason_message || '路线待确认' }}</small></span></label></div></div>
          <p v-if="routeFailure && !routeRecommendationLoading && !routePreviewLoading" class="semantic-warning sm:col-span-2">{{ routeFailure }}。可调整目标媒体库或手动选择兼容下载器。</p>
          <div class="sm:col-span-2"><label class="label">队列优先级</label><input v-model.number="downloadForm.priority" class="input" type="number" min="-100" max="100" /></div>
        </div>
        <div v-if="selectedLibrary && selectedRoute" class="semantic-inset mt-4 grid gap-3 p-4 text-sm sm:grid-cols-2"><div><span class="text-subtle block text-xs">最终媒体库</span><strong>{{ selectedLibrary.name }}</strong></div><div><span class="text-subtle block text-xs">分类与入库</span><strong>{{ selectedRoute.route_label }} · {{ selectedLibrary.profile_name }} · {{ selectedLibrary.transfer_mode }}</strong></div></div>
        <p v-if="shareSelection" class="semantic-inset mt-4 p-3 text-sm">本次仅转存所选 {{ shareSelection.fileCount }} 个文件 · {{ formatBytes(shareSelection.totalSize) }}。更换账号或文件范围请重新预览。</p>
        <p class="text-subtle mt-4 text-xs">{{ isCloudShareSource(downloadDialog.source_kind) ? '分享内容将转存到对应网盘的下载器目录，随后自动识别、整理并入库。' : '确认后获取资源，自动识别、整理并入库。' }}</p>
        <div class="mt-5 flex justify-end gap-3"><button class="btn-secondary" type="button" :disabled="submitting" @click="downloadDialog = null">取消</button><button class="btn-primary" :disabled="submitting || routeRecommendationLoading || routePreviewLoading || !downloadForm.downloaderID || !selectedRoute?.enabled || !selectedLibrary">{{ submitting ? '正在提交…' : isCloudShareSource(downloadDialog.source_kind) ? '确认转存' : '确认并入队' }}</button></div>
      </form>
    </div>
  </section>
</template>
