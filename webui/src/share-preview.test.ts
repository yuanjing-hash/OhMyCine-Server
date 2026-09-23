// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SharePreviewDialog from '@/components/SharePreviewDialog.vue'
import { api } from '@/api/client'
import type { DownloaderSummary } from '@/types/api'
import type { SharePreview } from '@/share-preview'
vi.mock('@/api/client', () => ({ api: vi.fn() }))
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
