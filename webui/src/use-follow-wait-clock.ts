import { onBeforeUnmount, onMounted, ref } from 'vue'
import type { FollowWait } from '@/follows'

// One clock per mounted view, irrespective of subscription/run count. Each
// response records its server offset once; rendering does not call a provider.
export function useFollowWaitClock() {
  const now = ref(Date.now())
  const offsets = new WeakMap<FollowWait, number>()
  let timer: ReturnType<typeof setInterval> | undefined
  onMounted(() => { timer = setInterval(() => { now.value = Date.now() }, 1000) })
  onBeforeUnmount(() => { if (timer !== undefined) clearInterval(timer) })
  function observe(waits: Array<FollowWait | undefined>) {
    const receivedAt = Date.now()
    for (const wait of waits) {
      if (wait && !offsets.has(wait)) offsets.set(wait, Date.parse(wait.server_time) - receivedAt)
    }
    now.value = receivedAt
  }
  function remaining(wait: FollowWait) {
    const offset = offsets.get(wait)
    if (offset === undefined || !Number.isFinite(offset) || !Number.isFinite(Date.parse(wait.not_before))) return ''
    const seconds = Math.max(0, Math.ceil((Date.parse(wait.not_before) - now.value - offset) / 1000))
    return seconds ? `还需 ${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒` : '本轮记录的冷却时间已结束'
  }
  return { observe, remaining }
}
