<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { api, APIError } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
import { useJobLiveRefresh } from '@/use-job-live-refresh'
import { useAuthStore } from '@/stores/auth'
import { Permissions } from '@/auth/generated-permissions'
import { formatBytes } from '@/downloads'
import TaskList from '@/dashboard/TaskList.vue'
import { capacityStatusLabel, getDashboardActivity, refreshStorageCapacity, type DashboardActivity, type DashboardFact, type DashboardSection, type DashboardSummary, type StorageCapacity } from '@/dashboard/activity'
import { calendarEpisodeLabel, calendarEventState, calendarTimezone, calendarToday, getSubscriptionCalendar, shiftCalendarDate, type CalendarPage } from '@/subscription-calendar'

const auth = useAuthStore()
const activity = ref<DashboardActivity | null>(null), summary = ref<DashboardSummary | null>(null)
const facts = ref<Record<string, DashboardSection<DashboardFact>>>({}), calendar = ref<CalendarPage | null>(null)
const activityError = ref(''), factsError = ref(''), summaryError = ref(''), calendarError = ref(''), capacityError = ref('')
const reads = createLatestRequest(), factReads = createLatestRequest(), summaryReads = createLatestRequest(), calendarReads = createLatestRequest()
const capacityReads = new Map<number, ReturnType<typeof createLatestRequest>>()
const measuring = ref(new Set<number>()), loading = ref(false)
const canJobs = computed(() => auth.canAny([Permissions.JobsReadOwn, Permissions.JobsReadAll]))
const canFollows = computed(() => auth.canAny([Permissions.FollowsReadOwn, Permissions.FollowsReadAll]))
const timezone = calendarTimezone()
const canRefreshCapacity = computed(() => auth.can(Permissions.StoragesRead) && auth.can(Permissions.StoragesTest))
const mediaFacts = computed(() => auth.can(Permissions.MediaLibrariesRead) && facts.value['media-summary']?.status === 'ok' ? facts.value['media-summary']!.list : [])
const downloadFacts = computed(() => auth.canAny([Permissions.DownloadsReadOwn, Permissions.DownloadsReadAll]) && facts.value['download-summary']?.status === 'ok' ? facts.value['download-summary']!.list : [])
const dateLabel = computed(() => new Date().toLocaleDateString([], { month: 'long', day: 'numeric', weekday: 'long' }))
const quickLinks = computed(() => [
  { label: '搜索影视', path: '/discovery/explore', allowed: auth.can(Permissions.DiscoveryRead) },
  { label: '媒体库', path: '/discovery/library', allowed: auth.can(Permissions.MediaLibrariesRead) },
  { label: '订阅管理', path: '/subscriptions/manage', allowed: canFollows.value },
  { label: '下载管理', path: '/automation/downloads', allowed: auth.canAny([Permissions.DownloadsReadOwn, Permissions.DownloadsReadAll]) },
].filter(link => link.allowed))
function formatTime(value?: string | null) { return value ? new Date(value).toLocaleString() : '尚未更新' }
function clearProtected() {
  reads.cancel(); factReads.cancel(); summaryReads.cancel(); calendarReads.cancel()
  for (const owner of capacityReads.values()) owner.cancel()
  capacityReads.clear(); measuring.value = new Set()
  loading.value = false
  activity.value = null; facts.value = {}; summary.value = null; calendar.value = null
  activityError.value = ''; factsError.value = ''; summaryError.value = ''; calendarError.value = ''; capacityError.value = ''
}
function message(reason: unknown, fallback: string) { return reason instanceof Error ? reason.message : fallback }
function rejected(reason: unknown) { return reason instanceof APIError && (reason.status === 401 || reason.status === 403) }
async function loadActivity() {
  const request = reads.begin(); loading.value = true
  try { const data = await getDashboardActivity(request.signal); if (request.isCurrent()) { activity.value = data; activityError.value = '' } }
  catch (reason) { if (request.isCurrent()) { if (rejected(reason)) activity.value = null; activityError.value = message(reason, '任务和媒体暂时无法读取') } }
  finally { if (request.isCurrent()) loading.value = false; request.finish() }
}
async function loadFacts() {
  const request = factReads.begin()
  try { const data = await api<{ sections: Record<string, DashboardSection<DashboardFact>> }>('/api/v1/dashboard/operations', { signal: request.signal }); if (request.isCurrent()) { facts.value = data.sections; factsError.value = '' } }
  catch (reason) { if (request.isCurrent()) { if (rejected(reason)) facts.value = {}; factsError.value = message(reason, '统计暂时无法读取') } }
  finally { request.finish() }
}
async function loadSummary() {
  const request = summaryReads.begin()
  try { const data = await api<DashboardSummary>('/api/v1/dashboard', { signal: request.signal }); if (request.isCurrent()) { summary.value = data; summaryError.value = '' } }
  catch (reason) { if (request.isCurrent()) { if (rejected(reason)) summary.value = null; summaryError.value = message(reason, '系统状态暂时无法读取') } }
  finally { request.finish() }
}
async function loadCalendar() {
  if (!canFollows.value) return
  const request = calendarReads.begin()
  const start = calendarToday(timezone)
  try { const data = await getSubscriptionCalendar({ start, end: shiftCalendarDate(start, 7), timezone, page_size: 8 }, request.signal); if (request.isCurrent()) { calendar.value = data; calendarError.value = '' } }
  catch (reason) { if (request.isCurrent()) { if (rejected(reason)) calendar.value = null; calendarError.value = message(reason, '订阅日程暂时无法读取') } }
  finally { request.finish() }
}
async function refresh() { if (auth.can(Permissions.DashboardRead)) await Promise.allSettled([loadActivity(), loadFacts(), loadSummary(), loadCalendar()]) }
async function measure(sample: StorageCapacity) {
  const owner = capacityReads.get(sample.id) ?? createLatestRequest(); capacityReads.set(sample.id, owner)
  const request = owner.begin(); measuring.value = new Set([...measuring.value, sample.id]); capacityError.value = ''
  try {
    const data = await refreshStorageCapacity(sample.id, request.signal)
    if (request.isCurrent() && activity.value) {
      // A read started before the measurement may finish afterwards. Revoke
      // its publication right before installing the confirmed sample.
      reads.cancel(); loading.value = false
      activity.value.capacities.list = activity.value.capacities.list.map(item => item.id === data.id ? data : item)
    }
  } catch (reason) { if (request.isCurrent()) capacityError.value = message(reason, '容量检测暂时失败') }
  finally { if (request.isCurrent()) { const next = new Set(measuring.value); next.delete(sample.id); measuring.value = next }; request.finish() }
}
function factValue(fact: DashboardFact) { return fact.value == null ? '暂无采样' : fact.unit?.startsWith('bytes') ? formatBytes(fact.value, fact.unit === 'bytes/s' ? '/s' : '') : String(fact.value) }
watch(() => auth.user, () => { clearProtected(); void refresh() }, { immediate: true, deep: true })
useJobLiveRefresh(async () => { await Promise.allSettled([loadActivity(), loadFacts(), loadCalendar()]) }, () => auth.can(Permissions.DashboardRead), () => canJobs.value)
onUnmounted(clearProtected)
</script>

<template>
  <section class="dashboard">
    <header class="dashboard-heading"><div><p class="date-label">{{ dateLabel }}</p><h1>欢迎回来，{{ auth.user?.display_name }}</h1><p class="page-description">查看媒体、任务和近期订阅更新。</p></div><div class="header-actions"><span class="text-muted text-xs">更新于 {{ formatTime(activity?.generated_at) }}</span><button class="btn-secondary" :disabled="loading" @click="refresh">{{ loading ? '刷新中…' : '刷新' }}</button></div></header>
    <p v-if="activityError" class="semantic-warning p-3" role="alert">{{ activity ? '部分数据未能更新，正在显示上次结果。' : '' }}{{ activityError }} <button class="btn-secondary" @click="loadActivity">重试</button></p>
    <p v-if="factsError" class="semantic-warning p-3" role="alert">{{ factsError }} <button class="btn-secondary" @click="loadFacts">重试统计</button></p>
    <div v-if="mediaFacts.length || downloadFacts.length" class="fact-grid">
      <RouterLink v-for="fact in [...mediaFacts, ...downloadFacts]" :key="fact.label" :to="fact.link || '/discovery/library'" class="panel fact-tile"><span>{{ fact.label }}</span><strong>{{ factValue(fact) }}</strong></RouterLink>
    </div>
    <div class="main-grid">
      <article v-if="canJobs" class="panel task-panel"><header class="section-heading"><h2>正在执行</h2><RouterLink to="/automation/tasks">任务中心</RouterLink></header><TaskList :section="activity?.active_tasks" :error="activityError" empty="暂无正在执行的任务" /><details v-if="activity?.waiting_tasks.list.length"><summary>等待执行与重试</summary><TaskList :section="activity.waiting_tasks" empty="暂无等待任务" /></details></article>
      <article v-if="canFollows" class="panel calendar-panel"><header class="section-heading"><h2>未来七天订阅更新</h2><RouterLink to="/subscriptions/calendar">查看日历</RouterLink></header><p class="text-muted text-xs">按预计播出日期展示，入库状态独立更新。</p><p v-if="calendarError" class="semantic-warning p-3" role="alert">{{ calendarError }} <button class="btn-secondary" @click="loadCalendar">重试</button></p><ul v-if="calendar?.events.length" class="agenda-list"><li v-for="event in calendar.events" :key="event.id"><img v-if="event.poster_ref" :src="event.poster_ref" alt="" loading="lazy"><div><strong>{{ event.title }}</strong><span>{{ event.air_date }} · {{ calendarEpisodeLabel(event) }}</span><span>{{ calendarEventState(event) }}</span></div></li></ul><p v-else-if="!calendar && !calendarError" class="empty">正在读取订阅日程…</p><p v-else-if="calendar" class="empty">{{ !calendar.subscriptions.length ? '还没有可查看的订阅' : calendar.missing_seasons ? '播出计划正在准备，请稍后查看日历。' : '未来七天暂无已知播出计划' }}</p><p v-if="calendar?.partial || calendar?.stale_seasons" class="text-muted text-xs">部分排期待更新，当前日程可能不完整。</p><p v-if="calendar?.has_more || calendar?.undated.length" class="text-muted text-xs">更多日程及日期待定的剧集可在日历查看。</p></article>
      <article v-if="canJobs && (activity?.attention_tasks.list.length || activity?.attention_tasks.status === 'unavailable')" class="panel attention-panel"><header class="section-heading"><h2>需要关注</h2><RouterLink to="/automation/tasks?status=failed">查看任务</RouterLink></header><TaskList :section="activity.attention_tasks" empty="暂无需要处理的任务" /></article>
    </div>
    <article v-if="auth.can(Permissions.MediaLibrariesRead)" class="panel recent-panel"><header class="section-heading"><h2>最近入库</h2><RouterLink to="/discovery/library">全部媒体</RouterLink></header><p v-if="activity?.recently_added.status === 'unavailable'" class="empty">最近入库暂时无法读取。</p><div v-else-if="activity?.recently_added.list.length" class="poster-strip"><RouterLink v-for="item in activity.recently_added.list" :key="`${item.library_id}:${item.work_id}`" :to="{ name: 'library-catalog-detail', params: { libraryID: item.library_id, workID: item.work_id } }" class="poster-card"><div><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong>{{ item.title }}</strong><small>{{ item.kind === 'series' ? `${item.season_count || 0} 季 · ${item.episode_count || 0} 集` : '电影' }}</small><small>{{ formatTime(item.added_at) }}</small></RouterLink></div><p v-else class="empty">{{ activity ? '当前还没有已入库作品' : activityError ? '最近入库暂时无法读取' : '正在读取最近入库…' }}</p></article>
    <div class="secondary-grid">
      <article v-if="auth.can(Permissions.StoragesRead)" class="panel"><header class="section-heading"><h2>存储空间</h2><RouterLink to="/system/connections">数据源</RouterLink></header><p v-if="capacityError" class="semantic-warning p-3" role="alert">{{ capacityError }}</p><ul v-if="activity?.capacities.status === 'ok'" class="capacity-list"><li v-for="sample in activity.capacities.list" :key="sample.id"><div class="capacity-heading"><strong>{{ sample.name }}</strong><button v-if="canRefreshCapacity" class="btn-secondary" :disabled="measuring.has(sample.id) || !sample.enabled" @click="measure(sample)">{{ measuring.has(sample.id) ? '检测中…' : '更新容量' }}</button></div><p v-if="sample.free_bytes != null && sample.total_bytes != null">剩余 {{ formatBytes(sample.free_bytes) }} / 总计 {{ formatBytes(sample.total_bytes) }}</p><p class="text-muted text-xs">{{ capacityStatusLabel(sample) }}<template v-if="sample.sampled_at"> · 成功采样 {{ formatTime(sample.sampled_at) }}</template></p><progress v-if="sample.total_bytes && sample.free_bytes != null" :value="sample.total_bytes - sample.free_bytes" :max="sample.total_bytes" :aria-label="`${sample.name} 已用空间`" /></li><li v-if="!activity.capacities.list.length" class="empty">尚未配置数据源</li></ul><p v-else class="empty">{{ activity?.capacities.status === 'unavailable' ? '存储信息暂时无法读取' : '正在读取存储信息…' }}</p><p v-if="activity?.capacities.has_more" class="text-muted text-xs">更多数据源请到数据源页面查看。</p></article>
      <article v-if="auth.can(Permissions.ConnectionsRead) || auth.can(Permissions.SettingsRead)" class="panel"><h2>连接与计划</h2><template v-if="auth.can(Permissions.ConnectionsRead)"><dl v-if="facts['connection-health']?.status === 'ok'" class="fact-list"><div v-for="fact in facts['connection-health']!.list" :key="fact.label"><dt>{{ fact.label.replace('最近检测：unknown', '尚未检测').replace('最近检测：online', '最近检测正常').replace('最近检测：offline', '最近检测失败') }}</dt><dd>{{ fact.value }}</dd></div></dl><p v-else class="text-muted text-sm">连接状态暂时无法读取</p></template><dl v-if="auth.can(Permissions.SettingsRead) && facts['scheduler-jobs']?.status === 'ok'" class="fact-list"><div v-for="fact in facts['scheduler-jobs']!.list" :key="fact.label"><dt><RouterLink to="/automation/schedules">{{ fact.label }}</RouterLink></dt><dd>{{ fact.value }}</dd></div></dl><p v-if="summaryError" class="text-muted text-xs">{{ summaryError }}</p><p v-else-if="summary?.recovery_required" class="semantic-warning p-3">管理数据需要恢复，请前往设置检查。</p><dl v-if="summary" class="fact-list management-facts"><div v-if="summary.users != null"><dt>有效账号 / 全部账号</dt><dd>{{ summary.active_users }} / {{ summary.users }}</dd></div><div v-if="summary.roles != null"><dt>角色</dt><dd>{{ summary.roles }}</dd></div><div v-if="summary.audit_events != null"><dt>审计事件</dt><dd>{{ summary.audit_events }}</dd></div></dl></article>
      <article v-if="quickLinks.length" class="panel"><h2>快捷入口</h2><nav class="quick-links" aria-label="仪表盘快捷入口"><RouterLink v-for="link in quickLinks" :key="link.path" :to="link.path" class="btn-secondary">{{ link.label }}</RouterLink></nav></article>
    </div>
  </section>
</template>

<style scoped>
.dashboard { display: grid; gap: 1rem; }
.dashboard > * { min-width: 0; }
.dashboard-heading { display: flex; justify-content: space-between; gap: 1rem; align-items: center; }
.dashboard-heading h1 { margin: .2rem 0 .4rem; font-size: 1.65rem; }
.date-label { margin: 0; color: var(--text-muted); font-size: .8rem; }
.header-actions { display: flex; align-items: center; gap: .75rem; flex-wrap: wrap; }
.fact-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(145px, 1fr)); gap: .75rem; }
.fact-tile { display: grid; gap: .5rem; color: var(--text); text-decoration: none; padding: .9rem 1rem; }
.fact-tile span { font-size: .75rem; color: var(--text-muted); overflow-wrap: anywhere; }
.fact-tile strong { font-size: 1.35rem; }
.main-grid { display: grid; grid-template-columns: minmax(0, 7fr) minmax(0, 5fr); gap: 1rem; }
.main-grid > *, .secondary-grid > * { min-width: 0; }
.attention-panel { grid-column: 1 / -1; }
.section-heading { display: flex; align-items: center; justify-content: space-between; gap: .8rem; }
h2 { font-size: 1.02rem; margin: 0; }
.section-heading a { font-size: .78rem; flex-shrink: 0; }
.empty { color: var(--text-muted); font-size: .85rem; padding: 1.5rem .2rem; }
details { border-top: 1px solid var(--border); padding-top: .8rem; }
summary { cursor: pointer; font-size: .8rem; color: var(--text-muted); }
.agenda-list { padding: 0; margin: .8rem 0 0; list-style: none; }
.agenda-list li { display: flex; gap: .6rem; padding: .6rem 0; border-bottom: 1px solid var(--border); }
.agenda-list li:last-child { border: 0; }
.agenda-list img { width: 36px; height: 54px; object-fit: cover; border-radius: var(--radius-sm); }
.agenda-list div { display: grid; gap: .2rem; min-width: 0; }
.agenda-list strong { font-size: .83rem; overflow-wrap: anywhere; }
.agenda-list span { font-size: .72rem; color: var(--text-muted); }
.poster-strip { display: flex; gap: .9rem; overflow-x: auto; margin-top: 1rem; padding-bottom: .3rem; }
.poster-card { flex: 0 0 128px; min-width: 0; color: var(--text); text-decoration: none; display: grid; align-content: start; gap: .3rem; }
.poster-card > div { aspect-ratio: 2 / 3; background: var(--surface-muted); border-radius: var(--radius-md); overflow: hidden; display: grid; place-items: center; font-size: .8rem; color: var(--text-muted); }
.poster-card img { width: 100%; height: 100%; object-fit: cover; }
.poster-card strong { font-size: .83rem; overflow-wrap: anywhere; }
.poster-card small { font-size: .68rem; color: var(--text-muted); }
.secondary-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 1rem; }
.capacity-list { list-style: none; margin: .7rem 0 0; padding: 0; }
.capacity-list li { padding: .7rem 0; border-bottom: 1px solid var(--border); }
.capacity-list li:last-child { border: 0; }
.capacity-heading { display: flex; justify-content: space-between; align-items: center; gap: .5rem; }
.capacity-heading strong { font-size: .85rem; overflow-wrap: anywhere; }
.capacity-heading button { font-size: .72rem; padding: .35rem .55rem; flex-shrink: 0; }
.capacity-list p { margin: .45rem 0; font-size: .78rem; }
progress { width: 100%; height: .4rem; accent-color: var(--accent); }
.fact-list { margin: .8rem 0; font-size: .78rem; }
.fact-list > div { display: flex; justify-content: space-between; gap: .75rem; padding: .35rem 0; }
.fact-list dt { color: var(--text-muted); }
.fact-list dd { margin: 0; }
.management-facts { border-top: 1px solid var(--border); padding-top: .5rem; }
.quick-links { display: flex; gap: .6rem; flex-wrap: wrap; margin-top: 1rem; }
.quick-links a { font-size: .8rem; text-decoration: none; }
@media (max-width: 1000px) { .secondary-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 767px) { .dashboard-heading { flex-direction: column; align-items: flex-start; } .header-actions { width: 100%; justify-content: space-between; } .fact-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .main-grid, .secondary-grid { grid-template-columns: minmax(0, 1fr); } .poster-card { flex-basis: 110px; } }
</style>
