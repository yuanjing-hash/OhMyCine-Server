export interface MediaVersionUpgrade {
  id: string
  state: string
  stage: string
  library_id: number
  entry_id: number
  work_token: string
  title: string
  media_type: 'movie' | 'tv'
  tmdb_id: number
  season?: number
  episode?: number
  revision: number
  expires_at: string
  recycle_supported: boolean
  error_code?: string
  error_message?: string
  download_task_id?: string
  transfer_task_id?: string
  next_attempt_at?: string
}
export function mediaVersionUpgradePath(id: string) { return `/api/v1/media-upgrades/${encodeURIComponent(id)}` }
export function mediaVersionUpgradePreviewPath(library: number, work: string, entry: number) { return `/api/v1/media-libraries/${library}/catalog/${encodeURIComponent(work)}/entries/${entry}/upgrade-preview` }
export function mediaVersionUpgradeSearchRoute(upgrade: MediaVersionUpgrade) {
  return { path: '/discovery/explore', query: { mode: 'resources', identity: 'tmdb', title: upgrade.title, media_type: upgrade.media_type, tmdb_id: String(upgrade.tmdb_id), upgrade_id: upgrade.id } }
}
export function isPreparedMediaUpgrade(upgrade: MediaVersionUpgrade) {
  return upgrade.state === 'prepared' && upgrade.recycle_supported && Number.isFinite(Date.parse(upgrade.expires_at)) && Date.parse(upgrade.expires_at) > Date.now()
}
