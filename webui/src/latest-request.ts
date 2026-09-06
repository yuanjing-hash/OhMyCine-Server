// Abort is best effort: generation ownership also guards adapters that resolve
// after cancellation, including stale catch/finally callbacks.
export function createLatestRequest() {
  let controller: AbortController | undefined
  let pending = false
  return {
    get pending() { return pending },
    begin() {
      controller?.abort()
      const current = new AbortController()
      controller = current
      pending = true
      return {
        signal: current.signal,
        isCurrent: () => controller === current && !current.signal.aborted,
        finish() { if (controller === current) pending = false },
      }
    },
    cancel() { controller?.abort(); controller = undefined; pending = false },
  }
}
