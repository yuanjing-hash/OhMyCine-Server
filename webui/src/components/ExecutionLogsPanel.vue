<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { createLatestRequest } from '@/latest-request'
import { useAuthStore } from '@/stores/auth'
import {
  deduplicateLogEntries, executionLogError, executionScopeKey, executionStatusLabel, executionTriggerLabel, filterReasonRows,
  getExecutionLogs, logFactRows, logLevelLabel, logOperationLabel, logOutcomeLabel, logStageLabel,
  type ExecutionLogEntry, type ExecutionLogPage, type ExecutionLogScope, type ExecutionSummary,
} from '@/execution-logs'

const props = defineProps<{ scope: ExecutionLogScope; active?: boolean; attemptCount?: number; fallbackSummary?: ExecutionSummary }>()
const auth = useAuthStore()
const request = createLatestRequest()
const result = ref<ExecutionLogPage | null>(null), entries = ref<ExecutionLogEntry[]>([])
const loading = ref(false), error = ref(''), paused = ref(false), level = ref(''), keyword = ref(''), attempt = ref('')
const snapshot = ref(''), pageIndex = ref(0), cursors = ref([''])
let appliedFilters: { attempt?: number; level: string; keyword: string } = { level: '', keyword: '' }
const denied = ref(false)
let alive = true, timer: number | undefined
// Once the server answers, an omitted summary is deliberate (for example a
// historical snapshot). A live parent projection must not fill that omission.
const summary = computed(() => !auth.user || denied.value ? undefined : result.value ? result.value.summary : attempt.value || snapshot.value || pageIndex.value > 0 ? undefined : props.fallbackSummary)
const filterRows = computed(() => filterReasonRows(summary.value?.filter_summary))
const duration = computed(() => {
  if (!summary.value?.started_at || !summary.value?.finished_at) return ''
  const seconds = (new Date(summary.value.finished_at).getTime() - new Date(summary.value.started_at).getTime()) / 1000
  return Number.isFinite(seconds) && seconds >= 0 ? (seconds < 60 ? `${Number(seconds.toFixed(1))} 秒` : `${Math.floor(seconds / 60)} 分 ${Math.round(seconds % 60)} 秒`) : ''
})
const attemptChoices = computed(() => {
  const count = Math.min(10000, Math.max(0, Math.floor(props.attemptCount ?? 0)))
  return Array.from({ length: Math.min(count, 200) }, (_, index) => count - index)
})
const canSeeJobs = computed(() => auth.canAny([Permissions.JobsReadOwn, Permissions.JobsReadAll]))
const globalLogLink = computed(() => {
  if (!auth.can(Permissions.LogsRead) || denied.value || result.value?.availability === 'restricted') return null
  const query: Record<string, string> = props.scope.kind === 'job' ? { job_id: props.scope.jobId } : { follow_id: props.scope.followId, follow_run_id: props.scope.runId }
  if (attempt.value || result.value?.attempt_number) query.attempt = attempt.value || String(result.value!.attempt_number)
  if (summary.value?.started_at) query.from = summary.value.started_at
  if (snapshot.value) query.to = snapshot.value
  return { path: '/logs/runtime', query }
})
function formatTime(value?: string) { if (!value) return '未知'; const date = new Date(value); return Number.isNaN(date.getTime()) ? '未知' : date.toLocaleString('zh-CN') }
function hasCount(value: unknown): value is number { return typeof value === 'number' && Number.isFinite(value) }
function relatedJob(entry: ExecutionLogEntry) { const id = entry.fields.related_job_id; return canSeeJobs.value && typeof id === 'string' && /^[a-zA-Z0-9_-]{1,128}$/.test(id) ? { path: '/automation/tasks', query: { job_id: id } } : null }
function resetData() { request.cancel(); result.value = null; entries.value = []; error.value = ''; loading.value = false; snapshot.value = ''; pageIndex.value = 0; cursors.value = [''] }
async function load(fresh = false) {
  if (!alive || !auth.user) return
  if (fresh) { snapshot.value = ''; pageIndex.value = 0; cursors.value = ['']; appliedFilters = { attempt: Number(attempt.value) || undefined, level: level.value, keyword: keyword.value } }
  const current = request.begin()
  loading.value = true; error.value = ''
  try {
    const data = await getExecutionLogs(props.scope, { ...appliedFilters, limit: 100, cursor: cursors.value[pageIndex.value], snapshot_to: snapshot.value }, current.signal)
    if (!current.isCurrent()) return
    denied.value = data.availability === 'restricted'
    result.value = data; entries.value = deduplicateLogEntries(data.list ?? []); snapshot.value = data.snapshot_to
  } catch (reason) {
    if (!current.isCurrent()) return
    error.value = executionLogError(reason)
    if (reason instanceof APIError && [401, 403, 404].includes(reason.status)) { denied.value = true; result.value = null; entries.value = [] }
  } finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
function refresh() { denied.value = false; void load(true) }
function filter() { void load(true) }
function nextPage() {
  if (!result.value?.next_cursor || loading.value) return
  paused.value = true
  cursors.value = [...cursors.value.slice(0, pageIndex.value + 1), result.value.next_cursor]
  pageIndex.value++
  void load()
}
function previousPage() { if (pageIndex.value === 0 || loading.value) return; paused.value = true; pageIndex.value--; void load() }
function toggleLive() { paused.value = !paused.value; if (!paused.value) refresh() }
function refreshVisible() { if (props.active && !paused.value && pageIndex.value === 0 && !request.pending && !denied.value && !document.hidden) void load(true) }
function invalidateAuth() { denied.value = true; resetData() }
watch(() => executionScopeKey(props.scope), () => { denied.value = false; resetData(); paused.value = false; attempt.value = ''; level.value = ''; keyword.value = ''; void load(true) }, { immediate: true })
watch(attempt, () => { denied.value = false; resetData(); void load(true) })
watch(() => `${auth.user?.id ?? ''}:${auth.user?.permissions?.join(',') ?? ''}`, () => { denied.value = false; resetData(); if (auth.user) void load(true) })
watch(() => props.active, value => { if (!value && !paused.value && pageIndex.value === 0 && !request.pending && !denied.value) void load(true) })
onMounted(() => { timer = window.setInterval(refreshVisible, 5000); document.addEventListener('visibilitychange', refreshVisible); window.addEventListener('omc:unauthorized', invalidateAuth) })
onBeforeUnmount(() => { alive = false; request.cancel(); window.clearInterval(timer); document.removeEventListener('visibilitychange', refreshVisible); window.removeEventListener('omc:unauthorized', invalidateAuth) })
</script>

<template>
  <section class="execution-logs" aria-label="本轮执行过程">
    <section v-if="summary" class="semantic-inset execution-summary" aria-label="本轮结果">
      <h3>本轮结果</h3>
      <p class="execution-summary__outcome" :class="summary.status === 'failed' || summary.error_code ? 'semantic-danger-text' : ''">{{ summary.error_code === 'follow_submit_partial' ? '部分下载已提交，其余提交失败' : executionStatusLabel(summary.status) }}</p>
      <p v-if="summary.title" class="text-muted">{{ summary.title }}</p>
      <p v-if="summary.error_message" class="semantic-danger-text">{{ summary.error_message }}</p>
      <p v-if="summary.status === 'no_match'" class="text-muted text-sm">可能是未找到资源、筛选未通过或站点查询失败，请查看下方过程中的具体原因。</p>
      <dl class="execution-counts">
        <template v-if="hasCount(summary.searched_names_count)"><dt>查询名称数</dt><dd>{{ summary.searched_names_count }}</dd></template>
        <template v-if="hasCount(summary.candidates)"><dt>筛选后候选</dt><dd>{{ summary.candidates }}</dd></template>
        <template v-if="hasCount(summary.accepted_submissions)"><dt>已接受提交</dt><dd>{{ summary.accepted_submissions }}</dd></template>
        <template v-if="summary.trigger"><dt>触发方式</dt><dd>{{ executionTriggerLabel(summary.trigger) }}</dd></template>
        <template v-if="summary.started_at"><dt>开始时间</dt><dd>{{ formatTime(summary.started_at) }}</dd></template>
        <template v-if="summary.finished_at"><dt>结束时间</dt><dd>{{ formatTime(summary.finished_at) }}</dd></template>
        <template v-if="duration"><dt>本轮耗时</dt><dd>{{ duration }}</dd></template>
      </dl>
      <p v-if="hasCount(summary.candidates) || hasCount(summary.accepted_submissions)" class="text-subtle text-xs">候选数量已通过筛选；已接受提交可能包含复用的任务，不等于新建下载或已入库数量。</p>
      <ul v-if="filterRows.length" class="execution-reasons" aria-label="筛选与跳过原因"><li v-for="row in filterRows" :key="row.code">{{ row.label }} <strong>{{ row.count }}</strong></li></ul>
    </section>
    <div class="execution-heading"><h3>执行日志</h3><div class="execution-actions"><button v-if="active || paused" class="btn-secondary" :disabled="loading" @click="toggleLive">{{ paused ? '返回实时' : '暂停刷新' }}</button><button class="btn-secondary" :disabled="loading" @click="refresh">刷新本轮过程</button></div></div>
    <p class="text-subtle text-xs">按发生顺序展示本次执行的业务记录。搜索结束、下载受理和媒体入库是不同阶段。</p>
    <form class="execution-filters" @submit.prevent="filter">
      <label v-if="attemptChoices.length"><span class="label">执行尝试</span><select v-model="attempt" class="input"><option value="">最新尝试</option><option v-for="number in attemptChoices" :key="number" :value="String(number)">第 {{ number }} 次</option></select></label>
      <label><span class="label">级别</span><select v-model="level" class="input"><option value="">全部级别</option><option v-for="value in ['info', 'warn', 'error', 'debug']" :key="value" :value="value">{{ logLevelLabel(value) }}</option></select></label>
      <label class="execution-filters__keyword"><span class="label">过程关键字</span><input v-model="keyword" class="input" maxlength="128" placeholder="作品、文件名或站点"></label>
      <button class="btn-secondary" :disabled="loading">筛选</button>
    </form>
    <p v-if="result?.attempt_number" class="text-subtle text-xs">当前第 {{ result.attempt_number }} 次尝试<template v-if="snapshot"> · 快照截至 {{ formatTime(snapshot) }}</template><template v-if="paused"> · 已暂停自动刷新</template></p>
    <p v-if="error" class="semantic-error p-3" role="alert">{{ error }} <button class="btn-secondary" :disabled="loading" @click="refresh">重新读取</button></p>
    <p v-if="result?.partial" class="semantic-warning p-3" role="status">当前只能读取部分记录，不能将这些记录当作完整的操作清单。</p>
    <p v-if="result?.availability === 'degraded'" class="semantic-warning p-3" role="status">日志写入或读取处于降级状态，记录可能不完整；业务结果请同时参考本轮结果与关联任务。</p>
    <p v-if="result?.availability === 'restricted'" class="semantic-warning p-3" role="status">当前权限不足以查看这次执行的部分资源明细。</p>
    <p v-if="loading && !entries.length" class="text-muted" role="status">正在读取本轮执行日志…</p>
    <p v-else-if="!error && !entries.length && result?.availability !== 'restricted'" class="semantic-inset p-3 text-muted" role="status">{{ result?.availability === 'unavailable' ? '没有可用执行日志，可能未记录或已超出保留范围。缺少日志不代表本轮未执行。' : '当前筛选下没有可见执行日志，不能据此判断本轮没有执行操作。' }}</p>
    <ol v-if="entries.length" class="execution-events" aria-label="执行日志记录">
      <li v-for="entry in entries" :key="entry.event_id">
        <div class="execution-event__meta"><time>{{ formatTime(entry.timestamp) }}</time><span :class="['execution-level', `execution-level--${['debug','info','warn','error'].includes(entry.level) ? entry.level : 'info'}`]">{{ logLevelLabel(entry.level) }}</span><span>{{ entry.operation_label || logOperationLabel(entry.operation || '') }}</span><span v-if="entry.stage">{{ logStageLabel(entry.stage) }}</span><span v-if="entry.outcome" class="status-chip">{{ logOutcomeLabel(entry.outcome) }}</span></div>
        <p class="execution-event__message">{{ entry.message }}</p>
        <dl v-if="logFactRows(entry.fields).length" class="execution-facts"><template v-for="row in logFactRows(entry.fields)" :key="row.key"><dt>{{ row.label }}</dt><dd>{{ row.text }}</dd></template></dl>
        <RouterLink v-if="relatedJob(entry)" class="semantic-link text-sm" :to="relatedJob(entry)!">查看关联任务</RouterLink>
        <details v-if="logFactRows(entry.fields, true).length" class="execution-technical"><summary>技术详情</summary><dl class="execution-facts"><template v-for="row in logFactRows(entry.fields, true)" :key="row.key"><dt>{{ row.label }}</dt><dd>{{ row.text }}</dd></template></dl></details>
      </li>
    </ol>
    <footer class="execution-footer"><span class="text-muted text-xs">第 {{ pageIndex + 1 }} 页 · 本页 {{ entries.length }} 条</span><div class="execution-actions"><button class="btn-secondary" :disabled="loading || pageIndex === 0" @click="previousPage">上一页过程</button><button class="btn-secondary" :disabled="loading || !result?.next_cursor" @click="nextPage">下一页过程</button></div></footer>
    <RouterLink v-if="globalLogLink" class="semantic-link text-sm" :to="globalLogLink">在日志中心查看同次执行</RouterLink>
  </section>
</template>

<style scoped>
.execution-logs{min-width:0}.execution-summary{padding:1rem}.execution-summary h3,.execution-heading h3{margin:0}.execution-summary p{margin:.5rem 0 0}.execution-summary__outcome{font-weight:700}.execution-counts{display:grid;grid-template-columns:minmax(6rem,auto) 1fr;gap:.4rem .8rem;font-size:.8rem;margin:.8rem 0}.execution-counts dt,.execution-facts dt{color:var(--text-subtle)}.execution-counts dd,.execution-facts dd{margin:0;min-width:0;white-space:pre-wrap;overflow-wrap:anywhere}.execution-reasons{list-style:none;display:flex;flex-wrap:wrap;gap:.35rem .8rem;margin:.8rem 0 0;padding:0;font-size:.75rem;color:var(--text-muted)}.execution-heading,.execution-footer{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:.65rem;margin:1rem 0 .5rem}.execution-actions{display:flex;flex-wrap:wrap;gap:.4rem}.execution-filters{display:flex;align-items:end;flex-wrap:wrap;gap:.6rem;margin:.8rem 0}.execution-filters label{flex:1;min-width:7rem}.execution-filters__keyword{flex:2!important}.execution-events{list-style:none;padding:0;display:grid;gap:.8rem}.execution-events>li{padding:.9rem;border:1px solid var(--border);border-left:3px solid var(--border-strong);border-radius:var(--radius-sm);background:var(--surface)}.execution-event__meta{display:flex;align-items:center;flex-wrap:wrap;gap:.35rem .7rem;color:var(--text-subtle);font-size:.72rem}.execution-event__message{white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.65;font-size:.88rem;margin:.5rem 0}.execution-level{font-weight:700}.execution-level--error{color:var(--danger)}.execution-level--warn{color:var(--warning)}.execution-level--info{color:var(--accent)}.execution-facts{display:grid;grid-template-columns:minmax(6rem,9rem) minmax(0,1fr);gap:.3rem .7rem;font-size:.76rem}.execution-technical{border-top:1px solid var(--border);margin-top:.7rem;padding-top:.6rem;font-size:.75rem;color:var(--text-muted)}.execution-technical summary{cursor:pointer}@media(max-width:480px){.execution-filters{display:grid;grid-template-columns:1fr 1fr}.execution-filters__keyword{grid-column:1/-1}.execution-filters>button{grid-column:1/-1}.execution-facts{grid-template-columns:1fr}.execution-facts dd{margin-bottom:.35rem}.execution-heading .execution-actions{width:100%}.execution-actions>button{flex:1}.execution-footer .execution-actions{width:100%}}
</style>
