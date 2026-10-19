<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import { createLatestRequest } from '@/latest-request'
import { deduplicateLogEntries, logFactRows, logLevelLabel, logOperationLabel, logStageLabel, logOutcomeLabel } from '@/execution-logs'
import { filtersFromQuery, filtersToParams, filtersToQuery, type RuntimeLogFilters } from '@/runtime-logs'
import type { RuntimeLogEntry, RuntimeLogFacets, RuntimeLogResult, RuntimeLogSettings } from '@/types/api'

const auth = useAuthStore(), route = useRoute(), router = useRouter()
const filters = reactive<RuntimeLogFilters>(filtersFromQuery(route.query))
let appliedFilters: RuntimeLogFilters = { ...filters }
const entries = ref<RuntimeLogEntry[]>([])
const facets = ref<RuntimeLogFacets>({ levels: [], modules: [], components: [], operations: [], plugin_ids: [] })
const settings = ref<RuntimeLogSettings | null>(null)
const loading = ref(false), saving = ref(false), exporting = ref(false), error = ref('')
const exportNotice = ref('')
const cursor = ref(''), snapshot = ref(''), partial = ref(false), malformed = ref(0), paused = ref(typeof route.query.to === 'string'), historical = ref(false)
const listRequest = createLatestRequest(), metadataRequest = createLatestRequest(), exportRequest = createLatestRequest(), settingsRequest = createLatestRequest()
let alive = true, timer: number | undefined
const advanced = ref(false)
const filtersOpen = ref(false)
const canExport = computed(() => auth.can(Permissions.LogsExport)), canConfigure = computed(() => auth.can(Permissions.LogsConfigure))
function localInput(iso: string) { const date = new Date(iso); if (Number.isNaN(date.getTime())) return ''; const pad = (n: number) => String(n).padStart(2, '0'); return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) + 'T' + pad(date.getHours()) + ':' + pad(date.getMinutes()) }
function updateDate(field: 'from' | 'to', value: string) { const date = new Date(value); if (!Number.isNaN(date.getTime())) { filters[field] = date.toISOString(); paused.value = true } }
function levelClass(level: string) { return 'runtime-level runtime-level--' + (['error','warn','debug','info'].includes(level) ? level : 'info') }
function entryKey(entry: RuntimeLogEntry, index: number) { return entry.event_id || (typeof entry.fields.event_id === 'string' ? entry.fields.event_id : entry.timestamp + ':' + index) }
function stage(entry: RuntimeLogEntry) { const value = entry.stage ?? entry.fields.stage ?? entry.fields.phase; return typeof value === 'string' ? logStageLabel(value) : '' }
function outcome(entry: RuntimeLogEntry) { const value = entry.outcome ?? entry.fields.outcome; return typeof value === 'string' ? logOutcomeLabel(value) : '' }
function logError(reason: unknown) { if (reason instanceof APIError && reason.status === 401) return '登录已失效，请重新登录。'; if (reason instanceof APIError && reason.status === 403) return '当前账号无权查看运行日志。'; if (reason instanceof APIError && [400,409,422].includes(reason.status)) return '筛选或分页快照无效，请重新查询。'; return '运行日志读取失败，请稍后重试。' }
function clearPrivateData() { listRequest.cancel(); metadataRequest.cancel(); exportRequest.cancel(); settingsRequest.cancel(); entries.value = []; settings.value = null; facets.value = { levels: [], modules: [], components: [], operations: [], plugin_ids: [] }; cursor.value = ''; snapshot.value = ''; loading.value = false; saving.value = false; exporting.value = false; exportNotice.value = '' }
async function load(append = false, syncRoute = true) {
  if (!auth.user || !auth.can(Permissions.LogsRead)) return
  const current = listRequest.begin(); loading.value = true; error.value = ''
  if (append) { paused.value = true; historical.value = true }
  const capturedFilters = { ...(append ? appliedFilters : filters) }
  try {
    const params = filtersToParams(capturedFilters); params.set('limit', '100')
    if (append && cursor.value) { params.set('cursor', cursor.value); if (snapshot.value) params.set('snapshot_to', snapshot.value) }
    const result = await api<RuntimeLogResult>('/api/v1/runtime-logs?' + params, { signal: current.signal })
    if (!current.isCurrent()) return
    entries.value = deduplicateLogEntries(append ? [...entries.value, ...result.list] : result.list)
    if (!append) appliedFilters = capturedFilters
    cursor.value = result.next_cursor ?? ''; snapshot.value = result.snapshot_to ?? filters.to
    partial.value = append ? partial.value || result.partial : result.partial
    malformed.value = append ? malformed.value + result.malformed : result.malformed
    if (!append && syncRoute && filtersToParams(filters).toString() === filtersToParams(capturedFilters).toString()) await router.replace({ query: filtersToQuery(capturedFilters) })
  } catch (reason) {
    if (!current.isCurrent()) return
    error.value = logError(reason)
    if (reason instanceof APIError && [401,403].includes(reason.status)) clearPrivateData()
  } finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
async function loadMetadata() {
  if (!auth.user || !auth.can(Permissions.LogsRead)) return
  const current = metadataRequest.begin()
  const results = await Promise.allSettled([
    api<RuntimeLogFacets>('/api/v1/runtime-logs/facets?' + filtersToParams(filters), { signal: current.signal }),
    api<RuntimeLogSettings>('/api/v1/runtime-logs/settings', { signal: current.signal }),
  ])
  if (current.isCurrent()) { if (results[0].status === 'fulfilled') facets.value = results[0].value; if (results[1].status === 'fulfilled') settings.value = results[1].value }
  current.finish()
}
async function apply() { cursor.value = ''; historical.value = false; await Promise.all([load(), loadMetadata()]) }
function reset() { Object.assign(filters, filtersFromQuery({})); paused.value = false; void apply() }
function refresh() { if (!paused.value) filters.to = new Date().toISOString(); void apply() }
function toggleLive() { paused.value = !paused.value; if (!paused.value) { historical.value = false; filters.to = new Date().toISOString(); if (new Date(filters.to).getTime() - new Date(filters.from).getTime() > 31*86400000) filters.from = new Date(Date.now() - 86400000).toISOString(); void apply() } }
function refreshVisible() { if (!alive || document.hidden || paused.value || historical.value || exporting.value || loading.value || !auth.user || !auth.can(Permissions.LogsRead)) return; filters.to = new Date().toISOString(); void load(false, false) }
async function saveSettings() {
  if (!settings.value || !canConfigure.value) return
  const current = settingsRequest.begin(); saving.value = true; error.value = ''
  try { const result = await api<RuntimeLogSettings>('/api/v1/runtime-logs/settings', { method: 'PATCH', body: JSON.stringify(settings.value), signal: current.signal }); if (current.isCurrent()) settings.value = result }
  catch { if (current.isCurrent()) error.value = '日志策略保存失败，请刷新后重试。' }
  finally { if (current.isCurrent()) saving.value = false; current.finish() }
}
async function exportLogs() {
  if (!canExport.value) return
  paused.value = true
  listRequest.cancel(); loading.value = false
  const current = exportRequest.begin(); exporting.value = true; error.value = ''; exportNotice.value = ''
  const body = Object.fromEntries(filtersToParams(appliedFilters).entries())
  const exportSnapshot = snapshot.value || body.to
  try {
    const headers = new Headers({ 'Content-Type': 'application/json' })
    const csrf = await api<{ csrf_token: string }>('/api/v1/auth/csrf', { signal: current.signal })
    if (!current.isCurrent()) return
    headers.set('X-CSRF-Token', csrf.csrf_token)
    const response = await fetch('/api/v1/runtime-logs/export', { method: 'POST', credentials: 'include', headers, signal: current.signal, body: JSON.stringify({
      from: body.from, to: body.to, levels: body.level ? [body.level] : [], modules: body.module ? [body.module] : [],
      components: body.component ? [body.component] : [], operations: body.operation ? [body.operation] : [], plugin_ids: body.plugin_id ? [body.plugin_id] : [],
      keyword: body.keyword ?? '', request_id: body.request_id ?? '', task_id: body.task_id ?? '', library_id: body.library_id ?? '',
      connection_id: body.connection_id ?? '', storage_id: body.storage_id ?? '', downloader_id: body.downloader_id ?? '', scan_run_id: body.scan_run_id ?? '',
      job_id: body.job_id ?? '', follow_id: body.follow_id ?? '', follow_run_id: body.follow_run_id ?? '', attempt: body.attempt ?? '', snapshot_to: exportSnapshot,
    }) })
    if (!response.ok) {
      if ([401, 403].includes(response.status)) { clearPrivateData(); error.value = response.status === 401 ? '登录已失效，请重新登录。' : '当前账号无权导出运行日志。'; return }
      throw new Error('export failed')
    }
    const blob = await response.blob()
    if (!current.isCurrent()) return
    const url = URL.createObjectURL(blob); const link = document.createElement('a'); link.href = url; link.download = 'ohmycine-runtime-logs.jsonl.gz'; link.click(); URL.revokeObjectURL(url)
    if (response.headers.get('X-Log-Partial') === 'true') exportNotice.value = '导出仅含部分可用记录，可能受查询、保留或大小限制；缺失记录不代表业务未执行。'
  } catch { if (current.isCurrent()) error.value = '运行日志导出失败，请重新登录或稍后重试。' }
  finally { if (current.isCurrent()) exporting.value = false; current.finish() }
}
watch(() => route.fullPath, () => {
  const next = filtersFromQuery(route.query)
  if (filtersToParams(next).toString() === filtersToParams(filters).toString()) return
  Object.assign(filters, next); paused.value = typeof route.query.to === 'string'; cursor.value = ''; historical.value = false
  advanced.value = false
  if (next.jobId || next.followId || next.followRunId || next.scanRunId || next.taskId) filtersOpen.value = false
  void Promise.all([load(false, false), loadMetadata()])
})
watch(() => auth.user?.id, (current, previous) => { if (current !== previous) clearPrivateData() })
watch(() => auth.can(Permissions.LogsRead), allowed => { if (!allowed) clearPrivateData() })
onMounted(() => { void Promise.all([load(), loadMetadata()]); timer = window.setInterval(refreshVisible, 10000); document.addEventListener('visibilitychange', refreshVisible); window.addEventListener('omc:unauthorized', clearPrivateData) })
onBeforeUnmount(() => { alive = false; clearPrivateData(); window.clearInterval(timer); document.removeEventListener('visibilitychange', refreshVisible); window.removeEventListener('omc:unauthorized', clearPrivateData) })
</script>

<template>
  <section>
    <div class="flex flex-wrap items-start justify-between gap-4"><div><h1 class="m-0 text-3xl font-800">运行日志</h1><p class="page-description mt-2">查看各模块实际处理的对象、步骤、原因与结果；技术标识收在详情中。</p></div><div class="flex flex-wrap gap-2"><button v-if="canExport" class="btn-secondary" :disabled="exporting" @click="exportLogs">{{ exporting?'正在导出…':'导出筛选结果' }}</button><button class="btn-secondary" :disabled="loading || exporting" @click="toggleLive">{{ paused ? '返回实时' : '暂停刷新' }}</button><button class="btn-secondary" :disabled="loading || exporting" @click="refresh">刷新</button></div></div>
    <p v-if="error" class="semantic-error mt-5 p-3" role="alert">{{ error }}</p>
    <p v-if="exportNotice" class="semantic-warning mt-5 p-3" role="status">{{ exportNotice }}</p>
    <p v-if="settings?.health.degraded" class="semantic-warning mt-5 p-3">文件日志当前处于降级状态，记录可能不完整；控制台输出仍继续。</p>
    <details class="panel mt-5" :open="filtersOpen" @toggle="filtersOpen = ($event.target as HTMLDetailsElement).open">
      <summary class="cursor-pointer text-sm font-700">筛选日志<template v-if="filters.jobId || filters.followId || filters.followRunId || filters.scanRunId || filters.taskId"> · 已限定关联执行</template><template v-if="filters.attempt"> · 第 {{ filters.attempt }} 次尝试</template></summary>
      <form class="runtime-filter mt-4" @submit.prevent="apply">
        <label><span class="label">开始时间</span><input class="input" type="datetime-local" :value="localInput(filters.from)" @change="updateDate('from',($event.target as HTMLInputElement).value)"></label>
        <label><span class="label">结束时间</span><input class="input" type="datetime-local" :value="localInput(filters.to)" @change="updateDate('to',($event.target as HTMLInputElement).value)"></label>
        <label><span class="label">级别</span><select v-model="filters.level" class="input"><option value="">全部</option><option v-for="item in ['debug','info','warn','error']" :key="item" :value="item">{{ logLevelLabel(item) }}</option></select></label>
        <label><span class="label">模块</span><select v-model="filters.module" class="input"><option value="">全部</option><option v-for="item in facets.modules" :key="item">{{ item }}</option></select></label>
        <label><span class="label">组件</span><select v-model="filters.component" class="input"><option value="">全部</option><option v-for="item in facets.components" :key="item">{{ item }}</option></select></label>
        <label><span class="label">业务操作</span><select v-model="filters.operation" class="input"><option value="">全部</option><option v-for="item in facets.operations" :key="item" :value="item">{{ logOperationLabel(item) }}</option></select></label>
        <label><span class="label">插件</span><select v-model="filters.pluginId" class="input"><option value="">全部</option><option v-for="item in facets.plugin_ids" :key="item" :value="item">{{ item }}</option></select></label>
        <label class="runtime-filter__keyword"><span class="label">关键字</span><input v-model="filters.keyword" class="input" maxlength="128" placeholder="仅搜索脱敏后的消息"></label>
        <div class="runtime-filter__actions"><button class="btn-primary" :disabled="loading">查询</button><button type="button" class="btn-secondary" @click="reset">重置</button><button type="button" class="btn-secondary" @click="advanced=!advanced">{{ advanced?'收起':'高级筛选' }}</button></div>
        <div v-if="advanced" class="runtime-filter__advanced"><label v-for="field in [{k:'jobId',n:'队列任务 ID'},{k:'followId',n:'订阅 ID'},{k:'followRunId',n:'订阅运行 ID'},{k:'attempt',n:'执行尝试序号'},{k:'requestId',n:'请求 ID'},{k:'taskId',n:'业务任务 ID'},{k:'libraryId',n:'媒体库 ID'},{k:'connectionId',n:'连接 ID'},{k:'storageId',n:'存储 ID'},{k:'downloaderId',n:'下载器 ID'},{k:'scanRunId',n:'扫描运行 ID'}]" :key="field.k"><span class="label">{{ field.n }}</span><input v-model="filters[field.k as keyof RuntimeLogFilters]" class="input" maxlength="128"></label></div>
      </form>
    </details>
    <div v-if="partial||malformed" class="semantic-warning mt-4 p-3">查询受到扫描上限或文件读取影响，结果可能不完整；跳过 {{ malformed }} 条损坏记录。</div>
    <p v-if="snapshot" class="text-subtle mt-4 text-xs">快照截至 {{ new Date(snapshot).toLocaleString('zh-CN') }} · {{ paused ? '已暂停自动刷新' : '每 10 秒刷新' }}<template v-if="historical"> · 正在查看历史记录，新增日志不会打乱当前页面</template></p>
    <div v-if="loading&&entries.length===0" class="text-subtle mt-8">正在读取运行日志…</div>
    <div v-else-if="entries.length===0 && !error" class="panel text-subtle mt-6">当前筛选范围内没有可用日志，可能未记录或已超出保留范围；这不代表业务没有执行。</div>
    <div v-else class="runtime-list mt-5">
      <article v-for="(entry,index) in entries" :key="entryKey(entry,index)" class="panel runtime-entry"><div class="runtime-entry__header"><time>{{ new Date(entry.timestamp).toLocaleString('zh-CN') }}</time><span :class="levelClass(entry.level)">{{ logLevelLabel(entry.level) }}</span><strong>{{ entry.operation_label || logOperationLabel(entry.operation || '') }}</strong><span v-if="stage(entry)" class="status-chip">{{ stage(entry) }}</span><span v-if="outcome(entry)" class="status-chip">{{ outcome(entry) }}</span><span v-if="entry.plugin_id" class="status-chip">{{ entry.plugin_id }}</span></div><p class="runtime-entry__message">{{ entry.message }}</p><dl v-if="logFactRows(entry.fields).length"><template v-for="row in logFactRows(entry.fields)" :key="row.key"><dt>{{ row.label }}</dt><dd>{{ row.text }}</dd></template></dl><details class="runtime-technical"><summary>技术详情</summary><dl><dt>模块 / 组件</dt><dd>{{ entry.module }} / {{ entry.component }}</dd><template v-for="row in logFactRows(entry.fields,true)" :key="row.key"><dt>{{ row.label }}</dt><dd>{{ row.text }}</dd></template></dl></details></article>
    </div>
    <button v-if="cursor" class="btn-secondary mt-4 w-full" :disabled="loading" @click="load(true)">加载更多</button>
    <form v-if="settings&&canConfigure" class="panel mt-8" @submit.prevent="saveSettings"><h2 class="m-0 text-lg">轮转与保留设置</h2><p class="text-subtle mt-2 text-sm">目录由部署环境控制，不在页面暴露；修改会写入审计日志。</p><div class="runtime-settings mt-5"><label><span class="label">最低级别</span><select v-model="settings.level" class="input"><option v-for="item in ['debug','info','warn','error']" :key="item">{{ item }}</option></select></label><label><span class="label">单文件 MiB</span><input v-model.number="settings.max_file_mib" class="input" type="number" min="1" max="256"></label><label><span class="label">压缩分片数</span><input v-model.number="settings.max_backups" class="input" type="number" min="1" max="100"></label><label><span class="label">保留天数</span><input v-model.number="settings.retention_days" class="input" type="number" min="1" max="365"></label><label><span class="label">总上限 MiB</span><input v-model.number="settings.max_total_mib" class="input" type="number" min="32" max="10240"></label></div><button class="btn-primary mt-5" :disabled="saving">{{ saving?'正在保存…':'保存日志策略' }}</button></form>
  </section>
</template>

<style scoped>
.runtime-filter{display:grid;grid-template-columns:repeat(6,minmax(8rem,1fr));gap:1rem}.runtime-filter__keyword{grid-column:span 4}.runtime-filter__actions{display:flex;grid-column:span 2;align-items:end;gap:.5rem}.runtime-filter__advanced{display:grid;grid-column:1/-1;grid-template-columns:repeat(auto-fit,minmax(12rem,1fr));gap:1rem;border-top:1px solid var(--border);padding-top:1rem}.runtime-list{display:grid;gap:.65rem}.runtime-entry{padding:1rem;min-width:0}.runtime-entry__header{display:flex;align-items:center;flex-wrap:wrap;gap:.5rem .8rem;font-size:.8rem}.runtime-entry time{color:var(--text-subtle);font-size:.75rem}.runtime-entry__message{white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.7;margin:.55rem 0;font-size:.9rem}.runtime-entry dl{display:grid;grid-template-columns:minmax(8rem,12rem) minmax(0,1fr);gap:.4rem 1rem;margin:.75rem 0 0;padding-top:.5rem}.runtime-entry dt{color:var(--text-subtle);font-size:.75rem}.runtime-entry dd{margin:0;min-width:0;color:var(--text-muted);white-space:pre-wrap;overflow-wrap:anywhere;font-size:.78rem}.runtime-technical{border-top:1px solid var(--border);margin-top:.8rem;padding-top:.6rem}.runtime-technical summary{cursor:pointer;color:var(--text-subtle);font-size:.75rem}.runtime-level{font-size:.72rem;font-weight:800}.runtime-level--error{color:var(--danger)}.runtime-level--warn{color:var(--warning)}.runtime-level--info{color:var(--accent)}.runtime-level--debug{color:var(--text-subtle)}.runtime-settings{display:grid;grid-template-columns:repeat(auto-fit,minmax(10rem,1fr));gap:1rem}@media(max-width:1100px){.runtime-filter{grid-template-columns:repeat(3,1fr)}.runtime-filter__keyword{grid-column:span 2}.runtime-filter__actions{grid-column:span 1}}@media(max-width:650px){.runtime-filter{grid-template-columns:1fr}.runtime-filter__keyword,.runtime-filter__actions{grid-column:1}.runtime-filter__actions{flex-wrap:wrap}.runtime-entry dl{grid-template-columns:1fr;gap:.2rem}.runtime-entry dd{margin-bottom:.4rem}}
</style>
