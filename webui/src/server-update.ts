import { api } from '@/api/client'
import type { ServerUpdateChannel, ServerUpdatePhase, ServerUpdateStatus } from '@/types/api'

export const serverUpdateStatusPath = '/api/v1/system/update'
export const serverUpdateCheckPath = `${serverUpdateStatusPath}/check`
export const serverUpdateSettingsPath = `${serverUpdateStatusPath}/settings`
export const serverUpdateInstallPath = `${serverUpdateStatusPath}/install`

export type UpdateAPI = <T>(path: string, options?: RequestInit) => Promise<T>

export function createUpdateRequestGuard() {
  let generation = 0
  return {
    next() { generation += 1; return generation },
    isCurrent(candidate: number) { return candidate === generation },
    invalidate() { generation += 1 },
  }
}

export const activeUpdatePhases = new Set<ServerUpdatePhase>([
  'checking',
  'downloading',
  'ready',
  'waiting_for_exit',
  'replacing',
  'restarting',
  'verifying',
])

const phaseLabels: Record<ServerUpdatePhase, string> = {
  idle: '等待检查',
  checking: '正在检查',
  available: '发现新版本',
  downloading: '正在下载',
  ready: '更新包已就绪',
  waiting_for_exit: '正在等待旧进程退出',
  replacing: '正在替换程序',
  restarting: '正在重启',
  verifying: '正在校验',
  succeeded: '更新成功',
  failed: '更新失败',
  rolled_back: '已自动回滚',
}

const errorLabels: Record<string, string> = {
  update_invalid_channel: '更新通道无效，请刷新页面后重试。',
  update_unsupported_platform: '当前平台暂不支持 Server 自更新。',
  update_invalid_release: '官方 Release 的版本或资产契约无效，已停止更新。',
  update_no_release: '当前通道暂无可用的官方 Server Release。',
  update_network_error: '无法连接官方 Release 服务，请检查网络后重试。',
  update_untrusted_source: 'Release 请求跳转到了非受信来源，已停止更新。',
  update_response_too_large: 'Release 响应超过安全大小限制，已停止更新。',
  update_checksum_invalid: '校验清单格式无效，未替换当前 Server。',
  update_checksum_mismatch: '更新包 SHA-256 不匹配，未替换当前 Server。',
  update_archive_invalid: '更新包结构不安全或不完整，未替换当前 Server。',
  update_candidate_too_large: '更新包中的 Server 程序超过安全大小限制，已停止更新。',
  update_persistence_error: '无法安全保存更新状态或更新包，未替换当前 Server。',
  update_plan_invalid: '更新计划无效，未执行程序替换。',
  update_parent_exit_timeout: '旧 Server 未在限定时间内退出，更新已停止。',
  update_replacement_failed: 'Server 程序替换失败，已尝试保留旧版本。',
  update_restart_failed: '新 Server 无法启动，已尝试恢复旧版本。',
  update_health_check_failed: '新版本未通过健康检查，Server 已尝试恢复旧版本。',
  update_rollback_failed: '自动回滚未完成，请查看 Server 运行日志并按部署方式手工恢复。',
}

const managedReasonLabels: Record<string, string> = {
  container: '当前 Server 运行在容器中，请更新镜像并重新部署。',
  deployment_managed: '当前部署明确关闭了进程内更新，请使用部署工具完成升级。',
  development_build: '开发构建可以检查版本，但需要先手工安装正式 Server 包。',
  unreplaceable_executable: 'Server 可执行文件不可替换，请使用原安装方式完成升级。',
  unsupported_platform: '当前平台没有可用的官方自更新资产。',
}

export function updatePhaseLabel(phase: ServerUpdatePhase) {
  return phaseLabels[phase] ?? phase
}

export function updateErrorLabel(code: string) {
  return code ? (errorLabels[code] ?? `更新未完成（${code}）`) : ''
}

export function updateManagedReasonLabel(reason?: string) {
  return (reason ? managedReasonLabels[reason] : undefined) ?? '当前安装由外部部署方式管理；仍可检查版本，但不能在此替换 Server。'
}

export function isUpdateBusy(status: ServerUpdateStatus | null) {
  return status ? activeUpdatePhases.has(status.phase) : false
}

export function canInstallServerUpdate(status: ServerUpdateStatus | null) {
  return Boolean(
    status
    && status.install_enabled
    && !status.deployment_managed
    && status.update_available
    && status.latest_version
    && !isUpdateBusy(status),
  )
}

export function loadServerUpdate(apiClient: UpdateAPI = api) {
  return apiClient<ServerUpdateStatus>(serverUpdateStatusPath)
}

export function checkServerUpdate(apiClient: UpdateAPI = api) {
  return apiClient<ServerUpdateStatus>(serverUpdateCheckPath, { method: 'POST' })
}

export function saveServerUpdateChannel(channel: ServerUpdateChannel, revision: number, apiClient: UpdateAPI = api) {
  return apiClient<ServerUpdateStatus>(serverUpdateSettingsPath, {
    method: 'PATCH',
    body: JSON.stringify({ channel, revision }),
  })
}

export function installServerUpdate(targetVersion: string, apiClient: UpdateAPI = api) {
  return apiClient<ServerUpdateStatus>(serverUpdateInstallPath, {
    method: 'POST',
    body: JSON.stringify({ target_version: targetVersion }),
  })
}

export interface ReconnectOptions {
  attempts?: number
  delayMs?: number
  sleep?: (milliseconds: number) => Promise<void>
}

/**
 * Wait for the restarted process, tolerating expected connection failures.
 * Success requires the requested version, so a response from the old process
 * immediately before shutdown cannot be mistaken for recovery.
 */
export async function waitForServerUpdateReconnect(
  targetVersion: string,
  loadStatus: () => Promise<ServerUpdateStatus>,
  options: ReconnectOptions = {},
) {
  const attempts = options.attempts ?? 80
  const delayMs = options.delayMs ?? 1500
  const sleep = options.sleep ?? (milliseconds => new Promise<void>(resolve => setTimeout(resolve, milliseconds)))
  let lastStatus: ServerUpdateStatus | null = null

  for (let attempt = 0; attempt < attempts; attempt++) {
    if (attempt > 0) await sleep(delayMs)
    try {
      lastStatus = await loadStatus()
      if (lastStatus.current_version === targetVersion && !isUpdateBusy(lastStatus)) return lastStatus
      if (lastStatus.phase === 'failed' || lastStatus.phase === 'rolled_back') return lastStatus
    } catch {
      // A refused connection is expected while the old process exits and the new one starts.
    }
  }
  return lastStatus
}
