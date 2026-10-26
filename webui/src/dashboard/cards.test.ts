import { describe, expect, it } from 'vitest'
import { Permissions } from '@/auth/generated-permissions'
import { dashboardCards, dashboardSectionPriority, getVisibleDashboardCards } from '@/dashboard/cards'

describe('mixed dashboard card contract', () => {
  it('keeps operational sections ahead of discovery content', () => {
    const priority = new Map(dashboardSectionPriority.map((section, index) => [section, index]))
    const positions = dashboardCards.map(card => priority.get(card.section) ?? -1)

    expect(positions).toEqual([...positions].sort((left, right) => left - right))
    expect(dashboardCards.at(-1)?.id).toBe('quick-actions')
  })

  it('marks only wired read models as live', () => {
    expect(dashboardCards.every(card => card.state === 'live')).toBe(true)
    expect(dashboardCards.map(card => card.id)).toEqual(['server-status', 'media-summary', 'storage-summary', 'connection-health', 'active-tasks', 'recent-imports', 'scheduler-jobs', 'download-summary', 'subscription-calendar', 'quick-actions'])
  })

  it('omits protected domain cards instead of leaking their counts', () => {
    const visible = getVisibleDashboardCards([Permissions.DashboardRead])
    expect(visible.map(card => card.id)).toEqual(['server-status'])

    expect(getVisibleDashboardCards([Permissions.TransfersReadOwn])).toEqual([])
    const libraryVisible = getVisibleDashboardCards([Permissions.MediaLibrariesRead])
    expect(libraryVisible.map(card => card.id)).toEqual(['media-summary', 'recent-imports', 'quick-actions'])
  })

  it('preserves the canonical order after permission filtering', () => {
    const allPermissions = Object.values(Permissions)
    expect(getVisibleDashboardCards(allPermissions).map(card => card.id)).toEqual(dashboardCards.map(card => card.id))
  })
})
