import { parseShareContentAnalysis, type ShareContentAnalysis } from '@/share-content'

export const resourceContentPreviewPath = '/api/v1/discovery/content-preview'
export const resourceContentSelectionPath = '/api/v1/discovery/content-selection'

export interface ContentFileFacts {
  token: string; path: string; name: string; size: number; is_dir: boolean
  file_kind: 'video' | 'subtitle' | 'directory' | 'archive' | 'other'
  season?: number; episodes: number[]; work_status: string; unresolved_reason?: string
  subtitles?: { language: string; script?: string; source: 'sidecar_filename' | 'release_claim' }[]
}
export interface ResourceContentPreview {
  version: 1; token: string; status: 'complete' | 'partial' | 'pending' | 'unavailable'; complete: boolean
  files: ContentFileFacts[]; analysis?: ShareContentAnalysis; total_count: number; displayed_count: number
  next_offset?: number; total_size: number; checked_at: string; expires_at: string; error_code?: string; message: string
}
export interface ResourceContentSelection { selection_token: string; preview_token: string; selected_entry_tokens: string[]; file_count: number; video_count: number; selected_bytes: number }

const object = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value)
const text = (value: unknown, limit: number): value is string => typeof value === 'string' && value.length <= limit && ![...value].some(char => char.charCodeAt(0) <= 31 || char.charCodeAt(0) === 127)
const integer = (value: unknown, max = Number.MAX_SAFE_INTEGER): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 && value <= max

export function parseResourceContent(value: unknown, now = Date.now()): ResourceContentPreview | undefined {
  if (!object(value) || value.version !== 1 || !text(value.token, 256) || !['complete', 'partial', 'pending', 'unavailable'].includes(String(value.status)) || typeof value.complete !== 'boolean'
    || value.complete && value.status !== 'complete' || !Array.isArray(value.files) || value.files.length > 200
    || !integer(value.total_count, 10000) || !integer(value.displayed_count, 200) || value.displayed_count !== value.files.length || !integer(value.total_size)
    || !text(value.checked_at, 64) || !Number.isFinite(Date.parse(value.checked_at)) || !text(value.expires_at, 64) || !(Date.parse(value.expires_at) > now) || !text(value.message, 2048)
    || value.next_offset !== undefined && !integer(value.next_offset, 10000)) return
  const files: ContentFileFacts[] = []
  for (const raw of value.files) {
    if (!object(raw) || !text(raw.token, 256) || !text(raw.path, 4096) || !text(raw.name, 512) || !integer(raw.size) || typeof raw.is_dir !== 'boolean'
      || !['video', 'subtitle', 'directory', 'archive', 'other'].includes(String(raw.file_kind)) || !Array.isArray(raw.episodes) || raw.episodes.length > 2000
      || raw.episodes.some(episode => !integer(episode, 100000) || episode === 0) || raw.season !== undefined && !integer(raw.season, 200) || !text(raw.work_status, 128)) return
    const subtitles: NonNullable<ContentFileFacts['subtitles']> = []
    if (raw.subtitles !== undefined) {
      if (!Array.isArray(raw.subtitles) || raw.subtitles.length > 16) return
      for (const evidence of raw.subtitles) {
        if (!object(evidence) || !text(evidence.language, 32) || !['sidecar_filename', 'release_claim'].includes(String(evidence.source)) || evidence.script !== undefined && !text(evidence.script, 32)) return
        subtitles.push({ language: evidence.language, script: evidence.script as string | undefined, source: evidence.source as 'sidecar_filename' | 'release_claim' })
      }
    }
    files.push({ token: raw.token, path: raw.path, name: raw.name, size: raw.size, is_dir: raw.is_dir, file_kind: raw.file_kind as ContentFileFacts['file_kind'], episodes: [...new Set(raw.episodes as number[])].sort((a, b) => a - b), season: raw.season as number | undefined, work_status: raw.work_status, unresolved_reason: typeof raw.unresolved_reason === 'string' ? raw.unresolved_reason.slice(0, 128) : undefined, subtitles })
  }
  const analysis = value.analysis === undefined ? undefined : parseShareContentAnalysis(value.analysis, now)
  if (value.analysis !== undefined && !analysis) return
  return { version: 1, token: value.token, status: value.status as ResourceContentPreview['status'], complete: value.complete, files, analysis, total_count: value.total_count, displayed_count: value.displayed_count, next_offset: value.next_offset as number | undefined, total_size: value.total_size, checked_at: value.checked_at, expires_at: value.expires_at, message: value.message, error_code: typeof value.error_code === 'string' ? value.error_code.slice(0, 128) : undefined }
}
