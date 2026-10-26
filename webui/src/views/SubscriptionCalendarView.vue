<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { APIError } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
import { useAuthStore } from '@/stores/auth'
import { Permissions } from '@/auth/generated-permissions'
import { useJobLiveRefresh } from '@/use-job-live-refresh'
import { followStatusLabel, type FollowStatus } from '@/follows'
import {
  calendarEpisodeLabel, calendarEventState, calendarMonthDays, calendarTimezone, calendarToday,
  getSubscriptionCalendar, refreshSubscriptionCalendar, shiftCalendarDate, shiftCalendarMonth,
  type CalendarEvent, type CalendarPage,
} from '@/subscription-calendar'

const auth = useAuthStore(), timezone = calendarTimezone(), today = calendarToday(timezone)
const month = ref(today.slice(0, 7)), selected = ref(today), status = ref(''), subscription = ref('')
const mode = ref<'month' | 'agenda'>(typeof matchMedia === 'function' && matchMedia('(max-width: 640px)').matches ? 'agenda' : 'month')
const data = ref<CalendarPage | null>(null), loading = ref(false), error = ref(''), refreshMessage = ref(''), refreshing = ref(false)
const requestOwner = createLatestRequest(), refreshOwner = createLatestRequest()
const days = computed(() => calendarMonthDays(month.value))
const byDay = computed(() => {
  const result = new Map<string, CalendarEvent[]>()
  for (const event of data.value?.events ?? []) if (event.air_date) result.set(event.air_date, [...(result.get(event.air_date) ?? []), event])
  return result
})
const selectedEvents = computed(() => byDay.value.get(selected.value) ?? [])
const agenda = computed(() => [...byDay.value.entries()].filter(([day]) => day.startsWith(month.value)))
const monthLabel = computed(() => `${month.value.slice(0, 4)}年${Number(month.value.slice(5))}月`)
const readAllowed = computed(() => !!auth.user && auth.canAny([Permissions.FollowsReadOwn, Permissions.FollowsReadAll]))
async function load(more = false, quiet = false) {
  if (!readAllowed.value) return
  const request = requestOwner.begin()
  if (!quiet) loading.value = true
  error.value = ''
  try {
    const result = await getSubscriptionCalendar({ start: days.value[0]!, end: shiftCalendarDate(days.value[41]!, 1), timezone, status: status.value,
      subscription_id: subscription.value, page_size: 200, cursor: more ? data.value?.next_cursor : undefined }, request.signal)
    if (!request.isCurrent()) return
    data.value = more && data.value ? { ...result, events: [...data.value.events, ...result.events] } : result
  } catch (reason) {
    if (!request.isCurrent()) return
    error.value = reason instanceof Error ? reason.message : '日历读取失败，请重试'
    if (reason instanceof APIError && [401, 403].includes(reason.status)) data.value = null
  } finally { if (request.isCurrent()) loading.value = false; request.finish() }
}
async function refreshSchedule() {
  if (!readAllowed.value) return
  const request = refreshOwner.begin(); refreshing.value = true; refreshMessage.value = ''
  try {
    const result = await refreshSubscriptionCalendar(subscription.value || undefined, request.signal)
    if (!request.isCurrent()) return
    refreshMessage.value = result.queued ? `已安排更新 ${result.queued} 个季的播出计划${result.partial ? '，部分排期尚未安排，请稍后重试或选择具体订阅' : ''}`
      : result.already_queued ? '播出计划正在后台更新，请稍后查看'
        : result.cooling_down ? '刚刚已安排过更新，请稍后查看'
          : result.partial ? '部分排期尚未安排，请稍后重试或选择具体订阅' : '当前没有可更新的订阅排期'
    await load(false, true)
  } catch (reason) { if (request.isCurrent()) refreshMessage.value = reason instanceof Error ? reason.message : '暂时无法更新播出计划' }
  finally { if (request.isCurrent()) refreshing.value = false; request.finish() }
}
function changeMonth(amount: number) { month.value = shiftCalendarMonth(month.value, amount); selected.value = `${month.value}-01` }
function goToday() { month.value = today.slice(0, 7); selected.value = today }
function selectDay(day: string) { selected.value = day }
function dayKey(event: KeyboardEvent, day: string) {
  const amounts: Record<string, number> = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7 }
  const amount = amounts[event.key]
  if (amount === undefined) return
  event.preventDefault(); selected.value = shiftCalendarDate(day, amount)
  if (!days.value.includes(selected.value)) month.value = selected.value.slice(0, 7)
  void nextTick(() => document.querySelector<HTMLButtonElement>(`[data-calendar-day="${selected.value}"]`)?.focus())
}
function statusLabel(value: string) { return followStatusLabel(value as FollowStatus) }
watch([month, status, subscription], () => {
  refreshOwner.cancel(); refreshing.value = false; refreshMessage.value = ''; data.value = null
  void load()
})
watch(() => auth.user, () => {
  requestOwner.cancel(); refreshOwner.cancel(); data.value = null; loading.value = false; error.value = ''; refreshMessage.value = ''; refreshing.value = false
  const filtersChanged = status.value !== '' || subscription.value !== ''
  status.value = ''; subscription.value = ''
  // The filter watcher owns the replacement request when account-local filters change.
  if (readAllowed.value && !filtersChanged) void load()
}, { immediate: true, deep: true, flush: 'sync' })
useJobLiveRefresh(async () => { if (!requestOwner.pending && readAllowed.value) await load(false, true) }, () => readAllowed.value,
  () => auth.canAny([Permissions.JobsReadOwn, Permissions.JobsReadAll]))
onBeforeUnmount(() => { requestOwner.cancel(); refreshOwner.cancel() })
</script>

<template>
  <div class="subscription-calendar">
    <header class="calendar-header">
      <div><p class="eyebrow">订阅更新</p><h1>播出日历</h1><p class="text-muted">按预计播出日期查看订阅，入库与处理状态以当前媒体库和任务为准。</p></div>
      <div class="calendar-actions"><button class="secondary-button" :disabled="loading || !readAllowed" @click="load()">刷新日历</button><button class="primary-button" :disabled="refreshing || !readAllowed" @click="refreshSchedule">{{ refreshing ? '正在安排更新…' : '更新播出计划' }}</button></div>
    </header>
    <p v-if="error" class="error-banner" role="alert">{{ error }} <button class="secondary-button" @click="load()">重试</button></p>
    <p v-if="refreshMessage" class="calendar-notice" role="status">{{ refreshMessage }}</p>
    <section class="panel calendar-toolbar">
      <div class="month-navigation"><button class="secondary-button" aria-label="上个月" @click="changeMonth(-1)">‹</button><h2>{{ monthLabel }}</h2><button class="secondary-button" aria-label="下个月" @click="changeMonth(1)">›</button><button class="secondary-button" @click="goToday">今天</button></div>
      <div class="calendar-filters"><label>订阅状态<select v-model="status"><option value="">全部状态</option><option value="active">追更中</option><option value="paused">已暂停</option><option value="completed">当前已补齐</option><option value="blocked">需要处理</option></select></label><label>订阅<select v-model="subscription"><option value="">全部可见订阅</option><option v-for="item in data?.subscriptions ?? []" :key="item.id" :value="item.id">{{ item.title }}</option></select></label></div>
      <div class="calendar-mode" role="group" aria-label="日历视图"><button class="secondary-button" :aria-pressed="mode === 'month'" @click="mode = 'month'">月历</button><button class="secondary-button" :aria-pressed="mode === 'agenda'" @click="mode = 'agenda'">日程</button></div>
    </section>
    <div class="calendar-meta text-muted"><span>{{ data?.scope === 'all' ? '全部获权订阅' : '我的订阅' }} · {{ timezone }}</span><span v-if="data?.generated_at">更新于 {{ new Date(data.generated_at).toLocaleTimeString() }}</span></div>
    <p v-if="loading && !data" class="panel calendar-empty" role="status">正在读取播出计划…</p>
    <div v-if="data" class="calendar-notices">
      <p v-if="data.missing_seasons" class="calendar-notice">{{ data.missing_seasons }} 个季的排期待更新。可点击“更新播出计划”，后台也会自动补齐。</p>
      <p v-if="data.stale_seasons" class="calendar-notice">{{ data.stale_seasons }} 个季正在显示之前保存的排期，播出日期可能调整。</p>
      <p v-if="data.reasons.includes('schedule_unavailable')" class="calendar-notice">部分播出计划暂时更新失败，之前保存的排期仍可查看。系统稍后会重试。</p>
      <p v-if="data.refreshing_seasons" class="calendar-notice" role="status">正在后台更新 {{ data.refreshing_seasons }} 个季的播出计划。</p>
      <p v-if="data.reasons.some(reason => reason.endsWith('_budget'))" class="calendar-notice">当前范围内容较多，部分信息尚未显示。请选择具体订阅查看。</p>
    </div>
    <section v-if="mode === 'month' && data" class="calendar-layout">
      <div class="panel month-panel">
        <div class="month-grid weekday-row" aria-hidden="true"><span v-for="day in ['一', '二', '三', '四', '五', '六', '日']" :key="day">{{ day }}</span></div>
        <div class="month-grid" role="group" aria-label="选择日期">
          <button
            v-for="day in days" :key="day" class="calendar-day" :class="{ outside: !day.startsWith(month), today: day === today, selected: day === selected }"
            :data-calendar-day="day" :tabindex="day === selected ? 0 : -1" :aria-pressed="day === selected" :aria-label="`${day}，${byDay.get(day)?.length ?? 0}集预计更新`" @click="selectDay(day)" @keydown="dayKey($event, day)"
          >
            <span class="date-number">{{ Number(day.slice(8)) }}<small v-if="day === today">今天</small></span>
            <span v-for="event in (byDay.get(day) ?? []).slice(0, 2)" :key="event.id" class="day-event"><strong>{{ event.title }}</strong><span>{{ calendarEpisodeLabel(event) }} · {{ calendarEventState(event) }}</span></span>
            <span v-if="(byDay.get(day)?.length ?? 0) > 2" class="day-overflow">另有 {{ (byDay.get(day)?.length ?? 0) - 2 }} 集</span>
            <span v-if="byDay.has(day)" class="mobile-day-count">{{ byDay.get(day)!.length }}集</span>
          </button>
        </div>
      </div>
      <aside class="panel selected-day"><h2>{{ selected }} <span class="text-muted">{{ selectedEvents.length }} 集</span></h2><p v-if="!selectedEvents.length" class="calendar-empty">这一天暂无已确认的播出计划。</p>
        <article v-for="event in selectedEvents" :key="event.id" class="calendar-event-card"><img v-if="event.poster_ref" :src="event.poster_ref" :alt="event.title" loading="lazy"><div><h3>{{ event.title }}</h3><p>{{ calendarEpisodeLabel(event) }}<span v-if="event.episode_title"> · {{ event.episode_title }}</span></p><p><span class="calendar-status" :class="{ present: event.library_status === 'present' }">{{ calendarEventState(event) }}</span> <span class="text-muted">{{ statusLabel(event.subscription_status) }}</span></p><RouterLink :to="{ path: '/subscriptions/manage', query: { follow_id: event.subscription_id } }">查看订阅</RouterLink></div></article>
      </aside>
    </section>
    <section v-if="mode === 'agenda' && data" class="panel agenda-panel">
      <p v-if="!agenda.length" class="calendar-empty">{{ data.missing_seasons ? '排期待更新，当前没有可显示的日程。' : '本月暂无已确认的更新计划。' }}</p>
      <section v-for="[day, events] in agenda" :key="day" class="agenda-day"><h2>{{ day }} <small v-if="day === today">今天</small></h2><article v-for="event in events" :key="event.id" class="calendar-event-card"><img v-if="event.poster_ref" :src="event.poster_ref" :alt="event.title" loading="lazy"><div><h3>{{ event.title }}</h3><p>{{ calendarEpisodeLabel(event) }}<span v-if="event.episode_title"> · {{ event.episode_title }}</span></p><p><span class="calendar-status" :class="{ present: event.library_status === 'present' }">{{ calendarEventState(event) }}</span> <span class="text-muted">{{ statusLabel(event.subscription_status) }}</span></p><RouterLink :to="{ path: '/subscriptions/manage', query: { follow_id: event.subscription_id } }">查看订阅</RouterLink></div></article></section>
    </section>
    <button v-if="data?.has_more" class="secondary-button load-more" :disabled="loading" @click="load(true)">{{ loading ? '正在加载…' : '继续加载此日期范围的日程' }}</button>
    <section v-if="data?.undated_count" class="panel undated-panel"><h2>播出日期待定 <span class="text-muted">{{ data.partial ? '已确认 ' : '' }}{{ data.undated_count }} 集</span></h2><p class="text-muted">以下已读取的剧集尚无明确播出日期，更新排期后会自动归入对应日期。</p><div class="undated-list"><p v-for="event in data.undated" :key="event.id"><strong>{{ event.title }}</strong> {{ calendarEpisodeLabel(event) }} · {{ calendarEventState(event) }}</p></div></section>
    <section v-if="data && !data.subscriptions.length" class="panel calendar-empty"><h2>还没有可显示的订阅</h2><p class="text-muted">添加电视剧订阅后，这里会展示所选季的预计播出计划。</p><RouterLink class="primary-button" to="/subscriptions/manage">前往订阅管理</RouterLink></section>
  </div>
</template>

<style scoped>
.subscription-calendar { min-width: 0; }
.calendar-header, .calendar-actions, .calendar-toolbar, .month-navigation, .calendar-filters, .calendar-mode, .calendar-meta { display: flex; align-items: center; gap: .75rem; }
.calendar-header { justify-content: space-between; gap: 1rem; flex-wrap: wrap; }
.calendar-header h1 { margin: .25rem 0; }
.eyebrow { color: var(--text-muted); font-size: .75rem; letter-spacing: .1em; margin: 0; }
.calendar-toolbar { margin: 1.4rem 0 .8rem; padding: 1rem; flex-wrap: wrap; justify-content: space-between; }
.month-navigation h2 { font-size: 1.15rem; margin: 0; min-width: 7rem; text-align: center; }
.calendar-filters label { display: grid; gap: .3rem; color: var(--text-muted); font-size: .8rem; min-width: 0; }
.calendar-filters select { max-width: 16rem; }
.calendar-meta { justify-content: space-between; flex-wrap: wrap; font-size: .8rem; margin: .6rem 0; }
.calendar-layout { display: grid; grid-template-columns: minmax(0, 1.8fr) minmax(16rem, 1fr); gap: 1rem; align-items: start; }
.month-panel { padding: .5rem; overflow: hidden; }
.month-grid { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); }
.weekday-row { text-align: center; color: var(--text-muted); padding: .6rem 0; font-size: .8rem; }
.calendar-day { min-width: 0; min-height: 7.7rem; padding: .5rem; border: 1px solid var(--border); background: var(--surface); color: var(--text); text-align: left; cursor: pointer; overflow: hidden; }
.calendar-day.outside { color: var(--text-muted); background: var(--canvas); }
.calendar-day.selected { outline: 2px solid var(--accent); outline-offset: -2px; }
.calendar-day:focus-visible { outline: 3px solid var(--accent); outline-offset: -3px; }
.calendar-day.today .date-number { color: var(--accent); font-weight: 700; }
.date-number { display: flex; justify-content: space-between; gap: .2rem; }
.date-number small { font-size: .6rem; }
.day-event { display: block; margin-top: .45rem; font-size: .68rem; line-height: 1.4; }
.day-event strong, .day-event span { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.day-event span { color: var(--text-muted); font-size: .6rem; }
.day-overflow { display: block; color: var(--accent); margin-top: .4rem; font-size: .65rem; }
.selected-day, .agenda-panel, .undated-panel { padding: 1.1rem; }
.selected-day h2, .undated-panel h2 { margin: 0 0 1rem; font-size: 1rem; }
.selected-day h2 span, .undated-panel h2 span { font-size: .8rem; }
.calendar-event-card { display: flex; gap: .75rem; padding: 1rem 0; border-bottom: 1px solid var(--border); overflow-wrap: anywhere; }
.calendar-event-card:last-child { border-bottom: 0; }
.calendar-event-card img { width: 3.5rem; height: 5.2rem; object-fit: cover; border-radius: .3rem; flex: 0 0 auto; }
.calendar-event-card > div { min-width: 0; }
.calendar-event-card h3 { margin: 0; font-size: .95rem; }
.calendar-event-card p { margin: .4rem 0; color: var(--text-muted); font-size: .8rem; line-height: 1.5; }
.calendar-event-card a { font-size: .8rem; color: var(--accent); }
.calendar-status { padding: .15rem .4rem; border: 1px solid var(--border); border-radius: .3rem; font-size: .75rem; color: var(--text); }
.calendar-status.present { color: var(--success); }
.calendar-notice { border: 1px solid var(--border); background: var(--surface); border-radius: .5rem; color: var(--text-muted); padding: .8rem; font-size: .85rem; }
.calendar-empty { padding: 1.5rem; text-align: center; color: var(--text-muted); line-height: 1.7; }
.agenda-day h2 { font-size: 1rem; padding-bottom: .5rem; border-bottom: 1px solid var(--border); }
.agenda-day small { color: var(--accent); margin-left: .5rem; }
.load-more { margin: 1rem auto; display: block; }
.undated-panel { margin-top: 1rem; }
.undated-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr)); gap: .6rem; }
.undated-list p { font-size: .8rem; margin: 0; }
.mobile-day-count { display: none; }
@media (max-width: 1100px) { .calendar-layout { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 640px) {
  .calendar-actions, .calendar-filters { width: 100%; flex-wrap: wrap; }
  .calendar-actions button { flex: 1; }
  .calendar-toolbar { padding: .75rem; }
  .calendar-filters label { flex: 1; }
  .calendar-filters select { width: 100%; max-width: 100%; }
  .month-navigation { gap: .4rem; width: 100%; justify-content: space-between; }
  .month-navigation h2 { min-width: 0; font-size: 1rem; }
  .calendar-day { min-height: 4rem; padding: .25rem; }
  .date-number { font-size: .75rem; }
  .date-number small, .day-event, .day-overflow { display: none; }
  .mobile-day-count { display: block; font-size: .6rem; margin-top: .5rem; color: var(--accent); }
}
</style>
