export type UserMediaSectionStatus = 'ok' | 'unavailable'

export interface UserMediaSection<T> {
  status: UserMediaSectionStatus
  list: T[]
  has_more: boolean
  error_code?: string
}

export interface UserMediaItem {
  library_id: number
  work_id: string
  title: string
  kind: 'movie' | 'series'
  release_year?: number
  rating?: number
  poster_url?: string
  backdrop_url?: string
  season_count: number
  episode_count: number
  category_name?: string
  modified_at: string
}

export interface UserHistoryItem {
  library_id: number
  work_id: string
  title: string
  subtitle?: string
  media_type?: string
  poster_url?: string
  backdrop_url?: string
  position: number
  duration?: number
  completed: boolean
  updated_at: number
}

export interface UserCollectionSummary {
  id: string
  name: string
  kind: 'collection' | 'playlist'
  source: 'tmdb' | 'manual'
  item_count: number
  poster_url?: string
  backdrop_url?: string
}

export interface UserMediaLibrarySummary {
  id: number
  name: string
  status: string
  entry_count: number
  work_count: number
  artwork_url?: string
  last_successful_scan_at?: string
}

export interface UserMediaOverview {
  version: 'v1'
  sections: {
    continue_watching: UserMediaSection<UserHistoryItem>
    recently_added: UserMediaSection<UserMediaItem>
    favorites: UserMediaSection<UserMediaItem>
    automatic_collections: UserMediaSection<UserCollectionSummary>
    manual_collections: UserMediaSection<UserCollectionSummary>
    media_libraries: UserMediaSection<UserMediaLibrarySummary>
  }
}

export interface UserHistoryPage {
  list: UserHistoryItem[]
  total: number
  page: number
  page_size: number
  has_more: boolean
}

export const userMediaEndpoints = {
  overview: '/api/v1/media-libraries/overview',
  history: (page: number, pageSize: number) => `/api/v1/media-libraries/history?page=${Math.max(1, Math.trunc(page))}&page_size=${Math.max(1, Math.trunc(pageSize))}`,
  favorites: '/api/v1/media-libraries/favorites',
  collections: (kind: 'collection' | 'playlist' = 'collection') => `/api/v1/media-libraries/collections?kind=${kind}`,
  collectionItems: (id: string) => `/api/v1/media-libraries/collections/${encodeURIComponent(id)}/items`,
} as const

export function emptyUserMediaOverview(): UserMediaOverview {
  const section = <T>(): UserMediaSection<T> => ({ status: 'ok', list: [], has_more: false })
  return {
    version: 'v1',
    sections: {
      continue_watching: section<UserHistoryItem>(),
      recently_added: section<UserMediaItem>(),
      favorites: section<UserMediaItem>(),
      automatic_collections: section<UserCollectionSummary>(),
      manual_collections: section<UserCollectionSummary>(),
      media_libraries: section<UserMediaLibrarySummary>(),
    },
  }
}

export function normalizeUserMediaOverview(value: unknown): UserMediaOverview {
  const root = record(value)
  const sections = record(root.sections)
  return {
    version: 'v1',
    sections: {
      continue_watching: normalizeSection(sections.continue_watching, normalizeHistoryItem, completeHistoryItem),
      recently_added: normalizeSection(sections.recently_added, normalizeMediaItem, completeMediaItem),
      favorites: normalizeSection(sections.favorites, normalizeMediaItem, completeMediaItem),
      automatic_collections: normalizeSection(sections.automatic_collections, normalizeCollection, item => !!item.id),
      manual_collections: normalizeSection(sections.manual_collections, normalizeCollection, item => !!item.id),
      media_libraries: normalizeSection(sections.media_libraries, normalizeLibrary, item => item.id > 0),
    },
  }
}

export function normalizeUserHistoryPage(value: unknown): UserHistoryPage {
  const source = record(value)
  return {
    list: array(source.list).map(normalizeHistoryItem).filter(completeHistoryItem),
    total: nonNegativeNumber(source.total), page: positiveNumber(source.page, 1),
    page_size: positiveNumber(source.page_size, 24), has_more: source.has_more === true,
  }
}

export function normalizeUserMediaItems(value: unknown): UserMediaItem[] {
  return array(record(value).list).map(normalizeMediaItem).filter(completeMediaItem)
}

export function normalizeUserCollections(value: unknown): UserCollectionSummary[] {
  return array(record(value).list).map(normalizeCollection).filter(item => !!item.id)
}

export function historyProgress(item: UserHistoryItem): number {
  if (!item.duration || item.duration <= 0) return 0
  return Math.max(0, Math.min(100, item.position / item.duration * 100))
}

function normalizeSection<T>(value: unknown, normalize: (value: unknown) => T, complete: (item: T) => boolean): UserMediaSection<T> {
  const source = record(value)
  return {
    status: source.status === 'unavailable' ? 'unavailable' : 'ok',
    list: array(source.list).map(normalize).filter(complete), has_more: source.has_more === true,
    ...(text(source.error_code) ? { error_code: text(source.error_code) } : {}),
  }
}

function normalizeMediaItem(value: unknown): UserMediaItem {
  const source = record(value)
  return {
    library_id: positiveNumber(source.library_id, 0), work_id: text(source.work_id),
    title: text(source.title) || '未命名作品', kind: source.kind === 'series' ? 'series' : 'movie',
    ...(optionalNumber(source.release_year) != null ? { release_year: optionalNumber(source.release_year) } : {}),
    ...(optionalNumber(source.rating) != null ? { rating: optionalNumber(source.rating) } : {}),
    ...(text(source.poster_url) ? { poster_url: text(source.poster_url) } : {}),
    ...(text(source.backdrop_url) ? { backdrop_url: text(source.backdrop_url) } : {}),
    season_count: nonNegativeNumber(source.season_count), episode_count: nonNegativeNumber(source.episode_count),
    ...(text(source.category_name) ? { category_name: text(source.category_name) } : {}), modified_at: text(source.modified_at),
  }
}

function normalizeHistoryItem(value: unknown): UserHistoryItem {
  const source = record(value)
  return {
    library_id: positiveNumber(source.library_id, 0), work_id: text(source.work_id),
    title: text(source.title) || '未命名作品', ...(text(source.subtitle) ? { subtitle: text(source.subtitle) } : {}),
    ...(text(source.media_type) ? { media_type: text(source.media_type) } : {}),
    ...(text(source.poster_url) ? { poster_url: text(source.poster_url) } : {}),
    ...(text(source.backdrop_url) ? { backdrop_url: text(source.backdrop_url) } : {}),
    position: nonNegativeNumber(source.position), ...(optionalNumber(source.duration) != null ? { duration: nonNegativeNumber(source.duration) } : {}),
    completed: source.completed === true, updated_at: nonNegativeNumber(source.updated_at),
  }
}

function normalizeCollection(value: unknown): UserCollectionSummary {
  const source = record(value)
  return {
    id: text(source.id), name: text(source.name) || '未命名合集', kind: source.kind === 'playlist' ? 'playlist' : 'collection',
    source: source.source === 'manual' ? 'manual' : 'tmdb', item_count: nonNegativeNumber(source.item_count),
    ...(text(source.poster_url) ? { poster_url: text(source.poster_url) } : {}),
    ...(text(source.backdrop_url) ? { backdrop_url: text(source.backdrop_url) } : {}),
  }
}

function normalizeLibrary(value: unknown): UserMediaLibrarySummary {
  const source = record(value)
  return {
    id: positiveNumber(source.id, 0), name: text(source.name) || '未命名媒体库', status: text(source.status),
    entry_count: nonNegativeNumber(source.entry_count), work_count: nonNegativeNumber(source.work_count),
    ...(text(source.artwork_url) ? { artwork_url: text(source.artwork_url) } : {}),
    ...(text(source.last_successful_scan_at) ? { last_successful_scan_at: text(source.last_successful_scan_at) } : {}),
  }
}

function completeMediaItem(item: UserMediaItem): boolean { return item.library_id > 0 && !!item.work_id }
function completeHistoryItem(item: UserHistoryItem): boolean { return item.library_id > 0 && !!item.work_id }
function record(value: unknown): Record<string, unknown> { return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {} }
function array(value: unknown): unknown[] { return Array.isArray(value) ? value : [] }
function text(value: unknown): string { return typeof value === 'string' ? value : '' }
function optionalNumber(value: unknown): number | undefined { return typeof value === 'number' && Number.isFinite(value) ? value : undefined }
function nonNegativeNumber(value: unknown): number { const number = optionalNumber(value); return number != null && number >= 0 ? number : 0 }
function positiveNumber(value: unknown, fallback: number): number { const number = optionalNumber(value); return number != null && number > 0 ? Math.trunc(number) : fallback }
