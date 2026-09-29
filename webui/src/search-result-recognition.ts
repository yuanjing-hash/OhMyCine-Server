import { ref, watch, type Ref } from 'vue'
import { api } from '@/api/client'
import { discoveryCoveragePath, normalizeMediaCoverage, coverageStatusLabel, type MediaCoverage } from '@/discovery'
import { torrentRecognitionPath, type TorrentRecognitionResult, type TorrentSearchResult } from '@/sites'

type LibraryState = { label: string; detail: string; present: boolean }
type Attempt = { controller: AbortController; generation: number; revision: number; cancel: () => void }

const concurrentRecognitions = 2
const recognitionDeadlineMs = 30000

// Each page owns its queue. No share contents are requested by title recognition.
export function useSearchResultRecognition(items: Ref<TorrentSearchResult[]>, canReadLibrary: () => boolean) {
  const recognitions = ref<Record<string, TorrentRecognitionResult>>({})
  const recognitionErrors = ref<Record<string, string>>({})
  const recognizingTokens = ref<string[]>([])
  const libraryStates = ref<Record<string, LibraryState>>({})
  let generation = 0
  let stopped = false
  let active = new Map<string, Attempt>()
  let completed = new Set<string>()
  let retryPriority = new Set<string>()
  let revisions = new Map<string, number>()
  let coverage = new Map<string, Promise<MediaCoverage>>()

  function coverageKey(result?: TorrentRecognitionResult) {
    return result?.status === 'matched' && result.tmdb_id && result.media_type ? `${result.media_type}:${result.tmdb_id}` : ''
  }

  async function readCoverage(result: TorrentRecognitionResult): Promise<LibraryState> {
    const key = coverageKey(result)
    if (!key) return { label: '库内状态待确认', detail: '尚未识别出作品，可使用手动检测确认身份', present: false }
    if (!canReadLibrary()) return { label: '无权查看库内状态', detail: '需要媒体库读取权限', present: false }
    let request = coverage.get(key)
    if (!request) {
      // Shared coverage must not inherit the first card's cancellation signal.
      const controller = new AbortController()
      const timeout = window.setTimeout(() => {
        controller.abort()
        if (coverage.get(key) === request) coverage.delete(key)
      }, recognitionDeadlineMs)
      request = api<unknown>(discoveryCoveragePath(result.media_type!, result.tmdb_id!), { signal: controller.signal })
        .then(normalizeMediaCoverage)
        .catch(reason => {
          if (coverage.get(key) === request) coverage.delete(key)
          throw reason
        })
        .finally(() => window.clearTimeout(timeout))
      if (coverage.size >= 1000) coverage.delete(coverage.keys().next().value!)
      coverage.set(key, request)
    }
    try {
      const value = await request
      const label = value.status === 'missing' ? '未入库' : coverageStatusLabel(value.status)
      const count = value.tv ? `；已入库 ${value.tv.counts.present} 集` : ''
      return { label, detail: `按当前账号可见媒体库判断，依据已扫描内容${count}；作品级状态，不代表该资源版本已存在`, present: value.status === 'present' }
    } catch {
      return { label: '库内状态暂不可用', detail: '未能读取媒体库状态，请稍后重新检测；不代表未入库', present: false }
    }
  }

  function isCurrent(item: TorrentSearchResult, attempt: Attempt) {
    return !stopped && !attempt.controller.signal.aborted && generation === attempt.generation
      && (revisions.get(item.token) ?? 0) === attempt.revision && active.get(item.token) === attempt
      && items.value.some(current => current.token === item.token)
  }

  function invalidate(token: string) {
    const attempt = active.get(token)
    if (!attempt) return
    active.delete(token)
    attempt.controller.abort()
    attempt.cancel()
    recognizingTokens.value = [...active.keys()]
  }

  function start(item: TorrentSearchResult) {
    const controller = new AbortController()
    let cancel!: () => void
    const cancelled = new Promise<'cancelled'>(resolve => { cancel = () => resolve('cancelled') })
    const attempt: Attempt = { controller, generation, revision: revisions.get(item.token) ?? 0, cancel }
    active.set(item.token, attempt)
    recognizingTokens.value = [...active.keys()]

    const work = (async () => {
      const result = recognitions.value[item.token] ?? await api<TorrentRecognitionResult>(torrentRecognitionPath, {
        method: 'POST', body: JSON.stringify({ result_token: item.token }), signal: controller.signal,
      })
      if (!isCurrent(item, attempt)) return
      recognitions.value[item.token] = result
      libraryStates.value[item.token] = { label: '正在查询库内状态…', detail: '', present: false }
      const state = await readCoverage(result)
      if (isCurrent(item, attempt)) libraryStates.value[item.token] = state
    })().catch(reason => {
      if (isCurrent(item, attempt)) recognitionErrors.value[item.token] = reason instanceof Error ? reason.message : '自动识别失败，可使用手动检测'
    })

    let timeout: number | undefined
    const deadline = new Promise<'timeout'>(resolve => { timeout = window.setTimeout(() => resolve('timeout'), recognitionDeadlineMs) })
    void (async () => {
      try {
        const outcome = await Promise.race([work.then(() => 'done' as const), deadline, cancelled])
        if (outcome === 'timeout' && isCurrent(item, attempt)) {
          controller.abort()
          recognitionErrors.value[item.token] = recognitions.value[item.token]
            ? '库内状态查询超时，请重新检测'
            : '作品自动识别超时，请重新检测或使用手动检测'
          if (libraryStates.value[item.token]?.label === '正在查询库内状态…') {
            libraryStates.value[item.token] = { label: '库内状态暂不可用', detail: '查询超时，不代表未入库', present: false }
          }
        }
      } finally {
        window.clearTimeout(timeout)
        if (active.get(item.token) === attempt) {
          active.delete(item.token)
          recognizingTokens.value = [...active.keys()]
          pump()
        }
      }
    })()
  }

  function pump() {
    if (stopped) return
    const byToken = new Map(items.value.map(item => [item.token, item]))
    const ordered = [...[...retryPriority].map(token => byToken.get(token)).filter((item): item is TorrentSearchResult => !!item), ...items.value]
    for (const item of ordered) {
      if (active.size >= concurrentRecognitions) break
      if (active.has(item.token) || completed.has(item.token)) continue
      completed.add(item.token)
      retryPriority.delete(item.token)
      if (!(Date.parse(item.expires_at) > Date.now())) {
        recognitionErrors.value[item.token] = '搜索结果已过期，请重新搜索'
        continue
      }
      start(item)
    }
  }

  const stopWatch = watch(items, () => {
    const tokens = new Set(items.value.map(item => item.token))
    for (const token of active.keys()) if (!tokens.has(token)) invalidate(token)
    // Page changes must not accumulate claims or recognition state indefinitely.
    for (const state of [recognitions, recognitionErrors, libraryStates]) {
      for (const token of Object.keys(state.value)) if (!tokens.has(token)) delete state.value[token]
    }
    completed = new Set([...completed].filter(token => tokens.has(token)))
    retryPriority = new Set([...retryPriority].filter(token => tokens.has(token)))
    revisions = new Map([...revisions].filter(([token]) => tokens.has(token)))
    pump()
  })

  function retry(token: string) {
    const item = items.value.find(candidate => candidate.token === token)
    if (stopped || !item) return false
    revisions.set(token, (revisions.get(token) ?? 0) + 1)
    invalidate(token)
    if (!(Date.parse(item.expires_at) > Date.now())) {
      recognitionErrors.value[token] = '搜索结果已过期，请重新搜索'
      completed.add(token)
      pump()
      return false
    }
    const key = coverageKey(recognitions.value[token])
    if (key) coverage.delete(key)
    delete recognitions.value[token]
    delete recognitionErrors.value[token]
    delete libraryStates.value[token]
    completed.delete(token)
    retryPriority.add(token)
    pump()
    return true
  }

  function acceptManual(token: string, result: TorrentRecognitionResult) {
    if (stopped || !items.value.some(item => item.token === token)) return
    revisions.set(token, (revisions.get(token) ?? 0) + 1)
    invalidate(token)
    recognitions.value[token] = result
    delete recognitionErrors.value[token]
    delete libraryStates.value[token]
    completed.delete(token)
    retryPriority.add(token)
    pump()
  }

  function reset() {
    generation++
    for (const token of active.keys()) invalidate(token)
    active = new Map()
    completed = new Set()
    retryPriority = new Set()
    revisions = new Map()
    coverage = new Map()
    recognitions.value = {}
    recognitionErrors.value = {}
    recognizingTokens.value = []
    libraryStates.value = {}
  }
  function dispose() { stopped = true; stopWatch(); reset() }
  return { recognitions, recognitionErrors, recognizingTokens, libraryStates, retry, acceptManual, reset, dispose }
}
