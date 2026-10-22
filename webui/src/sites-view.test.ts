// @vitest-environment happy-dom
import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SitesView from './views/SitesView.vue'
import { siteCatalogPath, sitesPath } from './sites'

const request = vi.hoisted(() => vi.fn())
const navigate = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: navigate }) }))
beforeEach(() => { request.mockReset(); navigate.mockReset() })

it('opens TG setup and generic PT setup when catalog entries have no preset URLs', async () => {
  const capabilities = { search: true, download: true }
  let savedSite: Record<string, unknown> | null = null
  request.mockImplementation(async (path: string, options?: { method?: string; body?: string }) => {
    if (path === sitesPath && options?.method === 'POST') {
      savedSite = { ...JSON.parse(options.body!), id: 42, revision: 1, site_type: 'cloud_share', credential_kind: 'none', capabilities, health: { status: 'online', error_code: '', username: '' } }
      return savedSite
    }
    if (path === sitesPath) return { list: savedSite ? [savedSite] : [] }
    if (path === siteCatalogPath) return { list: [
      { key: 'pttime', name: 'PTTime', engine: 'nexusphp', site_type: 'pt', credential_kind: 'cookie', base_urls: ['https://www.pttime.org'], capabilities },
      { key: 'nexusphp', name: '通用 NexusPHP', engine: 'nexusphp', site_type: 'pt', credential_kind: 'cookie', base_urls: null, capabilities },
      { key: 'pansou_tg', name: 'TG 网盘资源 · PanSou', engine: 'pansou', site_type: 'cloud_share', credential_kind: 'none', base_urls: null, capabilities },
    ] }
    throw new Error(`Unexpected request: ${path}`)
  })
  const errors = vi.fn()
  const wrapper = mount(SitesView, { global: { stubs: { RouterLink: true }, config: { errorHandler: errors } } })
  try {
    await flushPromises()
    await wrapper.get('header .btn-primary').trigger('click')
    await wrapper.findAll('.type-card').find(button => button.text().includes('网盘与磁力'))!.trigger('click')
    expect(errors).not.toHaveBeenCalled()
    expect(wrapper.get('#site-dialog-title').text()).toBe('添加网盘分享站')
    expect(wrapper.get<HTMLSelectElement>('#site-share-kind').element.value).toBe('pansou_tg')
    expect(wrapper.get<HTMLInputElement>('#site-url').element.value).toBe('https://so.252035.xyz')
    expect(wrapper.get<HTMLInputElement>('#site-name').element.value).toBe('盘搜 · 网盘与磁力')
    expect(wrapper.get<HTMLInputElement>('#site-timeout').element.value).toBe('30')
    expect(wrapper.findAll('form select, form input, form textarea').slice(0, 3).map(field => field.attributes('id'))).toEqual(['site-share-kind', 'site-url', 'pansou-source-scope'])
    await wrapper.get('#site-url').setValue('https://pansou.example.com')
    await wrapper.get('#site-channels').setValue('@example_channel')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(savedSite).toMatchObject({ kind: 'pansou_tg', base_url: 'https://pansou.example.com', cloud_config: { source_scope: 'all', channel_mode: 'defaults_plus_custom', type_policy: 'auto', channels: ['@example_channel'] } })
    await wrapper.get('article').findAll('button').find(button => button.text() === '编辑')!.trigger('click')
    expect(wrapper.get<HTMLInputElement>('#site-url').element.value).toBe('https://pansou.example.com')
    expect(wrapper.get<HTMLTextAreaElement>('#site-channels').element.value).toBe('@example_channel')
    await wrapper.get('form').findAll('button').find(button => button.text() === '关闭')!.trigger('click')

    await wrapper.get('header .btn-primary').trigger('click')
    await wrapper.findAll('.type-card').find(button => button.text().includes('PT 站点'))!.trigger('click')
    expect(wrapper.get<HTMLInputElement>('#site-url').element.value).toBe('https://www.pttime.org')
    await wrapper.get('#site-catalog').setValue('nexusphp')
    expect(wrapper.get<HTMLInputElement>('#site-url').element.value).toBe('')
    expect(errors).not.toHaveBeenCalled()
  } finally {
    wrapper.unmount()
  }
})

it('keeps a disabled plugin configuration in the disabled view with unavailable actions', async () => {
  request.mockImplementation(async (path: string) => path === siteCatalogPath ? { list: [] } : { list: [{ id: 9, name: '挂了.com', kind: 'plugin_resource', site_type: 'bt_resource', credential_kind: 'plugin', enabled: true, effective_enabled: false, unavailable_code: 'plugin_disabled', unavailable_reason: '插件已停用', capabilities: { search: false, download: false }, cloud_config: undefined, base_url: 'https://example.test', health: { status: 'healthy', username: '', error_code: '' }, rate_limit_per_minute: 12 }] })
  const wrapper = mount(SitesView, { global: { stubs: { RouterLink: { template: '<a><slot /></a>' } } } })
  try {
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(0)
    await wrapper.findAll('button').find(button => button.text() === '已停用（1）')!.trigger('click')
    expect(wrapper.get('article').text()).toContain('插件已停用')
    const search = wrapper.get('article').findAll('button').find(button => button.text() === '搜索')!
    expect(search.attributes('disabled')).toBeDefined()
    expect(search.attributes('title')).toBe('插件已停用')
    expect(wrapper.get('article').text()).toContain('前往插件连接管理')
  } finally { wrapper.unmount() }
})

it('edits a separate search budget and allows a recoverable plugin to enter search', async () => {
  const common = { enabled: true, capabilities: { search: true, download: true }, user_agent: '', priority: 100, timeout_seconds: 12, rate_limit_per_minute: 12, browser_emulation: false, revision: 1 }
  const jackett = { ...common, id: 7, kind: 'torznab', name: 'Jackett', site_type: 'bt', base_url: 'http://jackett:9117', credential_kind: 'api_key', api_key_configured: true, search_timeout_seconds: null, effective_search_timeout_seconds: 120, health: { status: 'online' } }
  const plugin = { ...common, id: 8, kind: 'plugin_resource', name: '挂了.com', site_type: 'bt_resource', base_url: 'https://site.example', credential_kind: 'plugin', health: { status: 'unavailable' } }
  request.mockImplementation(async (path: string, options?: RequestInit) => {
    if (options?.method === 'PATCH') return {}
    if (path === sitesPath) return { list: [jackett, plugin] }
    if (path === siteCatalogPath) return { list: [{ key: 'torznab', engine: 'torznab', site_type: 'bt', name: 'Torznab', credential_kind: 'api_key', capabilities: common.capabilities, base_urls: null }] }
    throw new Error(`Unexpected request: ${path}`)
  })
  const wrapper = mount(SitesView, { global: { stubs: { RouterLink: true } } })
  try {
    await flushPromises()
    const pluginSearch = wrapper.findAll('article')[1].findAll('button').find(button => button.text() === '搜索')!
    expect(pluginSearch.attributes('disabled')).toBeUndefined()
    await pluginSearch.trigger('click')
    expect(navigate).toHaveBeenCalledWith({ name: 'explore', query: { site_id: '8' } })
    await wrapper.findAll('article')[0].findAll('button').find(button => button.text() === '编辑')!.trigger('click')
    const field = wrapper.get<HTMLInputElement>('#site-search-timeout')
    expect(field.element.value).toBe('')
    expect(field.attributes('placeholder')).toContain('120')
    await field.setValue(150)
    await wrapper.get('form').trigger('submit'); await flushPromises()
    let patch = request.mock.calls.find(([, options]) => options?.method === 'PATCH')!
    expect(JSON.parse(String(patch[1].body))).toMatchObject({ timeout_seconds: 12, search_timeout_seconds: 150 })
    request.mockClear()
    await wrapper.findAll('article')[0].findAll('button').find(button => button.text() === '编辑')!.trigger('click')
    await wrapper.get('#site-search-timeout').setValue('')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    patch = request.mock.calls.find(([, options]) => options?.method === 'PATCH')!
    expect(JSON.parse(String(patch[1].body))).toMatchObject({ timeout_seconds: 12, search_timeout_seconds: 0 })
  } finally { wrapper.unmount() }
})
