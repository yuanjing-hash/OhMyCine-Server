// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import StorageView from './views/StorageView.vue'
const mocks = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/api/client', () => ({ api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
vi.mock('@/toast', () => ({ notify: vi.fn() }))
describe('cookie-only save', () => {
 it('does not patch or probe an unchanged storage after saving the credential', async () => {
  const connection = { id: 1, name: '115', provider: 'pan115', revision: 2, enabled: true, credential_configured: true, recycle_cleanup_enabled: false, recycle_cleanup_cron: '0 */7 * * *' }
  const storage = { id: 1, name: '媒体', type: 'pan115', connection_id: 1, enabled: true, root_path: '/', root_display_path: '/', probe: { error_code: '', status: 'ready' } }
  mocks.api.mockImplementation(async (path: string, init?: RequestInit) => {
   if (init?.method === 'PATCH') return { ...connection, revision: 3 }
   if (path === '/api/v1/storages') return { list: [storage] }
   return { list: [connection] }
  })
  const wrapper = mount(StorageView, { global: { stubs: { DirectoryPickerDialog: true, SecretInput: { props: ['modelValue'], emits: ['update:modelValue'], template: '<textarea :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' } } } }); await flushPromises()
  await wrapper.get('textarea').setValue('synthetic-test-cookie')
  await wrapper.get('form').trigger('submit'); await flushPromises()
  const writes = mocks.api.mock.calls.filter(call => call[1]?.method === 'PATCH')
  expect(writes).toHaveLength(1); expect(writes[0]?.[0]).toBe('/api/v1/connections/1')
  expect(wrapper.get('textarea').element.value).toBe('');wrapper.unmount()
 })
 it('keeps the saved connection revision when a subsequent directory save fails', async () => {
  const connection = { id: 1, name: '115', provider: 'pan115', revision: 2, enabled: true, credential_configured: true, recycle_cleanup_enabled: false, recycle_cleanup_cron: '0 */7 * * *' }
  const storage = { id: 1, name: '媒体', type: 'pan115', connection_id: 1, enabled: true, root_path: '/', probe: { error_code: '', readable: true } }
  mocks.api.mockReset()
  mocks.api.mockImplementation(async (path: string, init?: RequestInit) => {
    if (init?.method === 'PATCH' && path.startsWith('/api/v1/storages')) throw new Error('目录暂不可用')
    if (init?.method === 'PATCH') return { ...connection, revision: 3 }
    return { list: path === '/api/v1/storages' ? [storage] : [connection] }
  })
  const wrapper = mount(StorageView, { global: { stubs: { DirectoryPickerDialog: true, SecretInput: { props: ['modelValue'], emits: ['update:modelValue'], template: '<textarea :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' } } } }); await flushPromises()
  await wrapper.get('form input').setValue('新名称')
  await wrapper.get('textarea').setValue('synthetic-one')
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(wrapper.get('textarea').element.value).toBe('')
  await wrapper.get('textarea').setValue('synthetic-two')
  await wrapper.get('form').trigger('submit'); await flushPromises()
  const writes = mocks.api.mock.calls.filter(call => call[0] === '/api/v1/connections/1' && call[1]?.method === 'PATCH')
  expect(JSON.parse(writes[1]![1].body).revision).toBe(3)
  wrapper.unmount()
 })
})
