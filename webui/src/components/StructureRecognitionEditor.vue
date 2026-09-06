<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
import type { ListResponse, MediaRecognitionSummary, TMDBCandidate } from '@/types/api'

const props = defineProps<{ libraryId: number; recognitionToken: string }>()
const emit = defineEmits<{ close: []; saved: [item: MediaRecognitionSummary] }>()
const request = createLatestRequest()
const item = ref<MediaRecognitionSummary | null>(null)
const titleInput = ref<HTMLInputElement | null>(null)
const title = ref(''), year = ref(''), mediaType = ref<'movie' | 'tv'>('movie')
const loading = ref(false), saving = ref(false), searched = ref(false), error = ref('')
const candidates = ref<TMDBCandidate[]>([])
const endpoint = () => `/api/v1/media-libraries/${props.libraryId}/recognitions/${encodeURIComponent(props.recognitionToken)}`
const message = (reason: unknown) => reason instanceof Error ? reason.message : '请求失败'

async function load() {
  const current = request.begin()
  item.value = null; loading.value = true; saving.value = false; error.value = ''; candidates.value = []; searched.value = false
  try {
    const result = await api<MediaRecognitionSummary>(endpoint(), { signal: current.signal })
    if (!current.isCurrent()) return
    item.value = result
    title.value = (result.source_directory && !['媒体库根目录', '目录名不可显示'].includes(result.source_directory) ? result.source_directory : result.title || result.source_summary.replace(/\.[^.]+$/, '')).trim()
    year.value = result.release_year ? String(result.release_year) : ''
    mediaType.value = result.media_type || 'movie'
    await nextTick()
    if (current.isCurrent()) titleInput.value?.focus()
  } catch (reason) { if (current.isCurrent()) error.value = `无法读取该项识别记录：${message(reason)}` }
  finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
async function search() {
  if (!item.value || saving.value) return
  if (!title.value.trim()) { error.value = '请输入作品标题。'; return }
  if (year.value && (!/^\d{4}$/.test(year.value) || Number(year.value) < 1888 || Number(year.value) > 2200)) { error.value = '年份必须在 1888–2200 之间。'; return }
  const current = request.begin()
  loading.value = true; error.value = ''; candidates.value = []; searched.value = true
  const params = new URLSearchParams({ title: title.value.trim(), media_type: mediaType.value })
  if (year.value) params.set('year', year.value)
  try {
    const result = await api<ListResponse<TMDBCandidate>>(`${endpoint()}/tmdb-candidates?${params}`, { signal: current.signal })
    if (current.isCurrent()) candidates.value = result.list
  } catch (reason) { if (current.isCurrent()) error.value = message(reason) }
  finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
async function save(candidate: TMDBCandidate) {
  if (saving.value) return
  const current = request.begin()
  saving.value = true; error.value = ''
  try {
    const saved = await api<MediaRecognitionSummary>(`${endpoint()}/override`, { method: 'PUT', body: JSON.stringify({ tmdb_id: candidate.id, media_type: candidate.media_type }), signal: current.signal })
    if (current.isCurrent()) emit('saved', saved)
  } catch (reason) { if (current.isCurrent()) error.value = `保存未确认，请核对后再试：${message(reason)}` }
  finally { if (current.isCurrent()) saving.value = false; current.finish() }
}
watch(() => [props.libraryId, props.recognitionToken], () => void load(), { immediate: true })
onBeforeUnmount(() => request.cancel())
</script>

<template>
  <form class="semantic-inset mt-4 grid gap-3 p-4" @submit.prevent="search">
    <h3 class="m-0">手动识别当前问题</h3>
    <p class="text-subtle m-0 text-sm">只修改这一项的作品身份；保存后返回诊断列表。文件不会移动或回收，真正整理仍需预览后确认。</p>
    <p v-if="item" class="m-0 break-all text-sm">所在目录：{{ item.source_directory }} · {{ item.source_summary }}</p>
    <p v-if="error" class="semantic-error m-0 p-3" role="alert">{{ error }}</p>
    <template v-if="item">
      <label>搜索标题<input ref="titleInput" v-model="title" class="input" required maxlength="256" :disabled="saving" /></label>
      <div class="grid gap-3 sm:grid-cols-2"><label>作品类型<select v-model="mediaType" class="input" :disabled="saving"><option value="movie">电影</option><option value="tv">剧集</option></select></label><label>年份（可选）<input v-model="year" class="input" inputmode="numeric" maxlength="4" :disabled="saving" /></label></div>
      <button class="btn-primary" :disabled="loading || saving">{{ loading ? '搜索中…' : '搜索 TMDB' }}</button>
      <p v-if="searched && !loading && candidates.length === 0" class="text-subtle m-0">没有找到候选，请调整标题、类型或年份。</p>
      <button v-for="candidate in candidates" :key="`${candidate.media_type}-${candidate.id}`" class="btn-secondary text-left" type="button" :disabled="saving" @click="save(candidate)">{{ candidate.title }} · {{ candidate.release_year || '年份未知' }} · TMDB {{ candidate.id }} · 保存此识别</button>
    </template>
    <p v-else-if="loading" class="text-subtle m-0">正在读取当前问题的识别记录…</p>
    <button class="btn-secondary" type="button" :disabled="saving" @click="emit('close')">取消并返回诊断</button>
  </form>
</template>
