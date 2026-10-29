import { describe, expect, it } from 'vitest'
import { normalizeResourceSearchDraft, resourceSearchIntentFromRoute, resourceSearchRouteKey, sameResourceSearchDraft, type ResourceSearchDraft } from './search-intent'

const draft: ResourceSearchDraft = { keyword: '作品', mediaType: 'tv', year: 2026, tmdbID: 123, searchBy: 'title' }

describe('resource search intent identity', () => {
  it('normalizes user drafts and rejects ambiguous route values', () => {
    expect(normalizeResourceSearchDraft({ ...draft, keyword: ' 作品 ', year: Number.NaN, tmdbID: -1 })).toEqual({ ...draft, year: undefined, tmdbID: undefined, acquisitionContextKey: undefined })
    const invalid = resourceSearchIntentFromRoute({ title: ['A', 'B'], identity: 'tmdb', tmdb_id: ['123'], media_type: 'tv', year: 'Infinity' })
    expect(invalid.identity).toBeNull()
    expect(invalid.draft.keyword).toBe('')
    expect(invalid.draft.year).toBeUndefined()
  })

  it.each(['keyword', 'mediaType', 'year', 'tmdbID', 'searchBy', 'acquisitionContextKey'] as const)('does not reuse a bound identity after %s changes', field => {
    const changes = { keyword: '另一作品', mediaType: 'movie', year: 2025, tmdbID: 456, searchBy: 'tmdb_id', acquisitionContextKey: 'upgrade-two' }
    expect(sameResourceSearchDraft(draft, { ...draft, [field]: changes[field] })).toBe(false)
  })

  it('keeps an opaque acquisition target distinct for the same work and season', () => {
    const common = { title: '作品', identity: 'tmdb', tmdb_id: '123', media_type: 'tv' }
    const first = resourceSearchIntentFromRoute({ ...common, upgrade_id: 'opaque-entry-13' })
    const second = resourceSearchIntentFromRoute({ ...common, upgrade_id: 'opaque-entry-15' })
    expect(first.identity?.tmdbID).toBe(second.identity?.tmdbID)
    expect(sameResourceSearchDraft(first.draft, second.draft)).toBe(false)
    expect(resourceSearchRouteKey({ ...common, upgrade_id: 'opaque-entry-13' })).not.toBe(resourceSearchRouteKey({ ...common, upgrade_id: 'opaque-entry-15' }))
  })
})
