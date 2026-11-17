/** The only offline-history authority allowed into transient browser state. */
export interface OfflineFailureWarning {
  version: 1
  reason: 'offline_deadline_exceeded'
  timeout_minutes: 30
  confirmation_token: string
}

export const offlineConfirmationCode = 'offline_resource_confirmation_required'

export function parseOfflineFailureWarning(value: unknown): OfflineFailureWarning | undefined {
  if (typeof value !== 'object' || value === null) return undefined
  const version = Reflect.get(value, 'version')
  const reason = Reflect.get(value, 'reason')
  const minutes = Reflect.get(value, 'timeout_minutes')
  const token = Reflect.get(value, 'confirmation_token')
  if (version !== 1 || reason !== 'offline_deadline_exceeded' || minutes !== 30 || typeof token !== 'string' || !/^[0-9a-f]{64}$/.test(token)) return undefined
  return { version, reason, timeout_minutes: minutes, confirmation_token: token }
}
