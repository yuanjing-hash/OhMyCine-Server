// @vitest-environment happy-dom
import { expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SitesView from './views/SitesView.vue'
import { siteCatalogPath, sitesPath } from './sites'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))

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
    await wrapper.findAll('.type-card').find(button => button.text().includes('网盘分享站'))!.trigger('click')
    expect(errors).not.toHaveBeenCalled()
    expect(wrapper.get('#site-dialog-title').text()).toBe('添加网盘分享站')
    expect(wrapper.get<HTMLSelectElement>('#site-share-kind').element.value).toBe('pansou_tg')
    expect(wrapper.get<HTMLInputElement>('#site-url').element.value).toBe('https://so.252035.xyz')
    expect(wrapper.get<HTMLInputElement>('#site-name').element.value).toBe('盘搜 · 115 分享')
    expect(wrapper.get<HTMLInputElement>('#site-timeout').element.value).toBe('30')
    expect(wrapper.findAll('form select, form input, form textarea').slice(0, 3).map(field => field.attributes('id'))).toEqual(['site-share-kind', 'site-url', 'site-channels'])
    await wrapper.get('#site-url').setValue('https://pansou.example.com')
    await wrapper.get('#site-channels').setValue('@example_channel')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(savedSite).toMatchObject({ kind: 'pansou_tg', base_url: 'https://pansou.example.com', cloud_config: { provider: '115', channels: ['@example_channel'] } })
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
