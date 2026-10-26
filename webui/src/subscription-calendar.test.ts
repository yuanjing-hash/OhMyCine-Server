import { describe, expect, it } from 'vitest'
import { calendarEpisodeLabel, calendarEventState, calendarMonthDays, calendarToday, shiftCalendarDate, shiftCalendarMonth, type CalendarEvent } from './subscription-calendar'

describe('subscription calendar date and status semantics', () => {
  it('keeps all-day dates across month/year/leap boundaries', () => {
    expect(shiftCalendarDate('2024-02-28', 1)).toBe('2024-02-29')
    expect(shiftCalendarDate('2024-12-31', 1)).toBe('2025-01-01')
    expect(shiftCalendarMonth('2026-01', -1)).toBe('2025-12')
    expect(calendarMonthDays('2026-10')).toHaveLength(42)
    expect(calendarMonthDays('2026-10')[0]).toBe('2026-09-28')
    expect(calendarMonthDays('2026-10')[41]).toBe('2026-11-08')
  })
  it('uses display timezone for today without changing a provider air date', () => {
    const now = new Date('2026-10-26T00:30:00Z')
    expect(calendarToday('Asia/Shanghai', now)).toBe('2026-10-26')
    expect(calendarToday('America/Los_Angeles', now)).toBe('2026-10-25')
  })
  it('gives current catalog presence precedence over task activity and does not call completed downloads imported', () => {
    const event = { season_number: 0, episode_number: 2, library_status: 'present', activity: 'downloading' } as CalendarEvent
    expect(calendarEpisodeLabel(event)).toBe('S00E02')
    expect(calendarEventState(event)).toBe('已入库')
    expect(calendarEventState({ ...event, library_status: 'unknown', activity: 'organizing' })).toBe('待整理入库')
    expect(calendarEventState({ ...event, library_status: 'unknown', activity: 'none' })).toBe('入库状态待确认')
    expect(calendarEventState({ ...event, library_status: 'missing', activity: 'none' })).toBe('未入库')
    expect(calendarEventState({ ...event, activity: 'unknown' })).toBe('已入库')
    expect(calendarEventState({ ...event, library_status: 'missing', activity: 'unknown' })).toBe('处理状态待确认')
  })
})
