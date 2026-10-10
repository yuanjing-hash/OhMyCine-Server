// @vitest-environment happy-dom

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { APIError } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import UserResourceAccessPanel from '@/components/UserResourceAccessPanel.vue'
import { authorizationResourceOptions, copyResourceAccess, defaultResourcePolicies, resourceAccessScopes, resourceDraftAllows, resourceSelectionLocked } from '@/resource-access'
import { useAuthStore } from '@/stores/auth'
import type { ResourceAccessOptions, ResourceAccessPolicy, ResourceAccessScope, UserResourceAccess, UserSummary } from '@/types/api'
import UsersView from '@/views/UsersView.vue'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', async importOriginal => ({ ...await importOriginal<typeof import('@/api/client')>(), api: apiMock }))

function access(revision = 1, overrides: Partial<Record<ResourceAccessScope, Pick<ResourceAccessPolicy, 'mode' | 'resource_ids'>>> = {}): UserResourceAccess {
  return { revision, policies: defaultResourcePolicies().map(policy => ({ ...policy, ...overrides[policy.scope] })) }
}

function choices(revision = 1, downloaderName = '远程 qBittorrent'): ResourceAccessOptions {
  return {
    revision,
    scopes: resourceAccessScopes.map(item => ({
      scope: item.scope, resource_type: item.resourceType,
      options: [{ id: `${item.resourceType}-1`, name: item.resourceType === 'downloader' ? downloaderName : item.resourceType === 'site' ? '盘搜分享站' : '动画库', type: item.resourceType === 'downloader' ? 'qbittorrent' : item.resourceType === 'site' ? 'cloud_share' : 'local', status: 'enabled', deleted: false, effective_allowed: item.scope !== 'library_ingest', can_grant: true, denial_reason: item.scope === 'library_ingest' ? '尚未授予入库功能权限' : '' }],
    })),
  }
}

function card(wrapper: VueWrapper, scope: ResourceAccessScope) {
  return wrapper.get(`section[aria-labelledby="policy-title-${scope}"]`)
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { resolve, promise }
}

describe('user resource permissions', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => { wrapper?.unmount(); wrapper = null; apiMock.mockReset() })

  it('edits named scopes and submits all four policies while read access bounds the ingest draft', async () => {
    let saved: UserResourceAccess | null = null
    apiMock.mockImplementation((path: string, options?: RequestInit) => {
      if (options?.method === 'PUT') {
        saved = { ...JSON.parse(String(options.body)), revision: 2 } as UserResourceAccess
        return Promise.resolve(saved)
      }
      return Promise.resolve(path.endsWith('/options') ? choices(saved ? 2 : 1) : access())
    })
    wrapper = mount(UserResourceAccessPanel, { props: { userId: 2, editable: true } })
    await flushPromises()
    expect(wrapper.findAll('select').map(item => item.element.value)).toEqual(['all', 'all', 'all', 'all'])
    expect(wrapper.text()).toContain('远程 qBittorrent')
    expect(wrapper.text()).toContain('尚未授予入库功能权限')

    await wrapper.get('#policy-mode-downloader_use').setValue('allowlist')
    expect(card(wrapper, 'downloader_use').text()).toContain('空白名单会禁止此类全部资源')
    await card(wrapper, 'downloader_use').get('input[type="checkbox"]').setValue(true)
    await wrapper.get('#policy-mode-library_read').setValue('allowlist')
    await wrapper.get('#policy-mode-library_ingest').setValue('allowlist')
    await card(wrapper, 'library_ingest').get('input[type="checkbox"]').setValue(true)
    expect(card(wrapper, 'library_ingest').text()).toContain('名单范围：0 项')
    expect(card(wrapper, 'library_ingest').text()).toContain('还需在“媒体库访问”中允许此库')
    await card(wrapper, 'library_read').get('input[type="checkbox"]').setValue(true)
    expect(card(wrapper, 'library_ingest').text()).toContain('名单范围：1 项')
    expect(card(wrapper, 'library_ingest').text()).toContain('当前有效：禁止')
    expect(card(wrapper, 'library_ingest').text()).toContain('待保存名单：允许')

    await wrapper.get('form').trigger('submit')
    await flushPromises()
    const write = apiMock.mock.calls.find(([, options]) => options?.method === 'PUT')!
    expect(write[0]).toBe('/api/v1/users/2/resource-access')
    expect(JSON.parse(write[1].body)).toEqual(access(1, {
      downloader_use: { mode: 'allowlist', resource_ids: ['downloader-1'] },
      library_read: { mode: 'allowlist', resource_ids: ['media_library-1'] },
      library_ingest: { mode: 'allowlist', resource_ids: ['media_library-1'] },
    }))
    expect(wrapper.text()).toContain('资源名单已保存')
    expect(wrapper.emitted('saved')).toEqual([[2]])
  })

  it('preserves a conflicted draft and only discards it after an explicit reload', async () => {
    apiMock.mockImplementation((path: string, options?: RequestInit) => {
      if (options?.method === 'PUT') return Promise.reject(new APIError(409, 'CONFLICT', '版本冲突'))
      return Promise.resolve(path.endsWith('/options') ? choices() : access())
    })
    wrapper = mount(UserResourceAccessPanel, { props: { userId: 2, editable: true, refreshKey: 0 } })
    await flushPromises()
    await wrapper.get('#policy-mode-site_search').setValue('denylist')
    await card(wrapper, 'site_search').get('input[type="checkbox"]').setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('你的名单改动仍保留')
    expect(card(wrapper, 'site_search').get<HTMLInputElement>('input[type="checkbox"]').element.checked).toBe(true)
    expect(wrapper.get<HTMLSelectElement>('#policy-mode-site_search').element.disabled).toBe(true)
    await wrapper.setProps({ refreshKey: 1 })
    expect(card(wrapper, 'site_search').get<HTMLInputElement>('input[type="checkbox"]').element.checked).toBe(true)
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('#policy-mode-site_search').element.value).toBe('all')
    expect(wrapper.text()).not.toContain('你的名单改动仍保留')
  })

  it('isolates late reads and save responses from a newly selected user', async () => {
    const oldRead = deferred<UserResourceAccess>()
    const oldOptions = deferred<ResourceAccessOptions>()
    const oldSave = deferred<UserResourceAccess>()
    apiMock.mockImplementation((path: string, options?: RequestInit) => {
      if (options?.method === 'PUT') return oldSave.promise
      if (path === '/api/v1/users/2/resource-access') return oldRead.promise
      if (path === '/api/v1/users/2/resource-access/options') return oldOptions.promise
      return Promise.resolve(path.endsWith('/options') ? choices(3, '第三位用户的下载器') : access(3))
    })
    wrapper = mount(UserResourceAccessPanel, { props: { userId: 2, editable: true } })
    await wrapper.setProps({ userId: 3 })
    await flushPromises()
    oldRead.resolve(access(1, { site_search: { mode: 'allowlist', resource_ids: [] } }))
    oldOptions.resolve(choices(1, '不应显示的旧资源'))
    await flushPromises()
    expect(wrapper.text()).toContain('第三位用户的下载器')
    expect(wrapper.text()).not.toContain('不应显示的旧资源')
    expect(wrapper.get<HTMLSelectElement>('#policy-mode-site_search').element.value).toBe('all')
    expect(wrapper.emitted('options')?.map(args => args[0])).toEqual([3])

    await wrapper.get('#policy-mode-downloader_use').setValue('allowlist')
    await wrapper.get('form').trigger('submit')
    await wrapper.setProps({ userId: 4 })
    await flushPromises()
    oldSave.resolve(access(4, { site_search: { mode: 'denylist', resource_ids: ['site-1'] } }))
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('#policy-mode-site_search').element.value).toBe('all')
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.text()).not.toContain('资源名单已保存')
  })

  it('renders read-only owner/self views and refuses inconsistent read revisions', async () => {
    apiMock.mockImplementation((path: string) => Promise.resolve(path.endsWith('/options') ? choices() : access()))
    wrapper = mount(UserResourceAccessPanel, { props: { userId: 1, editable: false, readOnlyReason: 'Owner 的授权受到保护，此处仅供查看。' } })
    await flushPromises()
    expect(wrapper.text()).toContain('Owner 的授权受到保护')
    expect(wrapper.findAll('select').every(item => item.element.disabled)).toBe(true)
    await wrapper.get('form').trigger('submit')
    expect(apiMock.mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(false)
    await wrapper.setProps({ readOnlyReason: '不能修改自己的授权，请由其他管理员调整。' })
    expect(wrapper.text()).toContain('不能修改自己的授权')

    apiMock.mockImplementation((path: string) => Promise.resolve(path.endsWith('/options') ? choices(2) : access(1)))
    await wrapper.setProps({ userId: 5 })
    await flushPromises()
    expect(wrapper.text()).toContain('读取期间用户权限已更新')
    expect(wrapper.find('form').exists()).toBe(false)
  })

  it('keeps deleted direct-rule references and allows tightening without granting restricted resources', () => {
    const all = access()
    const copy = copyResourceAccess(all)
    copy.policies.find(item => item.scope === 'library_read')!.mode = 'allowlist'
    expect(resourceDraftAllows(copy.policies, 'library_ingest', 'library-1')).toBe(false)
    expect(all.policies.find(item => item.scope === 'library_read')!.mode).toBe('all')
    const named = authorizationResourceOptions(choices(), 'downloader', 'gone')
    const unavailable = named.find(item => item.id === 'gone')!
    expect(unavailable.deleted).toBe(false)
    const deleted = { ...unavailable, deleted: true }
    expect(deleted.name).toContain('保留原规则')
    const blank: ResourceAccessPolicy = { scope: 'downloader_use', mode: 'allowlist', resource_ids: [] }
    expect(resourceSelectionLocked(blank, all.policies[0], deleted)).toBe(true)
    const retained = { ...blank, resource_ids: ['gone'] }
    expect(resourceSelectionLocked(retained, retained, deleted)).toBe(false)
    expect(resourceSelectionLocked({ ...retained, mode: 'denylist' }, { ...retained, mode: 'denylist' }, deleted)).toBe(false)
    const restricted = { ...named[0], can_grant: false }
    expect(resourceSelectionLocked(blank, blank, restricted)).toBe(true)
    expect(resourceSelectionLocked({ ...blank, mode: 'denylist' }, blank, restricted)).toBe(false)
    expect(resourceSelectionLocked({ ...blank, mode: 'denylist', resource_ids: [restricted.id] }, blank, restricted)).toBe(true)
  })

  it('integrates named legacy rules in user management and keeps profile drafts when policy changes are saved', async () => {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)
    auth.user = { id: 1, username: 'owner', display_name: 'Owner', status: 'active', is_owner: true, roles: ['administrator'], permissions: [Permissions.UsersRead, Permissions.UsersUpdate, Permissions.RolesRead, Permissions.RolesAssign] }
    const target: UserSummary = { id: 2, username: 'member', display_name: '成员', status: 'active', is_owner: false, roles: [], authorization_rules: [
      { permission_code: Permissions.DownloadsCreate, effect: 'deny', resource_type: 'downloader', resource_id: 'downloader-1' },
      { permission_code: Permissions.DiscoveryRead, effect: 'deny', resource_type: 'site', resource_id: 'deleted-site' },
    ], last_login_at: null, created_at: '2026-10-10T00:00:00Z' }
    let policySaved = false
    apiMock.mockImplementation((path: string, options?: RequestInit) => {
      if (path === '/api/v1/users') return Promise.resolve({ list: [target], total: 1 })
      if (path === '/api/v1/roles') return Promise.resolve({ list: [], total: 0 })
      if (path === '/api/v1/permissions') return Promise.resolve([])
      if (path.endsWith('/resource-access/options')) return Promise.resolve(choices(policySaved ? 2 : 1))
      if (options?.method === 'PUT' && path.endsWith('/resource-access')) { policySaved = true; return Promise.resolve({ ...JSON.parse(String(options.body)), revision: 2 }) }
      if (options?.method === 'PUT' && path.endsWith('/authorization-rules')) return Promise.resolve(null)
      if (path.endsWith('/resource-access')) return Promise.resolve(access())
      throw new Error(`unexpected path ${path}`)
    })
    wrapper = mount(UsersView, { global: { plugins: [pinia] } })
    await flushPromises()
    expect(wrapper.text()).toContain('权限设置')
    const directRules = wrapper.findAll('fieldset')
    expect(directRules[0]!.text()).toContain('远程 qBittorrent')
    expect(directRules[1]!.text()).toContain('已删除或无权查看的资源（保留原规则）')
    expect(wrapper.find('input[placeholder="资源 ID"]').exists()).toBe(false)
    const profile = wrapper.findAll('form').find(form => form.text().includes('保存资料'))!
    await profile.get('input').setValue('尚未保存的昵称')
    const panel = wrapper.getComponent(UserResourceAccessPanel)
    await panel.get('#policy-mode-site_search').setValue('allowlist')
    await panel.get('form').trigger('submit')
    await flushPromises()
    expect(profile.get<HTMLInputElement>('input').element.value).toBe('尚未保存的昵称')
    const rulesForm = wrapper.findAll('form').find(form => form.text().includes('直接功能授权（高级）'))!
    await rulesForm.trigger('submit')
    await flushPromises()
    const ruleWrite = apiMock.mock.calls.find(([path]) => path.endsWith('/authorization-rules'))!
    expect(JSON.parse(ruleWrite[1].body)).toEqual({ rules: target.authorization_rules })
  })
})
