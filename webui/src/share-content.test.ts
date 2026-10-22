// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ShareContentSummary from '@/components/ShareContentSummary.vue'
import { compactEpisodeSet, parseShareContentAnalysis, type ShareContentAnalysis } from '@/share-content'

const analysis = (): ShareContentAnalysis => ({ status: 'complete', complete: true, groups: [{ title: 'Example', status: 'matched', media_type: 'tv', season: 1, episodes: [1, 3, 10, 11, 12], video_count: 6 }], video_count: 6, ignored_count: 2, unresolved_count: 0, message: '按实际视频文件分析', checked_at: new Date().toISOString(), expires_at: new Date(Date.now() + 60000).toISOString() })

describe('share content display-only facts', () => {
  it('compresses only actual consecutive episodes and deduplicates encodings', () => {
    expect(compactEpisodeSet([12, 1, 3, 11, 10, 3])).toBe('1、3、10–12')
    const parsed = parseShareContentAnalysis({ ...analysis(), groups: [{ ...analysis().groups[0], episodes: [12, 1, 3, 11, 10, 3] }] })!
    const wrapper = mount(ShareContentSummary, { props: { analysis: parsed } })
    expect(wrapper.text()).toContain('第 1、3、10–12 集（共 5 集） · 6 个视频')
    expect(wrapper.text()).not.toContain('第 1–12 集')
    wrapper.unmount()
  })
  it('keeps separate works and seasons, and distinguishes actual reading from metadata matching', () => {
    const value = analysis()
    value.groups.push({ title: 'Other', status: 'unrecognized', media_type: 'tv', season: 2, episodes: [7, 8], video_count: 2 })
    value.unresolved_count = 2
    const wrapper = mount(ShareContentSummary, { props: { analysis: value } })
    expect(wrapper.text()).toContain('已读取分享内容')
    expect(wrapper.text()).toContain('Example · 第 1 季 · 第 1、3、10–12 集')
    expect(wrapper.text()).toContain('Other · 第 2 季 · 第 7–8 集')
    expect(wrapper.text()).toContain('作品待确认')
    expect(wrapper.text()).not.toContain('第 1–12 集')
    wrapper.unmount()
  })
  it('does not turn unavailable or old-Server results into completed file enumeration', () => {
    const wrapper = mount(ShareContentSummary, { props: { analysis: { ...analysis(), status: 'unavailable', complete: false, groups: [], message: '没有可用的同来源分享接收器' } } })
    expect(wrapper.text()).toContain('分享内容读取未完成')
    expect(wrapper.text()).toContain('同来源分享接收器')
    expect(wrapper.text()).not.toContain('已读取分享内容')
    wrapper.unmount()
    const legacy = mount(ShareContentSummary, { props: { received: true } })
    expect(legacy.text()).toContain('标题仅供参考')
    legacy.unmount()
  })
  it('rejects malformed, stale and oversized facts without retaining private fields', () => {
    expect(parseShareContentAnalysis({ ...analysis(), complete: 'true' })).toBeUndefined()
    expect(parseShareContentAnalysis({ ...analysis(), status: 'unavailable' })).toBeUndefined()
    expect(parseShareContentAnalysis({ ...analysis(), expires_at: new Date(Date.now() - 1).toISOString() })).toBeUndefined()
    expect(parseShareContentAnalysis({ ...analysis(), groups: [{ ...analysis().groups[0], episodes: [1, -1] }] })).toBeUndefined()
    expect(parseShareContentAnalysis({ ...analysis(), groups: [{ ...analysis().groups[0], episodes: Array.from({ length: 2001 }, (_, i) => i + 1) }] })).toBeUndefined()
    expect(parseShareContentAnalysis({ ...analysis(), groups: Array.from({ length: 33 }, () => analysis().groups[0]) })).toBeUndefined()
    const parsed = parseShareContentAnalysis({ ...analysis(), provider_id: 'private', url: 'https://example.invalid/private', groups: [{ ...analysis().groups[0], poster_url: 'https://example.invalid/private', provider_id: 'private' }] })!
    expect(JSON.stringify(parsed)).not.toContain('private')
  })
})
