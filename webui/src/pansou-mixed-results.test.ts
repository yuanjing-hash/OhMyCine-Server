import { expect, it } from 'vitest'
import { buildTorrentSearchQuery, discoveryOrigins, filterAndSortTorrentResults, normalizeCloudSiteConfig, readTorrentSearchSession, safeDiscoveryLink, saveTorrentSearchSession, siteEffectiveEnabled, upsertPTGroup, type PTSearchGroup } from './sites'

const group: PTSearchGroup = { site_id: 7, site_name: '盘搜', site_type: 'cloud_share', status: 'success', page: 1, has_next: true, skipped: 0, snapshot_id: 'snapshot-a', items: [
  { token: '115', title: '115分享', source_kind: '115_share', result_type: '115', expires_at: '2026-10-22T12:10:00Z' },
  { token: 'quark', title: '夸克分享', source_kind: 'quark_share', result_type: 'quark', expires_at: '2026-10-22T12:10:00Z' },
  { token: 'magnet', title: '磁力', source_kind: 'bt', result_type: 'magnet', expires_at: '2026-10-22T12:10:00Z' },
] }
it('filters each PanSou result by its own family while preserving ordinary PT and plugin site categories', () => {
  const base = { activeChannel: 'all' as const, sort: 'published' as const, direction: 'desc' as const }
  expect(filterAndSortTorrentResults([group], { ...base, enabledSiteTypes: ['bt'] }).map(value => value.item.token)).toEqual(['magnet'])
  expect(filterAndSortTorrentResults([group], { ...base, enabledSiteTypes: ['cloud_share'] }).map(value => value.item.token)).toEqual(['115', 'quark'])
  expect(filterAndSortTorrentResults([{ ...group, site_type: 'pt' }], { ...base, enabledSiteTypes: ['pt'] })).toHaveLength(3)
})
it('retains valid earlier results on a later error and binds page/type query to the original snapshot', () => {
  const failed = upsertPTGroup([group], { ...group, status: 'error', error_code: 'site_unavailable', items: [] })
  expect(failed[0].items).toHaveLength(3)
  expect(failed[0].partial).toBe(true)
  expect(filterAndSortTorrentResults(failed, { activeChannel: 7, enabledSiteTypes: ['bt', 'cloud_share'], sort: 'published', direction: 'desc' })).toHaveLength(3)
  const query = buildTorrentSearchQuery({ keyword: 'Title', siteID: 7, page: 2, snapshotID: 'snapshot-a', typePolicy: 'manual', cloudTypes: ['quark', 'magnet'] })
  expect(query.get('snapshot_id')).toBe('snapshot-a')
  expect(query.get('client_capabilities')).toBe('mixed_discovery_v1')
  expect(query.getAll('cloud_types')).toEqual(['quark', 'magnet'])
  expect(query.has('refresh')).toBe(false)
})
it('keeps origin kinds independent, never generates a TG URL for plugins, and rejects unsafe links', () => {
  expect(discoveryOrigins({ origins: [{ kind: 'plugin', name: 'gying', post_url: 'https://t.me/fake/3' }, { kind: 'tg', name: 'channel', post_url: 'javascript:alert(1)' }] })).toEqual([{ kind: 'plugin', name: 'gying', post_url: undefined }, { kind: 'tg', name: 'channel', post_url: undefined }])
  expect(safeDiscoveryLink('javascript:alert(1)')).toBeUndefined()
  expect(safeDiscoveryLink('https://user:secret@example.com/s/abc')).toBeUndefined()
  expect(safeDiscoveryLink('http://example.com/s/abc')).toBe('http://example.com/s/abc')
  expect(siteEffectiveEnabled({ enabled: true, effective_enabled: false })).toBe(false)
  expect(normalizeCloudSiteConfig({ channels: ['alpha'], auth_enabled: false }).channel_mode).toBe('custom_only')
})
it('restores the applied type policy and snapshot without restoring account or link data', () => {
  const values = new Map<string, string>()
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value) }, removeItem: (key: string) => { values.delete(key) } }
  const now = Date.parse('2026-10-22T12:00:00Z')
  saveTorrentSearchSession(storage, { input: { keyword: 'Title', searchBy: 'title', mediaType: 'tv', siteID: 7, typePolicy: 'manual', cloudTypes: ['quark', 'magnet'] }, groups: [group], recognitions: {}, searched: true, savedAt: now })
  const restored = readTorrentSearchSession(storage, now)
  expect(restored?.input.cloudTypes).toEqual(['quark', 'magnet'])
  expect(restored?.groups[0].snapshot_id).toBe('snapshot-a')
})
