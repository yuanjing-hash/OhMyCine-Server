// @vitest-environment happy-dom

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import type { RoleSummary, UserSummary } from '@/types/api'
import UsersView from '@/views/UsersView.vue'

const { apiMock, clearCSRFMock } = vi.hoisted(() => ({ apiMock: vi.fn(), clearCSRFMock: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: apiMock, clearCSRFToken: clearCSRFMock }))

const roles: RoleSummary[] = [
  { id: 4, code: 'normal_user', name: '正常用户', kind: 'system', protected: true, active: true, description: '', permissions: [Permissions.MediaLibrariesRead], user_count: 1 },
  { id: 5, code: 'user', name: '正常用户', kind: 'custom', protected: false, active: true, description: '', permissions: [], user_count: 0 },
]
function user(id: number, isOwner = false): UserSummary {
  return { id, username: `member${id}`, display_name: `成员 ${id}`, status: 'active', is_owner: isOwner, roles: [roles[0]!], authorization_rules: [], last_login_at: null, created_at: '2026-10-14T00:00:00Z' }
}
function deferred() {
  let resolve!: (value: unknown) => void
  let reject!: (reason: Error) => void
  const promise = new Promise((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}

describe('user password save', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => { wrapper?.unmount(); wrapper = null; apiMock.mockReset(); clearCSRFMock.mockReset() })

  async function open(targets = [user(2), user(3), user(1, true)]) {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)
    auth.user = { id: 1, username: 'owner', display_name: 'Owner', status: 'active', is_owner: true, roles: ['administrator'], permissions: [Permissions.UsersRead, Permissions.UsersUpdate, Permissions.RolesRead, Permissions.RolesAssign] }
    apiMock.mockImplementation((path: string, options?: RequestInit) => {
      if (options?.method === 'PATCH') { targets[0]!.display_name = JSON.parse(String(options.body)).display_name; return Promise.resolve({}) }
      if (path.endsWith('/reset-password')) return Promise.resolve({ reset: true })
      if (path === '/api/v1/users') return Promise.resolve({ list: targets.map(item => ({ ...item })), total: targets.length })
      if (path === '/api/v1/roles') return Promise.resolve({ list: roles, total: roles.length })
      if (path === '/api/v1/permissions') return Promise.resolve([])
      throw new Error(`unexpected path ${path}`)
    })
    wrapper = mount(UsersView, { global: { plugins: [pinia], stubs: { UserResourceAccessPanel: true } } })
    await flushPromises()
    return auth
  }

  async function draft(newPassword = 'new-test-password', actorPassword = 'actor-test-password') {
    await wrapper!.get('#user-new-password').setValue(newPassword)
    await wrapper!.get('#user-current-password').setValue(actorPassword)
  }
  function passwordForm() { return wrapper!.get('form[aria-labelledby="user-password-title"]') }
  function select(id: number) { return wrapper!.findAll('button.semantic-list-item').find(button => button.text().includes(`@member${id}`))!.trigger('click') }

  it('keeps the password draft after profile reload and saves it only with the visible password action', async () => {
    await open()
    const password = passwordForm()
    const permissions = wrapper!.get('section[aria-labelledby="user-permissions-title"]')
    expect(password.element.compareDocumentPosition(permissions.element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(password.text()).toContain('保存新密码')
    expect(wrapper!.findAll('option').map(option => option.text())).toEqual(['正常用户 · 系统模板 · normal_user', '正常用户 · 自定义模板 · user'])
    await draft()
    await wrapper!.get('#user-display-name').setValue('更新后的名称')
    await wrapper!.get('#user-display-name').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    await flushPromises()
    const profile = apiMock.mock.calls.find(([, options]) => options?.method === 'PATCH')!
    expect(profile[0]).toBe('/api/v1/users/2')
    expect(JSON.parse(profile[1].body)).toEqual({ display_name: '更新后的名称' })
    expect(apiMock.mock.calls.some(([path]) => path.endsWith('/reset-password'))).toBe(false)
    expect(wrapper!.get<HTMLInputElement>('#user-new-password').element.value).toBe('new-test-password')
    expect(wrapper!.get<HTMLInputElement>('#user-current-password').element.value).toBe('actor-test-password')
    expect(wrapper!.text()).toContain('新密码尚未保存')
    await passwordForm().trigger('submit')
    await flushPromises()
    const reset = apiMock.mock.calls.find(([path]) => path.endsWith('/reset-password'))!
    expect(reset[0]).toBe('/api/v1/users/2/reset-password')
    expect(reset[1].method).toBe('POST')
    expect(JSON.parse(reset[1].body)).toEqual({ password: 'new-test-password', current_password: 'actor-test-password' })
    expect(wrapper!.get<HTMLInputElement>('#user-new-password').element.value).toBe('')
    expect(wrapper!.get<HTMLInputElement>('#user-current-password').element.value).toBe('')
    expect(wrapper!.text()).toContain('新密码已保存，目标用户现有会话已撤销')
    expect(wrapper!.text()).not.toContain('新密码尚未保存')
  })

  it('retains a failed reauthentication draft and keeps update and Owner guards on the password action', async () => {
    const auth = await open()
    await wrapper!.get('#user-display-name').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    await flushPromises()
    const original = apiMock.getMockImplementation()!
    apiMock.mockImplementation((path, options) => path.endsWith('/reset-password') ? Promise.reject(new APIError(401, 'INVALID_CREDENTIALS', '当前操作者密码错误')) : original(path, options))
    await draft()
    await passwordForm().trigger('submit')
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toBe('当前操作者密码错误')
    expect(wrapper!.text()).not.toContain('用户资料已保存')
    expect(wrapper!.text()).not.toContain('新密码已保存')
    expect(wrapper!.get<HTMLInputElement>('#user-new-password').element.value).toBe('new-test-password')
    expect(auth.user?.id).toBe(1)
    await select(1)
    auth.user!.id = 9
    await flushPromises()
    expect(wrapper!.find('form[aria-labelledby="user-password-title"]').exists()).toBe(false)
    await select(2)
    auth.user!.permissions = [Permissions.UsersRead]
    await flushPromises()
    expect(wrapper!.find('form[aria-labelledby="user-password-title"]').exists()).toBe(false)
  })

  it.each(['success', 'failure'])('isolates a late %s from a switched-away-and-back password draft', async outcome => {
    await open()
    const pending = deferred()
    const original = apiMock.getMockImplementation()!
    apiMock.mockImplementation((path, options) => path.endsWith('/reset-password') ? pending.promise : original(path, options))
    await draft()
    await passwordForm().get('button[aria-label="显示当前输入内容"]').trigger('click')
    expect(wrapper!.get('#user-new-password').attributes('type')).toBe('text')
    await passwordForm().trigger('submit')
    await select(3)
    expect(wrapper!.get<HTMLInputElement>('#user-new-password').element.value).toBe('')
    expect(wrapper!.get('#user-new-password').attributes('type')).toBe('password')
    await select(2)
    await draft('later-test-password', 'later-actor-password')
    if (outcome === 'success') pending.resolve({ reset: true })
    else pending.reject(new Error('旧目标请求失败'))
    await flushPromises()
    expect(wrapper!.get<HTMLInputElement>('#user-new-password').element.value).toBe('later-test-password')
    expect(wrapper!.get<HTMLInputElement>('#user-current-password').element.value).toBe('later-actor-password')
    expect(wrapper!.text()).not.toContain('旧目标请求失败')
    expect(wrapper!.text()).not.toContain('新密码已保存')
    expect(apiMock.mock.calls.filter(([path]) => path.endsWith('/reset-password')).map(([path]) => path)).toEqual(['/api/v1/users/2/reset-password'])
  })

  it.each(['unchanged', 'refreshed', 'expired'])('clears %s local auth after saving its own password without querying the revoked session', async state => {
    const auth = await open([user(1, true)])
    const pending = deferred()
    const original = apiMock.getMockImplementation()!
    apiMock.mockImplementation((path, options) => path.endsWith('/reset-password') ? pending.promise : original(path, options))
    await draft()
    const callsBefore = apiMock.mock.calls.length
    await passwordForm().trigger('submit')
    if (state === 'refreshed') auth.user = { ...auth.user! }
    else if (state === 'expired') auth.user = null
    pending.resolve({ reset: true })
    await flushPromises()
    expect(apiMock.mock.calls.slice(callsBefore).map(([path]) => path)).toEqual(['/api/v1/users/1/reset-password'])
    expect(auth.user).toBeNull()
    expect(clearCSRFMock).toHaveBeenCalledOnce()
    expect(wrapper!.get('[role="status"]').text()).toContain('新密码已保存')
    expect(wrapper!.get('[role="status"]').text()).toContain('请使用新密码重新登录')
    expect(wrapper!.get('a').attributes('href')).toBe('/login')
    expect(wrapper!.find('#user-new-password').exists()).toBe(false)
    expect(wrapper!.find('.semantic-list-item').exists()).toBe(false)
  })
})
