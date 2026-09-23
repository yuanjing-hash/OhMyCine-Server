export interface ShareValidation {
 status: 'valid' | 'expired' | 'password_required' | 'unavailable'
 error_code?: string
 message: string
 checked_at: string
 expires_at: string
 downloader_id: string
}
export function parseShareValidation(value: unknown): ShareValidation | undefined {
 if (!value || typeof value !== 'object') return
 const v = value as Record<string, unknown>
 if (!['valid', 'expired', 'password_required', 'unavailable'].includes(String(v.status)) ||
  typeof v.message !== 'string' || typeof v.checked_at !== 'string' || !Number.isFinite(Date.parse(v.checked_at)) ||
  typeof v.expires_at !== 'string' || !Number.isFinite(Date.parse(v.expires_at)) || typeof v.downloader_id !== 'string') return
 return { status: v.status as ShareValidation['status'], message: v.message, checked_at: v.checked_at, expires_at: v.expires_at, downloader_id: v.downloader_id, error_code: typeof v.error_code === 'string' ? v.error_code : undefined }
}
export function shareValidationLabel(value: ShareValidation | undefined, now = Date.now()): string {
 if (!value || Date.parse(value.expires_at) <= now) return value ? '验证已过期，请重新验证' : '未验证'
 return ({ valid: '已验证可读取', expired: '分享已失效', password_required: '提取码有误', unavailable: '暂时无法验证' })[value.status]
}

export interface SharePreviewEntry { token: string; path: string; name: string; is_dir: boolean; size: number }
export interface SharePreview { share_validation?: ShareValidation; token: string; entries: SharePreviewEntry[]; total_size: number; file_count: number; expires_at: string }
export interface ShareSelection { previewToken: string; entryTokens: string[]; downloaderID: string; fileCount: number; totalSize: number; expiresAt: string }
export function isShareVideo(entry: SharePreviewEntry) { return !entry.is_dir && /\.(mkv|mp4|avi|mov|m4v|ts|m2ts|wmv|iso|mpg|mpeg|webm)$/i.test(entry.name) }
export function shareEntryFiles(entries: SharePreviewEntry[], entry: SharePreviewEntry) { return entry.is_dir ? entries.filter(item => !item.is_dir && item.path.startsWith(entry.path + '/')) : [entry] }
export function toggleShareEntry(entries: SharePreviewEntry[], selected: string[], entry: SharePreviewEntry): string[] {
 const leaves = shareEntryFiles(entries, entry), next = new Set(selected)
 const checked = leaves.length > 0 && leaves.every(item => next.has(item.token))
 for (const item of leaves) { if (checked) next.delete(item.token); else next.add(item.token) }
 return [...next]
}
