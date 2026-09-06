import { describe, expect, it } from 'vitest'
import { Permissions } from '@/auth/generated-permissions'
import { dashboardCards, dashboardSectionPriority, getVisibleDashboardCards } from '@/dashboard/cards'

describe('mixed dashboard card contract', () => {
  it('keeps operational sections ahead of discovery content', () => {
    const priority = new Map(dashboardSectionPriority.map((section, index) => [section, index]))
    const positions = dashboardCards.map(card => priority.get(card.section) ?? -1)

    expect(positions).toEqual([...positions].sort((left, right) => left - right))
    expect(dashboardCards.at(-1)?.id).toBe('discovery-hero')
  })

  it('marks only wired read models as live', () => {
    expect(dashboardCards.filter(card => card.state === 'live').map(card => card.id)).toEqual(['server-status', 'media-summary', 'storage-summary', 'connection-health', 'active-tasks', 'scheduler-jobs', 'download-summary'])
  })

  it('omits protected domain cards instead of leaking their counts', () => {
    const visible = getVisibleDashboardCards([Permissions.DashboardRead])
    expect(visible.map(card => card.id)).toEqual(['server-status'])

    const transferVisible = getVisibleDashboardCards([Permissions.TransfersReadOwn])
    expect(transferVisible.map(card => card.id)).toEqual(['recent-imports'])
    expect(transferVisible[0]?.description).not.toContain('API 尚未实现')
  })

  it('preserves the canonical order after permission filtering', () => {
    const allPermissions = Object.values(Permissions)
    expect(getVisibleDashboardCards(allPermissions).map(card => card.id)).toEqual(dashboardCards.map(card => card.id))
  })
})
