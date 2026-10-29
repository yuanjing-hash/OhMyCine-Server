// @vitest-environment happy-dom
import { expect, it, vi, afterEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import SubtitlePolicyEditor from './components/SubtitlePolicyEditor.vue'
import FollowPolicyDefaultsPanel from './components/FollowPolicyDefaultsPanel.vue'
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
