// @vitest-environment happy-dom
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, expect, it, vi } from 'vitest'
import QuarkIntakeSettings from '@/components/QuarkIntakeSettings.vue'

const mocks = vi.hoisted(() => ({ api: vi.fn(), allowed: true }))
vi.mock('@/api/client', () => ({ api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => mocks.allowed }) }))
beforeEach(() => { mocks.api.mockReset(); mocks.allowed = true })
const settings = { enabled: false, downloader_id: '', directory: '' }
it('requires an explicit opt-in and persists the selected intake without changing library settings', async () => {
  mocks.api.mockImplementation(async (path: string, options: RequestInit) => path.endsWith('/downloaders') ? { list: [{ id: 'q', type: 'quark_share', enabled: true, name: '夸克', provider_directory_path: '/接收' }], total: 1 } : options.method === 'PUT' ? { enabled: true, downloader_id: 'q', directory: '/接收' } : settings)
  const w = mount(QuarkIntakeSettings, { props: { libraryId: 1 } }); await flushPromises()
  expect((w.get('input').element as HTMLInputElement).checked).toBe(false)
  expect(w.get('select').attributes('disabled')).toBeDefined()
  await w.get('input').setValue(true); await w.get('select').setValue('q'); await w.get('button').trigger('click'); await flushPromises()
  const call = mocks.api.mock.calls.find(([, options]) => options.method === 'PUT')
  expect(call?.[0]).toBe('/api/v1/media-libraries/1/quark-intake')
  expect(JSON.parse(call?.[1].body)).toEqual({ enabled: true, downloader_id: 'q' })
  expect(w.text()).toContain('已启用接收目录自动接管'); w.unmount()
})
it('does not show edit controls without authority or accept a stale previous-library response', async () => {
  mocks.allowed = false
  let resolve!: (value: unknown) => void
  mocks.api.mockImplementation((path: string) => path.includes('/1/') ? new Promise(r => { resolve = r }) : Promise.resolve(settings))
  const w = mount(QuarkIntakeSettings, { props: { libraryId: 1 } })
  await w.setProps({ libraryId: 2 }); await flushPromises(); resolve({ enabled: true, downloader_id: 'stale', directory: '/旧目录' }); await flushPromises()
  expect(w.find('select').exists()).toBe(false)
  expect(w.text()).not.toContain('/旧目录')
  expect((w.get('input').element as HTMLInputElement).checked).toBe(false)
  expect(mocks.api.mock.calls.some(([path]) => path.endsWith('/downloaders'))).toBe(false)
  w.unmount()
})
