import { describe, expect, it } from 'vitest'
import { filterAndSortTorrentResults, resultPayloadKind, resultPreviewSourceKind, resultSiteType, type PTSearchGroup, type PTSearchResult } from './sites'

function item(token: string, extra: Partial<PTSearchResult> = {}): PTSearchResult {
  return { token, title: token, expires_at: '2099-01-01T00:00:00Z', ...extra }
}
const publicGroup: PTSearchGroup = { site_id: 1, site_name: 'Mixed', site_type: 'cloud_share', status: 'success', page: 1, has_next: false, skipped: 0, items: [
  item('public-torrent', { source_kind: 'bt', payload_kind: 'torrent', specifications: { resolution: '1080p' } }),
  item('magnet', { source_kind: 'bt', result_type: 'magnet', specifications: { resolution: '2160p' } }),
  item('115', { source_kind: '115_share', result_type: '115', payload_kind: 'share' }),
  item('quark', { source_kind: 'quark_share', result_type: 'quark', payload_kind: 'share' }),
  item('legacy-unknown', { source_kind: 'bt' }),
] }
const privateGroup: PTSearchGroup = { ...publicGroup, site_id: 2, site_name: 'Private', site_type: 'pt', items: [item('pt-torrent', { payload_kind: 'torrent' }), item('pt-magnet', { payload_kind: 'magnet' })] }
const base = { activeChannel: 'all' as const, enabledSiteTypes: ['pt', 'bt', 'cloud_share'] as const, sort: 'seeders' as const, direction: 'desc' as const }
const tokens = (resultTypes: string[], extra = {}) => filterAndSortTorrentResults([publicGroup, privateGroup], { ...base, resultTypes, ...extra }).map(entry => entry.item.token).sort()

describe('truthful resource type filters', () => {
  it.each([
    ['pt', ['pt-magnet', 'pt-torrent']],
    ['torrent', ['public-torrent']],
    ['magnet', ['magnet', 'pt-magnet']],
    ['115', ['115']],
    ['quark', ['quark']],
  ])('filters %s without treating a site label as payload evidence', (kind, expected) => {
    expect(tokens([kind as string])).toEqual(expected)
  })

  it('combines resource types with OR and independent dimensions with AND', () => {
    expect(tokens(['torrent', 'magnet'], { resolution: '2160p' })).toEqual(['magnet'])
    expect(tokens(['115', 'quark'])).toEqual(['115', 'quark'])
    expect(tokens(['magnet'], { activeChannel: 2 })).toEqual(['pt-magnet'])
  })

  it('keeps unknown legacy payloads in all results and preserves private PT provenance', () => {
    expect(tokens([])).toContain('legacy-unknown')
    expect(tokens(['torrent', 'magnet'])).not.toContain('legacy-unknown')
    expect(resultPayloadKind(item('old', { source_kind: 'bt' }))).toBe('unknown')
    expect(resultSiteType(item('private', { source_kind: 'bt', payload_kind: 'magnet' }), 'pt')).toBe('pt')
  })

  it('uses the canonical recommendation and falls back safely for old servers', () => {
    expect(resultPreviewSourceKind(item('magnet', { result_type: 'magnet' }), 'url')).toBe('url')
    expect(resultPreviewSourceKind(item('magnet', { result_type: 'magnet' }))).toBe('url')
    expect(resultPreviewSourceKind(item('private-magnet', { source_kind: 'pt', payload_kind: 'magnet' }), 'torrent')).toBe('torrent')
    expect(resultPreviewSourceKind(item('old-private', { source_kind: 'pt' }))).toBe('torrent')
    expect(resultPreviewSourceKind(item('quark', { source_kind: 'quark_share' }))).toBe('quark_share')
  })
})
