import { ref } from 'vue'

export interface OwnedReadTask<T> {
  key: string
  run: (signal: AbortSignal) => Promise<T>
  resolve: (value: T) => void
  reject: (reason: unknown) => void
}

export type ResourceReadLane = 'title' | 'cloud-content' | 'torrent-content' | 'magnet-content' | 'coverage'
interface ReadTicket { signal: AbortSignal; grant: (lease: object) => void; reject: () => void; abort: () => void }
interface PhysicalReadLane { limit: number; pending: Set<object>; waiting: ReadTicket[] }
const limits: Record<ResourceReadLane, number> = { title: 4, 'cloud-content': 2, 'torrent-content': 2, 'magnet-content': 2, coverage: 4 }
// The Web API has one same-origin Server. Keep only finite lane identities and
// permits here, never actor data, result tokens or content. A retired component
// cannot release a still-running request by creating another queue instance.
const physicalLanes = new Map<ResourceReadLane, PhysicalReadLane>()
function physicalLane(name: ResourceReadLane) {
  let lane = physicalLanes.get(name)
  if (!lane) { lane = { limit: limits[name], pending: new Set(), waiting: [] }; physicalLanes.set(name, lane) }
  return lane
}
function drain(lane: PhysicalReadLane) {
  while (lane.pending.size < lane.limit && lane.waiting.length) {
    const ticket = lane.waiting.shift()!
    ticket.signal.removeEventListener('abort', ticket.abort)
    if (ticket.signal.aborted) { ticket.reject(); continue }
    const lease = {}; lane.pending.add(lease); ticket.grant(lease)
  }
}
function enqueueRead(lane: PhysicalReadLane, signal: AbortSignal, grant: ReadTicket['grant'], reject: () => void) {
  if (signal.aborted) { reject(); return }
  const ticket: ReadTicket = { signal, grant, reject, abort: () => {
    const index = lane.waiting.indexOf(ticket)
    if (index < 0) return
    lane.waiting.splice(index, 1); signal.removeEventListener('abort', ticket.abort); reject(); drain(lane)
  } }
  lane.waiting.push(ticket); signal.addEventListener('abort', ticket.abort, { once: true }); drain(lane)
}
function release(lane: PhysicalReadLane, lease: object) {
  lane.pending.delete(lease)
  drain(lane)
}
export async function runResourceRead<T>(name: ResourceReadLane, signal: AbortSignal, work: () => Promise<T>): Promise<T> {
  const lane = physicalLane(name)
  const lease = await new Promise<object>((resolve, reject) => enqueueRead(lane, signal, resolve, () => reject(new DOMException('The request owner retired', 'AbortError'))))
  try {
    if (signal.aborted) throw new DOMException('The request owner retired', 'AbortError')
    return await work()
  }
  finally { release(lane, lease) }
}

// A lane owns admission, cancellation, deadlines and every completion branch.
// Separate instances keep long content/coverage reads out of the title budget.
export function createOwnedReadQueue<T>(concurrency: number, timeoutMs: number, name?: ResourceReadLane) {
  const activeKeys = ref<string[]>([])
  let generation = 0
  let disposed = false
  let tasks: OwnedReadTask<T>[] = []
  let completed = new Set<string>()
  const lane = name ? physicalLane(name) : { limit: concurrency, pending: new Set<object>(), waiting: [] }
  const active = new Map<string, { controller: AbortController; timer?: number }>()

  function cancel(key: string) {
    const attempt = active.get(key)
    if (!attempt) return
    active.delete(key)
    window.clearTimeout(attempt.timer)
    attempt.controller.abort()
    activeKeys.value = [...active.keys()]
  }
  function pump() {
    if (disposed) return
    for (const task of tasks) {
      // Queue at most this owner's finite width. Refill goes behind already
      // waiting manual reads/new owners, so background work cannot starve them.
      if (active.size >= concurrency) break
      if (active.has(task.key) || completed.has(task.key)) continue
      completed.add(task.key)
      const controller = new AbortController()
      const run = generation
      const attempt: { controller: AbortController; timer?: number } = { controller }
      active.set(task.key, attempt)
      activeKeys.value = [...active.keys()]
      const current = () => !disposed && generation === run && !controller.signal.aborted && active.get(task.key) === attempt && tasks.some(owner => owner.key === task.key)
      enqueueRead(lane, controller.signal, lease => {
        attempt.timer = window.setTimeout(() => {
          if (current()) { task.reject(new Error('读取超时，请重新检测')); cancel(task.key); pump() }
        }, timeoutMs)
        void Promise.resolve().then(() => {
          if (!current()) throw new DOMException('The request owner retired', 'AbortError')
          return task.run(controller.signal)
        }).then(value => { if (current()) task.resolve(value) }).catch(reason => { if (current()) task.reject(reason) }).finally(() => {
          window.clearTimeout(attempt.timer)
          if (active.get(task.key) === attempt) { active.delete(task.key); activeKeys.value = [...active.keys()] }
          release(lane, lease); pump()
        })
      }, () => {})
    }
  }
  function update(next: OwnedReadTask<T>[]) {
    const unique = new Map(next.slice(0, 1000).map(task => [task.key, task]))
    tasks = [...unique.values()]
    for (const key of active.keys()) if (!unique.has(key)) cancel(key)
    completed = new Set([...completed].filter(key => unique.has(key)))
    pump()
  }
  function reset() {
    generation++
    tasks = []
    for (const key of active.keys()) cancel(key)
    completed.clear()
  }
  function dispose() { disposed = true; reset() }
  return { activeKeys, update, reset, dispose }
}
