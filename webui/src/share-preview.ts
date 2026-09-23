export interface SharePreviewEntry { token: string; path: string; name: string; is_dir: boolean; size: number }
export interface SharePreview { token: string; entries: SharePreviewEntry[]; total_size: number; file_count: number; expires_at: string }
export interface ShareSelection { previewToken: string; entryTokens: string[]; downloaderID: string; fileCount: number; totalSize: number; expiresAt: string }
export function isShareVideo(entry: SharePreviewEntry) { return !entry.is_dir && /\.(mkv|mp4|avi|mov|m4v|ts|m2ts|wmv|iso|mpg|mpeg|webm)$/i.test(entry.name) }
export function shareEntryFiles(entries: SharePreviewEntry[], entry: SharePreviewEntry) { return entry.is_dir ? entries.filter(item => !item.is_dir && item.path.startsWith(entry.path + '/')) : [entry] }
export function toggleShareEntry(entries: SharePreviewEntry[], selected: string[], entry: SharePreviewEntry): string[] {
 const leaves = shareEntryFiles(entries, entry), next = new Set(selected)
 const checked = leaves.length > 0 && leaves.every(item => next.has(item.token))
 for (const item of leaves) { if (checked) next.delete(item.token); else next.add(item.token) }
 return [...next]
}
