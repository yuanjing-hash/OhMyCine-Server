// @vitest-environment happy-dom

import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MediaWorkContextMenu, { type WorkMenuTarget } from './components/MediaWorkContextMenu.vue'

const request = vi.hoisted(() => vi.fn())
const routePush = vi.hoisted(() => vi.fn())
const notification = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: request }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => false }) }))
vi.mock('@/toast', () => ({ notify: notification }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: routePush }) }))
afterEach(() => { request.mockReset(); routePush.mockReset(); notification.mockReset(); document.body.innerHTML = '' })

const history: WorkMenuTarget = {
  title: '历史电影', kind: 'movie', historyId: 'a'.repeat(64),
  works: [{ library_id: 1, work_id: 'movie-token', library_name: '影视库', file_count: 1 }],
}

describe('media work context menu', () => {
  it('marks only the selected episode watched and supports the inverse', async () => {
    request.mockResolvedValue({ completed: true })
    const wrapper = mount(MediaWorkContextMenu, { attachTo: document.body })
    const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, target: WorkMenuTarget) => Promise<void> }
    const episode: WorkMenuTarget = { ...history, kind: 'series', title: '测试剧', completed: false }
    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), episode)
    const watched = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].find(button => button.textContent === '标记本集已看完')!
    expect(watched).toBeTruthy()
    watched.click()
    await flushPromises()
    expect(request).toHaveBeenLastCalledWith(`/api/v1/media-libraries/history/${history.historyId}/watched`, { method: 'PUT', body: JSON.stringify({ completed: true }), signal: expect.any(AbortSignal) })
    expect(wrapper.emitted('changed')).toHaveLength(1)

    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), { ...episode, completed: true })
    const unwatched = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].find(button => button.textContent === '标记本集未看完')!
    unwatched.click()
    await flushPromises()
    expect(request).toHaveBeenLastCalledWith(`/api/v1/media-libraries/history/${history.historyId}/watched`, { method: 'PUT', body: JSON.stringify({ completed: false }), signal: expect.any(AbortSignal) })
    wrapper.unmount()
  })

  it('keeps the selected watched action available when the server rejects it', async () => {
    request.mockRejectedValue(new Error('记录已失效'))
    const wrapper = mount(MediaWorkContextMenu, { attachTo: document.body })
    const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, target: WorkMenuTarget) => Promise<void> }
    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), history)
    const watched = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].find(button => button.textContent === '标记影片已看完')!
    watched.click()
    await flushPromises()
    expect(wrapper.emitted('changed')).toBeUndefined()
    expect(document.querySelector('.work-context-menu')).not.toBeNull()
    expect(watched.disabled).toBe(false)
    wrapper.unmount()
  })

  it.each([
    { action: 'watched', label: '标记影片已看完' },
    { action: 'delete-history', label: '删除这条观看记录' },
  ])('aborts a closed $action action and ignores its late result after another target opens', async ({ label }) => {
    let settle!: (value: { completed: boolean }) => void
    request.mockImplementation(() => new Promise<{ completed: boolean }>(resolve => { settle = resolve }))
    const wrapper = mount(MediaWorkContextMenu, { attachTo: document.body })
    const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, target: WorkMenuTarget) => Promise<void>; close: (restoreFocus?: boolean) => void }
    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), history)
    const watched = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].find(button => button.textContent === label)!
    watched.click()
    await flushPromises()
    const options = request.mock.calls[0][1] as RequestInit
    menu.close(false)
    expect(options.signal?.aborted).toBe(true)

    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), { ...history, title: '新目标', historyId: 'b'.repeat(64) })
    settle({ completed: true })
    await flushPromises()
    expect(wrapper.emitted('changed')).toBeUndefined()
    expect(notification).not.toHaveBeenCalled()
    expect(document.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe('新目标 操作')
    expect([...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].every(button => !button.disabled)).toBe(true)
    wrapper.unmount()
  })

  it('requires an explicit library choice before opening a multi-library work', async () => {
    const wrapper = mount(MediaWorkContextMenu, { attachTo: document.body })
    const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, target: WorkMenuTarget) => Promise<void> }
    await menu.open(new KeyboardEvent('keydown', { key: 'ContextMenu' }), {
      title: '跨库电影', kind: 'movie', works: [
        { library_id: 1, work_id: 'first', library_name: '库一', file_count: 1 },
        { library_id: 2, work_id: 'second', library_name: '库二', file_count: 2 },
      ],
    })
    const detail = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')]
      .find(button => button.textContent?.includes('查看详情'))!
    detail.click()
    await flushPromises()
    expect(routePush).not.toHaveBeenCalled()
    const libraries = [...document.querySelectorAll<HTMLButtonElement>('[data-work-choice]')]
    expect(libraries).toHaveLength(2)
    libraries[1]!.click()
    await flushPromises()
    expect(routePush).toHaveBeenCalledWith({
      name: 'library-catalog-detail', params: { libraryID: '2', workID: 'second' }, query: undefined,
    })
    wrapper.unmount()
  })
  it('opens from keyboard, restores focus on Escape, and deletes the selected history row', async () => {
    request.mockResolvedValue({ deleted: true })
    const wrapper = mount(MediaWorkContextMenu, { attachTo: document.body })
    const card = document.createElement('button')
    card.textContent = history.title
    document.body.append(card)
    const menu = wrapper.vm as unknown as { open: (event: KeyboardEvent, target: WorkMenuTarget) => Promise<void> }
    card.addEventListener('keydown', event => { if (event.key === 'ContextMenu') void menu.open(event, history) })
    card.focus()
    card.dispatchEvent(new KeyboardEvent('keydown', { key: 'ContextMenu', bubbles: true }))
    await flushPromises()
    const panel = document.querySelector<HTMLElement>('.work-context-menu')!
    expect(panel).toBeTruthy()
    expect(document.activeElement?.getAttribute('role')).toBe('menuitem')
    panel.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await flushPromises()
    expect(document.querySelector('.work-context-menu')).toBeNull()
    expect(document.activeElement).toBe(card)

    card.dispatchEvent(new KeyboardEvent('keydown', { key: 'ContextMenu', bubbles: true }))
    await flushPromises()
    const deleteAction = [...document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')]
      .find(button => button.textContent?.includes('删除这条观看记录'))!
    deleteAction.click()
    await flushPromises()
    expect(request).toHaveBeenCalledWith(`/api/v1/media-libraries/history/${history.historyId}`, { method: 'DELETE', signal: expect.any(AbortSignal) })
    expect(wrapper.emitted('changed')).toHaveLength(1)
    expect(document.querySelector('.work-context-menu')).toBeNull()
    wrapper.unmount()
    card.remove()
  })
})
