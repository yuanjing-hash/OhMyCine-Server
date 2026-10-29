// @vitest-environment happy-dom
import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MediaEntryContextMenu from './components/MediaEntryContextMenu.vue'
import type { MediaCatalogEpisode } from './types/api'

const request = vi.hoisted(() => vi.fn())
const push = vi.hoisted(() => vi.fn())
const authority = vi.hoisted(() => ({ allowed: true }))
vi.mock('@/api/client', () => ({ api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => authority.allowed, user: { id: 1 } }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }))
afterEach(() => { request.mockReset(); push.mockReset(); authority.allowed = true; document.body.innerHTML = '' })
const entry: MediaCatalogEpisode = { id: 77, title: 'Episode', season: 1, episode: 2, relative_path: '/Show.S01E02.old.mkv', size: 9, modified_at: '' }
const dto = { id: 'upgrade-id', state: 'prepared', stage: 'choose_version', library_id: 3, entry_id: 77, work_token: 'work', title: 'Show', media_type: 'tv', tmdb_id: 42, season: 1, episode: 2, revision: 1, expires_at: '2099-01-01T00:00:00Z', recycle_supported: true }
function menuButton(text: string) { const button = [...document.querySelectorAll<HTMLButtonElement>('button')].find(item => item.textContent?.trim() === text); if (!button) throw new Error(`Missing ${text}`); return button }
it('prepares one precise episode version and routes with opaque replacement context', async () => {
  request.mockResolvedValue(dto)
  const wrapper = mount(MediaEntryContextMenu, { attachTo: document.body, props: { libraryId: 3, workId: 'work', title: 'Show' } })
  const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, entry: MediaCatalogEpisode) => Promise<void> }
  await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), entry)
  menuButton('洗版 · 查找替代版本').click(); await flushPromises()
  expect(request).toHaveBeenCalledWith('/api/v1/media-libraries/3/catalog/work/entries/77/upgrade-preview', { method: 'POST', signal: expect.any(AbortSignal) })
  expect(document.body.textContent).toContain('Show.S01E02.old.mkv')
  menuButton('查找替代版本').click(); await flushPromises()
  expect(push).toHaveBeenCalledWith({ path: '/discovery/explore', query: { mode: 'resources', identity: 'tmdb', title: 'Show', media_type: 'tv', tmdb_id: '42', upgrade_id: 'upgrade-id' } })
  wrapper.unmount()
})
it('keeps unsupported recycle failure visible and never starts a download', async () => {
  request.mockRejectedValue(new Error('该存储尚未提供可恢复回收能力'))
  const wrapper = mount(MediaEntryContextMenu, { attachTo: document.body, props: { libraryId: 3, workId: 'work', title: 'Show' } })
  await (wrapper.vm as unknown as { open: (event: MouseEvent, entry: MediaCatalogEpisode) => Promise<void> }).open(new MouseEvent('contextmenu'), entry)
  menuButton('洗版 · 查找替代版本').click(); await flushPromises()
  expect(document.querySelector('[role="alert"]')?.textContent).toContain('尚未提供可恢复回收能力')
  expect(menuButton('查找替代版本').disabled).toBe(true)
  expect(push).not.toHaveBeenCalled()
  wrapper.unmount()
})
it('rejects a late prepare result after switching episode scope', async () => {
  let finish!: (value: typeof dto) => void
  request.mockImplementation(() => new Promise(resolve => { finish = resolve }))
  const wrapper = mount(MediaEntryContextMenu, { attachTo: document.body, props: { libraryId: 3, workId: 'work', title: 'Show' } })
  const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, entry: MediaCatalogEpisode) => Promise<void> }
  await menu.open(new KeyboardEvent('keydown'), entry); menuButton('洗版 · 查找替代版本').click(); await flushPromises()
  const options = request.mock.calls[0]![1] as RequestInit
  await wrapper.setProps({ workId: 'different-work' })
  expect(options.signal?.aborted).toBe(true)
  finish(dto); await flushPromises()
  expect(document.querySelector('[role="dialog"]')).toBeNull()
  expect(push).not.toHaveBeenCalled()
  wrapper.unmount()
})
it('does not offer a destructive replacement action without permission', async () => {
  authority.allowed = false
  const wrapper = mount(MediaEntryContextMenu, { attachTo: document.body, props: { libraryId: 3, workId: 'work', title: 'Show' } })
  await (wrapper.vm as unknown as { open: (event: KeyboardEvent, entry: MediaCatalogEpisode) => Promise<void> }).open(new KeyboardEvent('keydown'), entry)
  expect(document.querySelector('[role="menu"]')).toBeNull()
  expect(request).not.toHaveBeenCalled()
  wrapper.unmount()
})
