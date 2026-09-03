<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import { mediaCatalogEndpoint, mediaCatalogOpenTargets, mediaCatalogPageCount, mediaCatalogPageSizes, type MediaCatalogPageSize, type MediaCatalogTypeFilter } from '@/media-catalog'
import {
  emptyUserMediaOverview, historyProgress, normalizeUserCollections, normalizeUserHistoryPage,
  normalizeUserMediaItems, normalizeUserMediaOverview, userMediaEndpoints,
  type UserCollectionSummary, type UserHistoryItem, type UserHistoryPage, type UserMediaItem,
  type UserMediaLibrarySummary, type UserMediaOverview,
} from '@/user-media-overview'
import type { ListResponse, MediaCatalogItem, MediaCatalogLibraryWork, MediaCatalogPage, MediaLibraryDetail } from '@/types/api'

type LibraryView = 'overview' | 'history' | 'favorites' | 'automatic' | 'manual' | 'catalog'

const router = useRouter()
const activeView = ref<LibraryView>('overview')
const overview = ref<UserMediaOverview>(emptyUserMediaOverview())
const overviewLoading = ref(true)
const overviewLoaded = ref(false)
const overviewError = ref('')
const libraries = ref<UserMediaLibrarySummary[]>([])
const history = ref<UserHistoryPage>({ list: [], total: 0, page: 1, page_size: 24, has_more: false })
const favorites = ref<UserMediaItem[]>([])
const collections = ref<UserCollectionSummary[]>([])
const selectedCollection = ref<UserCollectionSummary | null>(null)
const collectionItems = ref<UserMediaItem[]>([])
const sectionLoading = ref(false)
const sectionError = ref('')

const selectedLibrary = ref<number | null>(null)
const mediaType = ref<MediaCatalogTypeFilter>('')
const category = ref('')
const query = ref('')
const page = ref(1)
const pageSize = ref<MediaCatalogPageSize>(20)
const result = ref<MediaCatalogPage>({ list: [], total: 0, page: 1, page_size: 20, categories: [] })
const catalogLoading = ref(false)
const catalogLoaded = ref(false)
const catalogError = ref('')
const pendingSelection = ref<{ title: string; works: MediaCatalogLibraryWork[] } | null>(null)

let overviewController: AbortController | null = null
let sectionController: AbortController | null = null
let catalogController: AbortController | null = null
let sectionGeneration = 0

const categories = computed(() => result.value.categories ?? [])
const pages = computed(() => mediaCatalogPageCount(result.value.total, pageSize.value))
const visibleCollections = computed(() => collections.value.filter(item => item.source === (activeView.value === 'manual' ? 'manual' : 'tmdb')))

async function loadOverview() {
  overviewController?.abort()
  overviewController = new AbortController()
  overviewLoading.value = true
  overviewError.value = ''
  try {
    overview.value = normalizeUserMediaOverview(await api<unknown>(userMediaEndpoints.overview, { signal: overviewController.signal }))
    libraries.value = overview.value.sections.media_libraries.list.filter(item => item.id > 0)
    overviewLoaded.value = true
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') return
    overviewError.value = message(reason, '媒体总览加载失败')
  } finally { overviewLoading.value = false }
}

async function ensureCatalogLibraries() {
  if (libraries.value.length) return
  const response = await api<ListResponse<MediaLibraryDetail>>('/api/v1/media-libraries')
  libraries.value = response.list.filter(item => item.enabled).map(item => ({
    id: item.id, name: item.name, status: item.status, entry_count: item.entry_count,
    work_count: 0, last_successful_scan_at: item.last_successful_scan_at ?? undefined,
  }))
}

async function loadSection(mode: LibraryView, requestedPage = 1) {
  const generation = ++sectionGeneration
  sectionController?.abort()
  sectionController = new AbortController()
  sectionLoading.value = true
  sectionError.value = ''
  selectedCollection.value = null
  collectionItems.value = []
  try {
    if (mode === 'history') history.value = normalizeUserHistoryPage(await api<unknown>(userMediaEndpoints.history(requestedPage, 24), { signal: sectionController.signal }))
    else if (mode === 'favorites') favorites.value = normalizeUserMediaItems(await api<unknown>(userMediaEndpoints.favorites, { signal: sectionController.signal }))
    else if (mode === 'automatic' || mode === 'manual') collections.value = normalizeUserCollections(await api<unknown>(userMediaEndpoints.collections(), { signal: sectionController.signal }))
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') return
    if (generation === sectionGeneration) sectionError.value = message(reason, '内容加载失败')
  } finally { if (generation === sectionGeneration) sectionLoading.value = false }
}

async function loadCollectionItems(item: UserCollectionSummary) {
  const generation = ++sectionGeneration
  sectionController?.abort()
  sectionController = new AbortController()
  selectedCollection.value = item
  collectionItems.value = []
  sectionLoading.value = true
  sectionError.value = ''
  try {
    collectionItems.value = normalizeUserMediaItems(await api<unknown>(userMediaEndpoints.collectionItems(item.id), { signal: sectionController.signal }))
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') return
    if (generation === sectionGeneration) sectionError.value = message(reason, '合集内容加载失败')
  } finally { if (generation === sectionGeneration) sectionLoading.value = false }
}

async function loadCatalog() {
  catalogController?.abort()
  catalogController = new AbortController()
  catalogLoading.value = true
  catalogError.value = ''
  try {
    await ensureCatalogLibraries()
    result.value = await api<MediaCatalogPage>(mediaCatalogEndpoint(selectedLibrary.value, {
      page: page.value, pageSize: pageSize.value, query: query.value, mediaType: mediaType.value,
      matchStatus: '', category: category.value,
    }), { signal: catalogController.signal })
    catalogLoaded.value = true
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') return
    catalogError.value = message(reason, '媒体库内容加载失败')
  } finally { catalogLoading.value = false }
}

function activate(mode: LibraryView) {
  activeView.value = mode
  if (mode === 'overview') { if (!overviewLoaded.value) void loadOverview(); return }
  if (mode === 'catalog') { if (!catalogLoaded.value) void loadCatalog(); return }
  void loadSection(mode)
}
function openMedia(item: UserMediaItem) { void router.push({ name: 'library-catalog-detail', params: { libraryID: String(item.library_id), workID: item.work_id } }) }
function openHistory(item: UserHistoryItem) { void router.push({ name: 'library-catalog-detail', params: { libraryID: String(item.library_id), workID: item.work_id } }) }
function openOverviewCollection(item: UserCollectionSummary) { activeView.value = item.source === 'manual' ? 'manual' : 'automatic'; collections.value = [item]; void loadCollectionItems(item) }
function openOverviewLibrary(item: UserMediaLibrarySummary) {
  const changed = selectedLibrary.value !== item.id
  activeView.value = 'catalog'
  selectedLibrary.value = item.id
  if (!changed && !catalogLoaded.value) void loadCatalog()
}
function openAllCatalog() {
  selectedLibrary.value = null
  mediaType.value = ''
  category.value = ''
  query.value = ''
  page.value = 1
  activeView.value = 'catalog'
  void loadCatalog()
}
function selectLibrary(id: number | null) { category.value = ''; selectedLibrary.value = id }
function applySearch() { page.value = 1; void loadCatalog() }
function openCatalogItem(item: MediaCatalogItem) {
  const targets = mediaCatalogOpenTargets(item, selectedLibrary.value)
  if (targets.length === 1) return openTarget(targets[0])
  if (targets.length > 1) pendingSelection.value = { title: item.title, works: targets }
}
function openTarget(target: MediaCatalogLibraryWork) { pendingSelection.value = null; void router.push({ name: 'library-catalog-detail', params: { libraryID: String(target.library_id), workID: target.work_id } }) }
function formatBytes(value: number) { if (value < 1024) return `${value} B`; const units = ['KB', 'MB', 'GB', 'TB']; let size = value / 1024; let index = 0; while (size >= 1024 && index < units.length - 1) { size /= 1024; index++ }; return `${size.toFixed(size >= 10 ? 1 : 2)} ${units[index]}` }
function formatDate(value: number | string) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '时间未知' : new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date) }
function message(reason: unknown, fallback: string) { return reason instanceof Error ? reason.message : fallback }

watch([selectedLibrary, mediaType, category, pageSize], () => { if (activeView.value === 'catalog') { page.value = 1; void loadCatalog() } })
watch(page, () => { if (activeView.value === 'catalog') void loadCatalog() })
onMounted(loadOverview)
onUnmounted(() => { overviewController?.abort(); sectionController?.abort(); catalogController?.abort() })
</script>

<template>
  <section class="space-y-5">
    <header><p class="text-xs font-700 uppercase tracking-widest text-[var(--text-subtle)]">Library</p><h1 class="mt-1 text-2xl font-800">媒体库</h1><p class="page-description mt-1">统一查看 Server 媒体、继续观看、收藏与合集；只展示当前账号有权访问且仍在库的作品。</p></header>
    <nav class="panel flex gap-2 overflow-x-auto p-2" aria-label="媒体库视图" role="tablist">
      <button v-for="tab in [{ id: 'overview', label: '总览' }, { id: 'history', label: '播放历史' }, { id: 'favorites', label: '我的收藏' }, { id: 'automatic', label: '自动合集' }, { id: 'manual', label: '我的合集' }, { id: 'catalog', label: '全部作品' }] as const" :key="tab.id" class="btn-secondary shrink-0" :class="{ tabActive: activeView === tab.id }" role="tab" :aria-selected="activeView === tab.id" @click="activate(tab.id)">{{ tab.label }}</button>
    </nav>

    <template v-if="activeView === 'overview'">
      <div v-if="overviewLoading" class="panel py-14 text-center text-muted">正在读取媒体总览…</div>
      <div v-else-if="overviewError" class="semantic-error p-4"><strong>媒体总览暂时不可用</strong><p class="mt-1 text-sm">{{ overviewError }}</p><button class="btn-secondary mt-3" @click="loadOverview">重试</button></div>
      <template v-else>
        <article class="panel overflow-hidden p-0">
          <header class="overview-heading"><div><h2>继续观看</h2><p>跨设备同步的 Server 媒体进度</p></div><button class="btn-secondary" @click="activate('history')">查看全部</button></header>
          <div v-if="overview.sections.continue_watching.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.continue_watching.error_code || 'UNKNOWN' }}）</div>
          <div v-else-if="overview.sections.continue_watching.list.length" class="discovery-row p-5"><button v-for="item in overview.sections.continue_watching.list" :key="`${item.library_id}:${item.work_id}:${item.subtitle || ''}`" class="discovery-poster" @click="openHistory(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.subtitle || '继续观看' }}</small><span class="history-progress" aria-hidden="true"><i :style="{ width: `${historyProgress(item)}%` }"></i></span></button></div>
          <div v-else class="overview-empty">暂无未看完的 Server 媒体。</div>
        </article>

        <article class="panel overflow-hidden p-0">
          <header class="overview-heading"><div><h2>我的收藏</h2><p>当前账号收藏的 Server 作品</p></div><button class="btn-secondary" @click="activate('favorites')">查看全部</button></header>
          <div v-if="overview.sections.favorites.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.favorites.error_code || 'UNKNOWN' }}）</div>
          <div v-else-if="overview.sections.favorites.list.length" class="discovery-row p-5"><button v-for="item in overview.sections.favorites.list" :key="`${item.library_id}:${item.work_id}`" class="discovery-poster" @click="openMedia(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.release_year || '年份未知' }}<template v-if="item.rating"> · {{ item.rating.toFixed(1) }}</template></small></button></div>
          <div v-else class="overview-empty">暂时没有收藏作品。</div>
        </article>

        <div class="grid gap-5 xl:grid-cols-2">
          <article class="panel overflow-hidden p-0"><header class="overview-heading"><div><h2>自动合集</h2><p>首次扫库后由 TMDB 归集</p></div><button class="btn-secondary" @click="activate('automatic')">查看全部</button></header><div v-if="overview.sections.automatic_collections.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.automatic_collections.error_code || 'UNKNOWN' }}）</div><div v-else-if="overview.sections.automatic_collections.list.length" class="discovery-row p-5"><button v-for="item in overview.sections.automatic_collections.list" :key="item.id" class="discovery-poster" @click="openOverviewCollection(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.name} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.name">{{ item.name }}</strong><small>{{ item.item_count }} 部作品</small></button></div><div v-else class="overview-empty">扫库后，包含至少两部在库电影的 TMDB 合集会自动出现。</div></article>
          <article class="panel overflow-hidden p-0"><header class="overview-heading"><div><h2>我的合集</h2><p>当前账号自行建立的合集</p></div><button class="btn-secondary" @click="activate('manual')">查看全部</button></header><div v-if="overview.sections.manual_collections.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.manual_collections.error_code || 'UNKNOWN' }}）</div><div v-else-if="overview.sections.manual_collections.list.length" class="discovery-row p-5"><button v-for="item in overview.sections.manual_collections.list" :key="item.id" class="discovery-poster" @click="openOverviewCollection(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.name} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.name">{{ item.name }}</strong><small>{{ item.item_count }} 部作品</small></button></div><div v-else class="overview-empty">暂时没有自建合集。</div></article>
        </div>

        <article class="panel overflow-hidden p-0"><header class="overview-heading"><div><h2>最近入库</h2><p>各媒体库最新完成入库的作品</p></div><button class="btn-secondary" @click="openAllCatalog">全部作品</button></header><div v-if="overview.sections.recently_added.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.recently_added.error_code || 'UNKNOWN' }}）</div><div v-else-if="overview.sections.recently_added.list.length" class="discovery-row p-5"><button v-for="item in overview.sections.recently_added.list" :key="`${item.library_id}:${item.work_id}`" class="discovery-poster" @click="openMedia(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.kind === 'series' ? `${item.season_count} 季 · ${item.episode_count} 集` : item.category_name || '电影' }}</small></button></div><div v-else class="overview-empty">当前还没有已入库作品。</div></article>

        <article class="panel overflow-hidden p-0"><header class="overview-heading"><div><h2>媒体库</h2><p>Server 已启用且当前账号可访问的库</p></div><button class="btn-secondary" @click="openAllCatalog">全部库</button></header><div v-if="overview.sections.media_libraries.status === 'unavailable'" class="overview-empty">该栏目暂时不可用（{{ overview.sections.media_libraries.error_code || 'UNKNOWN' }}）</div><div v-else-if="overview.sections.media_libraries.list.length" class="library-summary-grid p-5"><button v-for="item in overview.sections.media_libraries.list" :key="item.id" class="semantic-list-item grid grid-cols-[4rem_minmax(0,1fr)] items-center gap-4 p-3 text-left" @click="openOverviewLibrary(item)"><div class="library-artwork"><img v-if="item.artwork_url" :src="item.artwork_url" alt=""><span v-else>库</span></div><span class="min-w-0"><strong class="block truncate">{{ item.name }}</strong><small class="mt-1 block text-muted">{{ item.work_count }} 部作品 · {{ item.entry_count }} 个文件</small><small class="mt-1 block text-subtle">{{ item.last_successful_scan_at ? `最近扫描 ${formatDate(item.last_successful_scan_at)}` : '尚未完成扫描' }}</small></span></button></div><div v-else class="overview-empty">没有可访问的媒体库。</div></article>
      </template>
    </template>

    <template v-else-if="activeView === 'history'">
      <div v-if="sectionLoading" class="panel py-14 text-center text-muted">正在读取播放历史…</div>
      <div v-else-if="sectionError" class="semantic-error p-4"><strong>播放历史暂时不可用</strong><p class="mt-1 text-sm">{{ sectionError }}</p><button class="btn-secondary mt-3" @click="loadSection('history', history.page)">重试</button></div>
      <template v-else><div v-if="history.list.length" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5 2xl:grid-cols-7"><button v-for="item in history.list" :key="`${item.library_id}:${item.work_id}:${item.subtitle || ''}`" class="discovery-poster" @click="openHistory(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.subtitle || (item.completed ? '已看完' : '已观看') }}</small><small>{{ formatDate(item.updated_at) }}</small><span class="history-progress" aria-hidden="true"><i :style="{ width: `${historyProgress(item)}%` }"></i></span></button></div><div v-else class="panel py-14 text-center text-muted">当前 Server 媒体库没有播放历史。</div><footer v-if="history.total" class="panel flex flex-wrap items-center justify-between gap-3 py-3"><span class="text-sm text-muted">共 {{ history.total }} 条记录</span><div class="flex items-center gap-2"><button class="btn-secondary" :disabled="history.page <= 1" @click="loadSection('history', history.page - 1)">上一页</button><span class="text-sm">第 {{ history.page }} 页</span><button class="btn-secondary" :disabled="!history.has_more" @click="loadSection('history', history.page + 1)">下一页</button></div></footer></template>
    </template>

    <template v-else-if="activeView === 'favorites'">
      <div v-if="sectionLoading" class="panel py-14 text-center text-muted">正在读取收藏…</div><div v-else-if="sectionError" class="semantic-error p-4"><strong>收藏暂时不可用</strong><p class="mt-1 text-sm">{{ sectionError }}</p><button class="btn-secondary mt-3" @click="loadSection('favorites')">重试</button></div><div v-else-if="favorites.length" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5 2xl:grid-cols-7"><button v-for="item in favorites" :key="`${item.library_id}:${item.work_id}`" class="discovery-poster" @click="openMedia(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.kind === 'series' ? `${item.season_count} 季 · ${item.episode_count} 集` : item.release_year || '电影' }}</small></button></div><div v-else class="panel py-14 text-center text-muted">暂时没有收藏作品。</div>
    </template>

    <template v-else-if="activeView === 'automatic' || activeView === 'manual'">
      <div v-if="sectionError" class="semantic-error p-4"><strong>合集暂时不可用</strong><p class="mt-1 text-sm">{{ sectionError }}</p><button class="btn-secondary mt-3" @click="loadSection(activeView)">重试</button></div><div v-else-if="sectionLoading" class="panel py-14 text-center text-muted">正在读取合集…</div>
      <template v-else-if="selectedCollection"><header class="panel"><button class="btn-secondary mb-3" @click="loadSection(activeView)">返回合集</button><h2 class="m-0 text-xl">{{ selectedCollection.name }}</h2><p class="page-description mt-1">{{ selectedCollection.source === 'tmdb' ? 'TMDB 自动合集' : '我的合集' }} · {{ selectedCollection.item_count }} 部作品</p></header><div v-if="collectionItems.length" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5 2xl:grid-cols-7"><button v-for="item in collectionItems" :key="`${item.library_id}:${item.work_id}`" class="discovery-poster" @click="openMedia(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.release_year || (item.kind === 'series' ? '电视剧' : '电影') }}</small></button></div><div v-else class="panel py-14 text-center text-muted">这个合集里暂时没有可访问的在库作品。</div></template>
      <div v-else-if="visibleCollections.length" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5 2xl:grid-cols-7"><button v-for="item in visibleCollections" :key="item.id" class="discovery-poster" @click="loadCollectionItems(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.name} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.name">{{ item.name }}</strong><small>{{ item.item_count }} 部作品</small></button></div><div v-else class="panel py-14 text-center text-muted">{{ activeView === 'automatic' ? '当前没有可展示的自动合集。' : '暂时没有自建合集。' }}</div>
    </template>

    <template v-else>
      <nav class="panel flex gap-2 overflow-x-auto p-2" aria-label="选择媒体库"><button class="btn-secondary shrink-0" :class="{ tabActive: selectedLibrary == null }" @click="selectLibrary(null)">全部库</button><button v-for="library in libraries" :key="library.id" class="btn-secondary shrink-0" :class="{ tabActive: selectedLibrary === library.id }" @click="selectLibrary(library.id)">{{ library.name }}</button></nav>
      <div class="panel grid gap-3 md:grid-cols-[minmax(12rem,1fr)_auto_auto_auto]"><form class="flex gap-2" @submit.prevent="applySearch"><input v-model="query" class="input min-w-0 flex-1" maxlength="200" placeholder="搜索标题" aria-label="搜索媒体库标题"><button class="btn-primary">搜索</button></form><select v-model="mediaType" class="input" aria-label="媒体类型"><option value="">全部类型</option><option value="movie">电影</option><option value="series">电视剧</option></select><select v-model="category" class="input" aria-label="媒体分类"><option value="">全部分类</option><option v-for="item in categories" :key="item" :value="item">{{ item }}</option></select><select v-model="pageSize" class="input" aria-label="每页作品数"><option v-for="size in mediaCatalogPageSizes" :key="size" :value="size">每页 {{ size }}</option></select></div>
      <div v-if="catalogLoading" class="panel py-14 text-center text-muted">正在读取媒体库海报墙…</div><div v-else-if="catalogError" class="semantic-error p-4"><strong>媒体库暂时不可用</strong><p class="mt-1 text-sm">{{ catalogError }}</p><button class="btn-secondary mt-3" @click="loadCatalog">重试</button></div>
      <template v-else><div v-if="result.list.length" class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5 2xl:grid-cols-7"><button v-for="item in result.list" :key="item.id" class="discovery-poster" @click="openCatalogItem(item)"><div class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></div><strong :title="item.title">{{ item.title }}</strong><small>{{ item.kind === 'series' ? `${item.season_count} 季 · ${item.episode_count} 集` : formatBytes(item.size) }}</small><small>{{ item.library_works.length }} 个库 · {{ item.match_status === 'matched' ? '已匹配' : '待识别' }}</small></button></div><div v-else class="panel py-14 text-center text-muted">当前筛选下没有已入库作品。</div><footer v-if="result.total" class="panel flex flex-wrap items-center justify-between gap-3 py-3"><span class="text-sm text-muted">共 {{ result.total }} 部作品</span><div class="flex items-center gap-2"><button class="btn-secondary" :disabled="page <= 1" @click="page--">上一页</button><span class="text-sm">{{ page }} / {{ pages }}</span><button class="btn-secondary" :disabled="page >= pages" @click="page++">下一页</button></div></footer></template>
    </template>

    <div v-if="pendingSelection" class="modal-backdrop" @click.self="pendingSelection = null"><section class="panel modal-panel" role="dialog" aria-modal="true" aria-labelledby="library-selection-title"><header class="flex items-center justify-between gap-3"><div><h2 id="library-selection-title" class="m-0 text-lg">选择媒体库</h2><p class="page-description mt-1 text-sm">《{{ pendingSelection.title }}》存在于多个媒体库。请选择要查看和管理的单个库。</p></div><button class="btn-secondary" aria-label="关闭媒体库选择" @click="pendingSelection = null">关闭</button></header><div class="mt-4 grid gap-2"><button v-for="work in pendingSelection.works" :key="`${work.library_id}:${work.work_id}`" class="semantic-list-item flex items-center justify-between gap-3 p-4 text-left" @click="openTarget(work)"><strong>{{ work.library_name }}</strong><span class="status-chip">{{ work.file_count }} 个文件</span></button></div></section></div>
  </section>
</template>

<style scoped>
.tabActive { border-color: var(--accent); background: var(--accent-soft); color: var(--accent); }
.overview-heading { display: flex; align-items: center; justify-content: space-between; gap: 1rem; padding: 1rem 1.25rem; border-bottom: 1px solid var(--border); }
.overview-heading h2 { margin: 0; font-size: 1.05rem; }
.overview-heading p { margin: .2rem 0 0; color: var(--text-muted); font-size: .75rem; }
.overview-empty { padding: 2rem 1.25rem; color: var(--text-muted); font-size: .875rem; text-align: center; }
.history-progress { display: block; height: .22rem; margin-top: .55rem; overflow: hidden; border-radius: 999px; background: var(--surface-muted); }
.history-progress i { display: block; height: 100%; border-radius: inherit; background: var(--accent); }
.library-summary-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(18rem, 100%), 1fr)); gap: .75rem; }
.library-artwork { display: grid; aspect-ratio: 1; place-items: center; overflow: hidden; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--surface-muted); color: var(--text-subtle); }
.library-artwork img { width: 100%; height: 100%; object-fit: cover; }
.modal-backdrop { position: fixed; inset: 0; z-index: 80; display: grid; place-items: center; padding: 1rem; background: color-mix(in srgb, #000 64%, transparent); }
.modal-panel { width: min(36rem, 100%); }
@media (max-width: 639px) { .overview-heading { align-items: flex-start; } .overview-heading .btn-secondary { padding-inline: .65rem; } }
</style>
