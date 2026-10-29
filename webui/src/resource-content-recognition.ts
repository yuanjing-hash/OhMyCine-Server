import { computed, ref, watch, type Ref } from 'vue'
import { api } from '@/api/client'
import { coverageStatusLabel, discoveryCoveragePath, normalizeMediaCoverage, type MediaCoverage } from '@/discovery'
import { torrentRecognitionPath, type TorrentRecognitionResult, type TorrentSearchResult } from '@/sites'
import { createOwnedReadQueue, type OwnedReadTask } from '@/owned-read-queue'
import { parseResourceContent, resourceContentPreviewPath, type ResourceContentPreview } from '@/resource-content'
import { isCloudContentResult } from '@/share-content'

type LibraryState = { label: string; detail: string; present: boolean }
interface RecognitionState {
  recognitions: Ref<Record<string, TorrentRecognitionResult>>
  recognitionErrors: Ref<Record<string, string>>
  libraryStates: Ref<Record<string, LibraryState>>
}

export function useResourceContentRecognition(items: Ref<TorrentSearchResult[]>, canReadLibrary: () => boolean, state: RecognitionState) {
  const contentPreviews = ref<Record<string, ResourceContentPreview>>({})
  const contentErrors = ref<Record<string, string>>({})
  const titleLane = createOwnedReadQueue<TorrentRecognitionResult>(4, 30000, 'title')
  const cloudLane = createOwnedReadQueue<ResourceContentPreview>(2, 65000, 'cloud-content')
  const torrentLane = createOwnedReadQueue<ResourceContentPreview>(2, 65000, 'torrent-content')
  const magnetLane = createOwnedReadQueue<ResourceContentPreview>(2, 65000, 'magnet-content')
  const contentLanes = [cloudLane, torrentLane, magnetLane]
  const coverageLane = createOwnedReadQueue<MediaCoverage>(4, 30000, 'coverage')
  let titleRevisions = new Map<string, number>()
  let contentRevisions = new Map<string, number>()
  const coverageRevisions = new Map<string, number>()
  const priority = new Set<string>()

  const currentItems = () => items.value.filter(item => item.content_inspection && Date.parse(item.expires_at) > Date.now())
  const identity = (result?: TorrentRecognitionResult) => result?.status === 'matched' && result.tmdb_id && result.media_type ? `${result.media_type}:${result.tmdb_id}` : ''
  function coverageResult(item: TorrentSearchResult) {
    const title = state.recognitions.value[item.token]
    if (identity(title)) return title
    const groups = contentPreviews.value[item.token]?.analysis?.groups
    const actual = groups?.length === 1 ? groups[0] : undefined
    if (actual?.status === 'matched' && actual.tmdb_id && actual.media_type) return { engine_version: '', specifications: {}, ...actual } as TorrentRecognitionResult
    return title
  }
  function updateCoverage() {
    const tasks: OwnedReadTask<MediaCoverage>[] = []
    for (const item of currentItems()) {
      const result = coverageResult(item)
      const key = identity(result)
      if (!key) {
        if (result) state.libraryStates.value[item.token] = { label: '库内状态待确认', detail: '尚未核实作品身份；实际文件名仍可查看', present: false }
        continue
      }
      if (!canReadLibrary()) { state.libraryStates.value[item.token] = { label: '无权查看库内状态', detail: '需要媒体库读取权限', present: false }; continue }
      state.libraryStates.value[item.token] ??= { label: '正在查询库内状态…', detail: '', present: false }
      tasks.push({ key: `${key}:${coverageRevisions.get(key) ?? 0}`,
        run: signal => api<unknown>(discoveryCoveragePath(result!.media_type!, result!.tmdb_id!), { signal }).then(normalizeMediaCoverage),
        resolve: value => {
          for (const owner of currentItems()) if (identity(coverageResult(owner)) === key) state.libraryStates.value[owner.token] = { label: value.status === 'missing' ? '未入库' : coverageStatusLabel(value.status), detail: '依据当前账号可见媒体库的扫描事实；作品级状态', present: value.status === 'present' }
        },
        reject: () => {
          for (const owner of currentItems()) if (identity(coverageResult(owner)) === key) state.libraryStates.value[owner.token] = { label: '库内状态暂不可用', detail: '查询失败或超时，不代表未入库', present: false }
        },
      })
    }
    coverageLane.update(tasks)
  }
  function reconcile() {
    const values = currentItems()
    const ordered = [...values].sort((a, b) => Number(priority.has(b.token)) - Number(priority.has(a.token)))
    titleLane.update(ordered.map(item => ({
      key: `${item.token}:${titleRevisions.get(item.token) ?? 0}`,
      run: signal => state.recognitions.value[item.token] ? Promise.resolve(state.recognitions.value[item.token]!) : api<TorrentRecognitionResult>(torrentRecognitionPath, { method: 'POST', body: JSON.stringify({ result_token: item.token, title_only: true }), signal }),
      resolve: result => {
        const content = state.recognitions.value[item.token]?.share_analysis
        state.recognitions.value[item.token] = { ...result, share_analysis: content ?? result.share_analysis }
        delete state.recognitionErrors.value[item.token]
        priority.delete(item.token)
        updateCoverage()
      },
      reject: reason => { state.recognitionErrors.value[item.token] = reason instanceof Error ? reason.message : '标题识别暂不可用；实际文件检查继续' },
    })))
    const contentTask = (item: TorrentSearchResult): OwnedReadTask<ResourceContentPreview> => ({
      key: `${item.token}:${contentRevisions.get(item.token) ?? 0}`,
      run: async signal => {
        const response = await api<unknown>(resourceContentPreviewPath, { method: 'POST', body: JSON.stringify({ result_token: item.token }), signal })
        const preview = parseResourceContent(response)
        if (!preview) throw new Error('实际文件清单无效或已过期，请重新读取')
        return preview
      },
      resolve: preview => { contentPreviews.value[item.token] = preview; delete contentErrors.value[item.token]; updateCoverage() },
      reject: reason => { contentErrors.value[item.token] = reason instanceof Error ? reason.message : '实际文件读取失败，可独立重试' },
    })
    const lane = (item: TorrentSearchResult) => isCloudContentResult(item) ? cloudLane : item.payload_kind === 'magnet' || item.result_type === 'magnet' ? magnetLane : torrentLane
    for (const owner of contentLanes) owner.update(ordered.filter(item => lane(item) === owner).map(contentTask))
    updateCoverage()
  }
  const stop = watch(items, () => {
    const tokens = new Set(currentItems().map(item => item.token))
    for (const values of [contentPreviews.value, contentErrors.value]) for (const token of Object.keys(values)) if (!tokens.has(token)) delete values[token]
    titleRevisions = new Map([...titleRevisions].filter(([token]) => tokens.has(token)))
    contentRevisions = new Map([...contentRevisions].filter(([token]) => tokens.has(token)))
    reconcile()
  })
  function retryContent(token: string) {
    if (!currentItems().some(item => item.token === token)) return false
    contentRevisions.set(token, (contentRevisions.get(token) ?? 0) + 1)
    delete contentPreviews.value[token]; delete contentErrors.value[token]
    priority.add(token); reconcile(); return true
  }
  function retry(token: string) {
    if (!currentItems().some(item => item.token === token)) return false
    const work = identity(coverageResult(currentItems().find(item => item.token === token)!))
    if (work) coverageRevisions.set(work, (coverageRevisions.get(work) ?? 0) + 1)
    titleRevisions.set(token, (titleRevisions.get(token) ?? 0) + 1)
    delete state.recognitions.value[token]; delete state.recognitionErrors.value[token]; delete state.libraryStates.value[token]
    return retryContent(token)
  }
  function acceptManual(token: string, result: TorrentRecognitionResult) {
    if (!currentItems().some(item => item.token === token)) return
    titleRevisions.set(token, (titleRevisions.get(token) ?? 0) + 1)
    state.recognitions.value[token] = result
    delete state.recognitionErrors.value[token]; delete state.libraryStates.value[token]
    retryContent(token)
  }
  function reset() {
    titleLane.reset(); contentLanes.forEach(lane => lane.reset()); coverageLane.reset()
    titleRevisions.clear(); contentRevisions.clear(); coverageRevisions.clear(); priority.clear()
    contentPreviews.value = {}; contentErrors.value = {}
  }
  function dispose() { stop(); reset(); titleLane.dispose(); contentLanes.forEach(lane => lane.dispose()); coverageLane.dispose() }
  const inspectingTokens = computed(() => contentLanes.flatMap(lane => lane.activeKeys.value))
  return { contentPreviews, contentErrors, titleTokens: titleLane.activeKeys, inspectingTokens, retryContent, retry, acceptManual, reset, dispose }
}
