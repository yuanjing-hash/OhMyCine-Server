// @vitest-environment happy-dom
import { expect, it, vi, afterEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import SubtitlePolicyEditor from './components/SubtitlePolicyEditor.vue'
import FollowPolicyDefaultsPanel from './components/FollowPolicyDefaultsPanel.vue'
import AcquisitionPreferenceEditor from './components/AcquisitionPreferenceEditor.vue'
import type { SubtitlePolicy } from './follows'
const { request } = vi.hoisted(() => ({ request: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: request }))
const wrappers: VueWrapper[] = []
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); request.mockReset() })
it('preserves a missing legacy policy until the user changes it', async () => {
  const wrapper = mount(SubtitlePolicyEditor); wrappers.push(wrapper)
  expect(wrapper.find('select').element.value).toBe('ignore')
  expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  await wrapper.find('select').setValue('prefer')
  expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ mode: 'prefer', languages: ['zh-Hans', 'zh-Hant'] }])
  expect(wrapper.text()).toContain('发布名称声明不作为')
})
it('submits the shown default with CAS and retains the edited policy on a conflict', async () => {
  request.mockResolvedValueOnce({ revision: 7, subtitles: { mode: 'prefer', languages: ['zh-Hans', 'zh-Hant'] }, updated_at: '' }).mockRejectedValueOnce(new Error('默认订阅策略已被更新，请刷新'))
  const wrapper = mount(FollowPolicyDefaultsPanel, { props: { canUpdate: true } }); wrappers.push(wrapper); await flushPromises()
  wrapper.findComponent(SubtitlePolicyEditor).vm.$emit('update:modelValue', { mode: 'require', languages: ['en'] } satisfies SubtitlePolicy)
  await wrapper.find('form').trigger('submit'); await flushPromises()
  expect(JSON.parse(String(request.mock.calls[1]?.[1].body))).toEqual({ revision: 7, subtitles: { mode: 'require', languages: ['en'] } })
  expect(wrapper.find('[role="alert"]').text()).toContain('已被更新')
  expect(wrapper.find('select').element.value).toBe('require')
})
it('does not expose a save action without settings-update permission', async () => {
  request.mockResolvedValue({ revision: 1, subtitles: { mode: 'prefer', languages: ['zh-Hans'] }, updated_at: '' })
  const wrapper = mount(FollowPolicyDefaultsPanel, { props: { canUpdate: false } }); wrappers.push(wrapper); await flushPromises()
  expect(wrapper.find('fieldset').attributes('disabled')).toBeDefined()
  expect(wrapper.find('button').exists()).toBe(false)
})

it('preserves old snapshot omission until edited and submits the independent global preference', async () => {
  const editor = mount(AcquisitionPreferenceEditor); wrappers.push(editor)
  expect(editor.find('select').element.value).toBe('existing_order')
  expect(editor.emitted('update:modelValue')).toBeUndefined()
  await editor.find('select').setValue('transfer_first')
  expect(editor.emitted('update:modelValue')?.[0]).toEqual(['transfer_first'])
  request.mockResolvedValueOnce({ revision: 9, subtitles: { mode: 'prefer', languages: ['zh-Hans'] }, acquisition_preference: 'transfer_first', updated_at: '' }).mockRejectedValueOnce(new Error('conflict'))
  const defaults = mount(FollowPolicyDefaultsPanel, { props: { canUpdate: true } }); wrappers.push(defaults); await flushPromises()
  await defaults.findComponent(AcquisitionPreferenceEditor).find('select').setValue('existing_order')
  await defaults.find('form').trigger('submit'); await flushPromises()
  expect(JSON.parse(String(request.mock.calls[1]?.[1].body))).toMatchObject({ revision: 9, acquisition_preference: 'existing_order' })
  expect(defaults.findComponent(AcquisitionPreferenceEditor).find('select').element.value).toBe('existing_order')
})
