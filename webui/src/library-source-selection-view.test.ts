// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import MediaLibrariesView from './views/MediaLibrariesView.vue'
import DirectoryPickerDialog from './components/DirectoryPickerDialog.vue'
import { emptyMediaLibraryDraft, payloadFromDraft } from './media-libraries'
import type { DirectoryListing, MediaLibraryDetail, StorageSummary } from './types/api'

const mocks = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))

const storages = [1, 2].map(id => ({ id, name: `夸克来源 ${id}`, type: 'quark', root_path: '', root_display_path: `/媒体${id}`, enabled: true, capabilities: {} }) as StorageSummary)
const profile = { id: 1, name: '电影分类', revision: 1 }
const listing = (id = 1): DirectoryListing => ({ platform: 'quark', location: '/', current_token: `browse-root-${id}`, current_selection_token: `select-root-${id}`, breadcrumbs: [], items: [{ name: '电影', location: '/电影', token: `browse-child-${id}`, selection_token: `select-child-${id}`, enterable: true, selectable: true, kind: 'cloud-directory' }], truncated: false })
function library(): MediaLibraryDetail {
  const draft = emptyMediaLibraryDraft(1, 1)
  return { ...payloadFromDraft(draft, storages[0]), id: 9, name: '现有夸克媒体库', storage_name: storages[0].name, profile_name: profile.name, profile_revision: 1, relative_root: '/', enabled: false, status: 'disabled', structure_status: 'pending', entry_count: 0, baseline_generation: 0, dirty_generation: 0, ready: false, readiness_status: 'disabled' } as MediaLibraryDetail
}
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason?: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
let wrapper: VueWrapper | undefined
let rows: MediaLibraryDetail[]
function installAPI() {
  mocks.api.mockImplementation(async (path: string, options?: { method?: string; body?: string }) => {
    if (path === '/api/v1/storages') return { list: storages }
    if (path === '/api/v1/media-classification-profiles') return { list: [profile] }
    if (path === '/api/v1/media-libraries' && options?.method === 'POST') {
      const payload = JSON.parse(options.body!)
      const saved = { ...library(), ...payload, relative_root: payload.relative_root_token.startsWith('select-child') ? '/电影' : '/' }
      rows = [saved]
      return saved
    }
    if (path === '/api/v1/media-libraries') return { list: rows }
    if (path === '/api/v1/media-libraries/9' && options?.method === 'PUT') {
      const payload = JSON.parse(options.body!)
      const saved = { ...rows[0], ...payload, relative_root: payload.relative_root_token ? payload.relative_root_token.startsWith('select-child') ? '/电影' : '/' : payload.relative_root }
      rows = [saved]
      return saved
    }
    if (path.startsWith('/api/v1/storages/')) return listing(path.includes('/2/') ? 2 : 1)
    return { list: [], total: 0 }
  })
}
async function open() {
  wrapper = mount(MediaLibrariesView, { attachTo: document.body, global: { stubs: { RouterLink: { template: '<a><slot /></a>' }, MediaReorganizationDialog: true, QuarkIntakeSettings: true } } })
  await flushPromises()
  return wrapper
}
function button(text: string, scope: ParentNode = document.body) {
  const item = [...scope.querySelectorAll<HTMLButtonElement>('button')].find(button => button.textContent?.trim() === text)
  if (!item) throw Error(`Missing button: ${text}`)
  return item
}
async function click(text: string, scope: ParentNode = document.body) { button(text, scope).click(); await flushPromises() }
function writes(method: string) { return mocks.api.mock.calls.filter(([, options]) => options?.method === method) }
function createForm() { return wrapper!.get('form').element }
function editForm() { return wrapper!.get('#library-panel-settings').element }
async function beginCreate() { await click('创建媒体库'); await wrapper!.get('#library-create-name').setValue('新夸克媒体库') }
async function chooseCurrent() { await click('选择当前目录', document.querySelector('[role="dialog"]')!) }
async function openSettings() { await click('配置') }

beforeEach(() => { vi.useFakeTimers(); mocks.api.mockReset(); rows = []; installAPI() })
afterEach(() => { wrapper?.unmount(); wrapper = undefined; document.body.innerHTML = ''; vi.restoreAllMocks(); vi.useRealTimers() })

describe('mounted Quark media-library source selection', () => {
  it.each([false, true])('sends the exact picker token when creating a child=%s source', async child => {
    const view = await open()
    await beginCreate()
    expect(button('创建媒体库', createForm()).disabled).toBe(true)
    expect(view.get('form input[readonly]').attributes('readonly')).toBeDefined()
    await click('选择目录', createForm())
    const dialog = document.querySelector('[role="dialog"]')!
    expect(dialog.textContent).toContain('选择网盘目录')
    expect(dialog.textContent).not.toContain('选择 Server 目录')
    await click(child ? '选择' : '选择当前目录', dialog)
    expect(button('创建媒体库', createForm()).disabled).toBe(false)
    await view.get('form').trigger('submit'); await flushPromises()
    expect(writes('POST')).toHaveLength(1)
    const payload = JSON.parse(writes('POST')[0][1].body)
    expect(payload).toMatchObject({ storage_id: 1, relative_root_token: child ? 'select-child-1' : 'select-root-1' })
    expect(payload).not.toHaveProperty('relative_root')
    expect(payload).not.toHaveProperty('provider_root_id')
    expect(view.text()).toContain('首次全量扫描会自动开始')
  })

  it('clears selection on source changes and requires a fresh picker selection after cancellation', async () => {
    const view = await open(); await beginCreate()
    await click('选择目录', createForm()); await chooseCurrent()
    await view.get('#library-create-storage').setValue('2')
    expect(button('创建媒体库', createForm()).disabled).toBe(true)
    expect((view.get('form input[readonly]').element as HTMLInputElement).value).toBe('')
    await click('选择目录', createForm()); await click('关闭', document.querySelector('[role="dialog"]')!)
    expect(button('创建媒体库', createForm()).disabled).toBe(true)
    await click('选择目录', createForm()); await chooseCurrent()
    await click('取消', createForm()); await beginCreate()
    expect(button('创建媒体库', createForm()).disabled).toBe(true)
    expect(writes('POST')).toHaveLength(0)
  })

  it('cannot choose the previous Storage while a reopened picker request is pending or stale', async () => {
    const view = await open(); await beginCreate()
    await click('选择目录', createForm()); await chooseCurrent()
    await view.get('#library-create-storage').setValue('2')
    const pending = deferred<DirectoryListing>()
    const implementation = mocks.api.getMockImplementation()!
    let pendingSignal: AbortSignal | undefined
    mocks.api.mockImplementation((path, options) => {
      if (path === '/api/v1/storages/2/directory') { pendingSignal = options.signal; return pending.promise }
      return implementation(path, options)
    })
    await click('选择目录', createForm())
    expect(button('选择当前目录', document.querySelector('[role="dialog"]')!).disabled).toBe(true)
    await click('关闭', document.querySelector('[role="dialog"]')!)
    expect(pendingSignal?.aborted).toBe(true)
    await view.get('#library-create-storage').setValue('1')
    await click('选择目录', createForm())
    pending.resolve({ ...listing(2), location: '/过期目录' }); await flushPromises()
    expect(document.querySelector('[role="dialog"]')!.textContent).not.toContain('过期目录')
    await chooseCurrent()
    await view.get('form').trigger('submit'); await flushPromises()
    expect(JSON.parse(writes('POST')[0][1].body)).toMatchObject({ storage_id: 1, relative_root_token: 'select-root-1' })
  })

  it('preserves a saved root on tokenless edits, and sends a new token after a source change', async () => {
    rows = [library()]
    const view = await open(); await openSettings()
    await view.get('#library-panel-settings input').setValue('重命名夸克库')
    await view.get('#library-panel-settings').trigger('submit'); await flushPromises()
    expect(JSON.parse(writes('PUT')[0][1].body)).toMatchObject({ storage_id: 1, relative_root: '/' })
    expect(JSON.parse(writes('PUT')[0][1].body)).not.toHaveProperty('relative_root_token')
    await view.get('#library-panel-settings select').setValue('2')
    expect(button('保存配置', editForm()).disabled).toBe(true)
    await click('重新选择', editForm()); await click('选择', document.querySelector('[role="dialog"]')!)
    expect(button('保存配置', editForm()).disabled).toBe(false)
    await view.get('#library-panel-settings').trigger('submit'); await flushPromises()
    expect(JSON.parse(writes('PUT')[1][1].body)).toMatchObject({ storage_id: 2, relative_root_token: 'select-child-2' })
    expect(button('保存配置', editForm()).disabled).toBe(true)
  })

  it.each(['success', 'failure'] as const)('keeps the active Storage loading after an aborted old request %s, and rejects old row clicks', async outcome => {
    const view = await open(); await beginCreate()
    await click('选择目录', createForm())
    const oldRow = button('选择', document.querySelector('[role="dialog"]')!)
    const old = deferred<DirectoryListing>(); const current = deferred<DirectoryListing>()
    const implementation = mocks.api.getMockImplementation()!
    const signals: AbortSignal[] = []
    mocks.api.mockImplementation((path, options) => {
      if (path === '/api/v1/storages/1/directory') { signals.push(options.signal); return old.promise }
      if (path === '/api/v1/storages/2/directory') { signals.push(options.signal); return current.promise }
      return implementation(path, options)
    })
    await click('关闭', document.querySelector('[role="dialog"]')!)
    await click('选择目录', createForm())
    await view.get('#library-create-storage').setValue('2'); await flushPromises()
    expect(signals[0].aborted).toBe(true)
    if (outcome === 'success') old.resolve({ ...listing(1), location: '/旧目录' })
    else old.reject(Error('旧目标请求失败'))
    await flushPromises()
    const dialog = document.querySelector('[role="dialog"]')!
    expect(dialog.textContent).toContain('正在读取当前层目录')
    expect(dialog.textContent).not.toContain('旧目标请求失败')
    expect(button('选择当前目录', dialog).disabled).toBe(true)
    oldRow.click(); await flushPromises()
    expect(button('创建媒体库', createForm()).disabled).toBe(true)
    current.resolve(listing(2)); await flushPromises()
    expect(dialog.textContent).not.toContain('旧目标请求失败')
    oldRow.click(); await flushPromises()
    expect(document.querySelector('[role="dialog"]')).not.toBeNull()
    await chooseCurrent()
    await view.get('form').trigger('submit'); await flushPromises()
    expect(JSON.parse(writes('POST')[0][1].body)).toMatchObject({ storage_id: 2, relative_root_token: 'select-root-2' })
  })

  it.each(['browse', 'refresh'])('clears an old page-loading flag when %s supersedes pagination', async action => {
    const oldPage = deferred<DirectoryListing>()
    const root = { ...listing(), next_page_token: 'old-page' }
    const child = { ...listing(), location: '/电影', current_token: 'browse-child-1', current_selection_token: 'select-child-1', next_page_token: 'new-page', items: [] }
    let oldSignal: AbortSignal | undefined
    mocks.api.mockImplementation((path, options) => {
      if (path.endsWith('page_token=old-page')) { oldSignal = options.signal; return oldPage.promise }
      if (path.includes('?token=')) return Promise.resolve(child)
      if (path.endsWith('page_token=new-page')) return Promise.resolve({ ...child, next_page_token: undefined })
      return Promise.resolve(root)
    })
    wrapper = mount(DirectoryPickerDialog, { attachTo: document.body, props: { open: true, storageId: 1, restrictToStorage: true } })
    await flushPromises()
    await click('加载更多目录')
    await click(action === 'browse' ? '电影cloud-directory' : '刷新')
    expect(oldSignal?.aborted).toBe(true)
    expect(button('加载更多目录').disabled).toBe(false)
    oldPage.resolve({ ...root, location: '/过期分页', items: [] }); await flushPromises()
    expect(document.body.textContent).not.toContain('过期分页')
    await click('加载更多目录')
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('page_token=new-page'))).toHaveLength(1)
  })

  it('drops old breadcrumb callbacks and errors when the picker changes target', async () => {
    const root = { ...listing(), breadcrumbs: [{ name: '旧面包屑', token: 'old-crumb' }] }
    mocks.api.mockResolvedValue(root)
    wrapper = mount(DirectoryPickerDialog, { attachTo: document.body, props: { open: true, storageId: 1 } })
    await flushPromises()
    const oldCrumb = button('旧面包屑')
    const pending = deferred<DirectoryListing>()
    mocks.api.mockImplementation((path: string) => path === '/api/v1/storages/2/directory' ? pending.promise : Promise.reject(Error('旧目标错误')))
    await wrapper.setProps({ storageId: 2 }); await flushPromises()
    oldCrumb.click(); await flushPromises()
    expect(mocks.api.mock.calls.some(([path]) => path.includes('old-crumb'))).toBe(false)
    expect(document.body.textContent).toContain('正在读取当前层目录')
    pending.resolve(listing(2)); await flushPromises()
    oldCrumb.click(); await flushPromises()
    expect(mocks.api.mock.calls.some(([path]) => path.includes('old-crumb'))).toBe(false)
    await click('刷新')
    expect(document.body.textContent).toContain('旧目标错误')
    const next = deferred<DirectoryListing>()
    mocks.api.mockReturnValue(next.promise)
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ storageId: 1, open: true }); await flushPromises()
    expect(document.body.textContent).not.toContain('旧目标错误')
    next.resolve(listing(1)); await flushPromises()
  })
})
