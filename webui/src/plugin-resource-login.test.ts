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
