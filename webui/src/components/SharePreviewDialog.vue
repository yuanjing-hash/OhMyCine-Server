<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from '@/api/client'
import { formatBytes } from '@/downloads'
import { isShareVideo, shareEntryFiles, toggleShareEntry, type SharePreview, type SharePreviewEntry, type ShareSelection } from '@/share-preview'
import type { TorrentRecognitionResult } from '@/sites'
import type { DownloaderSummary } from '@/types/api'

const props = defineProps<{ resultToken: string; title: string; downloaders: DownloaderSummary[] }>()
const emit = defineEmits<{ close: []; select: [selection: ShareSelection] }>()
const available = computed(() => props.downloaders.filter(item => item.enabled && item.type === 'pan115_offline' && item.capabilities.share_receive))
const dialog = ref<HTMLElement | null>(null)
const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
const downloaderID = ref('')
const preview = ref<SharePreview | null>(null)
const selected = ref<string[]>([])
const collapsed = ref<string[]>([])
const limit = ref(50)
const loading = ref(false)
const error = ref('')
const recognition = ref<Record<string, TorrentRecognitionResult>>({})
const recognitionErrors = ref<Record<string, string>>({})
const recognizing = ref<string[]>([])
let request: AbortController | null = null
let recognitionRequest = new AbortController()
let recognitionActive = 0
let closed = false
const entries = computed(() => preview.value?.entries ?? [])
const visible = computed(() => entries.value.filter(item => !collapsed.value.some(dir => item.path.startsWith(dir + '/'))))
const rows = computed(() => visible.value.slice(0, limit.value))
const chosen = computed(() => entries.value.filter(item => !item.is_dir && selected.value.includes(item.token)))
const chosenSize = computed(() => chosen.value.reduce((sum, item) => sum + item.size, 0))
function message(reason: unknown) { return reason instanceof Error ? reason.message : '分享预览暂时不可用' }
function reset() {
 request?.abort(); request = null; loading.value = false; recognitionRequest.abort(); recognitionRequest = new AbortController()
 preview.value = null; selected.value = []; collapsed.value = []; limit.value = 50
 recognition.value = {}; recognitionErrors.value = {}; recognizing.value = []; error.value = ''
}
async function load() {
 reset()
 if (!downloaderID.value) return
 const controller = new AbortController(); request = controller; loading.value = true
 const timeout = window.setTimeout(() => controller.abort(), 60000)
 try {
  const result = await api<SharePreview>('/api/v1/discovery/share-preview', { method: 'POST', signal: controller.signal, body: JSON.stringify({ result_token: props.resultToken, downloader_id: downloaderID.value }) })
  if (controller.signal.aborted || closed) return
  preview.value = result
 } catch (reason) { if (request === controller && !closed) error.value = controller.signal.aborted ? '预览超时，请重试或选择内容更少的分享。' : message(reason) }
 finally { window.clearTimeout(timeout); if (request === controller) { request = null; loading.value = false } }
}
function leaves(entry: SharePreviewEntry) { return shareEntryFiles(entries.value, entry) }
function checked(entry: SharePreviewEntry) { const items = leaves(entry); return items.length > 0 && items.every(item => selected.value.includes(item.token)) }
function partial(entry: SharePreviewEntry) { return !checked(entry) && leaves(entry).some(item => selected.value.includes(item.token)) }
function toggleFolder(entry: SharePreviewEntry) { collapsed.value = collapsed.value.includes(entry.path) ? collapsed.value.filter(path => path !== entry.path) : [...collapsed.value, entry.path] }
function selectFiles(videosOnly = false) { selected.value = entries.value.filter(item => !item.is_dir && (!videosOnly || isShareVideo(item))).map(item => item.token) }
function submit() {
 if (!preview.value || !chosen.value.length || chosen.value.length > 500) return
 if (Date.parse(preview.value.expires_at) <= Date.now()) { error.value = '预览已过期，请重新读取分享。'; return }
 emit('select', { previewToken: preview.value.token, entryTokens: chosen.value.map(item => item.token), downloaderID: downloaderID.value, fileCount: chosen.value.length, totalSize: chosenSize.value, expiresAt: preview.value.expires_at })
}
function pumpRecognition() {
 if (closed || !preview.value || recognitionRequest.signal.aborted) return
 // Only visible rows are inspected. At most two metadata requests run at once.
 for (const entry of rows.value.filter(isShareVideo)) {
  if (recognitionActive >= 2) break
  if (recognition.value[entry.token] || recognitionErrors.value[entry.token] || recognizing.value.includes(entry.token)) continue
  const controller = recognitionRequest, previewToken = preview.value.token
  const fileRequest = new AbortController()
  const abortFile = () => fileRequest.abort()
  controller.signal.addEventListener('abort', abortFile, { once: true })
  const fileTimeout = window.setTimeout(abortFile, 35000)
  recognitionActive++; recognizing.value = [...recognizing.value, entry.token]
  void api<TorrentRecognitionResult>('/api/v1/discovery/share-preview/recognize', { method: 'POST', signal: fileRequest.signal, body: JSON.stringify({ preview_token: previewToken, entry_token: entry.token }) })
   .then(result => { if (!controller.signal.aborted) recognition.value = { ...recognition.value, [entry.token]: result } })
   .catch(reason => { if (!controller.signal.aborted) recognitionErrors.value = { ...recognitionErrors.value, [entry.token]: message(reason) } })
   .finally(() => { window.clearTimeout(fileTimeout); controller.signal.removeEventListener('abort', abortFile); recognitionActive--; recognizing.value = recognizing.value.filter(token => token !== entry.token); pumpRecognition() })
 }
}
watch(rows, pumpRecognition)
watch(available, value => { if (!value.some(item => item.id === downloaderID.value)) downloaderID.value = value.length === 1 ? value[0]!.id : '' }, { immediate: true })
watch(downloaderID, () => { if (downloaderID.value) void load(); else reset() }, { immediate: true })
function close() { closed = true; request?.abort(); recognitionRequest.abort(); emit('close') }
function trapFocus(event: KeyboardEvent) {
 if (event.key !== 'Tab') return
 const controls = [...(dialog.value?.querySelectorAll<HTMLElement>('button:not(:disabled), select:not(:disabled), input:not(:disabled)') ?? [])]
 const first = controls[0], last = controls[controls.length - 1]
 if (!first || !last) return
 if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
 else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
}
onMounted(() => { void nextTick(() => dialog.value?.querySelector<HTMLElement>('button')?.focus()) })
onBeforeUnmount(() => { closed = true; request?.abort(); recognitionRequest.abort(); previousFocus?.focus() })
</script>

<template>
  <div class="modal-backdrop fixed inset-0 z-50 flex items-center justify-center p-3 sm:p-6" @click.self="close" @keydown.esc="close">
    <section ref="dialog" class="panel flex max-h-[90vh] w-full max-w-4xl flex-col" role="dialog" aria-modal="true" aria-labelledby="share-preview-title" @keydown="trapFocus">
      <header class="flex items-start justify-between gap-4">
        <div class="min-w-0"><h2 id="share-preview-title" class="m-0 text-xl">预览分享链接内部内容</h2><p class="text-subtle mt-1 line-clamp-2 text-sm">{{ title }}</p></div>
        <button class="btn-secondary shrink-0" @click="close">关闭</button>
      </header>
      <div class="my-4 flex flex-wrap items-end gap-3">
        <label class="min-w-48 flex-1"><span class="label">使用哪个 115 账号预览</span><select v-model="downloaderID" class="input"><option value="" disabled>请选择下载器</option><option v-for="item in available" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
        <button class="btn-secondary" :disabled="loading || !downloaderID" @click="load">{{ loading ? '读取中…' : '重新读取' }}</button>
      </div>
      <p v-if="!available.length" class="semantic-warning">请先配置并启用支持分享转存的 115 下载器。</p>
      <p v-if="error" class="semantic-warning" role="alert">{{ error }}</p>
      <p v-if="loading" class="text-subtle py-8 text-center" role="status">正在读取完整目录和文件大小…</p>
      <template v-if="preview">
        <div class="mb-3 flex flex-wrap items-center gap-2 text-sm"><strong class="mr-auto">{{ preview.file_count }} 个文件 · {{ formatBytes(preview.total_size) }}</strong><button class="btn-secondary" @click="selectFiles()">全选</button><button class="btn-secondary" @click="selectFiles(true)">仅选视频</button><button class="btn-secondary" @click="selected = []">清空</button></div>
        <div class="min-h-0 overflow-auto rounded-lg border border-[var(--border)]">
          <div v-for="entry in rows" :key="entry.token" class="flex items-start gap-2 border-b border-[var(--border)] p-3 last:border-b-0" :style="{ paddingLeft: `${12 + (entry.path.split('/').length - 1) * 16}px` }">
            <input type="checkbox" class="mt-1 shrink-0" :aria-label="`选择 ${entry.path}`" :checked="checked(entry)" :indeterminate="partial(entry)" @change="selected = toggleShareEntry(entries, selected, entry)" />
            <button v-if="entry.is_dir" class="shrink-0" :aria-label="`${collapsed.includes(entry.path) ? '展开' : '收起'} ${entry.name}`" :aria-expanded="!collapsed.includes(entry.path)" @click="toggleFolder(entry)">{{ collapsed.includes(entry.path) ? '▸' : '▾' }} 📁</button>
            <span v-else class="shrink-0" aria-hidden="true">{{ isShareVideo(entry) ? '▹' : '·' }}</span>
            <div class="min-w-0 flex-1"><div class="break-all text-sm font-medium">{{ entry.name }}</div>
              <div v-if="recognition[entry.token]" class="text-subtle mt-1 text-xs">{{ recognition[entry.token]?.status === 'matched' ? '已识别' : '待确认' }} · {{ recognition[entry.token]?.title }} <span v-if="recognition[entry.token]?.year">({{ recognition[entry.token]?.year }})</span> · {{ recognition[entry.token]?.media_type === 'movie' ? '电影' : recognition[entry.token]?.media_type === 'tv' ? '剧集' : '类型待定' }}</div>
              <div v-else-if="recognizing.includes(entry.token)" class="text-subtle mt-1 text-xs">正在根据文件名检测…</div>
              <div v-else-if="recognitionErrors[entry.token]" class="semantic-warning mt-1 text-xs">{{ recognitionErrors[entry.token] }}</div>
            </div><span class="text-subtle shrink-0 text-xs">{{ formatBytes(entry.size) }}</span>
          </div>
          <button v-if="rows.length < visible.length" class="btn-secondary m-3" @click="limit += 50">继续显示（剩余 {{ visible.length - rows.length }} 项）</button>
          <p v-if="!entries.length" class="text-subtle p-4">分享内没有文件。</p>
        </div>
        <footer class="mt-4 flex flex-wrap items-center justify-between gap-3"><div><strong>已选 {{ chosen.length }} 个文件 · {{ formatBytes(chosenSize) }}</strong><p class="text-subtle mt-1 text-xs">仅转存勾选内容，保留原目录结构。视频分别入库，匹配字幕随视频处理；单次最多 500 个文件。</p><p v-if="chosen.length > 500" class="semantic-warning text-xs">所选文件过多，请减少选择。</p></div><button class="btn-primary" :disabled="!chosen.length || chosen.length > 500" @click="submit">仅将所选内容转存入库 →</button></footer>
      </template>
    </section>
  </div>
</template>
