import { describe, expect, test } from 'vitest'
import { emptyRuntimeLogFilters, filtersFromQuery, filtersToParams, filtersToQuery } from './runtime-logs'
import { logFactRows, logFieldText, logOperationLabel, logStageLabel } from './execution-logs'

describe('runtime log filters', () => {
  test('round trips only non-sensitive filter fields', () => {
    const filters=emptyRuntimeLogFilters(new Date('2026-08-13T12:00:00Z')); filters.module='storage';filters.operation='incremental_strm_generation';filters.pluginId='plugin.demo';filters.keyword='timeout'
    const query=filtersToQuery(filters);expect(query).toMatchObject({module:'storage',operation:'incremental_strm_generation',plugin_id:'plugin.demo',keyword:'timeout'});expect(JSON.stringify(query)).not.toContain('message')
    expect(filtersFromQuery(query)).toMatchObject({pluginId:'plugin.demo',operation:'incremental_strm_generation'})
  })
  test('serializes distinct queue, follow and business correlations',()=>{const filters=emptyRuntimeLogFilters();filters.libraryId='7';filters.requestId='req-1';filters.jobId='job-2';filters.followId='f-3';filters.followRunId='run-4';filters.taskId='download-5';filters.attempt='2';const params=filtersToParams(filters);expect(Object.fromEntries(params)).toMatchObject({library_id:'7',request_id:'req-1',job_id:'job-2',follow_id:'f-3',follow_run_id:'run-4',task_id:'download-5',attempt:'2'});expect(filtersFromQuery(filtersToQuery(filters))).toEqual(filters)})
  test('localizes real scan fields while retaining technical identities separately',()=>{expect(logStageLabel('recognition_running')).toBe('后台识别');expect(logStageLabel('future_phase')).toBe('其他处理步骤');expect(logOperationLabel('future_operation')).toBe('其他业务操作');expect(logFieldText('action','artifact_queued')).toBe('STRM/产物已入队');expect(logFieldText('persistence_stage','persist_entries')).toBe('提交媒体目录');expect(logFactRows({provider_call_count:3,provider_wait_ms:24,scan_run_id:9})).toEqual([{key:'provider_call_count',label:'数据源请求次数',text:'3'},{key:'provider_wait_ms',label:'限流/排队耗时（毫秒）',text:'24'}]);expect(logFactRows({scan_run_id:9},true)).toEqual([{key:'scan_run_id',label:'扫描运行 ID',text:'9'}])})
})
