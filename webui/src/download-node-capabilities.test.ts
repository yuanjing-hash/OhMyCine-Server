// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import DownloadsView from '@/views/DownloadsView.vue'
import { api } from '@/api/client'
import { notify } from '@/toast'
import { nodeSupportsDownloader } from '@/downloads'
import { Permissions } from '@/auth/generated-permissions'
import type { DownloaderSummary, TransferNodeSummary } from '@/types/api'

vi.mock('@/api/client', async importOriginal => ({ ...await importOriginal<typeof import('@/api/client')>(), api: vi.fn() }))
vi.mock('@/toast', () => ({ notify: vi.fn() }))
vi.mock('vue-router', () => ({ RouterLink: { template: '<a><slot /></a>' }, useRoute: () => ({ query: { section: 'downloaders' } }), useRouter: () => ({ replace: vi.fn() }) }))
const access = vi.hoisted(() => ({ current: null as null | { user: { id: number; permissions: string[] } } }))
vi.mock('@/stores/auth', async () => {
  const { reactive } = await import('vue')
  const auth = reactive({ user: { id: 1, permissions: ['downloaders.read', 'downloaders.create', 'downloaders.update', 'transfer_nodes.read'] } })
  access.current = auth
  return { useAuthStore: () => ({ get user() { return auth.user }, can: (code: string) => auth.user.permissions.includes(code), canAny: (codes: string[]) => codes.some(code => auth.user.permissions.includes(code)) }) }
})

const sourceCodes = ['quark_tv_read_v2', 'quark_share_receive_v2', 'content_digest_v2', 'range_export']
function node(id: string, codes: string[], status: TransferNodeSummary['status'] = 'online'): TransferNodeSummary {
  return { id, name: id, status, platform: 'linux', architecture: 'amd64', capabilities: { codes, max_concurrent: 1, managed_free_bytes_known: true } } as TransferNodeSummary
}
let wrapper: VueWrapper | undefined
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.useRealTimers(); vi.clearAllMocks() })

describe('Node downloader capability admission', () => {
  it('requires every real Quark source capability, not a version label or upload support', () => {
    expect(nodeSupportsDownloader(node('ready', sourceCodes), 'quark_share')).toBe(true)
    for (const missing of sourceCodes) {
      expect(nodeSupportsDownloader(node('partial', sourceCodes.filter(code => code !== missing)), 'quark_share'), missing).toBe(false)
    }
    expect(nodeSupportsDownloader(node('upload-only', ['quark_agent_upload_v2', 'content_digest_v2', 'range_export']), 'quark_share')).toBe(false)
    expect(nodeSupportsDownloader(node('offline', sourceCodes, 'offline'), 'quark_share')).toBe(false)
    expect(nodeSupportsDownloader(node('115', ['pan115_offline', 'pan115_read', 'pan115_share_receive', 'range_export']), 'pan115_offline')).toBe(true)
    expect(nodeSupportsDownloader(node('qbit', ['qbittorrent_control', 'range_export']), 'qbittorrent')).toBe(true)
    expect(nodeSupportsDownloader(node('quark', sourceCodes), 'qbittorrent')).toBe(false)
  })

  it('filters create and edit selectors and disables saving a selected Node after capability loss', async () => {
    vi.useFakeTimers()
    let nodes = [node('ready', sourceCodes), ...sourceCodes.map((missing, index) => node(`partial-${index}`, sourceCodes.filter(code => code !== missing))), node('offline', sourceCodes, 'offline'), node('old', ['pan115_read', 'range_export'])]
    const downloader = { id: 'quark', name: 'Quark receiver', type: 'quark_share', enabled: false, execution_location: 'node', node_id: 'ready', node_name: 'ready', provider_directory_path: '/Downloads', storage_name: 'Quark', capabilities: { share_receive: true }, health: { status: 'unknown', version: '', last_checked_at: null } } as DownloaderSummary
    vi.mocked(api).mockImplementation(async path => {
      if (path === '/api/v1/transfer-nodes') return { list: nodes, total: nodes.length }
      if (path === '/api/v1/downloaders') return { list: [downloader], total: 1 }
      throw new Error(`unexpected request ${path}`)
    })
    wrapper = mount(DownloadsView, { global: { stubs: { DirectoryPickerDialog: true, DownloadRouteTargetPicker: true, MediaReorganizationDialog: true, TransferNodesPanel: true, SecretInput: true, ClearHistoryButton: true } } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '添加下载器')!.trigger('click')
    await wrapper.find('select[aria-describedby="downloader-type-help"]').setValue('quark_share')
    const location = wrapper.findAll('select').find(select => select.find('option[value="node"]').exists())!
    await location.setValue('node')
    const createNode = wrapper.findAll('select').find(select => select.find('option[value="ready"]').exists())!
    expect(createNode.findAll('option').map(option => option.element.value)).toEqual(['', 'ready'])
    await createNode.setValue('ready')
    await wrapper.findAll('button').find(button => button.text() === '编辑')!.trigger('click')
    const selectors = wrapper.findAll('select').filter(select => select.find('option[value="ready"]').exists())
    expect(selectors).toHaveLength(2)
    expect(selectors[1]!.findAll('option').map(option => option.element.value)).toEqual(['', 'ready'])
    const save = wrapper.findAll('button').find(button => button.text() === '保存配置')!
    expect(save.attributes('disabled')).toBeUndefined()
    nodes = nodes.map(item => item.id === 'ready' ? node('ready', sourceCodes.filter(code => code !== 'content_digest_v2')) : item)
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(wrapper.findAll('option[value="ready"]')).toHaveLength(0)
    expect(save.attributes('disabled')).toBeDefined()
    // Even a synthetic form submit cannot bypass the disabled button.
    await save.element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    await flushPromises()
    expect(vi.mocked(api).mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(false)
    const optionRequests: { signal: AbortSignal; finish: (value: unknown) => void }[] = []
    vi.mocked(api).mockImplementation(async (path, options) => {
      if (path === '/api/v1/downloaders/options') return new Promise(finish => optionRequests.push({ signal: options!.signal as AbortSignal, finish }))
      if (path === '/api/v1/media-libraries') return { list: [], total: 0 }
      if (path === '/api/v1/download-routes/preview') return { options: [] }
      throw new Error(`unexpected ordinary request ${path}`)
    })
    access.current!.user = { id: 2, permissions: [Permissions.DownloadsCreate, Permissions.MediaLibrariesRead] }
    await flushPromises()
    expect(optionRequests).toHaveLength(1)
    access.current!.user = { id: 3, permissions: [Permissions.DownloadsCreate, Permissions.MediaLibrariesRead] }
    await flushPromises()
    expect(optionRequests[0]!.signal.aborted).toBe(true)
    optionRequests[1]!.finish({ list: [{ id: 'current', name: 'Current receiver', type: 'qbittorrent', capabilities: {}, execution_location: 'server' }] })
    await flushPromises()
    optionRequests[0]!.finish({ list: [{ id: 'retired', name: 'Private old receiver', type: 'qbittorrent', capabilities: {}, execution_location: 'server' }] })
    await flushPromises()
    expect(wrapper.get('select').findAll('option').map(option => option.element.value)).toEqual(['', 'current'])
    expect(wrapper.text()).not.toContain('Private old receiver')
    expect(wrapper.text()).not.toContain('下载器管理')
    access.current!.user = { ...access.current!.user, permissions: [...access.current!.user.permissions].reverse() }
    await flushPromises()
    expect(optionRequests).toHaveLength(2)
    expect(wrapper.get('select').element).toHaveProperty('value', 'current')
    access.current!.user = { ...access.current!.user, permissions: [] }
    await flushPromises()
    expect(wrapper.findAll('option[value="current"]')).toHaveLength(0)
  })
})

it('round-trips the independent acquisition limit in create and an existing Node edit', async () => {
  access.current!.user = { id: 1, permissions: [Permissions.DownloadersRead, Permissions.DownloadersCreate, Permissions.DownloadersUpdate, Permissions.TransferNodesRead] }
  const downloader = { id: 'qbit', name: 'Node qB', type: 'qbittorrent', execution_location: 'node', node_id: 'ready', base_url: 'http://qbit.example.test', downloader_save_root: '/downloads', node_mount_root: '/downloads', max_concurrent_downloads: 4, health: { status: 'online' }, capabilities: {} } as DownloaderSummary
  vi.mocked(api).mockImplementation(async (path, options) => {
    if (path === '/api/v1/transfer-nodes') return { list: [node('ready', ['qbittorrent_control', 'range_export'])] }
    if (path === '/api/v1/downloaders' && options?.method === 'POST' || path === '/api/v1/downloaders/qbit') return downloader
    if (path === '/api/v1/downloaders') return { list: [downloader] }
    throw new Error(`unexpected request ${path}`)
  })
  wrapper = mount(DownloadsView, { global: { stubs: { DirectoryPickerDialog: true, TransferNodesPanel: true, SecretInput: true, ClearHistoryButton: true } } })
  await flushPromises()
  await wrapper.findAll('button').find(button => button.text() === '添加下载器')!.trigger('click')
  expect(wrapper.get('#createForm-concurrency').element).toHaveProperty('value', '2')
  await wrapper.get('#createForm-concurrency').setValue(3)
  await wrapper.get('#createForm-concurrency').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await flushPromises()
  const create = vi.mocked(api).mock.calls.find(([path, options]) => path === '/api/v1/downloaders' && options?.method === 'POST')!
  expect(JSON.parse(String(create[1]!.body))).toMatchObject({ max_concurrent_downloads: 3 })
  await wrapper.findAll('button').find(button => button.text() === '编辑')!.trigger('click')
  expect(wrapper.get('#editForm-concurrency').element).toHaveProperty('value', '4')
  await wrapper.get('#editForm-concurrency').setValue(1)
  expect(wrapper.text()).toContain('调低上限会保留正在获取的任务')
  await wrapper.get('#editForm-concurrency').element.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await flushPromises()
  const edit = vi.mocked(api).mock.calls.find(([path, options]) => path === '/api/v1/downloaders/qbit' && options?.method === 'PATCH')!
  expect(JSON.parse(String(edit[1]!.body))).toMatchObject({ max_concurrent_downloads: 1, execution_location: 'node', node_id: 'ready' })
})

it('lets a use-only actor confirm exact warned batch items without replaying accepted sources', async () => {
  access.current!.user = { id: 2, permissions: [Permissions.DownloadsCreate, Permissions.MediaLibrariesRead] }
  const submitted: Record<string, unknown>[] = []
  const token = 'a'.repeat(64)
  let finish!: (value: unknown) => void
  let continued = 0
  vi.mocked(api).mockImplementation(async (path, options) => {
    if (path === '/api/v1/downloaders/options') return { list: [{ id: 'receiver', name: 'Allowed receiver', type: 'qbittorrent', execution_location: 'server', capabilities: {} }] }
    if (path === '/api/v1/media-libraries') return { list: [{ id: 1, name: 'Allowed library', enabled: true }] }
    if (path === '/api/v1/download-routes/preview') return { options: [{ media_library_id: 1, library_name: 'Allowed library', enabled: true }] }
    if (path === '/api/v1/downloads/batch') {
      const body = JSON.parse(String(options?.body)); submitted.push(body)
      if (body.offline_failure_overrides) {
        if (++continued === 1) throw new Error('reply lost')
        return new Promise(resolve => { finish = resolve })
      }
      return { submitted: body.sources.includes('magnet:accepted') ? 1 : 0, failed: body.sources.length - (body.sources.includes('magnet:accepted') ? 1 : 0), results: body.sources.map((source: string, index: number) => source === 'magnet:accepted' ? { index, task: { id: 'accepted' } } : source === 'magnet:warned' ? { index, error_code: 'offline_resource_confirmation_required', offline_failure_warning: { version: 1, reason: 'offline_deadline_exceeded', timeout_minutes: 30, confirmation_token: token } } : { index, error_code: 'invalid_source', message: '链接无效，请更换链接' }) }
    }
    throw new Error(`management request forbidden ${path}`)
  })
  wrapper = mount(DownloadsView, { global: { stubs: { DirectoryPickerDialog: true, TransferNodesPanel: true, SecretInput: true, ClearHistoryButton: true } } })
  await flushPromises()
  await wrapper.get('input[name="download-route-target"][value="1"]').setValue(true)
  await wrapper.get('textarea').setValue('magnet:accepted\nmagnet:warned\nmagnet:invalid')
  await wrapper.get('#download-panel-create').trigger('submit'); await flushPromises()
  expect(wrapper.get('textarea').element).toHaveProperty('value', 'magnet:warned\nmagnet:invalid')
  expect(notify).toHaveBeenCalledWith('已入队 1 个，1 个资源需要确认；未入队链接已保留。第 3 个链接：链接无效，请更换链接', 'warning')
  await wrapper.get('[aria-label="历史离线超时提示"] .btn-secondary').trigger('click')
  expect(submitted).toHaveLength(1)
  expect(wrapper.find('[aria-label="历史离线超时提示"]').exists()).toBe(false)
  await wrapper.get('#download-panel-create').trigger('submit'); await flushPromises()
  await wrapper.get('[aria-label="历史离线超时提示"] .btn-primary').trigger('click'); await flushPromises()
  const expected = { ...submitted[1], sources: ['magnet:warned'], offline_failure_overrides: [{ index: 0, confirmation_token: token }] }
  expect(submitted[2]).toEqual(expected)
  const button = wrapper.get('[aria-label="历史离线超时提示"] .btn-primary')
  await button.trigger('click'); await button.trigger('click'); await flushPromises()
  expect(submitted).toHaveLength(4)
  expect(submitted[3]).toEqual(expected)
  finish({ submitted: 1, failed: 0, results: [{ index: 0, task: { id: 'same-accepted' } }] }); await flushPromises()
  expect(wrapper.find('[aria-label="历史离线超时提示"]').exists()).toBe(false)
  expect(vi.mocked(api).mock.calls.some(([path]) => path === '/api/v1/downloaders')).toBe(false)
})

it.each([false, true])('shows authoritative share rejection reasons and preserves only unaccepted input (mixed=%s)', async mixed => {
  access.current!.user = { id: 2, permissions: [Permissions.DownloadsCreate, Permissions.MediaLibrariesRead] }
  const expired = 'https://115.com/s/expired'
  const unavailable = 'https://115.com/s/temporarily-unavailable'
  const sources = mixed ? ['https://115.com/s/accepted', expired, unavailable] : [expired]
  const expiryMessage = '115 分享已过期或失效，请更换有效分享链接'
  const unavailableMessage = '115 分享暂时无法读取，请稍后重试'
  let finish!: (value: unknown) => void
  vi.mocked(api).mockImplementation(async (path) => {
    if (path === '/api/v1/downloaders/options') return { list: [{ id: 'receiver', name: '115 receiver', type: 'pan115_offline', execution_location: 'server', capabilities: { share_receive: true } }] }
    if (path === '/api/v1/media-libraries') return { list: [{ id: 1, name: 'Allowed library', enabled: true }] }
    if (path === '/api/v1/download-routes/preview') return { options: [{ media_library_id: 1, library_name: 'Allowed library', enabled: true }] }
    if (path === '/api/v1/downloads/batch') return new Promise(resolve => { finish = resolve })
    throw new Error(`unexpected request ${path}`)
  })
  wrapper = mount(DownloadsView, { global: { stubs: { DirectoryPickerDialog: true, TransferNodesPanel: true, SecretInput: true, ClearHistoryButton: true } } })
  await flushPromises()
  await wrapper.get('input[type="radio"][value="share"]').setValue(true); await flushPromises()
  await wrapper.get('input[name="download-route-target"][value="1"]').setValue(true)
  await wrapper.get('textarea').setValue(sources.join('\n'))
  await wrapper.get('#download-panel-create').trigger('submit'); await flushPromises()
  expect(notify).not.toHaveBeenCalled()
  expect(wrapper.get('textarea').element).toHaveProperty('value', sources.join('\n'))
  finish({ submitted: mixed ? 1 : 0, failed: mixed ? 2 : 1, results: sources.map((source, index) => source === expired ? { index, error_code: 'pan115_share_expired', message: expiryMessage } : source === unavailable ? { index, error_code: 'pan115_unavailable', message: unavailableMessage } : { index, task: { id: 'accepted' } }) })
  await flushPromises()
  expect(wrapper.get('textarea').element).toHaveProperty('value', mixed ? `${expired}\n${unavailable}` : expired)
  const expected = mixed
    ? `已入队 1 个，失败 2 个；失败链接已保留。第 2 个链接：${expiryMessage}；第 3 个链接：${unavailableMessage}`
    : `已入队 0 个，失败 1 个；失败链接已保留。第 1 个链接：${expiryMessage}`
  expect(notify).toHaveBeenCalledExactlyOnceWith(expected, mixed ? 'warning' : 'error')
  const mutations = vi.mocked(api).mock.calls.filter(([, options]) => options?.method === 'POST')
  expect(mutations.filter(([path]) => path !== '/api/v1/download-routes/preview')).toHaveLength(1)
  const submitted = mutations.find(([path]) => path === '/api/v1/downloads/batch')!
  expect(JSON.parse(String(submitted[1]?.body))).toMatchObject({ source_kind: '115_share', sources })
  expect(wrapper.find('[aria-label="历史离线超时提示"]').exists()).toBe(false)
  expect(wrapper.find('#download-panel-create').exists()).toBe(true)
})
