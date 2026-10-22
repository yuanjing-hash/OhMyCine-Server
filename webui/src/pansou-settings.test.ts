// @vitest-environment happy-dom
import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PanSouSettings from './components/PanSouSettings.vue'
import { normalizeCloudSiteConfig, type CloudSiteConfig } from './sites'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: request }))
const scope = { source_scope: 'all', channel_mode: 'defaults_plus_custom', default_channels: ['alpha', 'beta'], custom_channels: ['beta', 'gamma'], effective_channels: ['alpha', 'beta', 'gamma'], default_count: 2, custom_count: 2, effective_count: 3 }
function render(config = normalizeCloudSiteConfig()) {
  const wrapper = mount(PanSouSettings, { props: { modelValue: config, baseURL: 'http://pansou:8888', username: '', password: '', authEnabled: false, 'onUpdate:modelValue': (value: CloudSiteConfig) => { void wrapper.setProps({ modelValue: value }) } } })
  return wrapper
}
afterEach(() => { request.mockReset(); vi.useRealTimers() })

it('uses all sources and append defaults for new config while preserving legacy custom-only channels', async () => {
  const wrapper = render()
  expect(wrapper.get<HTMLSelectElement>('#pansou-source-scope').element.value).toBe('all')
  expect(wrapper.get<HTMLTextAreaElement>('#site-channels').attributes('required')).toBeUndefined()
  await wrapper.setProps({ modelValue: normalizeCloudSiteConfig({ provider: '115', channels: ['alpha'], auth_enabled: false }) })
  expect(wrapper.get<HTMLTextAreaElement>('#site-channels').attributes('required')).toBeDefined()
  expect(wrapper.findAll('button').find(button => button.text() === '仅自定义')?.attributes('aria-pressed')).toBe('true')
  wrapper.unmount()
})

it('requests server-normalized actual counts and sends only the explicit draft to the scoped endpoint', async () => {
  request.mockResolvedValue(scope)
  const wrapper = render({ ...normalizeCloudSiteConfig(), channels: ['@beta', 'https://t.me/gamma'] })
  await wrapper.findAll('button').find(button => button.text() === '读取频道范围')!.trigger('click')
  await flushPromises()
  expect(wrapper.text()).toContain('默认 2 个＋自定义 2 个，去重后共 3 个频道')
  expect(request.mock.calls[0][0]).toBe('/api/v1/sites/pansou-scope')
  expect(JSON.parse(request.mock.calls[0][1].body)).toMatchObject({ refresh: true, base_url: 'http://pansou:8888', cloud_config: { source_scope: 'all', channel_mode: 'defaults_plus_custom' } })
  await wrapper.findAll('button').find(button => button.text() === '刷新频道范围')!.trigger('click')
  await flushPromises()
  expect(request).toHaveBeenCalledTimes(2)
  expect(JSON.parse(request.mock.calls[1][1].body).refresh).toBe(true)
  expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  wrapper.unmount()
})

it('plugin-only does not require TG or attempt default discovery and keeps plugin scope separate', async () => {
  const wrapper = render()
  await wrapper.get('#pansou-source-scope').setValue('plugin')
  expect(wrapper.find('#site-channels').exists()).toBe(false)
  expect(wrapper.text()).toContain('无须填写 TG 频道')
  expect(request).not.toHaveBeenCalled()
  await wrapper.get('#pansou-plugins').setValue('gying, mikan, gying')
  expect(wrapper.emitted('update:modelValue')?.at(-1)?.[0]).toMatchObject({ source_scope: 'plugin', plugins: ['gying', 'mikan'] })
  wrapper.unmount()
})

it('ignores an old count after URL/auth changes and aborts pending scope request on close', async () => {
  let finish!: (value: unknown) => void
  request.mockImplementation(() => new Promise(resolve => { finish = resolve }))
  const wrapper = render()
  await wrapper.findAll('button').find(button => button.text() === '读取频道范围')!.trigger('click')
  const signal = request.mock.calls[0][1].signal as AbortSignal
  await wrapper.setProps({ baseURL: 'http://other-pansou:8888', password: 'replacement-not-persisted' })
  expect(signal.aborted).toBe(true)
  finish(scope)
  await flushPromises()
  expect(wrapper.text()).not.toContain('去重后共 3 个频道')
  await wrapper.findAll('button').find(button => button.text() === '读取频道范围')!.trigger('click')
  const closing = request.mock.calls[1][1].signal as AbortSignal
  wrapper.unmount()
  expect(closing.aborted).toBe(true)
  finish(scope)
  await flushPromises()
})

it('reports missing TG defaults without hiding the plugin branch', async () => {
  request.mockResolvedValue({ ...scope, default_error_code: 'site_unavailable' })
  const wrapper = render()
  await wrapper.findAll('button').find(button => button.text() === '读取频道范围')!.trigger('click')
  await flushPromises()
  expect(wrapper.get('[role="alert"]').text()).toContain('已启用插件仍可搜索')
  expect(wrapper.get<HTMLSelectElement>('#pansou-source-scope').element.value).toBe('all')
  wrapper.unmount()
})
