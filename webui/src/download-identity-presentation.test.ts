// @vitest-environment happy-dom
import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import DownloadsView from '@/views/DownloadsView.vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { downloadErrorMessage } from '@/downloads'
import { notify } from '@/toast'
import type { DownloadTaskSummary } from '@/types/api'

vi.mock('@/api/client', async original => ({ ...await original<typeof import('@/api/client')>(), api: vi.fn() }))
vi.mock('vue-router', () => ({ RouterLink: { template: '<a><slot /></a>' }, useRoute: () => ({ query: { section: 'history' } }), useRouter: () => ({ replace: vi.fn() }) }))
vi.mock('@/toast', () => ({ notify: vi.fn() }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 7 }, can: (code: string) => [Permissions.DownloadsReadOwn, Permissions.JobsControlOwn].includes(code as typeof Permissions.DownloadsReadOwn), canAny: (codes: string[]) => codes.includes(Permissions.DownloadsReadOwn) }) }))

let wrapper: VueWrapper | undefined
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.useRealTimers(); vi.clearAllMocks() })

it('keeps the original recovery reason visible and never labels a legacy automatic lock as a manual action', async () => {
  vi.useFakeTimers()
  const message = '原任务的追更关联依据已不可用，暂未自动恢复入库；原下载包与文件保留，无需重新下载'
  const task = { id: 'original', job_id: 'original-job', owner_id: 7, display_name: '原已完成任务', downloader_name: '原下载器', job_status: 'failed', phase: 'failed', provider_status: 'completed', lifecycle_scope: 'history', scrape_status: 'completed_unrecognized', scrape_title: '已确认作品', scrape_tmdb_id: 100, scrape_category: '剧集', scrape_season: 1, scrape_episode: 11, identity_source: 'direct_id', identity_status: 'verified', identity_locked: true, identity_revision: 1, last_error_code: 'transfer_media_unrecognized', last_error_message: message, bytes_completed: null, bytes_total: null, progress: null, eta_seconds: null, download_speed: null, upload_speed: null, last_sampled_at: null } as DownloadTaskSummary
  expect(downloadErrorMessage(task)).toBe(message)
  vi.mocked(api).mockImplementation(async path => {
    if (path === '/api/v1/downloads?scope=history&limit=200') return { list: [task], total: 1 }
    if (path === '/api/v1/downloads?scope=active&limit=200' || path === '/api/v1/seeding-tasks?limit=100') return { list: [], total: 0 }
    if (path === '/api/v1/jobs/original-job/retry') return { id: 'original-job', status: 'queued' }
    throw new Error(`unexpected offline request ${path}`)
  })
  wrapper = mount(DownloadsView, { global: { stubs: { DirectoryPickerDialog: true, DownloadRouteTargetPicker: true, MediaReorganizationDialog: true, TransferNodesPanel: true, SecretInput: true, ClearHistoryButton: true } } })
  await flushPromises()
  expect(wrapper.text()).toContain('身份待核实 · r1')
  expect(wrapper.text()).toContain(message)
  expect(wrapper.text()).not.toContain('人工锁定')
  expect(wrapper.text()).not.toContain('媒体未识别，未自动入库')
  await wrapper.findAll('button').find(button => button.text() === '重新识别并入库')!.trigger('click')
  await flushPromises()
  expect(vi.mocked(api).mock.calls.filter(([path]) => path === '/api/v1/jobs/original-job/retry')).toHaveLength(1)
  expect(notify).toHaveBeenCalledWith('已请求使用原下载文件重新识别并入库，不会重新提交下载', 'success')
})
