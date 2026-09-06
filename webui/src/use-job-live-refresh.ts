import { onBeforeUnmount, onMounted } from 'vue'

// Events invalidate read models; coalesce bursts and retain polling after an
// interrupted WebSocket. Neither hidden tabs nor dead views keep doing work.
export function useJobLiveRefresh(refresh: () => Promise<void>, enabled: () => boolean = () => true, socketEnabled: () => boolean = enabled) {
  let stopped = false
  let socket: WebSocket | undefined
  let poll: number | undefined
  let queued: number | undefined
  let reconnect: number | undefined
  let failures = 0
  let refreshing = false
  let dirty = false
  const visible = () => document.visibilityState !== 'hidden'

  async function run() {
    if (stopped || !visible() || !enabled()) return
    if (refreshing) { dirty = true; return }
    refreshing = true
    try { await refresh() } finally {
      refreshing = false
      if (dirty) { dirty = false; invalidate() }
    }
  }
  function invalidate() {
    if (stopped || !visible() || !enabled() || queued !== undefined) return
    queued = window.setTimeout(() => { queued = undefined; void run() }, 250)
  }
  function retryConnection() {
    if (stopped || !visible() || !enabled() || !socketEnabled() || reconnect !== undefined) return
    const delay = Math.min(30_000, 1000 * 2 ** Math.min(failures++, 5))
    reconnect = window.setTimeout(() => { reconnect = undefined; connect() }, delay + Math.random() * 250)
  }
  function connect() {
    if (stopped || !visible() || !enabled() || !socketEnabled() || socket) return
    try {
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const current = new WebSocket(`${protocol}//${window.location.host}/api/v1/jobs/events/ws`)
      socket = current
      current.onmessage = invalidate
      current.onopen = () => { failures = 0; invalidate() }
      current.onclose = () => {
        if (socket !== current) return
        socket = undefined
        retryConnection()
      }
      current.onerror = () => current.close()
    } catch { retryConnection() }
  }
  function disconnect() {
    const current = socket
    socket = undefined
    if (current) { current.onclose = null; current.onmessage = null; current.onopen = null; current.onerror = null; current.close() }
    window.clearTimeout(reconnect)
    reconnect = undefined
  }
  function visibilityChanged() {
    if (visible()) { connect(); invalidate() } else { disconnect(); window.clearTimeout(queued); queued = undefined }
  }
  function stop() {
    stopped = true
    disconnect()
    window.clearInterval(poll)
    window.clearTimeout(queued)
  }
  onMounted(() => {
    connect()
    poll = window.setInterval(() => {
      if (!enabled() || !socketEnabled()) disconnect()
      else connect()
      invalidate()
    }, 15_000)
    document.addEventListener('visibilitychange', visibilityChanged)
    window.addEventListener('omc:unauthorized', stop)
  })
  onBeforeUnmount(() => {
    stop()
    document.removeEventListener('visibilitychange', visibilityChanged)
    window.removeEventListener('omc:unauthorized', stop)
  })
}
