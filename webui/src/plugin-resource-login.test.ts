// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import PluginResourceLogin from '@/components/PluginResourceLogin.vue'
import type { PluginConnectionSummary, ResourceLoginResponse } from '@/plugins'

const connection: PluginConnectionSummary = {
  id: 'connection id', plugin_id: 'org.example.resource', name: '资源站', config: { entryOrigin: 'https://mirror.example/' },
  credential_scope: 'resource.session', credential_mode: 'cookie', credential_configured: false,
  resource_type: 'bt_resource', entry_origin: 'https://mirror.example/', enabled: true, health_status: 'unknown',
  revision: 1, created_at: '2026-09-11T00:00:00Z', updated_at: '2026-09-11T00:00:00Z',
}

describe('plugin resource login', () => {
  it('normal login has no separate browser installation or browser-login workflow', () => {
    const wrapper = mount(PluginResourceLogin, { props: { pluginId: 'org.example.resource', connection } })
    expect(wrapper.text()).toContain('默认使用 Server 内置浏览器登录')
    expect(wrapper.text()).not.toContain('打开浏览器登录')
    expect(wrapper.text()).not.toContain('接受许可')
    expect(wrapper.findComponent({ name: 'PluginBrowserLogin' }).exists()).toBe(false)
    wrapper.unmount()
  })
  it('does not present stale login expiry or success when the latest check failed', () => {
    for (const status of ['healthy', 'auth_required'] as const) {
      const wrapper = mount(PluginResourceLogin, { props: {
        pluginId: 'org.example.resource', connection, healthResponse: { status },
        error: '资源站要求浏览器安全验证，暂时无法确认登录状态',
      } })
      expect(wrapper.text()).toContain('本次验证未通过，请查看下方原因')
      expect(wrapper.text()).not.toContain('入口可用，需要重新登录')
      expect(wrapper.text()).not.toContain('入口可用，登录有效')
      expect(wrapper.get('.status-chip').classes()).toContain('status-chip--warning')
    }
  })

  it('distinguishes persisted browser verification from expired credentials and rate limits', () => {
    const wrapper = mount(PluginResourceLogin, { props: {
      pluginId: 'org.example.resource', connection: { ...connection, health_status: 'browser_verification_required' },
    } })
    expect(wrapper.text()).toContain('站点要求浏览器验证，登录状态待确认')
    expect(wrapper.text()).not.toContain('登录已过期')
    expect(wrapper.text()).not.toContain('站点正在限流')
  })
  it('emits a generic health check and renders only the normalized result', async () => {
    const wrapper = mount(PluginResourceLogin, {
      props: { pluginId: 'org.example.resource', connection, healthResponse: { status: 'healthy', accountName: '测试账号' } },
    })
    expect(wrapper.text()).toContain('入口可用，登录有效')
    expect(wrapper.text()).toContain('测试账号')
    const health = wrapper.findAll('button').find(button => button.text() === '检测入口与登录')
    await health!.trigger('click')
    expect(wrapper.emitted('health')).toHaveLength(1)
  })

  it('clears password and Cookie fields immediately after emitting one-time values', async () => {
    const wrapper = mount(PluginResourceLogin, { props: { pluginId: 'org.example.resource', connection } })
    const inputs = wrapper.findAll('input')
    await inputs[0]!.setValue(' user@example.com ')
    await inputs[1]!.setValue('one-time-password')
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('login')?.[0]).toEqual([{ username: ' user@example.com ', password: 'one-time-password' }])
    expect((inputs[1]!.element as HTMLInputElement).value).toBe('')

    const cookieTab = wrapper.findAll('.management-tab').find(button => button.text() === '粘贴 Cookie')
    await cookieTab!.trigger('click')
    const textarea = wrapper.get('textarea')
    await textarea.setValue(' session=secret ')
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('cookie')?.[0]).toEqual([' session=secret '])
    expect((textarea.element as HTMLTextAreaElement).value).toBe('')
  })

  it('converts displayed captcha clicks to the declared image coordinate space', async () => {
    const response: ResourceLoginResponse = {
      state: 'captcha_required',
      challenge: { challengeId: 'challenge', imageAssetRef: 'asset/ref', width: 400, height: 200, prompt: '依次点击', maxPoints: 4 },
    }
    const wrapper = mount(PluginResourceLogin, { props: { pluginId: 'org.example/resource', connection, response } })
    const image = wrapper.get('img')
    Object.defineProperty(image.element, 'getBoundingClientRect', { value: () => ({ left: 10, top: 20, width: 200, height: 100, right: 210, bottom: 120, x: 10, y: 20, toJSON: () => ({}) }) })
    await image.trigger('click', { clientX: 110, clientY: 70 })
    const submit = wrapper.findAll('button').find(button => button.text() === '提交验证码')
    await submit!.trigger('click')
    expect(wrapper.emitted('captcha')?.[0]).toEqual([{ challengeId: 'challenge', points: [{ x: 200, y: 100 }] }])
    expect(image.attributes('src')).toBe('/api/v1/plugins/org.example%2Fresource/connections/connection%20id/resource/captcha/asset%2Fref')
		expect(image.element.parentElement?.className).toContain('w-fit')
  })

	it('labels successful Cookie authentication without retaining the submitted Cookie', async () => {
		const wrapper = mount(PluginResourceLogin, {
			props: { pluginId: 'org.example.resource', connection, response: { state: 'authenticated' } },
		})
		const cookieTab = wrapper.findAll('.management-tab').find(button => button.text() === '粘贴 Cookie')
		await cookieTab!.trigger('click')
		expect(wrapper.text()).toContain('Cookie 验证成功')
		expect(wrapper.text()).not.toContain('登录验证成功')
	})
})
