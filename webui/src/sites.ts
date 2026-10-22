import type { ShareValidation } from '@/share-preview'

export const defaultPanSouBaseURL = 'https://so.252035.xyz'

export interface SiteHealth {
  status: 'unknown' | 'online' | 'offline' | 'healthy' | 'auth_required' | 'rate_limited' | 'unavailable'
  error_code: string
  username: string
  checked_at?: string
}

export type PanSouSourceScope = 'all' | 'tg' | 'plugin'
export type PanSouChannelMode = 'custom_only' | 'defaults_plus_custom'
export type DiscoveryTypePolicy = 'auto' | 'all' | 'manual'
export interface CloudSiteConfig {
  provider?: '115' | 'quark' | 'all'
  channels: string[]
  auth_enabled: boolean
  source_scope?: PanSouSourceScope
  channel_mode?: PanSouChannelMode
  plugins?: string[]
  type_policy?: DiscoveryTypePolicy
  cloud_types?: string[]
}
export interface PanSouScope {
  source_scope: PanSouSourceScope
  channel_mode: PanSouChannelMode
  default_channels: string[]
  custom_channels: string[]
  effective_channels: string[]
  default_count: number
  custom_count: number
  effective_count: number
  default_error_code?: string
}
export interface DiscoveryOrigin { kind: 'tg' | 'plugin'; name: string; post_url?: string }
export const discoveryTypeOptions = [
  ['115', '115'], ['quark', '夸克'], ['magnet', '磁力'], ['aliyun', '阿里'],
  ['baidu', '百度'], ['tianyi', '天翼'], ['uc', 'UC'], ['xunlei', '迅雷'],
  ['123', '123 网盘'], ['pikpak', 'PikPak'], ['ed2k', '电驴'], ['others', '其他'],
] as const
export function discoveryTypeLabel(type?: string) {
  return discoveryTypeOptions.find(item => item[0] === type)?.[1] || type || '资源'
}
export function normalizeCloudSiteConfig(value?: CloudSiteConfig): CloudSiteConfig {
  return {
    provider: value?.provider ?? 'all', channels: [...(value?.channels ?? [])], auth_enabled: value?.auth_enabled ?? false,
    source_scope: value?.source_scope ?? 'all',
    channel_mode: value?.channel_mode ?? (value?.channels?.length ? 'custom_only' : 'defaults_plus_custom'),
    plugins: [...(value?.plugins ?? [])], type_policy: value?.type_policy ?? 'auto', cloud_types: [...(value?.cloud_types ?? [])],
  }
}
export function resultSiteType(item: Pick<PTSearchResult, 'source_kind' | 'result_type'>, fallback: PTSearchGroup['site_type']): PTSearchGroup['site_type'] {
  if (fallback !== 'cloud_share') return fallback
  if (item.source_kind === 'bt' || item.result_type === 'magnet') return 'bt'
  if (item.source_kind === '115_share' || item.source_kind === 'quark_share' || item.result_type && item.result_type !== 'magnet') return 'cloud_share'
  return fallback
}
export function siteEffectiveEnabled(site: Pick<SiteSummary, 'enabled' | 'effective_enabled'>) {
  return site.enabled && site.effective_enabled !== false
}
export function resultTypeLabel(item: Pick<PTSearchResult, 'result_type' | 'source_kind' | 'cloud_provider'>, fallback = '资源') {
  if (item.result_type) return discoveryTypeLabel(item.result_type)
  if (item.cloud_provider) return discoveryTypeLabel(item.cloud_provider)
  if (item.source_kind === '115_share') return '115'
  if (item.source_kind === 'quark_share') return '夸克'
  return item.source_kind === 'bt' ? '磁力' : fallback
}
export function safeDiscoveryPostURL(value?: string) {
  return value && /^https:\/\/t\.me\/[A-Za-z0-9_]{3,64}\/[1-9]\d{0,19}$/.test(value) ? value : undefined
}
export function discoveryOrigins(item: Pick<PTSearchResult, 'origins' | 'channel' | 'post_url'>): DiscoveryOrigin[] {
  const origins: DiscoveryOrigin[] = Array.isArray(item.origins) && item.origins.length ? item.origins : typeof item.channel === 'string' && item.channel ? [{ kind: 'tg', name: item.channel, post_url: item.post_url }] : []
  return origins.filter(origin => origin && (origin.kind === 'tg' || origin.kind === 'plugin') && typeof origin.name === 'string' && origin.name.length > 0 && origin.name.length <= 128 && !hasDiscoveryControl(origin.name)).map(origin => ({ kind: origin.kind, name: origin.name, post_url: origin.kind === 'tg' ? safeDiscoveryPostURL(origin.post_url) : undefined }))
}
function hasDiscoveryControl(value: string, rejectSpace = false) { return Array.from(value).some(character => character.charCodeAt(0) <= (rejectSpace ? 32 : 31) || character.charCodeAt(0) === 127) }
export function safeDiscoveryLink(value: unknown): string | undefined {
  if (typeof value !== 'string' || value.length > 8192 || hasDiscoveryControl(value, true)) return undefined
  try {
    const url = new URL(value)
    if ((url.protocol === 'http:' || url.protocol === 'https:') && url.hostname && !url.username && !url.password) return value
    if ((url.protocol === 'magnet:' && url.searchParams.has('xt')) || url.protocol === 'ed2k:') return value
  } catch { /* Malformed links never become a navigation target. */ }
  return undefined
}
export interface SiteSummary {
  cloud_config?: CloudSiteConfig
  login_username?: string
  password_configured?: boolean
  id: number
  name: string
  kind: string
  site_type: 'pt' | 'bt' | 'bt_resource' | 'cloud_share'
  credential_kind: 'cookie' | 'api_key' | 'none' | 'plugin'
  capabilities: SiteCapabilities
  base_url: string
  user_agent: string
  enabled: boolean
  effective_enabled?: boolean
  unavailable_code?: string
  unavailable_reason?: string
  priority: number
  timeout_seconds: number
  search_timeout_seconds?: number | null
  effective_search_timeout_seconds?: number
  rate_limit_per_minute: number
  browser_emulation: boolean
  browser_service_configured: boolean
  credential_configured: boolean
  cookie_configured: boolean
  passkey_configured: boolean
  api_key_configured: boolean
  health: SiteHealth
  revision: number
  created_at: string
  updated_at: string
}

export interface SearchSiteOption {
  id: number
  name: string
  site_type: 'pt' | 'bt' | 'bt_resource' | 'cloud_share'
  health_status: string
  searchable: boolean
  reason?: string
}

export interface SiteCapabilities {
  search: boolean
  download: boolean
}

export interface SiteCatalogItem {
  key: string
  name: string
  engine: string
  base_urls: string[] | null
  auto_discover: boolean
  site_type: 'pt' | 'bt' | 'cloud_share'
  credential_kind: 'cookie' | 'api_key' | 'none'
  capabilities: SiteCapabilities
}

export interface SiteResolution {
  kind: string
  name: string
  site_type: 'bt'
  credential_kind: 'none'
  canonical_base_url: string
  capabilities: SiteCapabilities
}

export type CookieCloudMode = 'disabled' | 'remote' | 'local'

export interface CookieCloudSettings {
  mode: CookieCloudMode
  base_url: string
  auto_sync_minutes: number
  credential_configured: boolean
  uuid_configured: boolean
  password_configured: boolean
  auth_header_configured: boolean
  local_upload_path?: string
  last_sync_status: string
  last_sync_error_code: string
  last_sync_at?: string
  revision: number
}

export interface CookieCloudSyncResult {
  status: string
  created: number
  updated: number
  skipped: number
  skipped_unsupported_domains: number
  skipped_missing_login_cookies: number
  failed: number
  issues?: CookieCloudSyncIssue[]
}

export interface CookieCloudSyncIssue {
  action: 'create' | 'update'
  site_id?: number
  kind: string
  error_code: string
}

export interface PTSearchResult {
  share_validation?: ShareValidation
  source_kind?: string
  cloud_provider?: string
  channel?: string
  post_url?: string
  result_type?: string
  origins?: DiscoveryOrigin[]
  acquisition_available?: boolean
  acquisition_reason?: string
  token: string
  matched_name?: string
  title: string
  subtitle?: string
  size_bytes?: number
  published_at?: string
  seeders?: number
  leechers?: number
  completed?: number
  promotion?: string
  quality?: string
  tags?: string[]
  specifications?: PTRecognitionSpecifications
  expires_at: string
}

export interface PTSearchGroup {
  site_id: number
  site_name: string
  site_type: 'pt' | 'bt' | 'bt_resource' | 'cloud_share'
  status: 'success' | 'error'
  error_code?: string
  page: number
  has_next: boolean
  skipped: number
  items: PTSearchResult[]
  snapshot_id?: string
  snapshot_expires_at?: string
  partial?: boolean
  truncated?: boolean
  completion?: 'received' | 'complete'
  warnings?: { code: string; message: string }[]
  scope?: PanSouScope
  complete?: boolean
  revision?: number
  incomplete_reason?: string
  queries_completed?: number
  queries_failed?: number
  queries_total?: number
}

export interface PTRecognitionSpecifications {
  resolution?: string
  source?: string
  video_codec?: string
  audio_codec?: string
  hdr?: string
  release_group?: string
}

export interface PTRecognitionEpisodeFacts {
  season?: number
  season_year?: number
  episode_min?: number
  episode_max?: number
  count?: number
}

export const ptRecognitionEngineVersion = 'nextgen-domain-v12'

export interface PTRecognitionResult {
  engine_version: string
  status: 'matched' | 'unrecognized'
  manual_override?: boolean
  error_code?: string
  title: string
  original_title?: string
  media_type?: 'movie' | 'tv'
  year?: number
  tmdb_id?: number
  poster_url?: string
  episodes?: PTRecognitionEpisodeFacts
  specifications: PTRecognitionSpecifications
}

export interface PTRecognitionCandidate {
  id: number
  title: string
  original_title?: string
  media_type: 'movie' | 'tv'
  original_language?: string
  release_year?: number
  confidence: number
  poster_url?: string
}

export interface PTSearchResponse { groups: PTSearchGroup[] }

export type TorrentResultSort = 'seeders' | 'published' | 'size'
export type TorrentResultDirection = 'asc' | 'desc'
export interface TorrentResultFilters {
  activeChannel: 'all' | number
  enabledSiteTypes: ReadonlyArray<'pt' | 'bt' | 'bt_resource' | 'cloud_share'>
  resultTypes?: readonly string[]
  resolution?: string
  promotion?: string
  minimumSeeders?: number
  sort: TorrentResultSort
  direction: TorrentResultDirection
}

export interface TorrentResultEntry {
  group: PTSearchGroup
  item: PTSearchResult
}

export const sitesPath = '/api/v1/sites'
export const siteCatalogPath = `${sitesPath}/catalog`
export const siteResolvePath = `${sitesPath}/resolve`
export const ptSearchPath = '/api/v1/discovery/pt-search'
export const ptSearchStreamPath = '/api/v1/discovery/pt-search/stream'
export const ptRecognitionPath = '/api/v1/discovery/pt-results/recognize'
export const ptRecognitionCandidatesPath = '/api/v1/discovery/pt-results/tmdb-candidates'
export const ptRecognitionOverridePath = '/api/v1/discovery/pt-results/recognition-override'
export const torrentSearchPath = '/api/v1/discovery/torrent-search'
export const torrentSearchStreamPath = '/api/v1/discovery/torrent-search/stream'
export const torrentRecognitionPath = '/api/v1/discovery/torrent-results/recognize'
export const torrentRecognitionCandidatesPath = '/api/v1/discovery/torrent-results/tmdb-candidates'
export const torrentRecognitionOverridePath = '/api/v1/discovery/torrent-results/recognition-override'
export const discoveryDownloadsPath = '/api/v1/discovery/downloads'
export const discoverySearchOptionsPath = '/api/v1/discovery/search-options'
export const discoveryResultLinkPath = '/api/v1/discovery/results/link'
export const panSouScopePath = `${sitesPath}/pansou-scope`
export const cookieCloudSettingsPath = '/api/v1/settings/sites/cookiecloud'
export const cookieCloudSyncPath = `${cookieCloudSettingsPath}/sync`

export function sitePath(id: number) { return `${sitesPath}/${id}` }
export function siteTestPath(id: number) { return `${sitePath(id)}/test` }

export function cookieCloudErrorLabel(code: string) {
  return ({
    site_authentication_failed: '站点登录 Cookie 已失效或不完整',
    site_unavailable: '站点暂时不可用',
    site_rate_limited: '站点请求受到限速',
    site_response_invalid: '站点返回内容无法识别',
    site_credential_invalid: '已保存的站点凭据不可用',
    CONFLICT: '站点配置已变化，请重新同步',
  } as Record<string, string>)[code] || code || '未知错误'
}

export function buildPTSearchQuery(input: { keyword: string; mediaType?: string; year?: number; tmdbID?: number; searchBy?: 'title' | 'tmdb_id'; page?: number; siteID?: number; siteIDs?: number[]; typePolicy?: DiscoveryTypePolicy; cloudTypes?: string[]; snapshotID?: string; refresh?: boolean }) {
  const query = new URLSearchParams({ keyword: input.keyword.trim(), page: String(input.page ?? 1) })
  query.set('client_capabilities', 'mixed_discovery_v1')
  if (input.typePolicy) query.set('type_policy', input.typePolicy)
  for (const type of input.cloudTypes ?? []) query.append('cloud_types', type)
  if (input.snapshotID) query.set('snapshot_id', input.snapshotID)
  if (input.refresh) query.set('refresh', 'true')
  if (input.mediaType) query.set('media_type', input.mediaType)
  if (input.year) query.set('year', String(input.year))
  if (input.tmdbID) query.set('tmdb_id', String(input.tmdbID))
  if (input.searchBy) query.set('search_by', input.searchBy)
  if (input.siteID && input.siteIDs) throw new Error('site_id and site_ids cannot be combined')
  if (input.siteID) query.set('site_id', String(input.siteID))
  if (input.siteIDs) {
    const ids = [...new Set(input.siteIDs.filter(id => Number.isInteger(id) && id > 0))]
    if (ids.length === 0 || ids.length > 64) throw new Error('site_ids must contain 1 to 64 sites')
    for (const id of ids) query.append('site_ids', String(id))
  }
  return query
}

export function ptSearchURL(base: string, input: Parameters<typeof buildPTSearchQuery>[0]) {
  return `${base}?${buildPTSearchQuery(input)}`
}

export function ptRecognitionSpecLabels(value: PTRecognitionSpecifications) {
  return [value.resolution, value.source, value.video_codec, value.audio_codec, value.hdr, value.release_group ? `压制组 ${value.release_group}` : ''].filter((item): item is string => Boolean(item))
}

export function ptRecognitionErrorLabel(code?: string) {
  return ({
    tmdb_credential_unavailable: 'TMDB 尚未配置，当前仅显示本地解析结果',
    tmdb_unavailable: 'TMDB 暂时不可用，当前仅显示本地解析结果',
    tmdb_network_unavailable: 'TMDB 网络不可用，当前仅显示本地解析结果',
    tmdb_no_match: 'TMDB 未找到可信匹配，当前仅显示本地解析结果',
    tmdb_low_confidence: 'TMDB 候选可信度不足，未自动认定作品',
    tmdb_candidate_conflict: '存在多个相近候选，未自动认定作品',
    tmdb_auth_failed: 'TMDB 凭据认证失败，当前仅显示本地解析结果',
    tmdb_invalid_response: 'TMDB 返回内容异常，当前仅显示本地解析结果',
    tmdb_request_failed: 'TMDB 请求失败，当前仅显示本地解析结果',
    tmdb_invalid_request: '发行标题无法安全解析',
  } as Record<string, string>)[code || ''] || '暂未识别到可信作品'
}

export function upsertPTGroup(groups: PTSearchGroup[], incoming: PTSearchGroup, append = false) {
  const normalized = normalizePTSearchGroup(incoming)
  const index = groups.findIndex(group => group.site_id === normalized.site_id)
  if (index < 0) return [...groups, normalized]
  const next = [...groups]
  const previous = normalizePTSearchGroup(next[index])
  if (normalized.status === 'error' && !normalized.items.length && previous.items.length) {
    next[index] = { ...normalized, items: previous.items, partial: true }
    return next
  }
  next[index] = append && normalized.status === 'success'
    ? { ...normalized, items: [...previous.items, ...normalized.items.filter(item => !previous.items.some(existing => existing.token === item.token))] }
    : normalized
  return next
}

export function normalizePTSearchGroup(group: PTSearchGroup): PTSearchGroup {
  return { ...group, items: Array.isArray(group.items) ? group.items : [] }
}

export type TorrentSearchResult = PTSearchResult
export type TorrentSearchGroup = PTSearchGroup
export type TorrentSearchResponse = PTSearchResponse
export type TorrentRecognitionResult = PTRecognitionResult
export interface TorrentSearchProgress {
  total: number
  pending: number
  running: number
  completed: number
  succeeded: number
  failed: number
  result_count: number
  site_id?: number
  site_name?: string
  site_status?: 'queued' | 'running' | 'success' | 'error' | 'timeout' | 'cancelled' | string
  error_code?: string
  event_sequence?: number
  phase?: string
  site_detail?: TorrentSearchSiteDetail
}
export interface TorrentSearchSiteDetail {
  site_id: number
  site_name: string
  phase: string
  query_name?: string
  query_index?: number
  queries_completed?: number
  queries_failed?: number
  queries_total?: number
  result_count: number
  elapsed_ms: number
  revision?: number
}
export const buildTorrentSearchQuery = buildPTSearchQuery
export const torrentSearchURL = ptSearchURL
export const upsertTorrentGroup = upsertPTGroup

export const torrentSearchSessionKey = 'omc:server:torrent-search:v1'
export const torrentSearchSiteSelectionKey = 'omc:server:torrent-search-sites:v1'
const torrentSearchSessionMaxBytes = 512 * 1024
const torrentSearchSessionMaxAgeMs = 30 * 60 * 1000

export interface TorrentSearchSession {
  actorID?: number
  input: { keyword: string; mediaType: string; year?: number; tmdbID?: number; searchBy: 'title' | 'tmdb_id'; siteID?: number; siteIDs?: number[]; typePolicy?: DiscoveryTypePolicy; cloudTypes?: string[] }
  groups: TorrentSearchGroup[]
  recognitions: Record<string, TorrentRecognitionResult>
  searched: boolean
  savedAt: number
}

export function filterAndSortTorrentResults(groups: readonly PTSearchGroup[], filters: TorrentResultFilters): TorrentResultEntry[] {
  const scoped = filters.activeChannel === 'all'
    ? [...groups].sort((left, right) => left.site_id - right.site_id)
    : groups.filter(group => group.site_id === filters.activeChannel)
  const promotion = filters.promotion?.trim().toLowerCase() ?? ''
  const resolution = filters.resolution?.trim() ?? ''
  const timestamp = (value?: string): number | undefined => {
    const parsed = value ? Date.parse(value) : Number.NaN
    return Number.isFinite(parsed) ? parsed : undefined
  }
  const compareOptionalNumber = (left?: number | null, right?: number | null) => {
    const leftValue = typeof left === 'number' && Number.isFinite(left) ? left : undefined
    const rightValue = typeof right === 'number' && Number.isFinite(right) ? right : undefined
    if (leftValue === undefined || rightValue === undefined) {
      if (leftValue === rightValue) return 0
      return leftValue === undefined ? 1 : -1
    }
    const compared = leftValue < rightValue ? -1 : leftValue > rightValue ? 1 : 0
    return filters.direction === 'asc' ? compared : -compared
  }
  const stableTieBreak = (left: TorrentResultEntry, right: TorrentResultEntry) => left.group.site_id - right.group.site_id
    || left.item.title.localeCompare(right.item.title)
    || left.item.token.localeCompare(right.item.token)
  return scoped
    .flatMap(group => group.items.map(item => ({ item, group })))
    .filter(({ group, item }) => filters.enabledSiteTypes.includes(resultSiteType(item, group.site_type)))
    .filter(({ item }) => !filters.resultTypes?.length || !item.result_type || filters.resultTypes.includes(item.result_type))
    .filter(({ item }) => !resolution || item.specifications?.resolution === resolution || item.quality === resolution)
    .filter(({ item, group }) => resultSiteType(item, group.site_type) === 'cloud_share' || !promotion || item.promotion?.toLowerCase() === promotion)
    .filter(({ item, group }) => resultSiteType(item, group.site_type) === 'cloud_share' || filters.minimumSeeders == null || (item.seeders ?? -1) >= filters.minimumSeeders)
    .sort((left, right) => {
      if (filters.sort === 'published') return compareOptionalNumber(timestamp(left.item.published_at), timestamp(right.item.published_at)) || left.group.site_id - right.group.site_id
      if (filters.sort === 'size') return compareOptionalNumber(left.item.size_bytes, right.item.size_bytes) || stableTieBreak(left, right)
      return compareOptionalNumber(left.item.seeders, right.item.seeders)
        || compareOptionalNumber(left.item.completed, right.item.completed)
        || compareOptionalNumber(timestamp(left.item.published_at), timestamp(right.item.published_at))
        || stableTieBreak(left, right)
    })
}

type SiteSelectionStorage = Pick<Storage, 'getItem' | 'setItem'>

export function readTorrentSearchSiteSelection(storage: SiteSelectionStorage | undefined, options: readonly SearchSiteOption[]): number[] {
  const selectable = options.filter(option => option.searchable).map(option => option.id)
  if (!storage) return selectable
  try {
    const raw = storage.getItem(torrentSearchSiteSelectionKey)
    if (!raw) return selectable
    const value = JSON.parse(raw) as { site_ids?: unknown }
    if (!Array.isArray(value.site_ids)) return selectable
    const selected = new Set(value.site_ids.filter((id): id is number => typeof id === 'number' && Number.isInteger(id) && id > 0).slice(0, 64))
    return selectable.filter(id => selected.has(id))
  } catch {
    return selectable
  }
}

export function saveTorrentSearchSiteSelection(storage: SiteSelectionStorage | undefined, siteIDs: readonly number[]) {
  if (!storage) return
  const normalized = [...new Set(siteIDs.filter(id => Number.isInteger(id) && id > 0))].slice(0, 64)
  try { storage.setItem(torrentSearchSiteSelectionKey, JSON.stringify({ site_ids: normalized })) }
  catch { /* Search remains usable when browser storage is blocked or full. */ }
}

export function ptRecognitionEpisodeLabel(value: PTRecognitionResult) {
  if (value.media_type !== 'tv' || !value.episodes) return ''
  const facts = value.episodes
  const labels: string[] = []
  if (Number.isInteger(facts.season) && (facts.season ?? -1) >= 0) {
    labels.push(`第 ${facts.season} 季${facts.season_year ? `（${facts.season_year}）` : ''}`)
  }
  if (Number.isInteger(facts.episode_min) && (facts.episode_min ?? 0) > 0) {
    const maximum = Number.isInteger(facts.episode_max) && (facts.episode_max ?? 0) >= (facts.episode_min ?? 0) ? facts.episode_max : facts.episode_min
    labels.push(maximum === facts.episode_min ? `第 ${facts.episode_min} 集` : `第 ${facts.episode_min}–${maximum} 集`)
  }
  if ((facts.count ?? 0) > 1) labels.push(`多集（共 ${facts.count} 集）`)
  return labels.join(' · ')
}

type SearchSessionStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>

export function saveTorrentSearchSession(storage: SearchSessionStorage | undefined, state: TorrentSearchSession) {
  if (!storage) return
  try {
    const groups: TorrentSearchGroup[] = []
    let itemCount = 0
    for (const rawGroup of state.groups.slice(0, 24)) {
      const group = normalizePTSearchGroup(rawGroup)
      const remaining = Math.max(0, 300 - itemCount)
      const items = group.items.slice(0, remaining).map(item => {
        const saved = { ...item }
        delete saved.share_validation
        return saved
      })
      itemCount += items.length
      groups.push({ ...group, items })
      if (itemCount >= 300) break
    }
    const allowedTokens = new Set(groups.flatMap(group => group.items.map(item => item.token)))
    const recognitions = Object.fromEntries(Object.entries(state.recognitions).filter(([token]) => allowedTokens.has(token)))
    const encoded = JSON.stringify({ ...state, groups, recognitions })
    if (encoded.length > torrentSearchSessionMaxBytes) {
      storage.removeItem(torrentSearchSessionKey)
      return
    }
    storage.setItem(torrentSearchSessionKey, encoded)
  } catch {
    // Search remains usable when browser storage is blocked or full.
  }
}

export function readTorrentSearchSession(storage: SearchSessionStorage | undefined, now = Date.now()): TorrentSearchSession | null {
  if (!storage) return null
  try {
    const raw = storage.getItem(torrentSearchSessionKey)
    if (!raw || raw.length > torrentSearchSessionMaxBytes) return null
    const value = JSON.parse(raw) as Partial<TorrentSearchSession>
    if (!value.input || !Array.isArray(value.groups) || !value.recognitions || typeof value.savedAt !== 'number' || now - value.savedAt < 0 || now - value.savedAt > torrentSearchSessionMaxAgeMs) {
      storage.removeItem(torrentSearchSessionKey)
      return null
    }
    if (value.input.searchBy !== 'title' && value.input.searchBy !== 'tmdb_id') return null
    if (value.input.typePolicy !== undefined && !['auto', 'all', 'manual'].includes(value.input.typePolicy)) return null
    const cloudTypes = Array.isArray(value.input.cloudTypes) ? [...new Set(value.input.cloudTypes.filter(type => discoveryTypeOptions.some(option => option[0] === type)))].slice(0, discoveryTypeOptions.length) : undefined
    if (value.input.typePolicy === 'manual' && !cloudTypes?.length) return null
    const scopeSiteID = typeof value.input.siteID === 'number' && Number.isInteger(value.input.siteID) && value.input.siteID > 0 ? value.input.siteID : undefined
    const scopeSiteIDs = Array.isArray(value.input.siteIDs) ? [...new Set(value.input.siteIDs.filter((id): id is number => typeof id === 'number' && Number.isInteger(id) && id > 0))].slice(0, 64) : undefined
    // Sessions created before explicit site scoping must never revive the old
    // implicit all-site behavior. The next search will reopen the selector.
    if ((!scopeSiteID && !scopeSiteIDs?.length) || (scopeSiteID && scopeSiteIDs?.length)) {
      storage.removeItem(torrentSearchSessionKey)
      return null
    }
    let remainingItems = 300
    const groups = value.groups
      .slice(0, 24)
      .map((group) => {
        const items = remainingItems > 0 && Array.isArray(group.items)
          ? group.items.filter(item => typeof item?.token === 'string' && typeof item.title === 'string' && typeof item.expires_at === 'string' && Date.parse(item.expires_at) > now).slice(0, remainingItems)
          : []
        remainingItems -= items.length
        return { ...group, items }
      })
    const tokens = new Set(groups.flatMap(group => group.items.map(item => item.token)))
    const recognitions = Object.fromEntries(Object.entries(value.recognitions)
      .filter(([token]) => tokens.has(token))
      .map(([token, recognition]) => [token, restoreTorrentRecognition(recognition)] as const)
      .filter((entry): entry is readonly [string, TorrentRecognitionResult] => entry[1] !== null))
    return {
      actorID: typeof value.actorID === 'number' && Number.isSafeInteger(value.actorID) && value.actorID > 0 ? value.actorID : undefined,
      input: {
        keyword: typeof value.input.keyword === 'string' ? value.input.keyword.slice(0, 160) : '',
        mediaType: typeof value.input.mediaType === 'string' ? value.input.mediaType : '',
        year: typeof value.input.year === 'number' ? value.input.year : undefined,
        tmdbID: typeof value.input.tmdbID === 'number' ? value.input.tmdbID : undefined,
        searchBy: value.input.searchBy,
        siteID: scopeSiteID,
        siteIDs: scopeSiteIDs,
        typePolicy: value.input.typePolicy,
        cloudTypes,
      },
      groups,
      recognitions,
      searched: Boolean(value.searched),
      savedAt: value.savedAt,
    }
  } catch {
    return null
  }
}

function restoreTorrentRecognition(value: unknown): TorrentRecognitionResult | null {
  if (!value || typeof value !== 'object') return null
  const candidate = value as Partial<TorrentRecognitionResult>
  if (candidate.engine_version !== ptRecognitionEngineVersion || (candidate.status !== 'matched' && candidate.status !== 'unrecognized') || typeof candidate.title !== 'string' || !candidate.specifications || typeof candidate.specifications !== 'object') return null
  const episodes = restoreEpisodeFacts(candidate.episodes)
  return { ...candidate, engine_version: candidate.engine_version, status: candidate.status, manual_override: candidate.manual_override === true || undefined, title: candidate.title.slice(0, 512), specifications: candidate.specifications, episodes } as TorrentRecognitionResult
}

function restoreEpisodeFacts(value: unknown): PTRecognitionEpisodeFacts | undefined {
  if (!value || typeof value !== 'object') return undefined
  const source = value as Record<string, unknown>
  const integer = (key: string, minimum: number, maximum: number) => typeof source[key] === 'number' && Number.isInteger(source[key]) && (source[key] as number) >= minimum && (source[key] as number) <= maximum ? source[key] as number : undefined
  const result: PTRecognitionEpisodeFacts = {
    season: integer('season', 0, 200),
    season_year: integer('season_year', 1888, 2200),
    episode_min: integer('episode_min', 1, 100000),
    episode_max: integer('episode_max', 1, 100000),
    count: integer('count', 1, 100000),
  }
  if (result.episode_min !== undefined && result.episode_max !== undefined && result.episode_max < result.episode_min) return undefined
  return Object.values(result).some(item => item !== undefined) ? result : undefined
}
