<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import { notify } from '@/toast'
import type { MediaCatalogLibraryWork } from '@/types/api'
import type { UserCollectionSummary, UserMediaPage } from '@/user-media-overview'

export type WorkMenuAction = 'play' | 'details' | 'favorite' | 'collection' | 'remove-collection' | 'metadata' | 'recognize' | 'rescrape' | 'exclude' | 'delete-source' | 'delete-history'
export interface WorkMenuTarget {
  title: string
  kind: 'movie' | 'series'
  works: MediaCatalogLibraryWork[]
  historyId?: string
  favorite?: boolean
  collectionId?: string
}
const emit = defineEmits<{ changed: [] }>()
const router = useRouter()
const auth = useAuthStore()
const target = ref<WorkMenuTarget | null>(null)
const position = ref({ left: 0, top: 0 })
const menu = ref<HTMLElement | null>(null)
const chooser = ref<'library' | 'collection' | null>(null)
const pending = ref<WorkMenuAction | null>(null)
const collections = ref<UserCollectionSummary[]>([])
const chosenWork = ref<MediaCatalogLibraryWork | null>(null)
const busy = ref(false)
const origin = ref<HTMLElement | null>(null)
const actions = computed(() => {
  if (!target.value) return []
  const list: { id: WorkMenuAction; label: string }[] = []
  if (target.value.works.length) {
    list.push({ id: 'play', label: '使用 Player 播放' }, { id: 'details', label: '查看详情' })
    list.push({ id: 'favorite', label: target.value.favorite ? '取消收藏' : '加入收藏' })
    list.push({ id: 'collection', label: '加入我的合集' })
    if (target.value.collectionId) list.push({ id: 'remove-collection', label: '移出当前合集' })
    if (auth.can(Permissions.MediaLibrariesScan)) list.push(
      { id: 'metadata', label: '修改元数据' }, { id: 'recognize', label: '手动识别' },
      { id: 'rescrape', label: '重新刮削' }, { id: 'exclude', label: '移出媒体库（保留源文件）' },
    )
    if (auth.can(Permissions.MediaLibrariesMediaDelete)) list.push({ id: 'delete-source', label: '删除源文件…' })
  }
  if (target.value.historyId) list.push({ id: 'delete-history', label: '删除这条观看记录' })
  return list
})

function close(restoreFocus = true) {
  target.value = null; chooser.value = null; pending.value = null
  if (restoreFocus) origin.value?.focus()
  origin.value = null
}
async function open(event: MouseEvent | KeyboardEvent, item: WorkMenuTarget) {
  event.preventDefault()
  close(false)
  origin.value = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  target.value = item
  if (event instanceof MouseEvent && event.type === 'contextmenu') position.value = { left: event.clientX, top: event.clientY }
  else {
    const bounds = origin.value?.getBoundingClientRect()
    position.value = { left: bounds?.left ?? 0, top: bounds?.bottom ?? 0 }
  }
  await nextTick()
  const bounds = menu.value?.getBoundingClientRect()
  if (bounds) position.value = {
    left: Math.max(8, Math.min(position.value.left, window.innerWidth - bounds.width - 8)),
    top: Math.max(8, Math.min(position.value.top, window.innerHeight - bounds.height - 8)),
  }
  menu.value?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
}
function keydown(event: KeyboardEvent) {
  if (event.key === 'Escape') { event.preventDefault(); close(); return }
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return
  event.preventDefault()
  const buttons = Array.from(menu.value?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? [])
  const index = buttons.indexOf(document.activeElement as HTMLButtonElement)
  buttons[(index + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length]?.focus()
}
function outside(event: PointerEvent) {
  if (target.value && !menu.value?.contains(event.target as Node) && !(event.target as Element).closest('.work-context-backdrop')) close()
}
function scroll(event: Event) { if (target.value && !chooser.value && !menu.value?.contains(event.target as Node)) close() }
async function cancelChoice() {
  chooser.value = null
  await nextTick()
  menu.value?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
}
function routeTo(work: MediaCatalogLibraryWork, panel?: string) {
  close(false)
  void router.push({ name: 'library-catalog-detail', params: { libraryID: String(work.library_id), workID: work.work_id }, query: panel ? { panel } : undefined })
}
function play(work: MediaCatalogLibraryWork) {
  const item = target.value
  if (!item) return
  const link = new URL('ohmycine://open')
  link.searchParams.set('server', window.location.origin)
  link.searchParams.set('library', String(work.library_id))
  link.searchParams.set('work', work.work_id)
  link.searchParams.set('autoplay', item.kind === 'movie' ? '1' : '0')
  close()
  window.location.href = link.toString()
}
async function execute(action: WorkMenuAction, work?: MediaCatalogLibraryWork) {
  const item = target.value
  if (!item || busy.value) return
  if (action === 'delete-history') {
    if (!item.historyId) return
    busy.value = true
    try { await api(`/api/v1/media-libraries/history/${encodeURIComponent(item.historyId)}`, { method: 'DELETE' }); notify('观看记录已删除', 'success'); close(); emit('changed') }
    catch (reason) { notify(errorMessage(reason), 'error') }
    finally { busy.value = false }
    return
  }
  if (!work) {
    if (item.works.length > 1) { pending.value = action; chooser.value = 'library'; await nextTick(); document.querySelector<HTMLElement>('[data-work-choice]')?.focus(); return }
    work = item.works[0]
  }
  if (!work) return
  if (action === 'play') return play(work)
  if (action === 'details') return routeTo(work)
  if (action === 'metadata' || action === 'recognize' || action === 'rescrape' || action === 'delete-source') return routeTo(work, action)
  if (action === 'exclude') {
    if (!window.confirm(`从“${work.library_name}”移出《${item.title}》？源媒体文件不会删除，可在“已移出”中恢复。`)) return
    busy.value = true
    try { await api(`/api/v1/media-libraries/${work.library_id}/catalog/${encodeURIComponent(work.work_id)}/exclude`, { method: 'POST' }); notify('作品已移出媒体库，源文件保留；正在重新扫描', 'success'); close(); emit('changed'); void api(`/api/v1/media-libraries/${work.library_id}/scan`, { method: 'POST', body: JSON.stringify({ mode: 'full' }) }).catch(reason => notify(`作品已移出，自动扫描失败：${errorMessage(reason)}`, 'error')) }
    catch (reason) { notify(errorMessage(reason), 'error') }
    finally { busy.value = false }
    return
  }
  if (action === 'favorite') {
    busy.value = true
    try { await api(`/api/v1/media-libraries/favorites/${encodeURIComponent(`${work.library_id}:${work.work_id}`)}`, { method: 'PUT', body: JSON.stringify({ favorite: !item.favorite }) }); notify(item.favorite ? '已取消收藏' : '已加入收藏', 'success'); close(); emit('changed') }
    catch (reason) { notify(errorMessage(reason), 'error') }
    finally { busy.value = false }
    return
  }
  if (action === 'remove-collection' && item.collectionId) {
    busy.value = true
    try { await api(`/api/v1/media-libraries/collections/${encodeURIComponent(item.collectionId)}/items/${encodeURIComponent(`${work.library_id}:${work.work_id}`)}`, { method: 'DELETE' }); notify('已移出合集', 'success'); close(); emit('changed') }
    catch (reason) { notify(errorMessage(reason), 'error') }
    finally { busy.value = false }
    return
  }
  if (action === 'collection') {
    chosenWork.value = work
    pending.value = action; chooser.value = 'collection'; busy.value = true
    try {
      collections.value = (await api<UserMediaPage<UserCollectionSummary>>('/api/v1/media-libraries/collections?source=manual&page=1&page_size=100')).list ?? []
      await nextTick()
      document.querySelector<HTMLElement>('[data-collection-choice]')?.focus()
    }
    catch (reason) { notify(errorMessage(reason), 'error'); await cancelChoice() }
    finally { busy.value = false }
    return
  }
}
async function selectLibrary(work: MediaCatalogLibraryWork) {
  const action = pending.value
  chooser.value = null
  if (action) await execute(action, work)
}
async function selectCollection(collection: UserCollectionSummary) {
  const work = chosenWork.value ?? target.value?.works[0]
  if (!work) return
  busy.value = true
  try { await api(`/api/v1/media-libraries/collections/${encodeURIComponent(collection.id)}/items`, { method: 'POST', body: JSON.stringify({ item_id: `${work.library_id}:${work.work_id}` }) }); notify('已加入合集', 'success'); close(); emit('changed') }
  catch (reason) { notify(errorMessage(reason), 'error') }
  finally { busy.value = false }
}
function errorMessage(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败' }
window.addEventListener('pointerdown', outside)
window.addEventListener('scroll', scroll, true)
onUnmounted(() => { window.removeEventListener('pointerdown', outside); window.removeEventListener('scroll', scroll, true) })
defineExpose({ open, close })
</script>

<template>
  <Teleport to="body">
    <div v-if="target" ref="menu" class="work-context-menu panel" role="menu" :aria-label="`${target.title} 操作`" :style="{ left: `${position.left}px`, top: `${position.top}px` }" @keydown="keydown">
      <button v-for="action in actions" :key="action.id" type="button" role="menuitem" :disabled="busy" class="work-context-menu__item" @click="execute(action.id)">{{ action.label }}</button>
    </div>
    <div v-if="target && chooser" class="modal-backdrop work-context-backdrop" @click.self="cancelChoice">
      <section class="panel work-context-choice" role="dialog" aria-modal="true" :aria-label="chooser === 'library' ? '选择媒体库' : '选择合集'" @keydown.esc.prevent="cancelChoice">
        <h2 class="mt-0 text-lg">{{ chooser === 'library' ? '选择目标媒体库' : '加入我的合集' }}</h2>
        <template v-if="chooser === 'library'"><button v-for="work in target.works" :key="`${work.library_id}:${work.work_id}`" data-work-choice class="semantic-list-item work-context-menu__item" @click="selectLibrary(work)">{{ work.library_name }} · {{ work.file_count }} 个文件</button></template>
        <template v-else><button v-for="collection in collections" :key="collection.id" class="semantic-list-item work-context-menu__item" data-collection-choice :disabled="busy" @click="selectCollection(collection)">{{ collection.name }}</button><p v-if="!collections.length" class="text-sm text-muted">暂无自建合集。</p></template>
        <button class="btn-secondary mt-4" @click="cancelChoice">取消</button>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.work-context-menu { position: fixed; z-index: 95; width: min(17rem, calc(100vw - 16px)); max-height: calc(100vh - 16px); overflow: auto; padding: .35rem; box-shadow: 0 16px 48px #0008; }
.work-context-menu__item { display: block; width: 100%; border: 0; border-radius: .45rem; padding: .65rem .75rem; background: transparent; color: var(--text); text-align: left; cursor: pointer; }
.work-context-menu__item:hover, .work-context-menu__item:focus-visible { background: var(--surface-muted); outline: 2px solid var(--accent); }
.work-context-menu__item:disabled { opacity: .5; cursor: default; }
.work-context-choice { width: min(28rem, 100%); max-height: 80vh; overflow: auto; }
.modal-backdrop { position: fixed; inset: 0; z-index: 96; display: grid; place-items: center; padding: 1rem; background: color-mix(in srgb, #000 64%, transparent); }
</style>
