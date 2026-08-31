import { describe, expect, it, vi } from 'vitest'
import {
  canInstallServerUpdate,
  checkServerUpdate,
  createUpdateRequestGuard,
  installServerUpdate,
  saveServerUpdateChannel,
  serverUpdateCheckPath,
  serverUpdateInstallPath,
  serverUpdateSettingsPath,
  updateErrorLabel,
  updateManagedReasonLabel,
  waitForServerUpdateReconnect,
  type UpdateAPI,
} from '@/server-update'
import type { ServerUpdateStatus } from '@/types/api'

function updateStatus(overrides: Partial<ServerUpdateStatus> = {}): ServerUpdateStatus {
  return {
    current_version: '1.2.0',
    current_commit: 'abcdef1',
    official_build: true,
    comparable: true,
    channel: 'beta',
    revision: 3,
    phase: 'available',
    latest_version: '1.3.0',
    target_version: '',
    update_available: true,
    install_enabled: true,
    deployment_managed: false,
    managed_reason: '',
    error_code: '',
    last_checked_at: '2026-08-31T12:00:00Z',
    ...overrides,
  }
}

describe('Server update API contract', () => {
  it('uses the fixed routes and sends only the channel revision or target version', async () => {
    const request = vi.fn().mockResolvedValue(updateStatus())
    const apiClient = request as unknown as UpdateAPI

    await checkServerUpdate(apiClient)
    await saveServerUpdateChannel('stable', 7, apiClient)
    await installServerUpdate('1.3.0', apiClient)

    expect(request).toHaveBeenNthCalledWith(1, serverUpdateCheckPath, { method: 'POST' })
    expect(request).toHaveBeenNthCalledWith(2, serverUpdateSettingsPath, { method: 'PATCH', body: '{"channel":"stable","revision":7}' })
    expect(request).toHaveBeenNthCalledWith(3, serverUpdateInstallPath, { method: 'POST', body: '{"target_version":"1.3.0"}' })
  })

  it('disables installation for managed, unsupported, busy, or unavailable states', () => {
    expect(canInstallServerUpdate(updateStatus())).toBe(true)
    expect(canInstallServerUpdate(updateStatus({ deployment_managed: true }))).toBe(false)
    expect(canInstallServerUpdate(updateStatus({ install_enabled: false }))).toBe(false)
    expect(canInstallServerUpdate(updateStatus({ phase: 'downloading' }))).toBe(false)
    expect(canInstallServerUpdate(updateStatus({ update_available: false }))).toBe(false)
  })

  it('uses stable Chinese labels without exposing upstream response details', () => {
    expect(updateErrorLabel('update_checksum_mismatch')).toContain('SHA-256')
    expect(updateErrorLabel('unknown_code')).toBe('更新未完成（unknown_code）')
    expect(updateManagedReasonLabel('container')).toContain('镜像')
    expect(updateManagedReasonLabel('development_build')).toContain('开发构建')
  })
})

describe('Server update request and reconnect races', () => {
  it('rejects stale generations after a newer request or unmount invalidation', () => {
    const guard = createUpdateRequestGuard()
    const first = guard.next()
    const second = guard.next()
    expect(guard.isCurrent(first)).toBe(false)
    expect(guard.isCurrent(second)).toBe(true)
    guard.invalidate()
    expect(guard.isCurrent(second)).toBe(false)
  })

  it('tolerates restart disconnects and does not accept the old process as recovered', async () => {
    const load = vi.fn()
      .mockResolvedValueOnce(updateStatus({ current_version: '1.2.0', phase: 'ready' }))
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(updateStatus({ current_version: '1.3.0', phase: 'restarting' }))
      .mockResolvedValueOnce(updateStatus({ current_version: '1.3.0', phase: 'succeeded', update_available: false }))
    const sleep = vi.fn().mockResolvedValue(undefined)

    await expect(waitForServerUpdateReconnect('1.3.0', load, { attempts: 4, delayMs: 1, sleep })).resolves.toEqual(
      expect.objectContaining({ current_version: '1.3.0', phase: 'succeeded' }),
    )
    expect(load).toHaveBeenCalledTimes(4)
    expect(sleep).toHaveBeenCalledTimes(3)
  })

  it('stops polling when the recovered service reports a rollback', async () => {
    const rolledBack = updateStatus({ current_version: '1.2.0', phase: 'rolled_back', error_code: 'update_health_check_failed' })
    const load = vi.fn().mockRejectedValueOnce(new TypeError('Failed to fetch')).mockResolvedValueOnce(rolledBack)
    const result = await waitForServerUpdateReconnect('1.3.0', load, { attempts: 10, delayMs: 1, sleep: async () => undefined })
    expect(result).toEqual(rolledBack)
    expect(load).toHaveBeenCalledTimes(2)
  })
})
