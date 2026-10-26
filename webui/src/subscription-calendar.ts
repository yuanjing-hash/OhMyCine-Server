import { api } from '@/api/client'

export interface CalendarEvent {
  id: string; subscription_id: string; tmdb_id: number; title: string; poster_ref?: string
  subscription_status: string; season_number: number; episode_number: number; episode_title?: string; air_date?: string
  library_status: 'present' | 'missing' | 'unknown'
  activity: 'none' | 'downloading' | 'organizing' | 'needs_attention' | 'unknown'
  schedule_state: 'fresh' | 'stale'
}
export interface CalendarSubscription { id: string; title: string; poster_ref?: string; status: string; seasons: number[] }
export interface CalendarPage {
  events: CalendarEvent[]; undated: CalendarEvent[]; subscriptions: CalendarSubscription[]
  has_more: boolean; next_cursor?: string; partial: boolean; reasons: string[]
  missing_seasons: number; stale_seasons: number; refreshing_seasons: number; undated_count: number
  date_basis: 'expected_air_date'; scope: 'own' | 'all'; generated_at: string; timezone: string; start: string; end: string
}
export interface CalendarQuery {
  start: string; end: string; timezone: string; page_size?: number
  status?: string; subscription_id?: string; cursor?: string
}
export function getSubscriptionCalendar(query: CalendarQuery, signal?: AbortSignal) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) if (value !== undefined && value !== '') params.set(key, String(value))
  return api<CalendarPage>(`/api/v1/follows/calendar?${params}`, { signal })
}
export function refreshSubscriptionCalendar(subscriptionID?: string, signal?: AbortSignal) {
  return api<{ queued: number; already_queued: number; cooling_down: number; partial: boolean }>('/api/v1/follows/calendar/refresh', {
    method: 'POST', body: JSON.stringify(subscriptionID ? { subscription_id: subscriptionID } : {}), signal,
  })
}
export function calendarEpisodeLabel(event: Pick<CalendarEvent, 'season_number' | 'episode_number'>) {
  return `S${String(event.season_number).padStart(2, '0')}E${String(event.episode_number).padStart(2, '0')}`
}
export function calendarEventState(event: CalendarEvent) {
  if (event.library_status === 'present') return '已入库'
  if (event.activity === 'downloading') return '下载中'
  if (event.activity === 'organizing') return '待整理入库'
  if (event.activity === 'needs_attention') return '需要处理'
  if (event.activity === 'unknown') return '处理状态待确认'
  return event.library_status === 'missing' ? '未入库' : '入库状态待确认'
}
export function calendarTimezone() { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' } catch { return 'UTC' } }
export function calendarToday(timezone = calendarTimezone(), now = new Date()) {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(now)
  return `${parts.find(p => p.type === 'year')!.value}-${parts.find(p => p.type === 'month')!.value}-${parts.find(p => p.type === 'day')!.value}`
}
export function shiftCalendarDate(day: string, amount: number) {
  const [year, month, date] = day.split('-').map(Number)
  return new Date(Date.UTC(year!, month! - 1, date! + amount)).toISOString().slice(0, 10)
}
export function calendarMonthDays(month: string) {
  const first = `${month}-01`
  const weekday = new Date(`${first}T00:00:00Z`).getUTCDay()
  const start = shiftCalendarDate(first, -(weekday + 6) % 7)
  return Array.from({ length: 42 }, (_, index) => shiftCalendarDate(start, index))
}
export function shiftCalendarMonth(month: string, amount: number) {
  const [year, value] = month.split('-').map(Number)
  return new Date(Date.UTC(year!, value! - 1 + amount, 1)).toISOString().slice(0, 7)
}
