export interface QuarkAuthorizationStatus {
  configured: boolean
  ready: boolean
  state: 'not_authorized' | 'identity_pending' | 'ready' | 'not_required' | 'unavailable'
}

export interface QuarkConnectionStatus {
  connection_id: number
  playback: QuarkAuthorizationStatus
  management: QuarkAuthorizationStatus
  additional_management: QuarkAuthorizationStatus
  verified_roots: number
  capabilities: Record<string, boolean>
}

export type QuarkAuthPurpose = 'playback' | 'management'
export interface QuarkAuthSession {
  session_id: string
  purpose: QuarkAuthPurpose
  state: 'starting' | 'waiting' | 'checking' | 'retryable' | 'complete' | 'expired' | 'cancelled'
  qr_data_url?: string
  authorization_url?: string
  expires_at: string
  poll_after_seconds: number
  error_code?: string
  message: string
}

export function quarkAuthorizationLabel(value?: QuarkAuthorizationStatus) {
  if (!value) return '正在读取'
  if (value.ready) return '已授权'
  if (value.state === 'identity_pending') return '授权已保存，待确认身份'
  if (value.state === 'not_required') return '按需启用'
  if (value.state === 'unavailable') return '当前不可用'
  return '未授权'
}

export function quarkAuthorizationURL(value?: string) {
  if (!value) return ''
  try {
    const url = new URL(value)
    return url.protocol === 'https:' && !url.username && !url.password &&
      (url.hostname === 'quark.cn' || url.hostname.endsWith('.quark.cn')) ? url.href : ''
  } catch { return '' }
}

export function quarkQRDataURL(value?: string) {
  return value && value.length <= 750_000 && /^data:image\/(?:png|jpeg);base64,[A-Za-z0-9+/]+=*$/.test(value) ? value : ''
}

export function quarkPollDelay(value: number) {
  return Number.isFinite(value) ? Math.min(30, Math.max(3, value)) * 1000 : 3000
}
