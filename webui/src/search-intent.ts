import type { DiscoveryMediaType } from '@/discovery'

export interface ResourceSearchDraft {
  keyword: string
  mediaType: string
  year?: number
  tmdbID?: number
  searchBy: 'title' | 'tmdb_id'
  /** Opaque acquisition target, independent of a work's display identity. */
  acquisitionContextKey?: string
}

export interface BoundResourceIdentity {
  mediaType: DiscoveryMediaType
  tmdbID: number
  title: string
  draft: ResourceSearchDraft
}

function boundedInteger(value: unknown, minimum: number, maximum: number) {
  const parsed = typeof value === 'number' ? value : typeof value === 'string' && value.trim() ? Number(value) : Number.NaN
  return Number.isSafeInteger(parsed) && parsed >= minimum && parsed <= maximum ? parsed : undefined
}

export function normalizeResourceSearchDraft(input: ResourceSearchDraft): ResourceSearchDraft {
  return {
    keyword: input.keyword.trim().slice(0, 160),
    mediaType: input.mediaType === 'movie' || input.mediaType === 'tv' ? input.mediaType : '',
    year: boundedInteger(input.year, 1880, 2200),
    tmdbID: boundedInteger(input.tmdbID, 1, Number.MAX_SAFE_INTEGER),
    searchBy: input.searchBy === 'tmdb_id' ? 'tmdb_id' : 'title',
    acquisitionContextKey: typeof input.acquisitionContextKey === 'string' && input.acquisitionContextKey.trim() ? input.acquisitionContextKey.trim().slice(0, 256) : undefined,
  }
}

export function resourceSearchIntentFromRoute(query: Record<string, unknown>) {
  const draft = normalizeResourceSearchDraft({
    keyword: typeof query.title === 'string' ? query.title : '',
    mediaType: typeof query.media_type === 'string' ? query.media_type : '',
    year: boundedInteger(query.year, 1880, 2200),
    tmdbID: boundedInteger(query.tmdb_id, 1, Number.MAX_SAFE_INTEGER),
    searchBy: query.search_by === 'tmdb_id' ? 'tmdb_id' : 'title',
    acquisitionContextKey: resourceAcquisitionContextKey(query),
  })
  const identity: BoundResourceIdentity | null = query.identity === 'tmdb' && (draft.mediaType === 'movie' || draft.mediaType === 'tv') && draft.tmdbID
    ? { mediaType: draft.mediaType, tmdbID: draft.tmdbID, title: draft.keyword, draft: { ...draft } }
    : null
  return { draft, identity }
}

export function sameResourceSearchDraft(left: ResourceSearchDraft, right: ResourceSearchDraft) {
  const a = normalizeResourceSearchDraft(left)
  const b = normalizeResourceSearchDraft(right)
  return a.keyword === b.keyword && a.mediaType === b.mediaType && a.year === b.year && a.tmdbID === b.tmdbID && a.searchBy === b.searchBy && a.acquisitionContextKey === b.acquisitionContextKey
}

export function resourceAcquisitionContextKey(query: Record<string, unknown>) {
  const value = query.upgrade_id ?? query.upgrade
  return typeof value === 'string' && value.trim() ? value.trim().slice(0, 256) : undefined
}

export function resourceSearchRouteKey(query: Record<string, unknown>) {
  return JSON.stringify(['mode', 'site_id', 'title', 'identity', 'media_type', 'year', 'tmdb_id', 'search_by', 'query', 'upgrade_id', 'upgrade'].map(key => query[key] ?? null))
}
