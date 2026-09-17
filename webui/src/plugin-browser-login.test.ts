// @vitest-environment happy-dom
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import PluginBrowserLogin from '@/components/PluginBrowserLogin.vue'
import ServerBrowserPanel from '@/components/ServerBrowserPanel.vue'
import { api, APIError } from '@/api/client'
vi.mock('@/api/client', () => ({ api: vi.fn(), APIError: class extends Error {
  constructor(public status: number, public errorCode: string, message: string) { super(message) }
} }))
const props = { pluginId: 'plugin', connectionId: 'connection', origin: 'https://mirror.example' }
beforeEach(() => { vi.mocked(api).mockReset() })
function browserMock() {
  vi.mocked(api).mockImplementation(async path => {
    if (path.endsWith('/status')) return { state: 'ready', session_id: 'opaque' }
    if (path.endsWith('/snapshot')) return { mime_type: 'image/png', image_base64: 'YWJj', width: 800, height: 600 }
    return {}
  })
}
describe('unified browser login', () => {
  it.each([
    ['resource_browser_login_expired', '网页仍可使用'],
    ['resource_entry_unavailable', '站点响应不可用或无法识别'],
    ['resource_browser_request_failed', '网络或不支持的跳转'],
    ['resource_browser_request_timeout', '请求超时'],
  ])('retains the page for safe confirmation error %s', async (code, message) => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    vi.mocked(api).mockRejectedValueOnce(new APIError(500, code, 'private-secret'))
    await wrapper.findAll('button').find(b => b.text() === '验证完成，继续登录')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain(message)
    expect(wrapper.text()).not.toContain('private-secret')
    expect(wrapper.find('img').exists()).toBe(true)
    expect(wrapper.emitted('result')).toBeUndefined()
    wrapper.unmount()
  })
  it('separates screenshot update from page reload and never resubmits credentials', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    vi.mocked(api).mockClear()
    await wrapper.findAll('button').find(b => b.text() === '更新画面')!.trigger('click'); await flushPromises()
    expect(vi.mocked(api).mock.calls.map(([path]) => path.split('/').at(-1))).toEqual(['snapshot'])
    vi.mocked(api).mockClear()
    await wrapper.findAll('button').find(b => b.text() === '重新加载网页')!.trigger('click'); await flushPromises()
    expect(vi.mocked(api).mock.calls.map(([path]) => path.split('/').at(-1))).toEqual(['reload', 'snapshot'])
    expect(vi.mocked(api).mock.calls[0]![1]).toEqual(expect.objectContaining({ method: 'POST', body: '{"session_id":"opaque"}' }))
    expect(wrapper.emitted('result')).toBeUndefined()
    wrapper.unmount()
  })
  it('shows safe resource diagnostics without declaring login expired or exposing details', async () => {
    browserMock()
    const base = vi.mocked(api).getMockImplementation()!
    vi.mocked(api).mockImplementation(async (path, ...args) => path.endsWith('/snapshot') ? { mime_type: 'image/png', image_base64: 'YWJj', width: 800, height: 600, network_error_code: 'resource_network_failed', blocked_resource_count: 3, url: 'https://private.invalid/?cookie=secret' } : base(path, ...args))
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    expect(wrapper.text()).toContain('部分公网资源连接失败')
    expect(wrapper.text()).toContain('本次未加载 3 项')
    expect(wrapper.text()).toContain('不代表账号密码错误或登录失效')
    expect(wrapper.text()).not.toContain('private.invalid')
    expect(wrapper.find('img').exists()).toBe(true)
    vi.mocked(api).mockResolvedValueOnce({ mime_type: 'image/png', image_base64: 'YWJj', width: 800, height: 600, network_error_code: '', blocked_resource_count: 0 })
    await wrapper.findAll('button').find(b => b.text() === '更新画面')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).not.toContain('部分公网资源连接失败')
    wrapper.unmount()
  })
  it('ignores unknown resource error text and out-of-range counts', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    vi.mocked(api).mockResolvedValueOnce({ mime_type: 'image/png', image_base64: 'YWJj', width: 800, height: 600, network_error_code: 'https://private.invalid', blocked_resource_count: 9000000 })
    await wrapper.findAll('button').find(b => b.text() === '更新画面')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).not.toContain('private.invalid')
    expect(wrapper.text()).not.toContain('9000000')
    wrapper.unmount()
  })
  it('does not follow a late reload with a snapshot of a new connection', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    let finish!: (value: unknown) => void
    const base = vi.mocked(api).getMockImplementation()!
    vi.mocked(api).mockImplementation(async (path, ...args) => path.endsWith('/reload') ? new Promise(resolve => { finish = resolve }) : base(path, ...args))
    await wrapper.findAll('button').find(b => b.text() === '重新加载网页')!.trigger('click'); await flushPromises()
    const reloadOptions = vi.mocked(api).mock.calls.find(([path]) => path.endsWith('/reload'))![1]!
    await wrapper.setProps({ connectionId: 'new' }); await flushPromises()
    expect(reloadOptions.signal?.aborted).toBe(true)
    const calls = vi.mocked(api).mock.calls.length
    finish({}); await flushPromises()
    expect(vi.mocked(api).mock.calls).toHaveLength(calls)
    expect(wrapper.emitted('result')).toBeUndefined()
    wrapper.unmount()
  })
  it('explains POST reload denial and preserves the verification session', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    vi.mocked(api).mockRejectedValueOnce(new APIError(400, 'resource_browser_reload_post_denied', 'secret'))
    await wrapper.findAll('button').find(b => b.text() === '重新加载网页')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('为避免重复提交')
    expect(wrapper.find('img').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('secret')
    expect(vi.mocked(api).mock.calls.at(-1)![0]).toContain('/reload')
    wrapper.unmount()
  })
  it('does not report disabled when the companion network state is unavailable', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'unavailable', installed: false })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: false } }); await flushPromises()
    expect(wrapper.text()).toContain('TUN / Fake-IP 兼容：状态未知')
    wrapper.unmount()
  })
  it('explains Fake-IP opt-in without blaming credentials or mirror permissions', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'launch_failed', installed: true, runtime_error: 'tun_fake_ip_requires_opt_in', tun_fake_ip_enabled: false })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: true } }); await flushPromises()
    expect(wrapper.text()).toContain('无需关闭 TUN')
    expect(wrapper.text()).toContain('OMC_CLOAK_TUN_FAKE_IP=true')
    expect(wrapper.text()).toContain('重启 Server')
    expect(wrapper.text()).not.toContain('镜像权限')
    expect(wrapper.text()).not.toContain('接受许可并安装')
    wrapper.unmount()
  })
  it('shows effective deployment TUN state', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'ready', installed: true, tun_fake_ip_enabled: true })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: false } }); await flushPromises()
    expect(wrapper.text()).toContain('TUN / Fake-IP 兼容：已启用')
    wrapper.unmount()
  })
  it('closes the old connection session when identity changes', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    await wrapper.setProps({ connectionId: 'new-connection' }); await flushPromises()
    expect(api).toHaveBeenCalledWith('/api/v1/plugins/plugin/connections/connection/resource/browser/close', expect.objectContaining({ body: '{"session_id":"opaque"}' }))
    wrapper.unmount()
  })
  it('closes a session returned by late status after unmount', async () => {
    let resolveStatus!: (value: unknown) => void
    vi.mocked(api).mockImplementation(async path => path.endsWith('/status') ? new Promise(resolve => { resolveStatus = resolve }) : {})
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    wrapper.unmount()
    resolveStatus({ state: 'ready', session_id: 'late' }); await flushPromises()
    expect(api).toHaveBeenCalledWith('/api/v1/plugins/plugin/connections/connection/resource/browser/close', expect.objectContaining({ body: '{"session_id":"late"}' }))
  })
  it('aborts on unmount and ignores a late screenshot', async () => {
    let resolveImage!: (value: unknown) => void
    vi.mocked(api).mockImplementation(async path => path.endsWith('/status')
      ? { state: 'ready', session_id: 'opaque' }
      : path.endsWith('/snapshot') ? new Promise(resolve => { resolveImage = resolve }) : {})
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    const options = vi.mocked(api).mock.calls.find(([path]) => path.endsWith('/snapshot'))![1]!
    wrapper.unmount()
    expect(options.signal?.aborted).toBe(true)
    resolveImage({ mime_type: 'image/png', image_base64: 'YWJj', width: 800, height: 600 }); await flushPromises()
    expect(wrapper.emitted('result')).toBeUndefined()
  })
  it('resumes verification without installing or starting another browser', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    expect(wrapper.find('img').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('接受许可')
    expect(wrapper.text()).not.toContain('打开浏览器登录')
    expect(api).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
  it('clears transient input and returns captcha from continuation', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    await wrapper.get('input[type=password]').setValue('synthetic')
    await wrapper.findAll('button').find(b => b.text() === '输入到当前焦点')!.trigger('click'); await flushPromises()
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('')
    vi.mocked(api).mockResolvedValueOnce({ state: 'captcha_required' })
    await wrapper.findAll('button').find(b => b.text() === '验证完成，继续登录')!.trigger('click'); await flushPromises()
    expect(wrapper.emitted('result')?.[0]).toEqual([{ state: 'captcha_required' }])
    wrapper.unmount()
  })
  it('does not declare saved authentication expired when interaction expires', async () => {
    browserMock()
    const wrapper = mount(PluginBrowserLogin, { props }); await flushPromises()
    vi.mocked(api).mockRejectedValueOnce(new APIError(400, 'resource_browser_session_expired', 'private raw error'))
    await wrapper.findAll('button').find(b => b.text() === '更新画面')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('已保存的登录状态不因此删除')
    expect(wrapper.text()).not.toContain('private raw error')
    expect(wrapper.find('img').exists()).toBe(false)
    wrapper.unmount()
  })
  it('global install requires explicit acceptance and permission', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'not_installed', installed: false })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: true } }); await flushPromises()
    const install = wrapper.findAll('button').find(b => b.text() === '接受许可并安装')!
    expect(install.attributes('disabled')).toBeDefined()
    await wrapper.get('input[type=checkbox]').setValue(true)
    await install.trigger('click'); await flushPromises()
    expect(api).toHaveBeenLastCalledWith('/api/v1/settings/browser/install', expect.objectContaining({ body: '{"license_accepted":true}' }))
    await wrapper.setProps({ canInstall: false })
    expect(wrapper.find('input[type=checkbox]').exists()).toBe(false)
    wrapper.unmount()
  })
  it('does not show ready alongside runtime failure or expose raw details', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'ready', installed: true, runtime_error: 'private-path' })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: true } }); await flushPromises()
    expect(wrapper.text()).toContain('最近启动失败')
    expect(wrapper.text()).not.toContain('实际启动结果以登录时检查为准')
    expect(wrapper.text()).not.toContain('private-path')
    wrapper.unmount()
  })
  it('distinguishes navigation timeout from missing dependencies', async () => {
    vi.mocked(api).mockResolvedValue({ state: 'ready', installed: true, runtime_error: 'network_timeout' })
    const wrapper = mount(ServerBrowserPanel, { props: { canInstall: true } }); await flushPromises()
    expect(wrapper.text()).toContain('站点连接超时')
    expect(wrapper.text()).not.toContain('运行依赖')
    wrapper.unmount()
  })
})
