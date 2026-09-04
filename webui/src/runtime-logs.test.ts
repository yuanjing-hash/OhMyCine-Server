import { describe, expect, test } from 'vitest'
import { readFileSync } from 'node:fs'
import { emptyRuntimeLogFilters, filtersFromQuery, filtersToParams, filtersToQuery } from './runtime-logs'

describe('runtime log filters', () => {
  test('round trips only non-sensitive filter fields', () => {
    const filters=emptyRuntimeLogFilters(new Date('2026-08-13T12:00:00Z')); filters.module='storage';filters.operation='incremental_strm_generation';filters.pluginId='plugin.demo';filters.keyword='timeout'
    const query=filtersToQuery(filters);expect(query).toMatchObject({module:'storage',operation:'incremental_strm_generation',plugin_id:'plugin.demo',keyword:'timeout'});expect(JSON.stringify(query)).not.toContain('message')
    expect(filtersFromQuery(query)).toMatchObject({pluginId:'plugin.demo',operation:'incremental_strm_generation'})
  })
  test('serializes advanced correlation ids',()=>{const filters=emptyRuntimeLogFilters();filters.libraryId='7';filters.requestId='req-1';const params=filtersToParams(filters);expect(params.get('library_id')).toBe('7');expect(params.get('request_id')).toBe('req-1')})
  test('renders scan phases and fields with readable labels',()=>{const source=readFileSync(new URL('./views/RuntimeLogsView.vue',import.meta.url),'utf8');for(const label of ['当前步骤','媒体','数据源请求次数','限流/排队耗时（毫秒）','后台识别','STRM/产物已入队','正在生成 STRM/产物','其他处理步骤'])expect(source).toContain(label);for(const stage of ['persist_source_assets','persist_recognition','persist_entries','prune_stale_entries','reconcile_tmdb_collections','advance_library_generation','persist_scan_run','record_media_change'])expect(source).toContain(stage)})
})
