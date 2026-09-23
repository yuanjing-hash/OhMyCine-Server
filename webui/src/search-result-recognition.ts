import { ref, watch, type Ref } from 'vue'
import { api } from '@/api/client'
import { discoveryCoveragePath, normalizeMediaCoverage, coverageStatusLabel, type MediaCoverage } from '@/discovery'
import { torrentRecognitionPath, type TorrentRecognitionResult, type TorrentSearchResult } from '@/sites'

type LibraryState = { label: string; detail: string; present: boolean }

// Each page owns its queue. No share contents are requested by title recognition.
export function useSearchResultRecognition(items: Ref<TorrentSearchResult[]>, canReadLibrary: () => boolean) {
  const recognitions = ref<Record<string, TorrentRecognitionResult>>({})
  const recognitionErrors = ref<Record<string, string>>({})
  const recognizingTokens = ref<string[]>([])
  const libraryStates = ref<Record<string, LibraryState>>({})
  let generation = 0
  let stopped = false
  let active = new Map<string, AbortController>()
  let completed = new Set<string>()
  let revisions = new Map<string, number>()
  let coverage = new Map<string, Promise<MediaCoverage>>()

  async function readCoverage(result: TorrentRecognitionResult, signal: AbortSignal) {
    if (result.status !== 'matched' || !result.tmdb_id || !result.media_type) {
      return { label: '库内状态待确认', detail: '尚未识别出作品，可使用手动检测确认身份', present: false }
    }
    if (!canReadLibrary()) return { label: '无权查看库内状态', detail: '需要媒体库读取权限', present: false }
    const key = `${result.media_type}:${result.tmdb_id}`
    let request = coverage.get(key)
    if (!request) {
      request = api<unknown>(discoveryCoveragePath(result.media_type, result.tmdb_id), { signal }).then(normalizeMediaCoverage)
      if (coverage.size >= 1000) coverage.delete(coverage.keys().next().value!)
      coverage.set(key, request)
    }
    try {
      const value = await request
      const label = value.status === 'missing' ? '未入库' : coverageStatusLabel(value.status)
      const count = value.tv ? `；已入库 ${value.tv.counts.present} 集` : ''
      return { label, detail: `按当前账号可见媒体库判断，依据已扫描内容${count}；作品级状态，不代表该资源版本已存在`, present: value.status === 'present' }
    } catch {
      return { label: '库内状态暂不可用', detail: '未能读取媒体库状态，请稍后重新搜索；不代表未入库', present: false }
    }
  }

  function pump() {
    if (stopped) return
    for (const item of items.value) {
      if (active.size >= 2) break
      if (active.has(item.token) || completed.has(item.token)) continue
      completed.add(item.token)
      if (Date.parse(item.expires_at) <= Date.now()) {
        recognitionErrors.value[item.token] = '搜索结果已过期，请重新搜索'
        continue
      }
      const controller = new AbortController()
      active.set(item.token, controller)
      recognizingTokens.value = [...active.keys()]
      const run = generation
      const revision = revisions.get(item.token) ?? 0
      const isCurrent = () => !controller.signal.aborted && generation === run && (revisions.get(item.token) ?? 0) === revision && items.value.some(current => current.token === item.token)
      const timeout = window.setTimeout(() => controller.abort(), 60000)
      void (async () => {
        try {
          const result = recognitions.value[item.token] ?? await api<TorrentRecognitionResult>(torrentRecognitionPath, {
            method: 'POST', body: JSON.stringify({ result_token: item.token }), signal: controller.signal,
          })
          if (!isCurrent()) return
          recognitions.value[item.token] = result
          libraryStates.value[item.token] = { label: '正在查询库内状态…', detail: '', present: false }
          const state = await readCoverage(result, controller.signal)
          if (isCurrent()) libraryStates.value[item.token] = state
        } catch (reason) {
          if (isCurrent()) recognitionErrors.value[item.token] = reason instanceof Error ? reason.message : '自动识别失败，可使用手动检测'
        } finally {
          window.clearTimeout(timeout)
          if (generation === run) {
            if (controller.signal.aborted && items.value.some(current => current.token === item.token) && (revisions.get(item.token) ?? 0) === revision) {
              recognitionErrors.value[item.token] = '自动识别或库内查询超时，可使用手动检测或重新搜索'
              if (libraryStates.value[item.token]?.label === '正在查询库内状态…') libraryStates.value[item.token] = { label: '库内状态暂不可用', detail: '查询超时，不代表未入库', present: false }
            }
            active.delete(item.token)
            recognizingTokens.value = [...active.keys()]
            pump()
          }
        }
      })()
    }
  }

  const stopWatch = watch(items, () => {
    const tokens = new Set(items.value.map(item => item.token))
    for (const [token, controller] of active) if (!tokens.has(token)) controller.abort()
    // Page changes must not accumulate claims or recognition state indefinitely.
    for (const state of [recognitions, recognitionErrors, libraryStates]) {
      for (const token of Object.keys(state.value)) if (!tokens.has(token)) delete state.value[token]
    }
    completed = new Set([...completed].filter(token => tokens.has(token)))
    revisions = new Map([...revisions].filter(([token]) => tokens.has(token)))
    pump()
  })

  function acceptManual(token: string, result: TorrentRecognitionResult) {
    if (stopped || !items.value.some(item => item.token === token)) return
    revisions.set(token, (revisions.get(token) ?? 0) + 1)
    recognitions.value[token] = result
    delete recognitionErrors.value[token]
    delete libraryStates.value[token]
    completed.delete(token)
    pump()
  }

  function reset() {
    generation++
    for (const controller of active.values()) controller.abort()
    active = new Map()
    completed = new Set()
    revisions = new Map()
    coverage = new Map()
    recognitions.value = {}
    recognitionErrors.value = {}
    recognizingTokens.value = []
    libraryStates.value = {}
  }
  function dispose() { stopped = true; stopWatch(); reset() }
  return { recognitions, recognitionErrors, recognizingTokens, libraryStates, acceptManual, reset, dispose }
}
