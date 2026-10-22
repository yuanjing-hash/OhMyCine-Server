// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SharePreviewDialog from '@/components/SharePreviewDialog.vue'
import { api, APIError } from '@/api/client'
import type { DownloaderSummary } from '@/types/api'
import { shareValidationLabel, type ShareValidation, type SharePreview } from '@/share-preview'
vi.mock('@/api/client', async importOriginal => ({ ...await importOriginal<typeof import('@/api/client')>(), api: vi.fn() }))
beforeEach(() => { vi.mocked(api).mockReset() })

function receiver(id = 'quark', type: DownloaderSummary['type'] = 'quark_share'): DownloaderSummary {
 return { id, name: id, type, enabled: true, storage_id: 1, updated_at: '2026-10-23T00:00:00Z', capabilities: { share_receive: true } } as DownloaderSummary
}
function previewResponse(token: string, video = false): SharePreview {
 return { token, total_size: 100, file_count: 1, expires_at: new Date(Date.now() + 300000).toISOString(), entries: [{ token: 'file', name: `${token}.${video ? 'mkv' : 'txt'}`, path: `${token}.${video ? 'mkv' : 'txt'}`, is_dir: false, size: 100 }] }
}
describe('share preview selection', () => {
 it('previews Quark using only the matching receiver even when 115 is first', async () => {
  vi.mocked(api).mockClear()
  vi.mocked(api).mockResolvedValue({ token: 'quark-preview', total_size: 0, file_count: 0, expires_at: new Date(Date.now() + 300000).toISOString(), entries: [] })
  const receivers = [
   { id: '115', name: '115 first', type: 'pan115_offline', enabled: true, capabilities: { share_receive: true } },
   { id: 'quark', name: 'Quark', type: 'quark_share', enabled: true, capabilities: { share_receive: true } },
  ] as DownloaderSummary[]
  const wrapper = mount(SharePreviewDialog, { props: { title: 'Show', resultToken: 'quark-claim', sourceKind: 'quark_share', downloaders: receivers } })
  await flushPromises()
  expect(wrapper.find('select').findAll('option').map(option => option.element.value)).toEqual(['', 'quark'])
  const call = vi.mocked(api).mock.calls.find(([url]) => url.endsWith('/share-preview'))
  expect(JSON.parse(String(call?.[1]?.body))).toMatchObject({ result_token: 'quark-claim', downloader_id: 'quark' })
  wrapper.unmount()
 })
 it('only submits selected leaves, reports their size, and closing aborts recognition', async () => {
  const response: SharePreview = { token: 'preview', total_size: 110, file_count: 2, expires_at: new Date(Date.now() + 300000).toISOString(), entries: [
   { token: 'dir', path: 'Movies', name: 'Movies', is_dir: true, size: 110 },
   { token: 'video', path: 'Movies/Film.mkv', name: 'Film.mkv', is_dir: false, size: 100 },
   { token: 'note', path: 'Movies/note.txt', name: 'note.txt', is_dir: false, size: 10 },
  ] }
  let recognitionSignal: AbortSignal | undefined
  vi.mocked(api).mockImplementation(async (url, options) => {
   if (url.endsWith('/recognize')) { recognitionSignal = options?.signal as AbortSignal; return new Promise(() => {}) }
   return response
  })
  const downloader = { id: 'd', name: '115', type: 'pan115_offline', enabled: true, capabilities: { share_receive: true } } as DownloaderSummary
  const wrapper = mount(SharePreviewDialog, { props: { title: 'Collection', resultToken: 'claim', downloaders: [downloader] } })
  await flushPromises()
  expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(3)
  await wrapper.findAll('input[type="checkbox"]')[0]!.setValue(true)
  expect(wrapper.findAll('input[type="checkbox"]')[2]!.element).toHaveProperty('checked', true)
  await wrapper.findAll('input[type="checkbox"]')[2]!.setValue(false)
  expect(wrapper.findAll('input[type="checkbox"]')[0]!.element).toHaveProperty('indeterminate', true)
  const submit = wrapper.findAll('button').find(button => button.text().includes('仅将所选'))!
  await submit.trigger('click')
  expect(wrapper.emitted('select')?.[0]?.[0]).toMatchObject({ previewToken: 'preview', entryTokens: ['video'], downloaderID: 'd', fileCount: 1, totalSize: 100 })
  await wrapper.findAll('button').find(button => button.text() === '关闭')!.trigger('click')
  expect(recognitionSignal?.aborted).toBe(true)
  wrapper.unmount()
 })
})

it('reports exact validation states, retries, and ignores closed requests', async () => {
 const now = Date.now()
 const invalid: ShareValidation = { status: 'expired', message: '分享已失效或已被取消', error_code: 'pan115_share_expired', downloader_id: 'd', checked_at: new Date(now).toISOString(), expires_at: new Date(now + 120000).toISOString() }
 const valid: ShareValidation = { ...invalid, status: 'valid', message: '已验证可读取', error_code: undefined }
 vi.mocked(api).mockRejectedValueOnce(new APIError(410, invalid.error_code!, invalid.message, invalid)).mockResolvedValueOnce({ token: 'preview', entries: [], file_count: 0, total_size: 0, expires_at: valid.expires_at, share_validation: valid })
 const downloader = { id: 'd', name: '115', type: 'pan115_offline', enabled: true, capabilities: { share_receive: true } } as DownloaderSummary
 const wrapper = mount(SharePreviewDialog, { props: { title: 'Movie', resultToken: 'claim', downloaders: [downloader] } })
 await flushPromises()
 expect(wrapper.text()).toContain(invalid.message)
 expect(wrapper.emitted('validation')?.[0]).toEqual(['claim', invalid])
 expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(0)
 await wrapper.findAll('button').find(b => b.text() === '重新读取')!.trigger('click')
 await flushPromises()
 expect(wrapper.emitted('validation')?.[1]).toEqual(['claim', valid])
 expect(shareValidationLabel(valid, now)).toBe('已验证可读取')
 expect(shareValidationLabel(valid, now + 120001)).toContain('过期')
 expect(shareValidationLabel({ ...invalid, status: 'unavailable' }, now)).toBe('暂时无法验证')
 let finish: (value: unknown) => void = () => {}
 vi.mocked(api).mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
 await wrapper.findAll('button').find(b => b.text() === '重新读取')!.trigger('click')
 await wrapper.findAll('button').find(b => b.text() === '关闭')!.trigger('click')
 finish({ share_validation: invalid })
 await flushPromises()
 expect(wrapper.emitted('validation')).toHaveLength(2)
 wrapper.unmount()
})

it.each(['resolve', 'reject'])('changes result ownership on the same receiver and ignores an old preview %s', async outcome => {
 const pending: { resolve: (value: unknown) => void; reject: (reason: Error) => void; signal: AbortSignal; body: unknown }[] = []
 vi.mocked(api).mockImplementation((_path, options) => new Promise((resolve, reject) => { pending.push({ resolve, reject, signal: options!.signal as AbortSignal, body: JSON.parse(String(options!.body)) }) }))
 const wrapper = mount(SharePreviewDialog, { props: { title: 'First', resultToken: 'first', sourceKind: 'quark_share', downloaders: [receiver()] } })
 try {
  await flushPromises()
  await wrapper.setProps({ resultToken: 'second', title: 'Second' })
  await flushPromises()
  expect(pending).toHaveLength(2)
  expect(pending[0]!.signal.aborted).toBe(true)
  expect(pending[1]!.body).toEqual({ result_token: 'second', downloader_id: 'quark' })
  if (outcome === 'resolve') pending[0]!.resolve(previewResponse('old'))
  else pending[0]!.reject(new Error('old request failed'))
  await flushPromises()
  expect(wrapper.text()).not.toContain('old')
  expect(wrapper.text()).toContain('正在读取完整目录和文件大小')
  pending[1]!.resolve(previewResponse('current'))
  await flushPromises()
  expect(wrapper.text()).toContain('current.txt')
  expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(1)
 } finally { wrapper.unmount() }
})

it('clears selected files when the same receiver changes provider or storage ownership', async () => {
 vi.mocked(api).mockResolvedValue(previewResponse('original'))
 const wrapper = mount(SharePreviewDialog, { props: { title: 'Share', resultToken: 'claim', sourceKind: 'quark_share', downloaders: [receiver('same')] } })
 try {
  await flushPromises()
  await wrapper.get('input[type="checkbox"]').setValue(true)
  const pending: { resolve: (value: unknown) => void; signal: AbortSignal; body: unknown }[] = []
  vi.mocked(api).mockImplementation((_path, options) => new Promise(resolve => { pending.push({ resolve, signal: options!.signal as AbortSignal, body: JSON.parse(String(options!.body)) }) }))
  await wrapper.setProps({ downloaders: [{ ...receiver('same'), storage_id: 2 }] })
  await flushPromises()
  expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(0)
  expect(wrapper.findAll('button').some(button => button.text().includes('仅将所选'))).toBe(false)
  expect(pending).toHaveLength(1)
  await wrapper.setProps({ sourceKind: '115_share', downloaders: [receiver('same', 'pan115_offline')] })
  await flushPromises()
  expect(pending).toHaveLength(2)
  expect(pending[0]!.signal.aborted).toBe(true)
  pending[0]!.resolve(previewResponse('old-quark'))
  pending[1]!.resolve(previewResponse('new-115'))
  await flushPromises()
  expect(wrapper.text()).not.toContain('old-quark')
  expect(wrapper.text()).toContain('new-115.txt')
  expect(wrapper.get('input[type="checkbox"]').element).toHaveProperty('checked', false)
  expect(wrapper.findAll('button').find(button => button.text().includes('仅将所选'))!.attributes('disabled')).toBeDefined()
 } finally { wrapper.unmount() }
})

it('drops a same-ID receiver when Server revokes its capability and rejects its late preview', async () => {
 let finish!: (value: unknown) => void
 let signal!: AbortSignal
 vi.mocked(api).mockImplementation((_path, options) => { signal = options!.signal as AbortSignal; return new Promise(resolve => { finish = resolve }) })
 const downloader = receiver()
 const wrapper = mount(SharePreviewDialog, { props: { title: 'Share', resultToken: 'claim', sourceKind: 'quark_share', downloaders: [receiver('115', 'pan115_offline'), downloader] } })
 try {
  await flushPromises()
  await wrapper.setProps({ downloaders: [receiver('115', 'pan115_offline'), { ...downloader, capabilities: { ...downloader.capabilities, share_receive: false } }] })
  expect(signal.aborted).toBe(true)
  finish(previewResponse('retired'))
  await flushPromises()
  expect(wrapper.text()).toContain('当前没有可用的夸克分享接收器')
  expect(wrapper.text()).not.toContain('retired')
  expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(0)
  expect(wrapper.get('select').findAll('option').map(option => option.element.value)).toEqual([''])
  expect(vi.mocked(api)).toHaveBeenCalledTimes(1)
 } finally { wrapper.unmount() }
})

it('retires recognition work with its preview so old completions cannot alter a new result', async () => {
 const recognitionRequests: { resolve: (value: unknown) => void; signal: AbortSignal }[] = []
 vi.mocked(api).mockImplementation((path, options) => path.endsWith('/recognize')
  ? new Promise(resolve => { recognitionRequests.push({ resolve, signal: options!.signal as AbortSignal }) })
  : Promise.resolve(previewResponse(JSON.parse(String(options!.body)).result_token, true)))
 const wrapper = mount(SharePreviewDialog, { props: { title: 'Share', resultToken: 'first', sourceKind: 'quark_share', downloaders: [receiver()] } })
 try {
  await flushPromises()
  await wrapper.setProps({ resultToken: 'second' })
  await flushPromises()
  await wrapper.setProps({ resultToken: 'third' })
  await flushPromises()
  expect(recognitionRequests).toHaveLength(3)
  expect(recognitionRequests[0]!.signal.aborted).toBe(true)
  expect(recognitionRequests[1]!.signal.aborted).toBe(true)
  recognitionRequests[0]!.resolve({ status: 'matched', title: 'OLD recognition' })
  await flushPromises()
  expect(wrapper.text()).not.toContain('OLD recognition')
  expect(wrapper.text()).toContain('正在根据文件名检测')
  recognitionRequests[2]!.resolve({ status: 'matched', title: 'CURRENT recognition' })
  await flushPromises()
  expect(wrapper.text()).toContain('CURRENT recognition')
 } finally { wrapper.unmount() }
})

it('requires an explicit choice for multiple current matching receivers and never maps an unsupported source to 115', async () => {
 vi.mocked(api).mockResolvedValue(previewResponse('chosen'))
 const downloaders = [receiver('115', 'pan115_offline'), receiver('quark-one'), receiver('quark-two')]
 const wrapper = mount(SharePreviewDialog, { props: { title: 'Share', resultToken: 'claim', sourceKind: 'quark_share', downloaders } })
 try {
  await flushPromises()
  expect(vi.mocked(api)).not.toHaveBeenCalled()
  expect(wrapper.get('select').findAll('option').map(option => option.element.value)).toEqual(['', 'quark-one', 'quark-two'])
  await wrapper.get('select').setValue('quark-two')
  await flushPromises()
  expect(JSON.parse(String(vi.mocked(api).mock.calls[0]![1]!.body))).toMatchObject({ downloader_id: 'quark-two' })
  await wrapper.setProps({ sourceKind: 'unsupported' })
  await flushPromises()
  expect(vi.mocked(api)).toHaveBeenCalledTimes(1)
  expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(0)
  expect(wrapper.get('select').findAll('option')).toHaveLength(1)
 } finally { wrapper.unmount() }
})
