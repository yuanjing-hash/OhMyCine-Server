// @vitest-environment happy-dom
import { readFileSync } from 'node:fs'
import { URL as NodeURL } from 'node:url'
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import PluginSettingsForm from './components/PluginSettingsForm.vue'

describe('declarative plugin settings login surface', () => {
  it('shows bounded last-observed membership after a page reload', () => {
    const wrapper = mount(PluginSettingsForm, { props: {
      page: { version: 1, tabs: [{ id: 'account', title: '账号', sections: [{ id: 'login', title: '登录', fields: [{ type: 'credential-status', label: '账号状态' }] }] }] },
      modelValue: {}, credentialConfigured: true, healthStatus: 'healthy',
      accountSummary: { id: 'fixture', name: '测试账号', membership: { status: 'active', label: 'VIP', expiresAt: '2027-01-01T00:00:00Z' } },
      accountCheckedAt: '2026-10-10T00:00:00Z',
    } })
    expect(wrapper.text()).toContain('已登录：测试账号')
    expect(wrapper.text()).toContain('VIP')
    expect(wrapper.text()).toContain('上次确认')
    expect(wrapper.text()).toContain('有效期至')
    expect(wrapper.find('img').exists()).toBe(false)
    wrapper.unmount()
  })
  it('renders Host-owned QR login inside the plugin-declared credential component', () => {
	const form = readFileSync(new NodeURL('./components/PluginSettingsForm.vue', import.meta.url), 'utf8')
	const view = readFileSync(new NodeURL('./views/PluginsView.vue', import.meta.url), 'utf8')

    expect(form).toContain("field.type === 'credential-status'")
    expect(form).toContain("emit('start-auth')")
    expect(form).toContain('qrAuthState.qrDataURL')
    expect(form).toContain("props.qrAuthState?.state === 'pending' || props.qrAuthState?.state === 'scanned'")
    expect(view).toContain('@start-auth="startConnectionAuth(plugin, connection)"')
    expect(view).toContain(':qr-auth-state="canManage ? connectionAuth[connection.id] : undefined"')
    expect(view).not.toContain('connectionAuth[connection.id].qrDataURL')
    expect(view).not.toContain('connection.credential_mode === \'cookie\' && plugin.capabilities.includes(\'site.interaction\')')
  })
})
