<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import MediaWorkContextMenu, { type WorkMenuTarget } from '@/components/MediaWorkContextMenu.vue'
import { createLatestRequest } from '@/latest-request'
import { mediaCatalogEndpoint, mediaCatalogOpenTargets } from '@/media-catalog'
import { normalizeUserCollections, normalizeUserMediaItems, normalizeUserMediaPage, type UserCollectionSummary, type UserMediaItem, type UserMediaPage } from '@/user-media-overview'
import type { MediaCatalogPage } from '@/types/api'

const props = defineProps<{ mode: 'favorites' | 'automatic' | 'manual'; initialCollection?: UserCollectionSummary | null }>()
const emit = defineEmits<{ changed: [] }>()
const router = useRouter()
const workMenu = ref<InstanceType<typeof MediaWorkContextMenu> | null>(null)
const selected = ref<UserCollectionSummary | null>(null)
const items = ref<UserMediaPage<UserMediaItem>>({ list: [], total: 0, page: 1, page_size: 24, has_more: false })
const collections = ref<UserMediaPage<UserCollectionSummary>>({ list: [], total: 0, page: 1, page_size: 24, has_more: false })
const loading = ref(false), error = ref(''), busy = ref(false), actionError = ref('')
const name = ref(''), rename = ref(''), search = ref(''), searching = ref(false), searchError = ref('')
const candidates = ref<{ library_id: number; work_id: string; label: string }[]>([])
const searchPage = ref(1), searchMore = ref(false)
const reads = createLatestRequest(), searches = createLatestRequest()
let alive = true
const page = computed(() => selected.value || props.mode === 'favorites' ? items.value : collections.value)
const canEdit = computed(() => props.mode === 'favorites' || selected.value?.source === 'manual')
const itemID = (item: { library_id: number; work_id: string }) => `${item.library_id}:${item.work_id}`

async function load(requestedPage = 1) {
  const request = reads.begin()
  loading.value = true; error.value = ''
  const selection = selected.value
  const base = selection ? `/api/v1/media-libraries/collections/${encodeURIComponent(selection.id)}/items` : props.mode === 'favorites' ? '/api/v1/media-libraries/favorites' : `/api/v1/media-libraries/collections?source=${props.mode === 'manual' ? 'manual' : 'tmdb'}`
  try {
    const data = await api<unknown>(`${base}${base.includes('?') ? '&' : '?'}page=${requestedPage}&page_size=24`, { signal: request.signal })
    if (!request.isCurrent()) return
    if (selection || props.mode === 'favorites') {
      items.value = normalizeUserMediaPage(data, normalizeUserMediaItems)
      if (selection) selected.value = { ...selection, revision: items.value.revision, item_count: items.value.total }
    } else collections.value = normalizeUserMediaPage(data, normalizeUserCollections)
  } catch (reason) { if (request.isCurrent()) error.value = reason instanceof Error ? reason.message : '读取失败，请重试' }
  finally { if (request.isCurrent()) loading.value = false; request.finish() }
}
function menuTarget(item: UserMediaItem): WorkMenuTarget {
  return { title: item.title, kind: item.kind, favorite: props.mode === 'favorites', collectionId: selected.value?.source === 'manual' ? selected.value.id : undefined,
    works: [{ library_id: item.library_id, work_id: item.work_id, library_name: `媒体库 ${item.library_id}`, file_count: 0 }] }
}
function openMenu(event: MouseEvent | KeyboardEvent, item: UserMediaItem) { void workMenu.value?.open(event, menuTarget(item)) }
function menuKey(event: KeyboardEvent, item: UserMediaItem) {
  if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) openMenu(event, item)
}
function menuChanged() { emit('changed'); void load(page.value.page) }
function openCollection(item: UserCollectionSummary) { selected.value = item; rename.value = item.name; clearSearch(); void load() }
function back() { selected.value = null; clearSearch(); void load(collections.value.page) }
function clearSearch() { searches.cancel(); searching.value = false; candidates.value = []; search.value = ''; searchMore.value = false; searchError.value = '' }
async function mutate(action: () => Promise<unknown>, after?: () => void) {
  if (busy.value) return
  const scope = `${props.mode}:${selected.value?.id ?? ''}`
  busy.value = true; actionError.value = ''
  try { await action(); if (!alive) return; emit('changed'); if (scope !== `${props.mode}:${selected.value?.id ?? ''}`) return; after?.(); await load(page.value.page) }
  catch (reason) { if (alive && scope === `${props.mode}:${selected.value?.id ?? ''}`) actionError.value = reason instanceof Error ? reason.message : '操作失败' }
  finally { if (alive) busy.value = false }
}
function create() {
  const savedName = name.value.trim()
  if (!savedName) return
  void mutate(() => api('/api/v1/media-libraries/collections', { method: 'POST', body: JSON.stringify({ name: savedName, kind: 'collection' }) }), () => { name.value = ''; collections.value.page = 1 })
}
function renameCollection() {
  const collection = selected.value
  if (!collection) return
  const savedName = rename.value.trim()
  void mutate(() => api(`/api/v1/media-libraries/collections/${collection.id}`, { method: 'PATCH', body: JSON.stringify({ name: savedName, revision: collection.revision }) }), () => { if (selected.value?.id === collection.id) selected.value.name = savedName })
}
function deleteCollection() {
  const collection = selected.value
  if (!collection || !window.confirm(`删除合集“${collection.name}”？只删除合集，不删除媒体文件。`)) return
  void mutate(() => api(`/api/v1/media-libraries/collections/${collection.id}`, { method: 'DELETE' }), () => { selected.value = null })
}
function remove(item: UserMediaItem) {
  const collection = selected.value
  if (!window.confirm(collection ? '从此合集移除作品？不会删除媒体文件。' : '取消收藏此作品？')) return
  void mutate(() => collection
    ? api(`/api/v1/media-libraries/collections/${collection.id}/items/${encodeURIComponent(itemID(item))}`, { method: 'DELETE' })
    : api(`/api/v1/media-libraries/favorites/${encodeURIComponent(itemID(item))}`, { method: 'PUT', body: JSON.stringify({ favorite: false }) }))
}
function moveUp(item: UserMediaItem, index: number) {
  const collection = selected.value, before = items.value.list[index - 1]
  if (!collection || !before) return
  void mutate(() => api(`/api/v1/media-libraries/collections/${collection.id}/reorder`, { method: 'POST', body: JSON.stringify({ item_id: itemID(item), before_item_id: itemID(before), revision: collection.revision }) }))
}
async function find(requestedPage = 1) {
  const request = searches.begin()
  searching.value = true; searchError.value = ''
  try {
    const data = await api<MediaCatalogPage>(mediaCatalogEndpoint(null, { query: search.value, page: requestedPage, pageSize: 20, mediaType: '' }), { signal: request.signal })
    if (!request.isCurrent()) return
    candidates.value = data.list.flatMap(item => mediaCatalogOpenTargets(item, null).map(work => ({ library_id: work.library_id, work_id: work.work_id, label: `${item.title} · ${work.library_name}` })))
    searchPage.value = requestedPage; searchMore.value = requestedPage * 20 < data.total
  } catch (reason) { if (request.isCurrent()) searchError.value = reason instanceof Error ? reason.message : '搜索失败' }
  finally { if (request.isCurrent()) searching.value = false; request.finish() }
}
function add(item: { library_id: number; work_id: string }) {
  const collection = selected.value
  void mutate(() => collection
    ? api(`/api/v1/media-libraries/collections/${collection.id}/items`, { method: 'POST', body: JSON.stringify({ item_id: itemID(item) }) })
    : api(`/api/v1/media-libraries/favorites/${encodeURIComponent(itemID(item))}`, { method: 'PUT', body: JSON.stringify({ favorite: true }) }))
}
watch(() => [props.mode, props.initialCollection] as const, () => {
  selected.value = props.initialCollection ?? null; rename.value = selected.value?.name ?? ''; actionError.value = ''; clearSearch(); void load()
}, { immediate: true })
onUnmounted(() => { alive = false; reads.cancel(); searches.cancel() })
</script>

<template>
  <section class="space-y-4" :aria-busy="busy || loading">
    <header v-if="selected" class="panel space-y-3">
      <button class="btn-secondary" :disabled="busy" @click="back">返回合集</button>
      <h2 class="m-0 text-xl">{{ selected.name }}</h2>
      <p class="text-sm text-muted">{{ selected.source === 'tmdb' ? 'TMDB 自动合集（只读）' : '我的合集' }} · {{ selected.item_count }} 部作品</p>
      <form v-if="selected.source === 'manual'" class="flex flex-wrap gap-2" @submit.prevent="renameCollection">
        <input v-model="rename" class="input" maxlength="128" aria-label="合集名称" :disabled="busy">
        <button class="btn-primary" :disabled="busy || !rename.trim() || loading">保存名称</button>
        <button type="button" class="btn-danger" :disabled="busy" @click="deleteCollection">删除合集</button>
      </form>
    </header>
    <form v-else-if="mode === 'manual'" class="panel flex flex-wrap gap-2" @submit.prevent="create">
      <input v-model="name" class="input" maxlength="128" placeholder="新合集名称" aria-label="新合集名称" :disabled="busy">
      <button class="btn-primary" :disabled="busy || !name.trim()">创建合集</button>
    </form>
    <div v-if="actionError" role="alert" class="semantic-error p-4">{{ actionError }} <button class="btn-secondary" :disabled="busy" @click="load(page.page)">刷新当前内容</button></div>
    <details v-if="canEdit" class="panel">
      <summary>从媒体库添加作品</summary>
      <form class="mt-3 flex gap-2" @submit.prevent="find()"><input v-model="search" class="input min-w-0 flex-1" maxlength="200" aria-label="搜索要添加的作品"><button class="btn-secondary" :disabled="searching">搜索</button></form>
      <p v-if="searchError" class="semantic-error p-3" role="alert">{{ searchError }}</p>
      <div v-if="searching" class="p-3 text-muted">正在搜索…</div>
      <ul v-else class="mt-3 space-y-2"><li v-for="item in candidates" :key="itemID(item)" class="flex items-center justify-between gap-3"><span>{{ item.label }}</span><button class="btn-secondary" :disabled="busy" @click="add(item)">添加</button></li></ul>
      <div v-if="candidates.length" class="mt-3 flex gap-2"><button class="btn-secondary" :disabled="searching || searchPage <= 1" @click="find(searchPage - 1)">上一页搜索</button><button class="btn-secondary" :disabled="searching || !searchMore" @click="find(searchPage + 1)">下一页搜索</button></div>
    </details>
    <div v-if="error" class="semantic-error p-4" role="alert">{{ error }} <button class="btn-secondary" @click="load(page.page)">重试</button></div>
    <div v-else-if="loading" class="panel p-8 text-center text-muted">正在读取…</div>
    <template v-else>
      <div v-if="selected || mode === 'favorites'" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5">
        <article v-for="(item, index) in items.list" :key="itemID(item)" class="panel p-3">
          <button class="discovery-poster w-full" @contextmenu.prevent="openMenu($event, item)" @keydown="menuKey($event, item)" @click="router.push({ name: 'library-catalog-detail', params: { libraryID: String(item.library_id), workID: item.work_id } })"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong>{{ item.title }}</strong></button>
          <div v-if="canEdit" class="mt-3 flex flex-wrap gap-2"><button class="btn-secondary" :disabled="busy" @click="remove(item)">{{ selected ? '移出合集' : '取消收藏' }}</button><button v-if="selected && index > 0" class="btn-secondary" :disabled="busy" @click="moveUp(item, index)">上移</button></div>
        </article>
      </div>
      <div v-else class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5"><button v-for="item in collections.list" :key="item.id" class="panel discovery-poster text-left" @click="openCollection(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.name} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong>{{ item.name }}</strong><p class="text-sm text-muted">{{ item.item_count }} 部作品</p></button></div>
      <p v-if="!page.list.length" class="panel p-8 text-center text-muted">当前页没有内容。</p>
      <footer class="panel flex flex-wrap items-center justify-between gap-3"><span>共 {{ page.total }} 项 · 第 {{ page.page }} 页</span><div class="flex gap-2"><button class="btn-secondary" :disabled="busy || page.page <= 1" @click="load(page.page - 1)">上一页</button><button class="btn-secondary" :disabled="busy || !page.has_more" @click="load(page.page + 1)">下一页</button></div></footer>
    </template>
    <MediaWorkContextMenu ref="workMenu" @changed="menuChanged" />
  </section>
</template>
