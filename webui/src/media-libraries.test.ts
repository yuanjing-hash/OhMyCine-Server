import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { defaultVideoExtensions, draftFromLibrary, emptyMediaLibraryDraft, isMediaLibraryDraftValid, mediaLibraryDraftFingerprint, mediaLibrarySourceDisplayPath, payloadFromDraft, presentLibraryStatus, supportsSTRM } from '@/media-libraries'
import type { StorageSummary } from '@/types/api'

function storage(type: string, direct = false, signed = false): StorageSummary {
  return { id: 1, name: 'source', type: type as StorageSummary['type'], root_path: 'hidden', connection_id: null, enabled: true,
    capabilities: { network_drive: false, directory_list: true, watch: true, native_offline_download: false, temporary_direct_url: direct, signed_proxy: signed, change_cursor: false },
    probe: { exists: true, readable: true, available: true, free_bytes: null, total_bytes: null, last_checked_at: '', error_code: '' }, created_at: '', updated_at: '' }
}

describe('media library form boundary', () => {
  it('submits an opaque relative-root token without reconstructing a path', () => {
    const draft = emptyMediaLibraryDraft(1, 2)
    draft.relative_root_token = 'opaque-selection'
    draft.source_path = 'D:\\Media\\Movies'
    const payload = payloadFromDraft(draft, storage('local'))
    expect(payload.relative_root_token).toBe('opaque-selection')
    expect(payload.relative_root).toBeUndefined()
    expect(payload).toMatchObject({ transfer_mode: 'move', conflict_policy: 'ask', movie_filename_template: '{title} ({year})', tv_filename_template: '{title} - S{season:02}E{episode:02}' })
    expect(JSON.stringify(payload)).not.toContain('D:\\Media')
  })

  it('strips STRM state and its picker token for local sources', () => {
    const draft = emptyMediaLibraryDraft(1, 2)
    draft.strm_enabled = true
    draft.strm_local_root_token = 'must-not-leak'
    expect(supportsSTRM(storage('local', true, true))).toBe(false)
    expect(payloadFromDraft(draft, storage('local'))).toMatchObject({ strm_enabled: false })
    expect(payloadFromDraft(draft, storage('local'))).not.toHaveProperty('strm_local_root_token')
  })

  it('requires both direct URL and signed proxy capabilities for cloud STRM', () => {
    expect(supportsSTRM(storage('cloud', true, false))).toBe(false)
    expect(supportsSTRM(storage('cloud', true, true))).toBe(true)
  })

  it('does not submit symlink mode for a 115 media library', () => {
    const draft = emptyMediaLibraryDraft(1, 2)
    draft.transfer_mode = 'symlink'
    expect(payloadFromDraft(draft, storage('pan115')).transfer_mode).toBe('move')
  })

  it('omits legacy media-library intake writes in favor of downloader life-event listening', () => {
    const draft = emptyMediaLibraryDraft(1, 2)
    const payload = payloadFromDraft(draft, storage('pan115'))
    expect(payload).not.toHaveProperty('ingest_enabled')
    expect(payload).not.toHaveProperty('ingest_downloader_id')
    expect(payload).not.toHaveProperty('ingest_relative_root_token')
    expect(JSON.stringify(payload)).not.toContain('ingest_path')
  })

  it('manages one 115 default library without exposing a second intake directory', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    expect(source).toContain('setDefaultIngestLibrary')
    expect(source).toContain('clearDefaultIngestLibrary')
    expect(source).toContain('自动监听默认入库库')
    expect(source).not.toContain('自动摄取的中转目录')
    expect(source).not.toContain('ingest_downloader_id')
  })

  it('presents initialization failure as an error state', () => {
    expect(presentLibraryStatus('initialization_failed')).toEqual({ label: '初始化失败', className: 'status-chip status-chip--error' })
  })

  it('tracks every editable persistent setting without treating picker tokens as configuration', () => {
    const target = { ...storage('pan115', true, true), capabilities: { ...storage('pan115', true, true).capabilities, small_file_upload: true } }
    const original = emptyMediaLibraryDraft(1, 2)
    original.name = '媒体库'
    original.source_path = '/媒体'
    const baseline = mediaLibraryDraftFingerprint(original, target)
    const mutations: Array<(draft: typeof original) => void> = [
      draft => { draft.name = '新名称' },
      draft => { draft.storage_id = 2 },
      draft => { draft.profile_id = 3 },
      draft => { draft.source_path = '/媒体/电视剧' },
      draft => { draft.enabled = false },
      draft => { draft.recursive = false },
      draft => { draft.full_scan_interval_hours = 48 },
      draft => { draft.incremental_minutes = 30 },
      draft => { draft.strm_asset_extra_extensions_text = 'png' },
      draft => { draft.ignore_patterns_text = 'sample' },
      draft => { draft.metadata_language = 'en-US' },
      draft => { draft.metadata_region = 'US' },
      draft => { draft.match_strategy = 'strict' },
      draft => { draft.provider_rate_per_second = 50 },
      draft => { draft.provider_concurrency = 3 },
      draft => { draft.metadata_rate_per_second = 4 },
      draft => { draft.metadata_concurrency = 2 },
      draft => { draft.metadata_artifacts_enabled = false },
      draft => { draft.upload_sidecars = true },
      draft => { draft.transfer_mode = 'copy' },
      draft => { draft.conflict_policy = 'skip' },
      draft => { draft.movie_directory_template = '电影/{title}' },
      draft => { draft.movie_filename_template = '{title}' },
      draft => { draft.tv_directory_template = '电视剧/{title}' },
      draft => { draft.tv_filename_template = '{title}-E{episode:02}' },
    ]
    for (const mutate of mutations) {
      const changed = { ...original }
      mutate(changed)
      expect(mediaLibraryDraftFingerprint(changed, target)).not.toBe(baseline)
    }

    const withTokens = { ...original, relative_root_token: 'opaque-source', strm_local_root_token: 'opaque-strm' }
    expect(mediaLibraryDraftFingerprint(withTokens, target)).toBe(baseline)

    const strm = { ...original, strm_enabled: true, strm_local_path: 'D:\\STRM' }
    const strmBaseline = mediaLibraryDraftFingerprint(strm, target)
    expect(strmBaseline).not.toBe(baseline)
    expect(mediaLibraryDraftFingerprint({ ...strm, strm_local_path: 'E:\\STRM' }, target)).not.toBe(strmBaseline)
  })

  it('disables saving for incomplete or out-of-range drafts', () => {
    const target = storage('pan115', true, true)
    const draft = emptyMediaLibraryDraft(1, 2)
    draft.name = '媒体库'
    draft.source_path = '/媒体'
    expect(isMediaLibraryDraftValid(draft, target)).toBe(true)
    expect(isMediaLibraryDraftValid({ ...draft, name: ' ' }, target)).toBe(false)
    expect(isMediaLibraryDraftValid({ ...draft, provider_concurrency: 0 }, target)).toBe(false)
    expect(isMediaLibraryDraftValid({ ...draft, strm_enabled: true, strm_local_path: '' }, target)).toBe(false)
  })

  it('rehydrates an authoritative saved detail without reusing consumed picker tokens', () => {
    const target = storage('pan115', true, true)
    const draft = emptyMediaLibraryDraft(1, 2)
    draft.relative_root_token = 'consumed-source-token'
    draft.strm_local_root_token = 'consumed-strm-token'
    const rehydrated = draftFromLibrary({
      ...draft,
      id: 9,
      storage_name: 'source',
      profile_name: 'profile',
      profile_revision: 1,
      auto_listen_default: false,
      video_extensions: defaultVideoExtensions,
      strm_asset_default_extensions: [],
      strm_asset_extra_extensions: [],
      strm_asset_effective_extensions: [],
      ignore_patterns: [],
      strm_local_path: 'D:\\STRM',
    } as never, target)
    expect(rehydrated.relative_root_token).toBe('')
    expect(rehydrated.strm_local_root_token).toBe('')
    expect(payloadFromDraft(rehydrated, target)).not.toHaveProperty('relative_root_token')
    expect(payloadFromDraft(rehydrated, target)).not.toHaveProperty('strm_local_root_token')
  })

  it('shows save lifecycle feedback beside the dirty-aware action', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    expect(source).toContain('mediaLibraryDraftFingerprint')
    expect(source).toContain('replaceEditDraft(saved)')
    expect(source).toContain("state: 'saving', message: '正在保存媒体库配置…'")
    expect(source).toContain("state: 'success', message: '保存成功，新的媒体库配置已生效。'")
    expect(source).toContain(':disabled="saving || !editDirty || !editFormValid"')
    expect(source).toContain('aria-live="polite"')
  })

  it('shows durable fast-scan progress and links each run to correlated logs', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const label of ['目录可用 · 识别中', '枚举 / 处理 / 落库 / 去重', '识别进度', '查看本次详细日志']) expect(source).toContain(label)
    for (const stage of ['persist_source_assets', 'persist_recognition', 'persist_entries', 'prune_stale_entries', 'reconcile_tmdb_collections', 'advance_library_generation', 'persist_scan_run', 'record_media_change']) expect(source).toContain(stage)
    expect(source).toContain("scan_run_id: String(runItem.id)")
    expect(source).toContain("run.status === 'catalog_ready'")
    expect(source).toContain("run.status === 'success'")
    expect(source).toContain('状态待确认')
    expect(source).not.toContain('checkpoint_json')
    expect(source).not.toContain('source_fingerprint')
  })

  it('offers safe directory-aware manual recognition before any structure move', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const text of ['所在目录', '手动整理', '搜索标题', '作品类型', '年份（可选）', '搜索 TMDB', '保存后只生成目录移动预览，不会直接移动文件']) expect(source).toContain(text)
    expect(source).toContain('item.source_directory')
    expect(source).toContain('manualRecognitionForm.title.trim()')
    expect(source).toContain("Number(year) < 1888 || Number(year) > 2200")
    expect(source).toContain('await openStructureDiagnostics()')
    expect(source).toContain("recognition_input_invalid: '无法从文件名推断标题，请手动整理'")
    expect(source).not.toContain('item.provider_id')
    expect(source).not.toContain('item.relative_path')
  })

  it('keeps background structure diagnosis read-only and does not restart it while viewing progress', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const text of ['目录结构诊断正在后台', '目录结构诊断系统失败', '诊断全程只读，不会移动文件', 'processed_items', 'classifications.duplicate_target', 'classifications.sidecar_target_conflict']) expect(source).toContain(text)
    expect(source).toContain('async function viewStructureDiagnostics()')
    expect(source).toContain('await showStructureDiagnostics(false)')
    expect(source).toContain('diagnostics.status !== \'issues\'')
    expect(source).toContain('@click="viewStructureDiagnostics">查看诊断进度</button>')
    expect(source).not.toContain('@click="openStructureDiagnostics">查看诊断进度</button>')
  })

  it('keeps every structure-diagnosis resolution entry visible from authoritative counters', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const text of ['可选整理建议', '需要处理', '无需处理', '生成安全整理预览', '重试生成整理预览', '去手动整理', '去规则管理', '视频目标冲突', '伴随文件冲突', '重新检查', '跨分类抽取的最多 100 条代表样本']) expect(source).toContain(text)
    for (const text of ['自动识别失败或无匹配', '目录结构初步检查完成 · 等待识别结果', '等待中的媒体不会计入“需要处理”', '识别完成后系统会自动重新检查', 'recognition_enqueue_failed']) expect(source).toContain(text)
    expect(source).toContain('diagnostics.repairable_count <= 0')
    expect(source).toContain("item.code !== 'missing_season_episode'")
    expect(source).toContain('structureDiagnostics.repairable_count > 0')
    expect(source).toContain('structurePreviewError.value = message(reason)')
    expect(source).not.toContain('structureDiagnostics.issues.some(item => item.repairable)')
  })

  it('shows every available source in a bounded target-conflict summary', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const text of ['当前路径 / 冲突来源', '同一目标的来源', 'issue.conflict_sources', 'issue.conflict_source_count', '个来源已省略']) expect(source).toContain(text)
  })

  it('separates recognition mistakes, catalog duplicates, and real file conflicts', () => {
    const source = readFileSync(new URL('./views/MediaLibrariesView.vue', import.meta.url), 'utf8')
    for (const text of [
      '多个不同作品疑似被识别成同一作品',
      '同一来源事实在目录中重复',
      '多个真实文件会得到同一目标',
      '不要删除来源文件',
      '核对并修正识别',
      "catalogMatch.value = 'review'",
      "catalogMatch.value === 'manual' || catalogMatch.value === 'review' ? 'matched' : 'unrecognized'",
      "item.status === 'matched' ? '修正识别' : '手动整理'",
      '(sourceTitle || item.title).trim()',
    ]) expect(source).toContain(text)
    expect(source).not.toContain('同一目标只能保留一个来源；请在来源侧改名或清理重复文件')
  })

  it('shows persisted storage root and child roots as readable Windows locations', () => {
    const local = { ...storage('local'), root_path: 'D:\\Downloads\\115\\媒体', root_display_path: 'D:\\Downloads\\115\\媒体' }
    const rootLibrary = { relative_root: '/' } as Parameters<typeof mediaLibrarySourceDisplayPath>[0]
    const childLibrary = { relative_root: '/电视剧/国产' } as Parameters<typeof mediaLibrarySourceDisplayPath>[0]
    expect(mediaLibrarySourceDisplayPath(rootLibrary, local)).toBe('D:\\Downloads\\115\\媒体')
    expect(mediaLibrarySourceDisplayPath(childLibrary, local)).toBe('D:\\Downloads\\115\\媒体\\电视剧\\国产')
    expect(draftFromLibrary({
      ...rootLibrary,
      name: 'root',
      video_extensions: [],
      strm_asset_extra_extensions: [],
      ignore_patterns: [],
    } as never, local).source_path).toBe('D:\\Downloads\\115\\媒体')
  })
})
