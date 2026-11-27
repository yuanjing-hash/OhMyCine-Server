<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, apiProgress } from '@/api/client'
import { mediaCatalogActionEndpoint, mediaCatalogDetailEndpoint } from '@/media-catalog'
import { createLatestRequest } from '@/latest-request'
import type { MediaCatalogDeletionPreview, MediaCatalogDeletionProgress, MediaCatalogDeletionResult } from '@/types/api'

const props = defineProps<{ libraryId: number; workId: string; title: string; deletionId?: string; onCompleted: (libraryId: number, workId: string) => void }>()
const emit = defineEmits<{ close: [] }>()
// Confirmation can finish after this window closes. Keep its original owner
// callback and work scope instead of emitting from an unmounted component.
const completedCallback = props.onCompleted
const libraryId = props.libraryId; const workId = props.workId
const route = useRoute(); const router = useRouter()
const preview = ref<MediaCatalogDeletionPreview | null>(null)
const progress = ref<MediaCatalogDeletionProgress | null>(null)
const preparing = ref(false); const confirming = ref(false); const observing = ref(false)
const phrase = ref(''); const error = ref(''); const deletionID = ref(props.deletionId ?? '')
const reads = createLatestRequest()
let mounted = true; let timer: ReturnType<typeof setTimeout> | undefined; let completedEmitted = false
const title = computed(() => preview.value?.title || progress.value?.title || props.title)
const completed = computed(() => progress.value?.phase === 'completed')
const active = computed(() => confirming.value || ['deleting', 'committing'].includes(progress.value?.phase ?? ''))
const heading = computed(() => preparing.value ? '正在准备删除' : completed.value ? '删除完成' : active.value ? '正在删除源文件' : preview.value ? '确认删除源文件' : '删除源文件')
const percentage = computed(() => progress.value?.total_files ? Math.round(progress.value.processed_files / progress.value.total_files * 100) : 0)
const base = mediaCatalogDetailEndpoint(props.libraryId, props.workId)

function report(value: MediaCatalogDeletionProgress) { if (mounted) progress.value = value }
function reportCompleted() { if (!completedEmitted) { completedEmitted = true; completedCallback(libraryId, workId) } }
async function prepare() {
  clearTimeout(timer)
  const request = reads.begin()
  preparing.value = true; error.value = ''; preview.value = null; progress.value = null; phrase.value = ''
  try {
    const value = await apiProgress<MediaCatalogDeletionPreview, MediaCatalogDeletionProgress>(mediaCatalogActionEndpoint(props.libraryId, props.workId, 'deletion-preview'), { method: 'POST', body: '{}', signal: request.signal }, value => { if (request.isCurrent()) report(value) })
    if (!request.isCurrent()) return
    preview.value = value; deletionID.value = value.deletion_id
  } catch (reason) { if (request.isCurrent()) error.value = message(reason) }
  finally { if (request.isCurrent()) preparing.value = false; request.finish() }
}
async function observe() {
  if (!deletionID.value || !mounted) return
  const request = reads.begin()
  observing.value = true
  try {
    const value = await api<MediaCatalogDeletionProgress>(`${base}/deletion-status/${encodeURIComponent(deletionID.value)}`, { signal: request.signal })
    if (!request.isCurrent()) return
    progress.value = value
    if (value.phase === 'completed') { error.value = ''; reportCompleted() }
    else if (value.phase === 'deleting' || value.phase === 'committing') timer = setTimeout(() => { void observe() }, 2000)
  } catch (reason) { if (request.isCurrent()) error.value = message(reason) }
  finally { if (request.isCurrent()) observing.value = false; request.finish() }
}
async function confirm() {
  const selected = preview.value
  if (!selected || phrase.value !== selected.title || confirming.value) return
  confirming.value = true; error.value = ''; progress.value = null; clearTimeout(timer)
  try {
    // The URL retains only a scoped record ID, never the confirmation token.
    await router.replace({ query: { ...route.query, deletion: selected.deletion_id, deletionLibrary: String(props.libraryId), deletionWork: props.workId } })
    const result = await apiProgress<MediaCatalogDeletionResult, MediaCatalogDeletionProgress>(mediaCatalogActionEndpoint(props.libraryId, props.workId, 'deletion-confirm'), { method: 'POST', body: JSON.stringify({ token: selected.confirmation_token }) }, report)
    if (result.deleted) {
      report({ deletion_id: selected.deletion_id, phase: 'completed', title: selected.title, processed_files: selected.file_count, total_files: selected.file_count, removed_files: result.removed_files, missing_files: result.missing_files })
      reportCompleted()
    }
  } catch (reason) {
    if (mounted) { error.value = message(reason); await observe() }
  } finally { if (mounted) { confirming.value = false; preview.value = null } }
}
async function close() {
  reads.cancel(); clearTimeout(timer)
  if (!active.value && route.query.deletion === deletionID.value) {
    const query = { ...route.query }; delete query.deletion; delete query.deletionLibrary; delete query.deletionWork
    await router.replace({ query })
  }
  emit('close')
}
function message(reason: unknown) { return reason instanceof Error ? reason.message : '删除操作未完成' }
function bytes(value: number) { return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 }).format(value / 1024 ** 3) + ' GiB' }
onMounted(() => { if (props.deletionId) void observe(); else void prepare() })
onBeforeUnmount(() => { mounted = false; reads.cancel(); clearTimeout(timer) })
</script>

<template>
  <Teleport to="body">
    <div class="deletion-backdrop" @click.self="close" @keydown.esc.stop="close">
      <section class="panel deletion-panel" role="dialog" aria-modal="true" aria-labelledby="catalog-deletion-title">
        <div class="flex items-start justify-between gap-4"><h2 id="catalog-deletion-title" class="m-0 text-xl">{{ heading }}</h2><button class="btn-secondary" type="button" aria-label="关闭删除窗口" @click="close">关闭</button></div>
        <p class="mt-3 break-words font-semibold">{{ title }}</p>
        <div v-if="preparing || active || observing || progress" class="semantic-inset mt-4 p-4" role="status" aria-live="polite">
          <p class="m-0 text-sm">{{ preparing ? '正在读取本次删除清单' : preview && !confirming ? '删除清单已准备，等待确认' : completed ? '源文件删除与媒体库更新已完成' : progress?.phase === 'committing' ? '正在更新媒体库目录' : progress?.phase === 'interrupted' ? '删除已中断，逐文件进度已保存' : active ? '正在删除源文件' : observing ? '正在读取已保存的进度' : '尚未执行删除' }}</p>
          <template v-if="progress?.total_files">
            <progress class="mt-3 w-full" :max="progress.total_files" :value="progress.processed_files" />
            <p class="mb-0 mt-2 text-sm">{{ preparing || preview && !confirming ? '已读取' : '已处理' }} {{ progress.processed_files }} / {{ progress.total_files }} 个文件 · {{ percentage }}%</p>
            <p v-if="!preparing && !preview && (progress.removed_files || progress.missing_files || completed)" class="mb-0 mt-1 text-sm text-muted">已移除 {{ progress.removed_files }} 个<template v-if="progress.missing_files">，原文件已不存在 {{ progress.missing_files }} 个</template></p>
          </template>
        </div>
        <div v-if="error" class="semantic-error mt-4 p-3" role="alert">{{ error }}</div>
        <template v-if="preview && !confirming">
          <p class="mt-4 text-sm">将从“{{ preview.library_name }}”删除 {{ preview.file_count }} 个文件（{{ bytes(preview.total_bytes) }}）。</p>
          <div class="semantic-warning p-3"><p v-for="warning in preview.warnings" :key="warning" class="mb-1 text-sm">{{ warning }}</p></div>
          <ul class="mt-3 max-h-36 overflow-y-auto text-xs text-muted"><li v-for="path in preview.relative_paths" :key="path" class="break-all">{{ path }}</li></ul>
          <label class="mt-4 block text-sm">输入作品标题 <strong>{{ preview.title }}</strong> 以确认<input v-model="phrase" class="input mt-2 w-full" autocomplete="off"></label>
        </template>
        <div class="mt-5 flex flex-wrap justify-end gap-2">
          <button v-if="preview && !confirming" class="btn-danger" type="button" :disabled="phrase !== preview.title" @click="confirm">确认删除源文件</button>
          <button v-else-if="!preparing && !active && !completed" class="btn-primary" type="button" :disabled="observing" @click="prepare">重新读取删除清单</button>
          <button v-if="completed" class="btn-primary" type="button" @click="close">完成</button>
        </div>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.deletion-backdrop { position: fixed; inset: 0; z-index: 100; display: grid; place-items: center; padding: 1rem; background: color-mix(in srgb, #000 64%, transparent); }
.deletion-panel { width: min(38rem, 100%); max-height: 90vh; overflow: auto; padding: 1.5rem; }
progress { height: .6rem; accent-color: var(--accent); }
</style>
