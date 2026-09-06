import { Permissions, type PermissionCode } from '@/auth/generated-permissions'
import { hasAnyPermission } from '@/navigation'

export type DashboardSection = 'status' | 'activity' | 'pipeline' | 'subscriptions' | 'discovery'
export type DashboardCardState = 'live' | 'planned'

export interface DashboardCardDefinition {
  id: string
  title: string
  section: DashboardSection
  state: DashboardCardState
  span: 3 | 4 | 5 | 7 | 12
  permissionsAny: readonly PermissionCode[]
  description: string
  owner: string
}

export const dashboardCards: readonly DashboardCardDefinition[] = [
  { id: 'server-status', title: 'Server 总状态', section: 'status', state: 'live', span: 12, permissionsAny: [Permissions.DashboardRead], description: '来自 GET /api/v1/dashboard 的初始化与恢复基线。', owner: 'runtime / administration' },
  { id: 'media-summary', title: '媒体概览', section: 'status', state: 'live', span: 4, permissionsAny: [Permissions.MediaLibrariesRead], description: '当前账号可见库、库内作品与文件数量。', owner: 'media catalog / import history' },
  { id: 'storage-summary', title: '存储空间', section: 'status', state: 'live', span: 4, permissionsAny: [Permissions.StoragesRead], description: '最近一次保存的容量检测，不在页面加载时探测存储。', owner: 'destination service / storage drivers' },
  { id: 'connection-health', title: '连接健康', section: 'status', state: 'live', span: 4, permissionsAny: [Permissions.ConnectionsRead], description: '已保存的连接健康状态；未知与未检测不会冒充正常。', owner: 'connection service / provider adapters' },
  { id: 'active-tasks', title: '活动任务', section: 'activity', state: 'live', span: 7, permissionsAny: [Permissions.JobsReadOwn, Permissions.JobsReadAll], description: '当前权限范围内的任务状态与可恢复详情入口。', owner: 'task read model over domain services' },
  { id: 'pipeline-status', title: '流水线状态', section: 'activity', state: 'planned', span: 5, permissionsAny: [Permissions.DownloadsReadOwn, Permissions.DownloadsReadAll, Permissions.FollowsReadOwn, Permissions.FollowsReadAll, Permissions.StrmRunsRead], description: 'Discover → Download → Transfer → Import → Notify 各阶段尚无真实聚合状态。', owner: 'pipeline projection' },
  { id: 'recent-imports', title: '近期入库 / 整理', section: 'pipeline', state: 'planned', span: 7, permissionsAny: [Permissions.TransfersReadOwn, Permissions.TransfersReadAll], description: '媒体整理已有独立任务中心；仪表盘近期入库聚合仍待接入，完整记录可前往媒体整理查看。', owner: 'transfer / import history' },
  { id: 'scheduler-jobs', title: '后台任务队列', section: 'pipeline', state: 'live', span: 5, permissionsAny: [Permissions.SettingsRead], description: '当前账号可见的计划任务启停统计。', owner: 'scheduler service' },
  { id: 'download-summary', title: '下载速率 / 队列', section: 'pipeline', state: 'live', span: 4, permissionsAny: [Permissions.DownloadsReadOwn, Permissions.DownloadsReadAll], description: '账号范围内任务数量与新鲜采样速率；未知值保留为空。', owner: 'downloader service / adapters' },
  { id: 'subscription-calendar', title: '订阅日历', section: 'subscriptions', state: 'planned', span: 5, permissionsAny: [Permissions.FollowsReadOwn, Permissions.FollowsReadAll], description: '订阅管理已实现；独立日历视图仍在规划，可前往订阅管理查看。', owner: 'follow service / scheduler' },
  { id: 'quick-actions', title: '快捷操作', section: 'subscriptions', state: 'planned', span: 3, permissionsAny: [Permissions.ConnectionsCreate, Permissions.FollowsCreate, Permissions.DownloadsCreate, Permissions.StrmRunsCreate, Permissions.MediaServersRefresh], description: '业务操作已在各自页面提供；仪表盘快捷提交仍待接入。', owner: 'each domain write API' },
  { id: 'discovery-hero', title: '发现内容', section: 'discovery', state: 'planned', span: 12, permissionsAny: [Permissions.DiscoveryRead], description: '发现页面已实现；仪表盘推荐卡片尚未接入，请前往发现页面。', owner: 'discovery service / metadata providers' },
] as const

export const dashboardSectionPriority: readonly DashboardSection[] = ['status', 'activity', 'pipeline', 'subscriptions', 'discovery']

export function getVisibleDashboardCards(granted: Iterable<PermissionCode>): DashboardCardDefinition[] {
  const permissionSet = granted instanceof Set ? granted : new Set(granted)
  return dashboardCards.filter(card => hasAnyPermission(card.permissionsAny, permissionSet))
}
