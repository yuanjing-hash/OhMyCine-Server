// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import QuarkConnectionPanel from '@/components/QuarkConnectionPanel.vue'
import { APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import type { ConnectionSummary, StorageSummary } from '@/types/api'
import type { QuarkAuthSession, QuarkConnectionStatus, QuarkRootRecovery, QuarkRootVerification } from '@/quark'

const mocks = vi.hoisted(() => ({ api: vi.fn(), allowed: true, denied: new Set<string>() }))
vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: (permission: string) => mocks.allowed && !mocks.denied.has(permission) }) }))

const connection = { id: 1, name: '我的夸克', provider: 'quark', enabled: true, revision: 1 } as ConnectionSummary
function status(id = 1, ready = false): QuarkConnectionStatus {
  const authorization = { configured: ready, ready, state: ready ? 'ready' as const : 'not_authorized' as const }
  return { connection_id: id, playback: authorization, management: authorization, additional_management: { configured: false, ready: false, state: 'not_required' }, verified_roots: 0, capabilities: {} }
}
function session(state: QuarkAuthSession['state'] = 'waiting'): QuarkAuthSession {
  return { session_id: 'private-session', purpose: 'playback', state, qr_data_url: 'data:image/png;base64,YWJj', expires_at: new Date(Date.now() + 120_000).toISOString(), poll_after_seconds: 3, message: state === 'retryable' ? '网络暂不可用，请重新检查' : state === 'complete' ? '授权已保存' : '请扫码' }
}
function render() { return mount(QuarkConnectionPanel, { props: { connection }, global: { stubs: { DirectoryPickerDialog: true, Teleport: true } } }) }
function button(wrapper: VueWrapper, label: string) { return wrapper.findAll('button').find(value => value.text() === label)! }
function select(wrapper: VueWrapper, token = 'opaque-directory', path = '/媒体') { wrapper.getComponent({ name: 'DirectoryPickerDialog' }).vm.$emit('select', { path, token }) }
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
const unresolvedMessage = '夸克目录验证在“核对原验证文件分片”阶段未完成：提交结果尚未确认'
const recoveryInfo = { available: true, recovery_token: 'opaque-recovery-intent', message: '原验证结果仍未确认。可保留旧记录并明确开始一次独立新验证。' } satisfies QuarkRootRecovery
const verified = { verified: true, path: '/媒体', message: '目录验证已通过', cleanup_state: 'complete' } satisfies QuarkRootVerification
function recoveryScenario(info: QuarkRootRecovery = recoveryInfo, newAttempt: () => Promise<QuarkRootVerification> = async () => verified) {
  mocks.api.mockImplementation(async (path: string) => {
    if (path.endsWith('/status')) return status(1, true)
    if (path.endsWith('/verify-root/recovery')) return info
    if (path.endsWith('/verify-root/new-attempt')) return newAttempt()
    if (path.endsWith('/verify-root')) throw new APIError(503, 'connection_unavailable', unresolvedMessage)
    if (path === '/api/v1/storages') return { id: 7, name: '媒体', type: 'quark' }
    throw new Error('unexpected request')
  })
}
async function failOrdinaryVerification(wrapper: VueWrapper) { await flushPromises(); select(wrapper); await flushPromises(); await button(wrapper, '验证目录并添加数据源').trigger('click'); await flushPromises() }
beforeEach(() => { mocks.api.mockReset(); mocks.allowed = true; mocks.denied.clear(); vi.useFakeTimers() })
afterEach(() => { vi.useRealTimers() })

describe('Quark authorization panel', () => {
  it('stops on provider failure and resumes the existing session instead of starting another authorization', async () => {
    let attempts = 0
    mocks.api.mockImplementation(async (path: string) => {
      if (path.endsWith('/status')) return status(1, attempts > 1)
      if (path.endsWith('/poll')) return session(++attempts === 1 ? 'retryable' : 'complete')
      return session()
    })
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '登录授权')!.trigger('click'); await flushPromises()
    expect(wrapper.get('img').attributes('src')).toContain('data:image/png')
    await vi.advanceTimersByTimeAsync(3000); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('网络暂不可用')
    await vi.advanceTimersByTimeAsync(20_000); await flushPromises()
    expect(attempts).toBe(1)
    await wrapper.findAll('button').find(button => button.text() === '重新检查授权')!.trigger('click'); await flushPromises()
    expect(attempts).toBe(2)
    expect(mocks.api.mock.calls.filter(call => call[0].endsWith('/auth'))).toHaveLength(1)
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.emitted('changed')).toHaveLength(1)
    expect(wrapper.text()).toContain('已授权')
    wrapper.unmount()
  })

  it('discards and cancels a late authorization after switching to another connection', async () => {
    let finish!: (value: QuarkAuthSession) => void
    mocks.api.mockImplementation((path: string) => {
      if (path.endsWith('/status')) return Promise.resolve(status(path.includes('/2/') ? 2 : 1))
      if (path.endsWith('/auth')) return new Promise<QuarkAuthSession>(resolve => { finish = resolve })
      return Promise.resolve({})
    })
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button')[0]!.trigger('click')
    await wrapper.setProps({ connection: { ...connection, id: 2, name: '第二个账号' } }); await flushPromises()
    finish(session()); await flushPromises()
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).toContain('第二个账号')
    expect(mocks.api.mock.calls.some(call => call[0] === '/api/v1/connections/1/quark/auth/private-session/cancel')).toBe(true)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/poll'))).toBe(false)
    wrapper.unmount()
  })

  it('preserves the safe verification phase for connection_unavailable and does not create a storage', async () => {
    const diagnostic = '夸克目录验证在“定位管理授权下的目录”阶段未完成：夸克未接受请求或未返回可用结果（HTTP 400）（提供方错误码 23018）'
    mocks.api.mockImplementation(async (path: string) => {
      if (path.endsWith('/status')) return status(1, true)
      if (path.endsWith('/verify-root')) throw new APIError(503, 'connection_unavailable', diagnostic)
      return {}
    })
    const wrapper = render(); await flushPromises()
    wrapper.getComponent({ name: 'DirectoryPickerDialog' }).vm.$emit('select', { path: '/媒体', token: 'opaque-directory' }); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '验证目录并添加数据源')!.trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe(diagnostic)
    expect(mocks.api.mock.calls.some(call => call[0] === '/api/v1/storages')).toBe(false)
    expect(mocks.api.mock.calls.find(call => call[0].endsWith('/verify-root'))?.[1].body).toBe('{"provider_picker_token":"opaque-directory"}')
    wrapper.unmount()
  })

  it('shows authorization status without write controls to a read-only viewer', async () => {
    mocks.allowed = false
    mocks.api.mockResolvedValue(status())
    const wrapper = render(); await flushPromises()
    expect(wrapper.text()).toContain('未授权')
    expect(wrapper.findAll('button')).toHaveLength(0)
    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('adds storage with a valid proof and retains the nonblocking cleanup warning', async () => {
    mocks.api.mockImplementation(async (path: string) => {
      if (path.endsWith('/status')) return { ...status(1, true), root_cleanup_pending: 1 }
      if (path.endsWith('/verify-root')) return { verified: true, path: '/媒体', cleanup_state: 'pending', cleanup_message: '目录验证已通过；验证文件清理尚未确认' }
      if (path === '/api/v1/storages') return { id: 7, name: '媒体', type: 'quark' }
      return {}
    })
    const wrapper = render(); await flushPromises()
    wrapper.getComponent({ name: 'DirectoryPickerDialog' }).vm.$emit('select', { path: '/媒体', token: 'opaque-directory' }); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '验证目录并添加数据源')!.trigger('click'); await flushPromises()
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(wrapper.text()).toContain('不影响添加或使用数据源')
    expect(wrapper.get('[role="status"]').text()).toContain('验证文件清理尚未确认')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('retries original cleanup after storage creation without a directory token or verification upload', async () => {
    let pending = 1
    mocks.api.mockImplementation(async (path: string) => {
      if (path.endsWith('/status')) return { ...status(1, true), root_cleanup_pending: pending }
      if (path.endsWith('/cleanup')) { pending = 0; return { pending: 0, message: '原验证文件已清理' } }
      throw new Error('unexpected request')
    })
    const wrapper = mount(QuarkConnectionPanel, { props: { connection, storage: { id: 7, name: '媒体', enabled: true, root_display_path: '/媒体' } as StorageSummary }, global: { stubs: { DirectoryPickerDialog: true } } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '重新核对验证文件')!.trigger('click'); await flushPromises()
    expect(mocks.api.mock.calls.filter(call => call[0].endsWith('/cleanup'))).toHaveLength(1)
    expect(mocks.api.mock.calls.find(call => call[0].endsWith('/cleanup'))?.[1].body).toBe('{}')
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/verify-root') || call[0] === '/api/v1/storages')).toBe(false)
    expect(wrapper.text()).toContain('原验证文件已清理')
    expect(wrapper.findAll('button').some(button => button.text() === '重新核对验证文件')).toBe(false)
    wrapper.unmount()
  })

  it('discards a cleanup completion after changing connection', async () => {
    let finish!: (value: { pending: number; message: string }) => void
    mocks.api.mockImplementation((path: string) => {
      if (path.endsWith('/status')) return Promise.resolve({ ...status(path.includes('/2/') ? 2 : 1, true), root_cleanup_pending: path.includes('/2/') ? 0 : 1 })
      return new Promise(resolve => { finish = resolve })
    })
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '重新核对验证文件')!.trigger('click')
    await wrapper.setProps({ connection: { ...connection, id: 2, name: '第二个账号' } }); await flushPromises()
    finish({ pending: 0, message: '旧账号清理完成' }); await flushPromises()
    expect(wrapper.text()).not.toContain('旧账号清理完成')
    expect(wrapper.text()).toContain('第二个账号')
    wrapper.unmount()
  })
})

describe('Quark independent root verification recovery', () => {
  it('offers recovery only from current server eligibility and never starts it automatically', async () => {
    recoveryScenario()
    const wrapper = render(); await flushPromises()
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    await failOrdinaryVerification(wrapper)
    expect(wrapper.get('[role="alert"]').text()).toBe(unresolvedMessage)
    expect(wrapper.text()).toContain(recoveryInfo.message)
    expect(button(wrapper, '开始一次新验证')).toBeDefined()
    expect(mocks.api.mock.calls.find(call => call[0].endsWith('/verify-root/recovery'))?.[1].body).toBe('{"provider_picker_token":"opaque-directory"}')
    await button(wrapper, '验证目录并添加数据源').trigger('click'); await flushPromises()
    expect(mocks.api.mock.calls.filter(call => call[0].endsWith('/verify-root'))).toHaveLength(2)
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/new-attempt') || call[0] === '/api/v1/storages')).toBe(false)
    expect(wrapper.html()).not.toContain(recoveryInfo.recovery_token)
    wrapper.unmount()
  })

  it.each([
    { available: false, message: '当前验证仍可核对，暂不提供新验证' },
    { available: true, message: '缺少恢复凭证' },
  ] satisfies QuarkRootRecovery[])('does not invent a recovery action for ineligible or incomplete information: $message', async info => {
    recoveryScenario(info)
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    expect(wrapper.text()).toContain(info.message)
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/new-attempt'))).toBe(false)
    wrapper.unmount()
  })

  it('preserves the original error when recovery information cannot be read', async () => {
    recoveryScenario()
    const implementation = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path: string, options: RequestInit) => path.endsWith('/recovery') ? Promise.reject(new Error('recovery unavailable')) : implementation(path, options))
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    expect(wrapper.get('[role="alert"]').text()).toBe(unresolvedMessage)
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    wrapper.unmount()
  })

  it('requires deliberate confirmation, keeps the old operation warning, and submits one new attempt on double click', async () => {
    const pending = deferred<QuarkRootVerification>()
    recoveryScenario(recoveryInfo, () => pending.promise)
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('Server 会保留原记录')
    expect(wrapper.get('[role="dialog"]').text()).toContain('目录：/媒体')
    expect(wrapper.get('[role="dialog"]').text()).toContain('另一个极小验证文件')
    expect(wrapper.get('[role="dialog"]').text()).toContain('由播放授权独立读回')
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/new-attempt'))).toBe(false)
    await button(wrapper, '取消').trigger('click'); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    await button(wrapper, '开始一次新验证').trigger('click'); await flushPromises()
    const confirm = button(wrapper, '确认并开始新验证')
    await Promise.all([confirm.trigger('click'), confirm.trigger('click')]); await flushPromises()
    const calls = mocks.api.mock.calls.filter(call => call[0].endsWith('/new-attempt'))
    expect(calls).toHaveLength(1)
    expect(calls[0]?.[1].body).toBe('{"provider_picker_token":"opaque-directory","recovery_token":"opaque-recovery-intent"}')
    expect(button(wrapper, '继续核对本次新验证').attributes('disabled')).toBeDefined()
    pending.resolve({ ...verified, cleanup_state: 'pending', cleanup_message: '验证文件清理尚未确认' }); await flushPromises()
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(mocks.api.mock.calls.filter(call => call[0] === '/api/v1/storages')).toHaveLength(1)
    expect(wrapper.text()).toContain('验证文件清理尚未确认')
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    expect(wrapper.html()).not.toContain('opaque-recovery-intent')
    wrapper.unmount()
  })

  it('keeps the same submitted intent after an ambiguous response instead of fetching or creating another one', async () => {
    let attempts = 0
    recoveryScenario(recoveryInfo, async () => { if (++attempts === 1) throw new Error('本次结果尚未确认'); return verified })
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click')
    await button(wrapper, '确认并开始新验证').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('本次结果尚未确认')
    expect(wrapper.text()).toContain('继续核对同一次验证')
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    await button(wrapper, '继续核对本次新验证').trigger('click'); await flushPromises()
    const calls = mocks.api.mock.calls.filter(call => call[0].endsWith('/new-attempt'))
    expect(calls).toHaveLength(2)
    expect(calls[0]?.[1].body).toBe(calls[1]?.[1].body)
    expect(mocks.api.mock.calls.filter(call => call[0].endsWith('/recovery'))).toHaveLength(1)
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/auth') || call[0].includes('/directories'))).toBe(false)
    wrapper.unmount()
  })

  it('invalidates a rejected stale intent without automatically requesting a replacement', async () => {
    recoveryScenario(recoveryInfo, async () => { throw new APIError(409, 'CONFLICT', '授权或目录已变化，请重新核对') })
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click')
    await button(wrapper, '确认并开始新验证').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('授权或目录已变化，请重新核对')
    expect(button(wrapper, '继续核对本次新验证')).toBeUndefined()
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    expect(button(wrapper, '验证目录并添加数据源')).toBeDefined()
    expect(mocks.api.mock.calls.filter(call => call[0].endsWith('/recovery'))).toHaveLength(1)
    wrapper.unmount()
  })

  it('discards late eligibility for a replaced selection even when its displayed path is the same', async () => {
    const pending = deferred<QuarkRootRecovery>()
    recoveryScenario()
    const implementation = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path: string, options: RequestInit) => path.endsWith('/recovery') ? pending.promise : implementation(path, options))
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    select(wrapper, 'new-selection-same-path'); await flushPromises()
    pending.resolve(recoveryInfo); await flushPromises()
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    expect(wrapper.text()).not.toContain(recoveryInfo.message)
    expect(button(wrapper, '验证目录并添加数据源').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it.each(['connection', 'revision', 'selection', 'unmount'])('fences late new-attempt completion after %s changes', async change => {
    const pending = deferred<QuarkRootVerification>()
    recoveryScenario(recoveryInfo, () => pending.promise)
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click')
    await button(wrapper, '确认并开始新验证').trigger('click'); await flushPromises()
    const options = mocks.api.mock.calls.find(call => call[0].endsWith('/new-attempt'))?.[1] as RequestInit
    if (change === 'connection') await wrapper.setProps({ connection: { ...connection, id: 2, name: '第二个账号' } })
    else if (change === 'revision') await wrapper.setProps({ connection: { ...connection, revision: 2 } })
    else if (change === 'selection') { select(wrapper, 'new-root', '/另一个目录'); await flushPromises() }
    else wrapper.unmount()
    pending.resolve(verified); await flushPromises()
    expect(options.signal?.aborted).toBe(true)
    expect(mocks.api.mock.calls.some(call => call[0] === '/api/v1/storages')).toBe(false)
    expect(wrapper.emitted('created')).toBeUndefined()
    if (change !== 'unmount') { expect(wrapper.text()).not.toContain('夸克数据源已添加'); expect(button(wrapper, '继续核对本次新验证')).toBeUndefined(); wrapper.unmount() }
  })

  it('closes a prepared confirmation when its connection revision changes', async () => {
    recoveryScenario()
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click'); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    await wrapper.setProps({ connection: { ...connection, revision: 2 } }); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/new-attempt'))).toBe(false)
    wrapper.unmount()
  })

  it('contains keyboard focus, supports Escape, and restores focus without submitting an attempt', async () => {
    recoveryScenario()
    const wrapper = mount(QuarkConnectionPanel, { attachTo: document.body, props: { connection }, global: { stubs: { DirectoryPickerDialog: true, Teleport: true } } })
    await failOrdinaryVerification(wrapper)
    const opener = button(wrapper, '开始一次新验证')
    ;(opener.element as HTMLButtonElement).focus()
    await opener.trigger('click'); await flushPromises()
    const cancel = button(wrapper, '取消'), confirm = button(wrapper, '确认并开始新验证')
    expect(document.activeElement).toBe(cancel.element)
    await cancel.trigger('keydown', { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(confirm.element)
    await confirm.trigger('keydown', { key: 'Tab' })
    expect(document.activeElement).toBe(cancel.element)
    await wrapper.get('[role="dialog"]').trigger('keydown', { key: 'Escape' }); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(document.activeElement).toBe(opener.element)
    expect(mocks.api.mock.calls.some(call => call[0].endsWith('/new-attempt'))).toBe(false)
    wrapper.unmount()
  })

  it('does not create storage from an incomplete new verification result', async () => {
    recoveryScenario(recoveryInfo, async () => ({ ...verified, verified: false }))
    const wrapper = render(); await failOrdinaryVerification(wrapper)
    await button(wrapper, '开始一次新验证').trigger('click')
    await button(wrapper, '确认并开始新验证').trigger('click'); await flushPromises()
    expect(mocks.api.mock.calls.some(call => call[0] === '/api/v1/storages')).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('目录验证尚未通过')
    expect(button(wrapper, '继续核对本次新验证')).toBeDefined()
    wrapper.unmount()
  })

  it('requires connection-update permission for verification even when directory selection is permitted', async () => {
    mocks.denied.add(Permissions.ConnectionsUpdate)
    recoveryScenario()
    const wrapper = render(); await flushPromises()
    expect(button(wrapper, '选择云端目录')).toBeDefined()
    expect(button(wrapper, '验证目录并添加数据源')).toBeUndefined()
    expect(button(wrapper, '开始一次新验证')).toBeUndefined()
    expect(mocks.api.mock.calls.some(call => call[0].includes('/verify-root'))).toBe(false)
    wrapper.unmount()
  })
})
