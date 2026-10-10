import type { AuthorizationResourceType, ResourceAccessMode, ResourceAccessOption, ResourceAccessOptions, ResourceAccessPolicy, ResourceAccessScope, UserResourceAccess } from '@/types/api'

export const resourceAccessScopes: { scope: ResourceAccessScope; title: string; description: string; resourceType: AuthorizationResourceType }[] = [
  { scope: 'downloader_use', title: '下载器使用', description: '限制下载、分享预览、转存和自动订阅可使用的下载器。', resourceType: 'downloader' },
  { scope: 'site_search', title: '站点搜索', description: '限制搜索和使用 PT、公开 BT、网盘分享站的资源。', resourceType: 'site' },
  { scope: 'library_read', title: '媒体库访问', description: '限制可浏览、搜索、播放和查看历史内容的媒体库。', resourceType: 'media_library' },
  { scope: 'library_ingest', title: '媒体库入库', description: '限制下载、整理和订阅的目标库；还须允许访问该库。', resourceType: 'media_library' },
]

export function defaultResourcePolicies(): ResourceAccessPolicy[] {
  return resourceAccessScopes.map(item => ({ scope: item.scope, mode: 'all', resource_ids: [] }))
}

export function copyResourceAccess(value: UserResourceAccess): UserResourceAccess {
  return { revision: value.revision, policies: value.policies.map(policy => ({ ...policy, resource_ids: [...policy.resource_ids] })) }
}

export function resourcePolicyAllows(policy: ResourceAccessPolicy | undefined, id: string): boolean {
  if (!policy) return false
  if (policy.mode === 'all') return true
  const included = policy.resource_ids.includes(id)
  return policy.mode === 'allowlist' ? included : !included
}

export function resourceDraftAllows(policies: ResourceAccessPolicy[], scope: ResourceAccessScope, id: string): boolean {
  if (!resourcePolicyAllows(policies.find(item => item.scope === scope), id)) return false
  return scope !== 'library_ingest' || resourcePolicyAllows(policies.find(item => item.scope === 'library_read'), id)
}

export function setResourceMode(policy: ResourceAccessPolicy, mode: ResourceAccessMode) {
  policy.mode = mode
  // The all mode has no list on the wire. Other mode changes keep the visible
  // selection so their different meanings remain explicit to the administrator.
  if (mode === 'all') policy.resource_ids = []
}

export function resourceSelectionLocked(policy: ResourceAccessPolicy, initial: ResourceAccessPolicy | undefined, option: ResourceAccessOption): boolean {
  if (option.deleted) return !initial?.resource_ids.includes(option.id)
  if (option.can_grant || resourcePolicyAllows(initial, option.id)) return false
  // A selected denylist entry cannot be removed when that would expand access
  // beyond the administrator's scope. A fresh deny can always be undone.
  return policy.mode === 'allowlist' || (policy.mode === 'denylist' && policy.resource_ids.includes(option.id))
}

export function resourceAccessFingerprint(value: UserResourceAccess): string {
  return JSON.stringify({ revision: value.revision, policies: resourceAccessScopes.map(({ scope }) => {
    const policy = value.policies.find(item => item.scope === scope)
    return { scope, mode: policy?.mode, resource_ids: [...(policy?.resource_ids ?? [])].sort() }
  }) })
}

export function authorizationResourceOptions(options: ResourceAccessOptions | null, type: AuthorizationResourceType, retainedID = ''): ResourceAccessOption[] {
  const values = new Map<string, ResourceAccessOption>()
  for (const scope of options?.scopes ?? []) {
    if (scope.resource_type !== type) continue
    for (const item of scope.options) if (!values.has(item.id)) values.set(item.id, item)
  }
  if (retainedID && !values.has(retainedID)) {
    values.set(retainedID, { id: retainedID, name: '已删除或无权查看的资源（保留原规则）', type, status: 'unavailable', deleted: false, effective_allowed: false, can_grant: false, denial_reason: '此资源目前不可查看，原规则仍会保留。' })
  }
  return [...values.values()]
}

export function resourceTypeLabel(type: string): string {
  const labels: Record<string, string> = { qbittorrent: 'qBittorrent', transmission: 'Transmission', pan115: '115 网盘', pt: 'PT', public_bt: '公开 BT', cloud_share: '网盘分享站', local: '本地', downloader: '下载器', site: '站点', media_library: '媒体库' }
  return labels[type] ?? type
}

export function resourceStatusLabel(option: ResourceAccessOption): string {
  if (option.deleted) return '已删除'
  if (['enabled', 'active', 'online'].includes(option.status)) return '已启用'
  if (['disabled', 'offline'].includes(option.status)) return '已停用'
  return option.status === 'restricted' || option.status === 'unavailable' ? '不可查看' : option.status
}
