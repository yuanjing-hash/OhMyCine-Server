<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import { isPreparedMediaUpgrade, mediaVersionUpgradePreviewPath, mediaVersionUpgradeSearchRoute, type MediaVersionUpgrade } from '@/media-version-upgrade'
import type { MediaCatalogEpisode } from '@/types/api'

const props = defineProps<{ libraryId: number; workId: string; title: string }>()
const auth = useAuthStore()
const router = useRouter()
const target = ref<MediaCatalogEpisode | null>(null)
const visible = ref(false)
const dialog = ref(false)
const busy = ref(false)
const error = ref('')
const upgrade = ref<MediaVersionUpgrade | null>(null)
const point = ref({ left: '0px', top: '0px' })
const action = ref<HTMLButtonElement | null>(null)
const closeButton = ref<HTMLButtonElement | null>(null)
let origin: HTMLElement | null = null
let request: AbortController | null = null
let generation = 0
function allowed() { return auth.can(Permissions.DownloadsCreate) && auth.can(Permissions.MediaLibrariesMediaDelete) && auth.can(Permissions.MediaLibrariesRead) }
function close(restore = true) {
  generation++; request?.abort(); request = null
  visible.value = false; dialog.value = false; target.value = null; upgrade.value = null; busy.value = false; error.value = ''
  if (restore) origin?.focus()
}
async function open(event: MouseEvent | KeyboardEvent, entry: MediaCatalogEpisode) {
  if (!allowed()) return
  event.preventDefault(); event.stopPropagation()
  close(false)
  target.value = { ...entry }; origin = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const bounds = origin?.getBoundingClientRect()
  const x = event instanceof MouseEvent ? event.clientX : bounds?.left ?? 16
  const y = event instanceof MouseEvent ? event.clientY : bounds?.bottom ?? 16
  point.value = { left: `${Math.min(Math.max(x, 8), Math.max(8, window.innerWidth - 240))}px`, top: `${Math.min(Math.max(y, 8), Math.max(8, window.innerHeight - 90))}px` }
  visible.value = true
  await nextTick(); action.value?.focus()
}
async function prepare() {
  if (!target.value || !allowed()) return
  visible.value = false; dialog.value = true; busy.value = true; error.value = ''
  const selected = target.value.id; const current = ++generation
  const controller = new AbortController(); request = controller
  await nextTick()
  if (current !== generation || controller.signal.aborted) return
  closeButton.value?.focus()
  try {
    const result = await api<MediaVersionUpgrade>(mediaVersionUpgradePreviewPath(props.libraryId, props.workId, selected), { method: 'POST', signal: controller.signal })
    if (current !== generation || controller.signal.aborted || !allowed()) return
    if (result.library_id !== props.libraryId || result.entry_id !== selected || result.work_token !== props.workId) throw new Error('洗版目标已变化，请重新选择')
    upgrade.value = result
  } catch (reason) { if (current === generation && !controller.signal.aborted) error.value = reason instanceof Error ? reason.message : '无法准备此版本的洗版' }
  finally { if (current === generation) { busy.value = false; request = null } }
}
function searchReplacement() { if (upgrade.value && isPreparedMediaUpgrade(upgrade.value)) void router.push(mediaVersionUpgradeSearchRoute(upgrade.value)) }
watch(() => [props.libraryId, props.workId, auth.user?.id, allowed()], () => close(false))
onBeforeUnmount(() => close(false))
defineExpose({ open, close })
</script>

<template>
  <Teleport to="body">
    <div v-if="visible" class="entry-menu-backdrop" @click.self="close()" @contextmenu.prevent.self="close()">
      <div role="menu" :aria-label="`${title} 文件版本操作`" class="entry-version-menu" :style="point" @keydown.esc.prevent="close()">
        <button ref="action" role="menuitem" type="button" @click="prepare">洗版 · 查找替代版本</button>
      </div>
    </div>
    <div v-if="dialog && target" class="entry-upgrade-backdrop" @click.self="close()" @keydown.esc.prevent="close()">
      <section role="dialog" aria-modal="true" aria-labelledby="entry-upgrade-title" class="panel entry-upgrade-dialog">
        <header class="flex items-center justify-between gap-3"><h2 id="entry-upgrade-title" class="m-0 text-lg">洗版：{{ title }}</h2><button ref="closeButton" class="btn-secondary" type="button" @click="close()">关闭</button></header>
        <p v-if="target.season != null && target.episode != null" class="text-sm">第 {{ target.season }} 季 · 第 {{ target.episode }} 集 · 当前选中的文件版本</p>
        <p class="break-all text-sm text-muted">{{ target.relative_path }}</p>
        <p class="semantic-warning p-3 text-sm">先选择并确认新的版本。新文件入库且产物就绪后，才回收本次选中的旧视频及其独占配套字幕；失败时保留旧版本。</p>
        <p v-if="busy" role="status">正在核对所选版本与回收能力…</p>
        <p v-else-if="error" role="alert" class="semantic-error p-3">{{ error }}</p>
        <p v-else-if="upgrade && upgrade.state !== 'prepared'" role="status">这个版本已有洗版任务：{{ upgrade.stage }}。{{ upgrade.error_message }}</p>
        <footer class="mt-4 flex justify-end gap-2"><button class="btn-secondary" type="button" @click="close()">取消</button><button class="btn-primary" type="button" :disabled="busy || !upgrade || !isPreparedMediaUpgrade(upgrade)" @click="searchReplacement">查找替代版本</button></footer>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.entry-menu-backdrop { position: fixed; inset: 0; z-index: 90; }
.entry-version-menu { position: fixed; min-width: 14rem; padding: .35rem; border: 1px solid var(--border); border-radius: .6rem; background: var(--surface); box-shadow: 0 12px 40px #0004; }
.entry-version-menu button { width: 100%; padding: .7rem .8rem; text-align: left; border-radius: .3rem; }
.entry-version-menu button:hover, .entry-version-menu button:focus-visible { background: var(--surface-muted); outline: 2px solid var(--primary); }
.entry-upgrade-backdrop { position: fixed; inset: 0; z-index: 95; display: grid; place-items: center; padding: 1rem; background: #0009; }
.entry-upgrade-dialog { width: min(40rem, 100%); max-height: 90vh; overflow-y: auto; }
</style>
