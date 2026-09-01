<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '@/api/client'
import { notify } from '@/toast'
import type { MediaMetadataDocument, MediaMetadataEditable, TMDBCompany, TMDBGenre, TMDBPerson } from '@/types/api'

const props = defineProps<{ open: boolean; libraryId: number; workId: string }>()
const emit = defineEmits<{ close: []; saved: [] }>()
type Tab = 'basic' | 'people' | 'images'
const tab = ref<Tab>('basic')
const loading = ref(false); const saving = ref(false); const error = ref('')
const document = ref<MediaMetadataDocument | null>(null)
const draft = ref<MediaMetadataEditable | null>(null)
const genresText = ref(''); const productionText = ref(''); const originText = ref(''); const languagesText = ref(''); const studiosText = ref('')
const directorsText = ref(''); const writersText = ref(''); const castText = ref('')
const title = computed(() => draft.value?.title || '编辑媒体元数据')

function endpoint() { return `/api/v1/media-libraries/${props.libraryId}/catalog/${encodeURIComponent(props.workId)}/metadata` }
function message(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败' }
function cloneEditable(value: MediaMetadataEditable) { return JSON.parse(JSON.stringify(value)) as MediaMetadataEditable }
function csv(values: string[]) { return values.join(', ') }
function lines(values: Array<{ name: string; character?: string; job?: string }>, mode: 'character' | 'job' | 'plain') {
  return values.map(item => mode === 'plain' ? item.name : `${item.name}${item[mode] ? ` | ${item[mode]}` : ''}`).join('\n')
}
function splitValues(value: string) { return [...new Set(value.split(/[,，\n]/).map(item => item.trim()).filter(Boolean))] }
function splitNamed(value: string, previous: TMDBCompany[]): TMDBCompany[] {
  const byName = new Map(previous.map(item => [item.name.toLocaleLowerCase(), item]))
  return splitValues(value).map(name => ({ tmdb_id: byName.get(name.toLocaleLowerCase())?.tmdb_id ?? 0, name }))
}
function splitPeople(value: string, previous: TMDBPerson[], field: 'character' | 'job'): TMDBPerson[] {
  const byName = new Map(previous.map(item => [item.name.toLocaleLowerCase(), item]))
  return value.split(/\r?\n/).map(item => item.trim()).filter(Boolean).map(line => {
    const [namePart, detailPart = ''] = line.split('|', 2)
    const name = namePart?.trim() ?? ''
    const old = byName.get(name.toLocaleLowerCase())
    return { tmdb_id: old?.tmdb_id ?? 0, name, profile_path: old?.profile_path, [field]: detailPart.trim() }
  }).filter(item => item.name)
}
function splitGenres(value: string, previous: TMDBGenre[]): TMDBGenre[] {
  const byName = new Map(previous.map(item => [item.name.toLocaleLowerCase(), item]))
  return splitValues(value).map(name => ({ id: byName.get(name.toLocaleLowerCase())?.id ?? 0, name }))
}
function bindTextFields(value: MediaMetadataEditable) {
  genresText.value = value.genres.map(item => item.name).join(', ')
  productionText.value = csv(value.production_countries); originText.value = csv(value.origin_countries); languagesText.value = csv(value.spoken_languages)
  studiosText.value = value.studios.map(item => item.name).join('\n')
  directorsText.value = lines(value.directors, 'plain'); writersText.value = lines(value.writers, 'plain'); castText.value = lines(value.cast, 'character')
}
async function load() {
  loading.value = true; error.value = ''; document.value = null; draft.value = null
  try {
    const data = await api<MediaMetadataDocument>(endpoint())
    document.value = data; draft.value = cloneEditable(data.editable); bindTextFields(data.editable)
  } catch (reason) { error.value = message(reason) }
  finally { loading.value = false }
}
async function save() {
  if (!document.value || !draft.value) return
  saving.value = true; error.value = ''
  try {
    const editable = cloneEditable(draft.value)
    editable.genres = splitGenres(genresText.value, editable.genres)
    editable.production_countries = splitValues(productionText.value); editable.origin_countries = splitValues(originText.value); editable.spoken_languages = splitValues(languagesText.value)
    editable.studios = splitNamed(studiosText.value, editable.studios)
    editable.directors = splitPeople(directorsText.value, editable.directors, 'job'); editable.writers = splitPeople(writersText.value, editable.writers, 'job'); editable.cast = splitPeople(castText.value, editable.cast, 'character')
    const updated = await api<MediaMetadataDocument>(endpoint(), { method: 'PUT', body: JSON.stringify({ revision: document.value.revision, editable }) })
    document.value = updated; draft.value = cloneEditable(updated.editable); bindTextFields(updated.editable)
    notify('完整元数据和图片选择已保存，媒体产物会自动收敛', 'success'); emit('saved')
  } catch (reason) { error.value = message(reason); notify(error.value, 'error') }
  finally { saving.value = false }
}
function close() { if (!saving.value) emit('close') }
watch(() => props.open, value => { if (value) { tab.value = 'basic'; void load() } }, { immediate: true })
</script>

<template>
  <div v-if="open" class="metadata-backdrop" @click.self="close">
    <form class="panel metadata-panel" role="dialog" aria-modal="true" aria-labelledby="full-metadata-title" @submit.prevent="save">
      <header class="flex items-start justify-between gap-4"><div><h2 id="full-metadata-title" class="m-0 text-xl">编辑《{{ title }}》</h2><p class="page-description mt-1 text-sm">作品身份仍由识别器验证；这里维护标题、简介、分类事实、演职员和 TMDB 图片。</p></div><button class="btn-secondary" type="button" :disabled="saving" @click="close">关闭</button></header>
      <div v-if="document" class="semantic-inset mt-4 flex flex-wrap gap-x-5 gap-y-1 p-3 text-xs"><span>TMDB {{ document.tmdb_id }}</span><span>{{ document.media_type === 'tv' ? '电视剧' : '电影' }}</span><span>{{ document.manual_override ? '人工元数据' : '自动元数据' }}</span></div>
      <div class="management-tabs mt-4" role="tablist" aria-label="元数据编辑分区"><button v-for="item in ([['basic','基本信息'],['people','分类与演职员'],['images','图片']] as const)" :key="item[0]" class="management-tab" :class="tab === item[0] ? 'management-tab--active' : ''" type="button" @click="tab = item[0]">{{ item[1] }}</button></div>
      <div v-if="loading" class="py-14 text-center text-muted">正在读取完整元数据…</div>
      <div v-else-if="error && !draft" class="semantic-error mt-4 p-4">{{ error }}</div>
      <div v-else-if="draft" class="metadata-body mt-4 overflow-y-auto pr-1">
        <section v-if="tab === 'basic'" class="grid gap-4 md:grid-cols-2">
          <label><span class="label">标题</span><input v-model="draft.title" class="input" required maxlength="512"></label>
          <label><span class="label">原始标题</span><input v-model="draft.original_title" class="input" maxlength="512"></label>
          <label><span class="label">上映 / 首播日期</span><input v-model="draft.release_date" class="input" type="date"></label>
          <label><span class="label">状态</span><input v-model="draft.status" class="input" maxlength="128" placeholder="Released / Returning Series"></label>
          <label><span class="label">评分（0–10）</span><input v-model.number="draft.vote_average" class="input" type="number" min="0" max="10" step="0.1"></label>
          <label><span class="label">评分人数</span><input v-model.number="draft.vote_count" class="input" type="number" min="0" step="1"></label>
          <label><span class="label">时长（分钟）</span><input v-model.number="draft.runtime_minutes" class="input" type="number" min="0" step="1"></label>
          <label v-if="document?.media_type === 'tv'"><span class="label">总季数 / 总集数</span><span class="grid grid-cols-2 gap-2"><input v-model.number="draft.season_count" class="input" type="number" min="0"><input v-model.number="draft.episode_count" class="input" type="number" min="0"></span></label>
          <label class="md:col-span-2"><span class="label">标语</span><input v-model="draft.tagline" class="input" maxlength="2048"></label>
          <label class="md:col-span-2"><span class="label">简介</span><textarea v-model="draft.overview" class="input min-h-40" maxlength="32768"></textarea></label>
        </section>
        <section v-else-if="tab === 'people'" class="grid gap-4 md:grid-cols-2">
          <label><span class="label">类型（逗号分隔）</span><textarea v-model="genresText" class="input min-h-24" placeholder="剧情, 动画"></textarea></label>
          <label><span class="label">制片公司（一行一个）</span><textarea v-model="studiosText" class="input min-h-24"></textarea></label>
          <label><span class="label">制作国家 / 地区</span><textarea v-model="productionText" class="input min-h-20" placeholder="CN, US"></textarea></label>
          <label><span class="label">原产国家 / 地区</span><textarea v-model="originText" class="input min-h-20"></textarea></label>
          <label><span class="label">原始语言</span><input v-model="draft.original_language" class="input" maxlength="16" placeholder="zh"></label>
          <label><span class="label">对白语言</span><input v-model="languagesText" class="input" placeholder="zh, en"></label>
          <label><span class="label">导演（一行一个）</span><textarea v-model="directorsText" class="input min-h-28"></textarea></label>
          <label><span class="label">编剧（一行一个）</span><textarea v-model="writersText" class="input min-h-28"></textarea></label>
          <label class="md:col-span-2"><span class="label">演员（一行一个，可写“姓名 | 角色”）</span><textarea v-model="castText" class="input min-h-40"></textarea></label>
        </section>
        <section v-else class="space-y-6">
          <div><h3 class="m-0 text-base">海报</h3><p class="page-description mt-1 text-xs">只能选择 Server 已从 TMDB 验证并保存的图片身份。</p><div class="image-grid mt-3"><label class="image-option" :class="draft.poster_path === '' ? 'image-option--selected' : ''"><input v-model="draft.poster_path" type="radio" value=""><span>不使用海报</span></label><label v-for="option in document?.poster_options ?? []" :key="option.path" class="image-option" :class="draft.poster_path === option.path ? 'image-option--selected' : ''"><input v-model="draft.poster_path" type="radio" :value="option.path"><img :src="option.url" alt="候选海报"></label></div></div>
          <div><h3 class="m-0 text-base">背景图</h3><div class="backdrop-grid mt-3"><label class="image-option" :class="draft.backdrop_path === '' ? 'image-option--selected' : ''"><input v-model="draft.backdrop_path" type="radio" value=""><span>不使用背景图</span></label><label v-for="option in document?.backdrop_options ?? []" :key="option.path" class="image-option" :class="draft.backdrop_path === option.path ? 'image-option--selected' : ''"><input v-model="draft.backdrop_path" type="radio" :value="option.path"><img :src="option.url" alt="候选背景图"></label></div></div>
        </section>
      </div>
      <p v-if="error && draft" class="semantic-error mt-4 p-3 text-sm">{{ error }}</p>
      <footer class="semantic-divider mt-4 flex justify-end gap-3 border-t pt-4"><button class="btn-secondary" type="button" :disabled="saving" @click="close">取消</button><button class="btn-primary" :disabled="saving || loading || !draft">{{ saving ? '正在保存并重建媒体产物…' : '保存元数据' }}</button></footer>
    </form>
  </div>
</template>

<style scoped>
.metadata-backdrop { position: fixed; inset: 0; z-index: 90; display: grid; place-items: center; padding: 1rem; background: color-mix(in srgb, #000 68%, transparent); }
.metadata-panel { width: min(72rem, 100%); max-height: calc(100vh - 2rem); overflow: hidden; }
.metadata-body { max-height: calc(100vh - 19rem); }
.image-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(8rem, 1fr)); gap: .75rem; }
.backdrop-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr)); gap: .75rem; }
.image-option { position: relative; min-height: 8rem; overflow: hidden; border: 2px solid var(--border); border-radius: .7rem; background: var(--surface-muted); display: grid; place-items: center; cursor: pointer; }
.image-option--selected { border-color: var(--accent); box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent) 25%, transparent); }
.image-option input { position: absolute; left: .5rem; top: .5rem; z-index: 1; }
.image-option img { width: 100%; height: 100%; min-height: 8rem; object-fit: cover; }
</style>
