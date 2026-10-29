// @vitest-environment happy-dom
import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import LibraryCatalogDetailView from './views/LibraryCatalogDetailView.vue'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true, user: { id: 1 } }) }))
vi.mock('@/toast', () => ({ notify: vi.fn() }))
const wrappers: VueWrapper[] = []
afterEach(() => { for (const wrapper of wrappers.splice(0)) wrapper.unmount(); request.mockReset(); document.body.innerHTML = '' })
async function render(kind: 'series' | 'movie') {
  const files = [{ id: 77, title: 'Show', season: 1, episode: 2, relative_path: '/Show.S01E02.old.mkv', size: 9 }, { id: 88, title: 'Show', season: 1, episode: 2, relative_path: '/Show.S01E02.other.mkv', size: 10 }]
  request.mockImplementation((path: string) => path.endsWith('/upgrade-preview') ? Promise.reject(new Error('fixture: selected version')) : Promise.resolve({ work: { id: 'work', title: 'Show', kind, match_status: 'matched', file_count: 2 }, seasons: kind === 'series' ? [{ number: 1, episodes: files }] : [], files: kind === 'movie' ? files : [] }))
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/library/:libraryID/:workID', component: LibraryCatalogDetailView }] })
  await router.push('/library/3/work'); await router.isReady()
  const wrapper = mount(LibraryCatalogDetailView, { attachTo: document.body, global: { plugins: [router], stubs: { MediaMetadataEditorDialog: true, MediaReorganizationDialog: true } } })
  wrappers.push(wrapper); await flushPromises()
  if (kind === 'series') { const season = wrapper.findAll('button').find(button => button.text().includes('第 1 季'))!; await season.trigger('click') }
  return wrapper
}
it.each(['right-click', 'more', 'keyboard'] as const)('opens the selected episode version using %s', async action => {
  const wrapper = await render('series')
  const row = wrapper.findAll('.semantic-inset').find(item => item.text().includes('other.mkv'))!
  if (action === 'right-click') await row.trigger('contextmenu', { clientX: 100, clientY: 100 })
  else if (action === 'more') await row.get('button').trigger('click')
  else await row.trigger('keydown', { key: 'F10', shiftKey: true })
  await flushPromises()
  const option = document.querySelector<HTMLButtonElement>('[role="menuitem"]')!
  expect(option).not.toBeNull(); option.click(); await flushPromises()
  expect(request.mock.calls.some(([path]) => path === '/api/v1/media-libraries/3/catalog/work/entries/88/upgrade-preview')).toBe(true)
  expect(request.mock.calls.some(([path]) => String(path).includes('/entries/77/upgrade-preview'))).toBe(false)
})
it('offers the same precise action for one selected movie version', async () => {
  const wrapper = await render('movie')
  const row = wrapper.findAll('.semantic-inset').find(item => item.text().includes('old.mkv'))!
  await row.trigger('contextmenu'); await flushPromises()
  document.querySelector<HTMLButtonElement>('[role="menuitem"]')!.click(); await flushPromises()
  expect(request.mock.calls.some(([path]) => path === '/api/v1/media-libraries/3/catalog/work/entries/77/upgrade-preview')).toBe(true)
})
