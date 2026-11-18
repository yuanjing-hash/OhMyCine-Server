// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import AppLayout from './layouts/AppLayout.vue'
import ExploreView from './views/ExploreView.vue'

const request = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))

const title = '孤单灿烂的神'
const work = { provider: 'tmdb', provider_id: '67915', media_type: 'tv', tmdb_id: 67915, title: '孤单又灿烂的神：鬼怪' }
let wrapper: VueWrapper

async function render(path: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: AppLayout, children: [
      { path: '', component: { template: '<div>首页</div>' } },
      { path: 'discovery/explore', component: ExploreView }
    ] }]
  })
  await router.push(path)
  wrapper = mount(AppLayout, { global: { plugins: [router], stubs: { ThemeToggle: true, NotificationCenter: true } } })
  await flushPromises()
  return router
}

async function submitFromTopbar(query: string) {
  await wrapper.get('.search-trigger').trigger('click')
  await wrapper.get('#global-media-search').setValue(query)
  await wrapper.get('#global-tool-panel form').trigger('submit')
  await flushPromises()
}

beforeEach(() => {
  request.mockImplementation(async (path: string) => {
    const url = new URL(path, 'https://server.test')
    if (url.pathname !== '/api/v1/discovery/media-search') throw new Error('Unexpected request ' + path)
    const query = url.searchParams.get('query')
    return { query, media_type: url.searchParams.get('media_type'), page: 1, total_pages: 1, items: query === title ? [work] : [] }
  })
})
afterEach(() => { wrapper?.unmount(); request.mockReset() })

it('finds a TV title from home without retaining a movie filter', async () => {
  await render('/')
  await submitFromTopbar(title)
  expect(wrapper.text()).toContain(work.title)
  expect(request).toHaveBeenCalledTimes(1)
  expect(new URL(request.mock.calls[0]![0], 'https://server.test').searchParams.get('media_type')).toBe('all')
})

it('finds a TV title on a reused Explore page with a prior movie filter', async () => {
  await render('/discovery/explore?query=old&media_type=movie')
  await submitFromTopbar(title)
  expect(wrapper.text()).toContain(work.title)
  expect(request).toHaveBeenCalledTimes(2)
  expect(new URL(request.mock.calls[1]![0], 'https://server.test').searchParams.get('media_type')).toBe('all')
})

it('submits a top-bar query that matches an unsubmitted Explore draft', async () => {
  const router = await render('/discovery/explore')
  await wrapper.get('#media-search-query').setValue(title)
  expect(request).not.toHaveBeenCalled()
  await submitFromTopbar(title)
  expect(router.currentRoute.value.query.query).toBe(title)
  expect(request).toHaveBeenCalledTimes(1)
  expect(wrapper.text()).toContain(work.title)
})

it('updates the URL after a local form search without repeating the request', async () => {
  const router = await render('/discovery/explore')
  const input = wrapper.get('#media-search-query')
  await input.setValue(title)
  await wrapper.get('form.panel').trigger('submit')
  await flushPromises()
  expect(router.currentRoute.value.query.query).toBe(title)
  expect(request).toHaveBeenCalledTimes(1)
  expect(wrapper.text()).toContain(work.title)
})
