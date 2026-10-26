import { api } from '@/api/client'
import type { Job } from '@/jobs'

export interface DashboardSection<T> { status: 'ok' | 'unavailable'; list: T[]; has_more: boolean; error_code?: string }
export interface DashboardMedia { library_id: number; work_id: string; title: string; kind: string; poster_url?: string; added_at: string; season_count?: number; episode_count?: number }
export interface StorageCapacity { id: number; name: string; enabled: boolean; status: 'not_sampled' | 'ok' | 'stale' | 'refreshing' | 'error' | 'unsupported' | 'disabled'; free_bytes: number | null; total_bytes: number | null; sampled_at: string | null; checked_at: string | null; error_code?: string }
export interface DashboardActivity { generated_at: string; active_tasks: DashboardSection<Job>; waiting_tasks: DashboardSection<Job>; attention_tasks: DashboardSection<Job>; recently_added: DashboardSection<DashboardMedia>; capacities: DashboardSection<StorageCapacity> }
export interface DashboardFact { label: string; value: number | null; unit?: string; link?: string }
export interface DashboardSummary { initialized: boolean; recovery_required: boolean; users: number | null; active_users: number | null; roles: number | null; audit_events: number | null }
export const getDashboardActivity = (signal?: AbortSignal) => api<DashboardActivity>('/api/v1/dashboard/activity', { signal })
export const refreshStorageCapacity = (id: number, signal?: AbortSignal) => api<StorageCapacity>(`/api/v1/storages/${id}/capacity`, { method: 'POST', body: '{}', signal })
export function capacityStatusLabel(sample: StorageCapacity): string {
  if (sample.status === 'error') return ({ storage_capacity_timeout: '检测超时，请稍后重试', storage_capacity_invalid: '返回的容量数据无效，请稍后重试', storage_capacity_authorization: '授权已失效，请到数据源检查账号', storage_capacity_rate_limited: '服务暂时限流，请稍后重试', connection_unavailable: '连接不可用，请检查账号授权' } as Record<string, string>)[sample.error_code ?? ''] ?? '最近检测失败，可保留上次采样并重试'
  return ({ not_sampled: '尚未检测', ok: '已更新', stale: '采样已过期', refreshing: '正在检测', unsupported: '当前授权未提供容量', disabled: '数据源已停用' })[sample.status]
}
