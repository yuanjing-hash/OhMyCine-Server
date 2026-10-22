// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import DownloadsView from '@/views/DownloadsView.vue'
import { api } from '@/api/client'
import { nodeSupportsDownloader } from '@/downloads'
import { Permissions } from '@/auth/generated-permissions'
import type { DownloaderSummary, TransferNodeSummary } from '@/types/api'

vi.mock('@/api/client', async importOriginal => ({ ...await importOriginal<typeof import('@/api/client')>(), api: vi.fn() }))
vi.mock('vue-router', () => ({ RouterLink: { template: '<a><slot /></a>' }, useRoute: () => ({ query: { section: 'downloaders' } }), useRouter: () => ({ replace: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: (code: string) => [Permissions.DownloadersRead, Permissions.DownloadersCreate, Permissions.DownloadersUpdate, Permissions.TransferNodesRead].includes(code as typeof Permissions.DownloadersRead), canAny: () => false }) }))

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
  })
})
