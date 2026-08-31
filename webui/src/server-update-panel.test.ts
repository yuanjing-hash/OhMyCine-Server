// @vitest-environment happy-dom

import { readFileSync } from 'node:fs'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Permissions } from '@/auth/generated-permissions'
import ServerUpdatePanel from '@/components/ServerUpdatePanel.vue'
import { serverUpdateCheckPath, serverUpdateStatusPath } from '@/server-update'
import { useAuthStore } from '@/stores/auth'
import type { PermissionCode } from '@/auth/generated-permissions'
import type { ServerUpdateStatus } from '@/types/api'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: apiMock }))

function status(overrides: Partial<ServerUpdateStatus> = {}): ServerUpdateStatus {
  return {
    current_version: '1.2.0', current_commit: 'abcdef1', official_build: true, comparable: true,
    channel: 'beta', revision: 1, phase: 'available', latest_version: '1.3.0', target_version: '',
    update_available: true, install_enabled: true, deployment_managed: false, managed_reason: '',
    error_code: '', last_checked_at: '2026-08-31T12:00:00Z', ...overrides,
  }
}

function mountPanel(permissions: PermissionCode[]) {
  const pinia = createPinia()
  setActivePinia(pinia)
  useAuthStore().user = { id: 1, username: 'tester', display_name: 'Tester', status: 'active', is_owner: false, roles: [], permissions }
  return mount(ServerUpdatePanel, { global: { plugins: [pinia] } })
}

describe('Server update settings panel', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    apiMock.mockReset()
  })

  it('does not render or request update state without system.admin', async () => {
    wrapper = mountPanel([Permissions.SettingsUpdate])
    await flushPromises()
    expect(wrapper.html()).toBe('<!--v-if-->')
    expect(apiMock).not.toHaveBeenCalled()
  })

  it('renders deployment-managed guidance and keeps install disabled', async () => {
    apiMock.mockResolvedValue(status({
      install_enabled: false,
      deployment_managed: true,
      managed_reason: 'container',
    }))
    wrapper = mountPanel([Permissions.SystemAdmin])
    await flushPromises()

    expect(apiMock).toHaveBeenCalledWith(serverUpdateStatusPath)
    expect(wrapper.text()).toContain('由部署方式管理')
    expect(wrapper.text()).toContain('更新镜像并重新部署')
    const install = wrapper.findAll('button').find(button => button.text().includes('下载并更新'))!
    expect(install.attributes('disabled')).toBeDefined()
  })

  it('checks again and renders the newest response', async () => {
    apiMock
      .mockResolvedValueOnce(status({ latest_version: '1.3.0' }))
      .mockResolvedValueOnce(status({ latest_version: '1.4.0' }))
    wrapper = mountPanel([Permissions.SystemAdmin])
    await flushPromises()

    await wrapper.findAll('button').find(button => button.text() === '立即检查')!.trigger('click')
    await flushPromises()

    expect(apiMock).toHaveBeenLastCalledWith(serverUpdateCheckPath, { method: 'POST' })
    expect(wrapper.text()).toContain('1.4.0')
  })

  it('distinguishes an up-to-date official build from an unsupported install', async () => {
    apiMock.mockResolvedValue(status({ latest_version: '1.2.0', update_available: false, install_enabled: false, phase: 'idle' }))
    wrapper = mountPanel([Permissions.SystemAdmin])
    await flushPromises()
    expect(wrapper.text()).toContain('没有需要安装的新版本')
    expect(wrapper.text()).not.toContain('当前版本不能在页面内更新')
  })

  it('keeps the Settings integration permission-gated and responsive', () => {
    const settings = readFileSync('src/views/SettingsView.vue', 'utf8')
    const panel = readFileSync('src/components/ServerUpdatePanel.vue', 'utf8')
    expect(settings).toContain('<ServerUpdatePanel v-if="auth.can(Permissions.SystemAdmin)" />')
    expect(panel).toContain('sm:grid-cols-2 xl:grid-cols-4')
    expect(panel).toContain('sm:grid-cols-[minmax(12rem,1fr)_auto]')
    expect(panel).toContain('正在等待 Server 重启')
  })
})
