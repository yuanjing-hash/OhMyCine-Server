// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import QuarkConnectionPanel from '@/components/QuarkConnectionPanel.vue'
import type { ConnectionSummary } from '@/types/api'
import type { QuarkAuthSession, QuarkConnectionStatus } from '@/quark'

const mocks = vi.hoisted(() => ({ api: vi.fn(), allowed: true }))
vi.mock('@/api/client', () => ({ api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => mocks.allowed }) }))

const connection = { id: 1, name: '我的夸克', provider: 'quark', enabled: true, revision: 1 } as ConnectionSummary
function status(id = 1, ready = false): QuarkConnectionStatus {
  const authorization = { configured: ready, ready, state: ready ? 'ready' as const : 'not_authorized' as const }
  return { connection_id: id, playback: authorization, management: authorization, additional_management: { configured: false, ready: false, state: 'not_required' }, verified_roots: 0, capabilities: {} }
}
function session(state: QuarkAuthSession['state'] = 'waiting'): QuarkAuthSession {
  return { session_id: 'private-session', purpose: 'playback', state, qr_data_url: 'data:image/png;base64,YWJj', expires_at: new Date(Date.now() + 120_000).toISOString(), poll_after_seconds: 3, message: state === 'retryable' ? '网络暂不可用，请重新检查' : state === 'complete' ? '授权已保存' : '请扫码' }
}
function render() { return mount(QuarkConnectionPanel, { props: { connection }, global: { stubs: { DirectoryPickerDialog: true } } }) }
beforeEach(() => { mocks.api.mockReset(); mocks.allowed = true; vi.useFakeTimers() })
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

  it('does not create a storage when shared-root verification fails', async () => {
    mocks.api.mockImplementation(async (path: string) => {
      if (path.endsWith('/status')) return status(1, true)
      if (path.endsWith('/verify-root')) throw new Error('所选目录不属于共同范围')
      return {}
    })
    const wrapper = render(); await flushPromises()
    wrapper.getComponent({ name: 'DirectoryPickerDialog' }).vm.$emit('select', { path: '/媒体', token: 'opaque-directory' }); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '验证目录并添加数据源')!.trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('共同范围')
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
})
