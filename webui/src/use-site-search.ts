import { onBeforeUnmount, ref, type Ref } from 'vue'
import { api, APIError } from '@/api/client'
import { parseSearchProgress, searchIssueLabel, type SearchOutcome } from '@/search-progress'
import { upsertTorrentGroup, type TorrentSearchGroup, type TorrentSearchProgress, type TorrentSearchResponse, type TorrentSearchSiteDetail } from '@/sites'

interface SearchOptions {
  groups: Ref<TorrentSearchGroup[]>
  progress: Ref<TorrentSearchProgress | null>
  error: Ref<string>
  searching: Ref<boolean>
  searched: Ref<boolean>
  scope: () => number[]
  siteName: (id: number) => string
  url: (stream: boolean, scope: number[], page: number, refresh: boolean) => string
  media: (value: unknown) => void
}
interface Activity extends TorrentSearchSiteDetail { receivedAt: number }
interface Attempt {
  generation: number
  scope: number[]
  page: number
  controller: AbortController
  source?: EventSource
  timer?: number
  revisions: Map<number, number>
  terminal: Set<number>
  progress?: TorrentSearchProgress
  sequence: number
  previousProgress: TorrentSearchProgress | null
  previous: TorrentSearchGroup[]
  hasSnapshot: boolean
}
const requestLimitMs = 390_000
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value)
const count = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0

export function useSiteSearch(options: SearchOptions) {
  const activities = ref<Record<number, Activity>>({})
  const retainedSiteIDs = ref<number[]>([])
  const requestedPages = ref<Record<number, number>>({})
  const outcome = ref<SearchOutcome>('idle')
  const clock = ref(Date.now())
  let generation = 0
  let active: Attempt | undefined
  const ticker = window.setInterval(() => { if (options.searching.value) clock.value = Date.now() }, 1000)
  const owns = (attempt: Attempt) => active === attempt && generation === attempt.generation && !attempt.controller.signal.aborted

  function stop() {
    const attempt = active
    active = undefined
    if (attempt) {
      attempt.source?.close()
      window.clearTimeout(attempt.timer)
      attempt.controller.abort()
    }
  }

  function elapsed(siteID: number): number {
    const detail = activities.value[siteID]
    if (!detail) return 0
    return detail.elapsed_ms + (detail.phase !== 'finished' && options.searching.value ? Math.max(0, clock.value - detail.receivedAt) : 0)
  }

  function freeze(scope: number[]) {
    for (const id of scope) {
      const detail = activities.value[id]
      if (detail) activities.value[id] = { ...detail, elapsed_ms: elapsed(id), receivedAt: Date.now(), phase: 'finished' }
    }
  }

  function mergeProgress(progress: TorrentSearchProgress, scope: number[]) {
    const all = options.scope().length ? options.scope() : scope
    const outside = all.filter(id => !scope.includes(id))
    const groups = options.groups.value.filter(group => outside.includes(group.site_id))
    const succeeded = groups.filter(group => group.status === 'success' && group.complete !== false).length
    const failed = groups.filter(group => group.status === 'error' && group.complete !== false).length
    options.progress.value = {
      ...progress, total: all.length,
      pending: progress.pending + outside.length - succeeded - failed,
      completed: progress.completed + succeeded + failed,
      succeeded: progress.succeeded + succeeded, failed: progress.failed + failed,
      result_count: progress.result_count + groups.reduce((sum, group) => sum + group.items.length, 0),
    }
  }

  function applyProgress(attempt: Attempt, value: unknown, terminal = false): boolean {
    const progress = parseSearchProgress(value, attempt.scope.length)
    if (!progress || progress.site_id && !attempt.scope.includes(progress.site_id)
      || progress.site_detail && !attempt.scope.includes(progress.site_detail.site_id)) return false
    if (attempt.sequence >= 0 && progress.event_sequence === undefined) return false
    if (progress.event_sequence !== undefined && (progress.event_sequence < attempt.sequence || !terminal && progress.event_sequence === attempt.sequence)) return false
    if (progress.completed < (attempt.progress?.completed ?? 0)) return false
    if (progress.event_sequence !== undefined) attempt.sequence = progress.event_sequence
    if (progress.phase === 'preparing_media' && progress.total === 0) {
      if (options.progress.value) options.progress.value = { ...options.progress.value, phase: progress.phase }
      return true
    }
    attempt.progress = progress
    const detail = progress.site_detail
    if (detail) {
      const previous = activities.value[detail.site_id]
      if (previous?.revision === undefined || detail.revision !== undefined && detail.revision > previous.revision) {
        activities.value[detail.site_id] = { ...detail, receivedAt: Date.now() }
      }
    } else if (progress.site_id) {
      const id = progress.site_id
      activities.value[id] = {
        ...activities.value[id], site_id: id, site_name: progress.site_name || options.siteName(id),
        phase: progress.site_status === 'running' ? 'querying' : progress.site_status === 'queued' ? 'queued' : 'finished',
        result_count: options.groups.value.find(group => group.site_id === id)?.items.length ?? 0,
        elapsed_ms: elapsed(id), receivedAt: Date.now(),
      }
    }
    mergeProgress(progress, attempt.scope)
    return true
  }

  function acceptGroup(attempt: Attempt, value: unknown): boolean {
    if (record(value) && value.items === null) value = { ...value, items: [] }
    if (!record(value) || !count(value.site_id) || !attempt.scope.includes(value.site_id) || value.page !== attempt.page
      || typeof value.site_name !== 'string' || !['success', 'error'].includes(String(value.status))
      || !['pt', 'bt', 'bt_resource', 'cloud_share'].includes(String(value.site_type)) || typeof value.has_next !== 'boolean' || !count(value.skipped)
      || value.complete !== undefined && typeof value.complete !== 'boolean' || value.partial !== undefined && typeof value.partial !== 'boolean'
      || !Array.isArray(value.items) || value.items.some(item => !record(item) || typeof item.token !== 'string' || typeof item.title !== 'string' || typeof item.expires_at !== 'string')
      || ['revision', 'queries_completed', 'queries_failed', 'queries_total'].some(key => value[key] !== undefined && !count(value[key]))) return false
    const group = value as unknown as TorrentSearchGroup
    const previousRevision = attempt.revisions.get(group.site_id)
    if (previousRevision !== undefined && group.revision === undefined) return true
    if (group.revision !== undefined && previousRevision !== undefined && group.revision <= previousRevision) return true
    if (attempt.terminal.has(group.site_id)) return true
    if (group.revision !== undefined) attempt.revisions.set(group.site_id, group.revision)
    const previous = options.groups.value.find(item => item.site_id === group.site_id)
    const keepPrevious = !group.items.length && previous?.items.length && (group.status === 'error' || group.partial || group.complete === false)
    const incoming = keepPrevious ? { ...group, page: previous.page, items: previous.items, partial: true, incomplete_reason: group.error_code || group.incomplete_reason || 'retry_failed_previous_results' } : group
    options.groups.value = upsertTorrentGroup(options.groups.value, incoming)
    attempt.hasSnapshot = true
    if (!keepPrevious) retainedSiteIDs.value = retainedSiteIDs.value.filter(id => id !== group.site_id)
    if (group.complete !== false) attempt.terminal.add(group.site_id)
    const detail = activities.value[group.site_id]
    activities.value[group.site_id] = {
      ...detail, site_id: group.site_id, site_name: group.site_name,
      phase: group.complete === false ? 'processing_results' : 'finished',
      result_count: group.items.length, elapsed_ms: elapsed(group.site_id), receivedAt: Date.now(),
      queries_completed: group.queries_completed ?? detail?.queries_completed,
      queries_failed: group.queries_failed ?? detail?.queries_failed,
      queries_total: group.queries_total ?? detail?.queries_total,
    }
    options.searched.value = true
    return true
  }

  function finish(attempt: Attempt, result: SearchOutcome) {
    if (!owns(attempt)) return
    clock.value = Date.now()
    freeze(attempt.scope)
    stop()
    options.searching.value = false
    options.searched.value = true
    outcome.value = result
  }

  function terminalOutcome(): SearchOutcome {
    const scope = options.scope()
    const groups = options.groups.value.filter(group => scope.includes(group.site_id))
    if (groups.length < scope.length || groups.some(group => group.partial || group.complete === false || group.status === 'error')) {
      return groups.some(group => group.status === 'success' || group.items.length) ? 'partial' : 'failed'
    }
    return 'complete'
  }

  function interrupt(attempt: Attempt, code: string, message = searchIssueLabel(code)) {
    if (!owns(attempt)) return
    for (const id of attempt.scope.filter(id => !attempt.terminal.has(id))) {
      const previous = options.groups.value.find(group => group.site_id === id)
      options.groups.value = upsertTorrentGroup(options.groups.value, {
        ...previous, site_id: id, site_name: previous?.site_name || options.siteName(id), site_type: previous?.site_type || 'bt',
        status: attempt.hasSnapshot && previous?.items.length ? 'success' : 'error', error_code: code, page: previous?.page ?? attempt.page,
        has_next: previous?.has_next ?? false, skipped: previous?.skipped ?? 0, items: previous?.items ?? [], complete: true,
        partial: true, incomplete_reason: code,
      })
    }
    options.error.value = message
    const currentGroups = options.groups.value.filter(group => attempt.scope.includes(group.site_id))
    mergeProgress({
      total: attempt.scope.length, pending: 0, running: 0, completed: attempt.scope.length,
      succeeded: currentGroups.filter(group => group.status === 'success').length,
      failed: currentGroups.filter(group => group.status === 'error').length,
      result_count: currentGroups.reduce((sum, group) => sum + group.items.length, 0),
    }, attempt.scope)
    finish(attempt, currentGroups.some(group => group.items.length) ? 'partial' : 'failed')
  }

  function cancel() {
    const attempt = active
    if (!attempt) return
    clock.value = Date.now()
    freeze(attempt.scope)
    if (!attempt.hasSnapshot && attempt.previous.length && attempt.previousProgress) options.progress.value = attempt.previousProgress
    else if (options.progress.value) options.progress.value = { ...options.progress.value, pending: 0, running: 0, site_status: 'cancelled' }
    for (const group of options.groups.value.filter(group => attempt.scope.includes(group.site_id) && group.complete === false)) {
      options.groups.value = upsertTorrentGroup(options.groups.value, { ...group, complete: true, partial: true, incomplete_reason: 'cancelled' })
    }
    stop()
    generation++
    options.searching.value = false
    outcome.value = 'cancelled'
  }

  function reset() {
    stop()
    generation++
    activities.value = {}
    retainedSiteIDs.value = []
    requestedPages.value = {}
    outcome.value = 'idle'
  }

  function start(scope: number[], page = 1, refresh = false) {
    const allowed = options.scope()
    if (!scope.length || scope.length > 64 || new Set(scope).size !== scope.length || scope.some(id => !count(id) || id === 0 || !allowed.includes(id)) || !count(page) || page < 1 || page > 20) {
      options.error.value = '搜索站点或页码不在本次选择范围内，请重新选择'
      return
    }
    stop()
    const attempt: Attempt = {
      generation: ++generation, scope: [...scope], page, controller: new AbortController(),
      revisions: new Map(), terminal: new Set(), sequence: -1,
      previous: options.groups.value.filter(group => scope.includes(group.site_id)), previousProgress: options.progress.value, hasSnapshot: false,
    }
    active = attempt
    retainedSiteIDs.value = [...new Set([...retainedSiteIDs.value, ...attempt.previous.filter(group => group.items.length).map(group => group.site_id)])]
    options.error.value = ''
    options.searching.value = true
    outcome.value = 'running'
    clock.value = Date.now()
    for (const id of scope) {
      requestedPages.value[id] = page
      activities.value[id] = { site_id: id, site_name: options.siteName(id), phase: 'queued', result_count: 0, elapsed_ms: 0, receivedAt: Date.now() }
    }
    mergeProgress({ total: scope.length, pending: scope.length, running: 0, completed: 0, succeeded: 0, failed: 0, result_count: 0 }, scope)
    attempt.timer = window.setTimeout(() => interrupt(attempt, 'search_budget_exceeded'), requestLimitMs)
    if (typeof EventSource === 'undefined') {
      mergeProgress({ total: scope.length, pending: 0, running: scope.length, completed: 0, succeeded: 0, failed: 0, result_count: 0 }, scope)
      void api<TorrentSearchResponse>(options.url(false, scope, page, refresh), { signal: attempt.controller.signal }).then(response => {
        if (!owns(attempt)) return
        if (!Array.isArray(response.groups) || response.groups.length !== scope.length || new Set(response.groups.map(group => group.site_id)).size !== scope.length) throw new Error('站点搜索结果不完整，请重试')
        for (const group of response.groups) if (!acceptGroup(attempt, group)) throw new Error('站点搜索结果不完整，请重试')
        if (attempt.terminal.size !== scope.length) throw new Error('站点搜索结果不完整，请重试')
        options.media(response)
        const normalizedGroups = options.groups.value.filter(group => scope.includes(group.site_id))
        const succeeded = normalizedGroups.filter(group => group.status === 'success').length
        mergeProgress({ total: scope.length, pending: 0, running: 0, completed: scope.length, succeeded, failed: scope.length - succeeded, result_count: normalizedGroups.reduce((sum, group) => sum + group.items.length, 0) }, scope)
        finish(attempt, terminalOutcome())
      }).catch((reason: unknown) => {
        if (!owns(attempt)) return
        if (reason instanceof APIError && [401, 403].includes(reason.status)) {
          options.groups.value = options.groups.value.filter(group => !scope.includes(group.site_id))
          retainedSiteIDs.value = retainedSiteIDs.value.filter(id => !scope.includes(id))
        }
        interrupt(attempt, 'site_unavailable', reason instanceof Error ? reason.message : '搜索请求失败，请重试')
      })
      return
    }
    try {
      const url = new URL(options.url(true, scope, page, refresh), window.location.origin)
      url.searchParams.set('progress_version', '2')
      const source = new EventSource(`${url.pathname}${url.search}`)
      attempt.source = source
      source.addEventListener('media', event => { if (owns(attempt)) { try { options.media(JSON.parse((event as MessageEvent<string>).data)) } catch { /* Metadata is optional. */ } } })
      source.addEventListener('progress', event => { if (owns(attempt)) { try { applyProgress(attempt, JSON.parse((event as MessageEvent<string>).data)) } catch { /* Keep the last valid progress. */ } } })
      source.addEventListener('site', event => {
        if (!owns(attempt)) return
        try { if (!acceptGroup(attempt, JSON.parse((event as MessageEvent<string>).data))) interrupt(attempt, 'invalid_response') }
        catch { interrupt(attempt, 'invalid_response') }
      })
      source.addEventListener('done', event => {
        if (!owns(attempt)) return
        try {
          const value: unknown = JSON.parse((event as MessageEvent<string>).data)
          const finalProgress = parseSearchProgress(value, scope.length)
          if (!finalProgress || finalProgress.completed !== scope.length || attempt.terminal.size !== scope.length) { interrupt(attempt, 'stream_interrupted'); return }
          if (!applyProgress(attempt, value, true)) { interrupt(attempt, 'invalid_response'); return }
          finish(attempt, terminalOutcome())
        } catch { interrupt(attempt, 'invalid_response') }
      })
      source.addEventListener('error', event => {
        if (!owns(attempt) || !(event instanceof MessageEvent)) return
        try {
          const value: unknown = JSON.parse(String(event.data))
          interrupt(attempt, record(value) && typeof value.code === 'string' ? value.code : 'stream_interrupted')
        } catch { interrupt(attempt, 'stream_interrupted') }
      })
      source.onerror = () => interrupt(attempt, 'stream_interrupted')
    } catch { interrupt(attempt, 'stream_interrupted') }
  }

  function restore() {
    retainedSiteIDs.value = options.groups.value.filter(group => group.complete === false || group.partial).map(group => group.site_id)
    for (const group of options.groups.value.filter(group => group.complete === false)) {
      options.groups.value = upsertTorrentGroup(options.groups.value, { ...group, complete: true, partial: true, incomplete_reason: 'stream_interrupted' })
    }
    outcome.value = 'retained'
  }

  function dispose() { stop(); generation++; window.clearInterval(ticker) }
  onBeforeUnmount(dispose)
  return { activities, retainedSiteIDs, requestedPages, outcome, elapsed, start, cancel, reset, restore, dispose }
}
