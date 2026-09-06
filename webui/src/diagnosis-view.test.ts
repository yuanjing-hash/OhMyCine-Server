// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import MediaLibrariesView from './views/MediaLibrariesView.vue'
import { APIError } from './api/client'

const mocks = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/api/client', async original => ({ ...await original<object>(), api: mocks.api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ can: () => true }) }))
const diagnostics = (status = 'issues', processed = 100) => ({ library_id: 1, generation: 1, revision: 'rev', status, processed_items: processed, total_items: 100, issue_count: 2, repairable_count: 0, classifications: { unrecognized: 1, duplicate_target: 1 }, issues: [] })
const issue = { token: 'issue', recognition_token: 'recognition', code: 'media_unrecognized', kind: 'video', state: 'unrecognized', repairable: false, title: '待识别', current_path: 'Season 02/01.mkv', members: [] }
const wrappers: VueWrapper[] = []
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
function button(wrapper: VueWrapper, text: string) { const result = wrapper.findAll('button').find(item => item.text() === text); if (!result) throw Error(`Missing button ${text}`); return result }
async function open() {
  const wrapper = mount(MediaLibrariesView, { attachTo: document.body, global: { stubs: { RouterLink: true, DirectoryPickerDialog: true, MediaReorganizationDialog: true, MediaLibrarySettingsFields: true } } })
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper
}
beforeEach(() => {
  vi.useFakeTimers(); mocks.api.mockReset()
  mocks.api.mockImplementation(async (path: string) => {
    if (path === '/api/v1/media-libraries') return { list: [1, 2].map(id => ({ id, name: `库${id}`, enabled: true, status: 'listening', structure_status: 'issues', storage_id: 1, profile_id: 1, video_extensions: [], ignore_patterns: [], strm_asset_extra_extensions: [] })) }
    if (path.endsWith('/structure')) return diagnostics()
    if (path.endsWith('/structure/selection-status')) return { revision: 'rev', invalid_issue_tokens: [] }
    if (path.includes('/structure/issues?')) return { list: [issue], total: 101, page: Number(new URLSearchParams(path.split('?')[1]).get('page')), page_size: 50 }
    return { list: [] }
  })
})
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); vi.restoreAllMocks(); vi.useRealTimers() })
describe('diagnosis dialog', () => {
  it('filters unrecognized issues inside the dialog', async () => {
    const wrapper = await open()
    await wrapper.findAll('button').find(item => item.text().startsWith('识别失败或无匹配'))!.trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(mocks.api.mock.calls.some(([path]) => path.includes('code=media_unrecognized'))).toBe(true)
  })
  it('automatically previews deterministic video and sidecar repairs without per-row selection', async () => {
    const base = mocks.api.getMockImplementation()!
    const automatic = { ...issue, code: 'path_mismatch', kind: 'sidecar', state: 'pending_repair', repairable: true, title: '影片', current_path: '旧目录/poster.jpg', expected_path: '电影/影片 (1995)/poster.jpg', recognition_token: undefined }
    mocks.api.mockImplementation((path, options, ...args) => {
      if (path.endsWith('/structure')) return Promise.resolve({ ...diagnostics(), repairable_count: 1 })
      if (path.includes('/structure/issues?')) return Promise.resolve({ list: [automatic], total: 1, page: 1, page_size: 50, review_revision: 0, pending_total: 1, handled_total: 0 })
      if (path.endsWith('/selection-preview')) return Promise.resolve({ revision: 'rev', confirmation_token: 'frozen', move_count: 1, recycle_count: 0, issue_count: 1, skipped_count: 0, items: { list: [{ action: 'move', kind: 'sidecar', current_path: automatic.current_path, expected_path: automatic.expected_path }], total: 1, page: 1, page_size: 50 } })
      return base(path, options, ...args)
    })
    const wrapper = await open()
    expect(wrapper.get('[role="dialog"]').text()).toContain('将自动整理')
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('选择整理')
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    const previewCall = mocks.api.mock.calls.find(([path]) => path.endsWith('/selection-preview'))!
    expect(JSON.parse(previewCall[1].body)).toEqual(expect.objectContaining({ include_automatic_repairs: true, selections: [] }))
    expect(wrapper.get('[aria-label="文件变更预览"]').text()).toContain('poster.jpg')
  })
  it('keeps an explicit frozen preview request alive across tab visibility changes', async () => {
    const base = mocks.api.getMockImplementation()!, pending = deferred<object>()
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/selection-preview') ? pending.promise : base(path, ...args))
    const wrapper = await open()
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    const previewCall = mocks.api.mock.calls.find(([path]) => path.endsWith('/selection-preview'))!
    const signal = previewCall[1].signal as AbortSignal
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    document.dispatchEvent(new Event('visibilitychange')); await flushPromises()
    expect(signal.aborted).toBe(false)
    expect(wrapper.get('[role="dialog"]').text()).toContain('正在生成预览')
    pending.resolve({ revision: 'rev', confirmation_token: 'frozen', move_count: 1, recycle_count: 0, issue_count: 1, skipped_count: 0, items: { list: [], total: 1, page: 1, page_size: 50 } })
    await flushPromises()
    expect(wrapper.find('[aria-label="文件变更预览"]').exists()).toBe(true)
    hidden.mockRestore()
  })
  it('shows preview failures beside the preview action with safe retry guidance', async () => {
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/selection-preview') ? Promise.reject(Error('连接超时')) : base(path, ...args))
    const wrapper = await open()
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    const action = button(wrapper, '开始整理（先预览全部确定项）').element.closest('.semantic-success')
    expect(action?.textContent).toContain('生成冻结预览失败：连接超时')
    expect(action?.textContent).toContain('没有移动任何文件')
  })
  it('persists one review choice, shows it in handled, and restores it to pending after undo', async () => {
    const base = mocks.api.getMockImplementation()!
    let reviewRevision = 0
    let reviewAction = ''
    mocks.api.mockImplementation((path, options, ...args) => {
      if (path.includes('/structure/issues?')) {
        const params = new URLSearchParams(path.split('?')[1])
        const handled = params.get('review_state') === 'handled'
        const visible = handled ? Boolean(reviewAction) : !reviewAction
        return Promise.resolve({
          list: visible ? [{ ...issue, ...(reviewAction ? { review_action: reviewAction, review_state: 'draft' } : {}) }] : [],
          total: visible ? 1 : 0,
          page: 1,
          page_size: 50,
          review_revision: reviewRevision,
          pending_total: reviewAction ? 0 : 1,
          handled_total: reviewAction ? 1 : 0,
        })
      }
      if (path.endsWith('/structure/review/issues/issue') && options?.method === 'PUT') {
        const payload = JSON.parse(options.body)
        expect(payload.review_revision).toBe(reviewRevision)
        reviewAction = payload.action
        reviewRevision++
        return Promise.resolve({ review_revision: reviewRevision })
      }
      if (path.endsWith('/structure/review/issues/issue') && options?.method === 'DELETE') {
        const payload = JSON.parse(options.body)
        expect(payload.review_revision).toBe(reviewRevision)
        reviewAction = ''
        reviewRevision++
        return Promise.resolve({ review_revision: reviewRevision })
      }
      return base(path, options, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 1 项')
    expect(wrapper.get('[role="dialog"]').text()).toContain('当前筛选没有待处理问题')
    await button(wrapper, '本次已处理 1').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('当前选择：本次跳过')
    await button(wrapper, '撤销本次选择').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次检测还没有已处理项目')
    await button(wrapper, '待处理 1').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('待识别')
  })
  it('locks every mutation control after a choice is submitted', async () => {
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => {
      if (path.includes('/structure/issues?')) {
        const handled = new URLSearchParams(path.split('?')[1]).get('review_state') === 'handled'
        return Promise.resolve({
          list: handled ? [{ ...issue, review_action: 'skip', review_state: 'submitted' }] : [],
          total: handled ? 1 : 0,
          page: 1,
          page_size: 50,
          review_revision: 2,
          pending_total: 0,
          handled_total: 1,
        })
      }
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '本次已处理 1').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('已提交执行')
    expect(wrapper.get('fieldset').attributes('disabled')).toBeDefined()
    expect(button(wrapper, '重新选择识别').element.closest('fieldset')).toBe(wrapper.get('fieldset').element)
    expect(button(wrapper, '撤销本次选择').attributes('disabled')).toBeDefined()
    expect(button(wrapper, '本次跳过').element.closest('fieldset')).toBe(wrapper.get('fieldset').element)
  })
  it('preserves progress and recovers after a failed status read', async () => {
    const base = mocks.api.getMockImplementation()!
    let reads = 0
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/structure') ? (++reads === 1 ? Promise.resolve(diagnostics('running', 42)) : reads === 2 ? Promise.reject(Error('暂时不可用')) : Promise.resolve(diagnostics())) : base(path, ...args))
    const wrapper = await open()
    await vi.advanceTimersByTimeAsync(750); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('42 / 100')
    expect(wrapper.get('[role="dialog"]').text()).toContain('状态暂时读取失败')
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('发现目录结构问题')
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('暂时不可用')
  })
  it('keeps selections and retries a legacy infrastructure error mislabeled as 401', async () => {
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click')
    const base = mocks.api.getMockImplementation()!
    let reads = 0
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/structure') ? (++reads === 1 ? Promise.reject(new APIError(401, 'INTERNAL_ERROR', '服务器内部错误')) : Promise.resolve(diagnostics())) : base(path, ...args))
    document.dispatchEvent(new Event('visibilitychange')); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('状态暂时读取失败')
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 1 项')
    await vi.advanceTimersByTimeAsync(1500); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('服务器内部错误')
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 1 项')
  })
  it('reconciles an uncertain POST using GET only without hiding the dialog', async () => {
    const base = mocks.api.getMockImplementation()!
    let submitted = false
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/structure/diagnose')) { submitted = true; return Promise.reject(Error('服务器内部错误')) }
      if (submitted && path.endsWith('/structure')) return Promise.resolve(diagnostics('running', 42))
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '重新检查').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('提交结果暂未确认')
    await vi.advanceTimersByTimeAsync(750); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('42 / 100')
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure/diagnose'))).toHaveLength(1)
    expect(wrapper.text()).not.toContain('服务器内部错误')
  })
  it('ignores old status errors after close and reopening the same library', async () => {
    const wrapper = await open(), pending = deferred<ReturnType<typeof diagnostics>>()
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementationOnce(() => pending.promise)
    await button(wrapper, '重新检查').trigger('click'); await flushPromises()
    const signal = mocks.api.mock.calls.at(-1)![1].signal as AbortSignal
    await wrapper.get('[role="dialog"] button').trigger('click')
    await button(wrapper, '查看诊断与处理入口').trigger('click'); await flushPromises()
    pending.reject(Error('old failure')); await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(wrapper.get('[role="dialog"]').text()).toContain('发现目录结构问题')
    expect(wrapper.text()).not.toContain('old failure')
    mocks.api.mockImplementation(base)
  })
  it('does not let delayed issue pages overwrite a newer filter', async () => {
    const wrapper = await open(), pending = deferred<object>()
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => path.includes('code=duplicate_target') ? pending.promise : base(path, ...args))
    await button(wrapper, '视频目标冲突 1').trigger('click'); await flushPromises()
    await button(wrapper, '识别失败或无匹配 1').trigger('click'); await flushPromises()
    pending.resolve({ list: [{ ...issue, title: '旧页面不应出现' }], total: 1, page: 1, page_size: 50 }); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('旧页面不应出现')
    expect(wrapper.get('[role="dialog"]').text()).toContain('待识别')
  })
  it('aborts status reads across library switches and never adopts the old result', async () => {
    const wrapper = await open(), pending = deferred<object>()
    mocks.api.mockImplementationOnce(() => pending.promise)
    await button(wrapper, '重新检查').trigger('click'); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().startsWith('库2'))!.trigger('click'); await flushPromises()
    await button(wrapper, '查看诊断与处理入口').trigger('click'); await flushPromises()
    pending.resolve(diagnostics('running', 999)); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('999')
  })
  it('pauses requests while hidden and recovers when visible, then tears down on unmount', async () => {
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/structure') ? Promise.resolve(diagnostics('running', 42)) : base(path, ...args))
    const wrapper = await open()
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    document.dispatchEvent(new Event('visibilitychange'))
    const count = mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure')).length
    await vi.advanceTimersByTimeAsync(30000)
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure'))).toHaveLength(count)
    hidden.mockReturnValue(false); document.dispatchEvent(new Event('visibilitychange')); await flushPromises()
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure'))).toHaveLength(count + 1)
    wrapper.unmount(); wrappers.splice(wrappers.indexOf(wrapper), 1)
    await vi.advanceTimersByTimeAsync(30000)
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure'))).toHaveLength(count + 1)
    hidden.mockRestore()
  })
  it('opens exact recognition and preserves only unaffected cross-page choices after issue tokens change', async () => {
    const base = mocks.api.getMockImplementation()!
    let saved = false
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/recognitions/recognition')) return Promise.resolve({ token: 'recognition', title: '目标', media_type: 'tv', source_directory: '哆啦A梦 (2005)', source_summary: '01.mkv', status: 'unrecognized' })
      if (path.includes('/tmdb-candidates?')) return Promise.resolve({ list: [{ id: 123, title: '正确作品', media_type: 'tv', release_year: 2005 }] })
      if (path.endsWith('/override')) { saved = true; return Promise.resolve({ token: 'recognition', title: '正确作品', status: 'matched', media_type: 'tv', release_year: 2005, tmdb_id: 123, manual_override: true }) }
      if (path.endsWith('/structure/review/recognitions/recognition')) {
        expect(JSON.parse(args[0].body).review_revision).toBe(7)
        return Promise.resolve({ review_revision: 8 })
      }
      if (path.endsWith('/structure/selection-status')) return Promise.resolve({ revision: 'new-rev', invalid_issue_tokens: ['issue'] })
      if (path.includes('/structure/issues?')) {
        const page = Number(new URLSearchParams(path.split('?')[1]).get('page'))
        const unrecognized = new URLSearchParams(path.split('?')[1]).get('code') === 'media_unrecognized'
        return Promise.resolve({ list: saved && page === 2 && unrecognized ? [] : [page === 1 ? { ...issue, token: 'unaffected', title: '其他作品' } : saved ? { ...issue, token: 'replacement', code: 'path_mismatch', title: '正确作品', state: 'manual_identity_resolved', repairable: true } : issue], total: 101, page, page_size: 50, review_revision: saved ? 7 : 0 })
      }
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '识别失败或无匹配 1').trigger('click'); await flushPromises()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '下一页').trigger('click'); await flushPromises()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '手动识别此项').trigger('click'); await flushPromises()
    expect(mocks.api.mock.calls.some(([path]) => path.endsWith('/recognitions/recognition'))).toBe(true)
    await button(wrapper, '取消并返回诊断').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('第 2 / 3 页')
    expect(wrapper.get('[role="dialog"]').text()).toContain('已保存到本次工作区：本次跳过')
    await button(wrapper, '手动识别此项').trigger('click'); await flushPromises()
    await wrapper.get('[role="dialog"] form').trigger('submit'); await flushPromises()
    await button(wrapper, '正确作品 · 2005 · TMDB 123 · 保存此识别').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('作品身份已保存，文件尚未整理')
    expect(wrapper.get('[role="dialog"]').text()).toContain('正确作品')
    expect(wrapper.get('[aria-label="刚保存的识别结果"]').text()).toContain('TMDB 123')
    expect(wrapper.get('[role="dialog"]').text()).toContain('当前筛选没有待处理问题')
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('已保存到本次工作区：本次跳过')
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 1 项')
    expect(wrapper.get('[role="dialog"]').text()).toContain('1 项旧问题已变化')
    expect(wrapper.get('[role="dialog"]').text()).toContain('第 2 / 3 页')
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure/diagnose') || path.endsWith('/selection-repair'))).toHaveLength(0)
    const lastPage = mocks.api.mock.calls.filter(([path]) => path.includes('/structure/issues?')).at(-1)![0]
    expect(lastPage).toContain('page=2'); expect(lastPage).toContain('code=media_unrecognized')
    const validation = mocks.api.mock.calls.find(([path]) => path.endsWith('/structure/selection-status'))!
    expect(JSON.parse(validation[1].body)).toEqual({ issue_tokens: ['unaffected', 'issue'] })
    await button(wrapper, '上一页').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('其他作品')
    expect(wrapper.get('[role="dialog"]').text()).toContain('已保存到本次工作区：本次跳过')
  })
  it('explains retain-all and shows every final filename through a paged frozen preview', async () => {
    const base = mocks.api.getMockImplementation()!
    const file = { action: 'move', kind: 'video', current_path: '01 (1).mkv', expected_path: 'S02E01 (2).mkv' }
    mocks.api.mockImplementation((path, ...args) => {
      if (path.includes('/structure/issues?')) return Promise.resolve({ list: [{ ...issue, code: 'duplicate_target', state: 'needs_attention', members: [{ token: 'a', source_path: '01.mkv' }, { token: 'b', source_path: '01 (1).mkv' }] }], total: 1, page: 1, page_size: 50 })
      if (path.endsWith('/selection-preview')) return Promise.resolve({ revision: 'rev', confirmation_token: 'frozen', move_count: 51, recycle_count: 0, issue_count: 1, skipped_count: 0, items: { list: [file], total: 51, page: 1, page_size: 50 } })
      if (path.endsWith('/selection-preview/items')) return Promise.resolve({ list: [{ ...file, expected_path: 'S02E02 (2).mkv' }], total: 51, page: 2, page_size: 50 })
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '保留全部文件（自动区分重名）').trigger('click')
    expect(wrapper.get('[role="dialog"]').text()).toContain('这不代表已证明编码或内容不同')
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    const previewCall = mocks.api.mock.calls.find(([path]) => path.endsWith('/selection-preview'))!
    expect(JSON.parse(previewCall[1].body)).toEqual(expect.objectContaining({ include_automatic_repairs: true, selections: [], bulk_actions: [] }))
    expect(wrapper.get('[aria-label="文件变更预览"]').text()).toContain('S02E01 (2).mkv')
    await button(wrapper, '预览下一页').trigger('click'); await flushPromises()
    expect(wrapper.get('[aria-label="文件变更预览"]').text()).toContain('S02E02 (2).mkv')
    const call = mocks.api.mock.calls.find(([path]) => path.endsWith('/selection-preview/items'))!
    expect(JSON.parse(call[1].body)).toEqual({ confirmation_token: 'frozen', page: 2, page_size: 50 })
    expect(call[0]).not.toContain('frozen')
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/selection-repair'))).toHaveLength(0)
  })
  it('does not guess a recognition from a title when the issue lacks an opaque token', async () => {
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => path.includes('/structure/issues?') ? Promise.resolve({ list: [{ ...issue, recognition_token: undefined }], total: 1, page: 1, page_size: 50 }) : base(path, ...args))
    const wrapper = await open()
    await button(wrapper, '手动识别此项').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('不会按标题猜测作品')
    expect(mocks.api.mock.calls.some(([path]) => path.includes('/recognitions'))).toBe(false)
  })
  it('ignores a late manual save after closing and opening a fresh diagnosis window', async () => {
    const base = mocks.api.getMockImplementation()!, pending = deferred<object>()
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/recognitions/recognition')) return Promise.resolve({ token: 'recognition', title: '目标', media_type: 'tv', source_directory: '目标目录', source_summary: '01.mkv' })
      if (path.includes('/tmdb-candidates?')) return Promise.resolve({ list: [{ id: 1, title: '候选', media_type: 'tv' }] })
      if (path.endsWith('/override')) return pending.promise
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '手动识别此项').trigger('click'); await flushPromises()
    await wrapper.get('[role="dialog"] form').trigger('submit'); await flushPromises()
    await button(wrapper, '候选 · 年份未知 · TMDB 1 · 保存此识别').trigger('click'); await flushPromises()
    const signal = mocks.api.mock.calls.find(([path]) => path.endsWith('/override'))![1].signal as AbortSignal
    await wrapper.get('[role="dialog"] button').trigger('click'); await flushPromises()
    await button(wrapper, '查看诊断与处理入口').trigger('click'); await flushPromises()
    pending.resolve({}); await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(wrapper.get('[role="dialog"]').text()).not.toContain('作品身份已保存，文件尚未整理')
  })
  it('retries failed draft reconciliation without repeating recognition save and keeps preview blocked meanwhile', async () => {
    const base = mocks.api.getMockImplementation()!
    let validations = 0
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/recognitions/recognition')) return Promise.resolve({ token: 'recognition', title: '目标', media_type: 'tv', source_directory: '目标目录', source_summary: '01.mkv' })
      if (path.includes('/tmdb-candidates?')) return Promise.resolve({ list: [{ id: 1, title: '候选', media_type: 'tv' }] })
      if (path.endsWith('/override')) return Promise.resolve({})
      if (path.endsWith('/structure/selection-status')) return ++validations === 1 ? Promise.reject(Error('读取暂时失败')) : Promise.resolve({ revision: 'new-rev', invalid_issue_tokens: ['issue'] })
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '手动识别此项').trigger('click'); await flushPromises()
    await wrapper.get('[role="dialog"] form').trigger('submit'); await flushPromises()
    await button(wrapper, '候选 · 年份未知 · TMDB 1 · 保存此识别').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('身份已保存，但草稿核对暂未完成')
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 1 项')
    expect(button(wrapper, '开始整理（先预览全部确定项）').attributes('disabled')).toBeDefined()
    await button(wrapper, '重新核对草稿').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('本次已处理 0 项')
    expect(button(wrapper, '开始整理（先预览全部确定项）').attributes('disabled')).toBeUndefined()
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/override'))).toHaveLength(1)
    expect(mocks.api.mock.calls.filter(([path]) => path.endsWith('/structure/selection-status'))).toHaveLength(2)
  })
  it('cannot confirm when frozen preview files are unavailable or stale', async () => {
    const base = mocks.api.getMockImplementation()!
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/selection-preview')) return Promise.resolve({ revision: 'rev', confirmation_token: 'frozen', move_count: 0, recycle_count: 0, issue_count: 1, skipped_count: 1 })
      if (path.endsWith('/selection-preview/items')) return Promise.reject(new APIError(409, 'conflict', '预览已过期，请重新生成'))
      return base(path, ...args)
    })
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    expect(wrapper.get('[aria-label="文件变更预览"]').text()).toContain('预览已过期')
    expect(button(wrapper, '确认执行预览').attributes('disabled')).toBeDefined()
  })
  it('discards a delayed preview after the selection changes', async () => {
    const base = mocks.api.getMockImplementation()!, pending = deferred<object>()
    mocks.api.mockImplementation((path, ...args) => path.endsWith('/selection-preview') ? pending.promise : base(path, ...args))
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    await button(wrapper, '全部冲突跳过').trigger('click')
    pending.resolve({ confirmation_token: 'stale' }); await flushPromises()
    expect(wrapper.find('[aria-label="文件变更预览"]').exists()).toBe(false)
    expect(button(wrapper, '开始整理（先预览全部确定项）').attributes('disabled')).toBeUndefined()
  })
  it('uses Escape to return from recognition first, then close and restore trigger focus', async () => {
    const wrapper = await open()
    await wrapper.get('[role="dialog"] button').trigger('click'); await flushPromises()
    const trigger = button(wrapper, '查看诊断与处理入口')
    ;(trigger.element as HTMLElement).focus()
    await trigger.trigger('click'); await flushPromises()
    expect(document.activeElement).toBe(wrapper.get('[role="dialog"] button').element)
    const manual = button(wrapper, '手动识别此项')
    ;(manual.element as HTMLElement).focus()
    await manual.trigger('click'); await flushPromises()
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' })); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(document.activeElement).toBe(manual.element)
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' })); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(document.activeElement).toBe(trigger.element)
  })
  it('does not close a different library dialog when an old repair finishes', async () => {
    const base = mocks.api.getMockImplementation()!, pending = deferred<object>()
    mocks.api.mockImplementation((path, ...args) => {
      if (path.endsWith('/selection-preview')) return Promise.resolve({ revision: 'rev', confirmation_token: 'frozen', move_count: 0, recycle_count: 0, issue_count: 1, skipped_count: 1, items: { list: [], total: 0, page: 1, page_size: 50 } })
      if (path.endsWith('/selection-repair')) return pending.promise
      return base(path, ...args)
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const wrapper = await open()
    await button(wrapper, '本次跳过').trigger('click')
    await button(wrapper, '开始整理（先预览全部确定项）').trigger('click'); await flushPromises()
    await button(wrapper, '确认执行预览').trigger('click'); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().startsWith('库2'))!.trigger('click'); await flushPromises()
    await button(wrapper, '查看诊断与处理入口').trigger('click'); await flushPromises()
    pending.resolve({ total_items: 999 }); await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('已提交 999')
  })
})
