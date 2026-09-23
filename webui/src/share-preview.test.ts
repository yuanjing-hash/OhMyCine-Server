// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SharePreviewDialog from '@/components/SharePreviewDialog.vue'
import { api, APIError } from '@/api/client'
import type { DownloaderSummary } from '@/types/api'
import { shareValidationLabel, type ShareValidation, type SharePreview } from '@/share-preview'
vi.mock('@/api/client', async importOriginal => ({ ...await importOriginal<typeof import('@/api/client')>(), api: vi.fn() }))
describe('share preview selection', () => {
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
