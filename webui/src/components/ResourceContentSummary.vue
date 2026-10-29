<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { formatBytes } from '@/downloads'
import { compactEpisodeSet, shareContentGroupLabel } from '@/share-content'
import { runResourceRead, type ResourceReadLane } from '@/owned-read-queue'
import { parseResourceContent, resourceContentPreviewPath, resourceContentSelectionPath, type ResourceContentPreview, type ResourceContentSelection } from '@/resource-content'

const props = defineProps<{ resultToken: string; preview?: ResourceContentPreview; loading?: boolean; error?: string; canSelect?: boolean; readLane?: ResourceReadLane }>()
const emit = defineEmits<{ retry: []; select: [{ resultToken: string; selection: ResourceContentSelection }] }>()
const current = ref<ResourceContentPreview>()
const expanded = ref(false)
const selected = ref<string[]>([])
const busy = ref(false)
const localError = ref('')
let generation = 0
let request: AbortController | undefined
const visibleFiles = computed(() => (current.value?.files ?? []).slice(0, expanded.value ? undefined : 8))
const selectedBytes = computed(() => (current.value?.files ?? []).filter(file => selected.value.includes(file.token) && !file.is_dir).reduce((sum, file) => sum + file.size, 0))
watch(() => [props.resultToken, props.preview?.token] as const, () => {
  generation++; request?.abort(); request = undefined
  current.value = props.preview; selected.value = []; expanded.value = false; busy.value = false; localError.value = ''
}, { immediate: true })

async function more() {
  const preview = current.value
  if (!preview || preview.next_offset === undefined || busy.value) return
  const owned = ++generation
  const token = props.resultToken
  const controller = new AbortController(); request = controller; busy.value = true; localError.value = ''
  try {
    const value = parseResourceContent(await runResourceRead(props.readLane ?? 'torrent-content', controller.signal, () => api<unknown>(resourceContentPreviewPath, { method: 'POST', body: JSON.stringify({ result_token: token, preview_token: preview.token, offset: preview.next_offset }), signal: controller.signal })))
    if (generation !== owned || controller.signal.aborted) return
    if (!value || value.token !== preview.token) throw new Error('文件清单已变化，请重新读取')
    current.value = { ...value, files: [...preview.files, ...value.files] }
    expanded.value = true
  } catch (reason) { if (generation === owned && !controller.signal.aborted) localError.value = reason instanceof Error ? reason.message : '文件清单暂不可用' }
  finally { if (generation === owned) { request = undefined; busy.value = false } }
}
async function accept() {
  const preview = current.value
  if (!props.canSelect || !preview?.complete || !selected.value.length || busy.value) return
  const owned = ++generation
  const token = props.resultToken
  const controller = new AbortController(); request = controller; busy.value = true; localError.value = ''
  try {
    const selectedTokens = [...selected.value]
    const selection = await runResourceRead(props.readLane ?? 'torrent-content', controller.signal, () => api<ResourceContentSelection>(resourceContentSelectionPath, { method: 'POST', body: JSON.stringify({ result_token: token, preview_token: preview.token, selected_entry_tokens: selectedTokens }), signal: controller.signal }))
    if (generation !== owned || controller.signal.aborted) return
    if (!selection.selection_token || !Number.isSafeInteger(selection.selected_bytes) || selection.selected_bytes < 0) throw new Error('选择凭证无效，请重新读取')
    emit('select', { resultToken: token, selection })
  } catch (reason) { if (generation === owned && !controller.signal.aborted) localError.value = reason instanceof Error ? reason.message : '文件选择暂不可用' }
  finally { if (generation === owned) { request = undefined; busy.value = false } }
}
onBeforeUnmount(() => { generation++; request?.abort() })
</script>

<template>
  <section class="semantic-inset mt-3 min-w-0 p-3 text-sm" aria-label="实际文件内容">
    <div class="flex items-center justify-between gap-3"><strong>{{ current?.complete ? '已读取实际文件' : loading ? '正在读取实际文件…' : '实际文件尚未确认' }}</strong><button class="btn-secondary py-1 text-xs" type="button" :disabled="loading || busy" @click="emit('retry')">重新读取文件</button></div>
    <template v-if="current">
      <p class="text-subtle mb-0 mt-2 text-xs">共 {{ current.total_count }} 项 · {{ formatBytes(current.total_size) }}<template v-if="current.analysis"> · {{ current.analysis.video_count }} 个视频</template></p>
      <p v-for="(group, index) in current.analysis?.groups ?? []" :key="index" class="mb-0 mt-2">{{ shareContentGroupLabel(group) }}</p>
      <ul class="m-0 mt-3 list-none space-y-2 p-0" aria-label="实际文件名">
        <li v-for="file in visibleFiles" :key="file.token" class="min-w-0 border-t border-[var(--border)] pt-2">
          <label class="flex min-w-0 items-start gap-2"><input v-if="canSelect && !file.is_dir" v-model="selected" class="mt-1" type="checkbox" :value="file.token" :disabled="busy" /><span class="min-w-0"><strong class="break-all">{{ file.name }}</strong><small class="text-subtle block break-all">{{ file.path }} · {{ formatBytes(file.size) }} · {{ file.file_kind === 'video' ? '视频' : file.file_kind === 'subtitle' ? '字幕文件' : file.file_kind === 'archive' ? '压缩包' : file.is_dir ? '目录' : '其他文件' }}</small><small v-if="file.episodes.length" class="block">{{ file.season !== undefined ? `第 ${file.season} 季 · ` : '季待确认 · ' }}第 {{ compactEpisodeSet(file.episodes) }} 集<template v-if="file.work_status !== 'matched'"> · 作品待确认</template></small><small v-else-if="file.file_kind === 'video'" class="text-subtle block">集数或作品身份待确认</small><small v-for="(evidence, index) in file.subtitles ?? []" :key="index" class="text-subtle block">{{ evidence.language === 'zh' ? evidence.script === 'Hans' ? '简体中文' : evidence.script === 'Hant' ? '繁体中文' : '中文（文字体系未确认）' : evidence.language }} · {{ evidence.source === 'sidecar_filename' ? '实际字幕文件的名称标注' : '发布名称声明，未核实内嵌字幕' }}</small></span></label>
        </li>
      </ul>
      <div class="mt-3 flex flex-wrap gap-2"><button v-if="current.files.length > 8" class="btn-secondary py-1 text-xs" type="button" @click="expanded = !expanded">{{ expanded ? '收起文件列表' : `展开已读取的 ${current.files.length} 项` }}</button><button v-if="current.next_offset !== undefined" class="btn-secondary py-1 text-xs" type="button" :disabled="busy" @click="more">{{ busy ? '读取中…' : '读取更多文件' }}</button><button v-if="canSelect" class="btn-secondary py-1 text-xs" type="button" :disabled="busy || !selected.length || !current.complete" @click="accept">{{ busy ? '正在确认选择…' : `选择文件入库（${selected.length} 个 · ${formatBytes(selectedBytes)}）` }}</button></div>
    </template>
    <p v-if="localError || error" class="text-warning mb-0 mt-2 text-xs" role="status">{{ localError || error }}</p>
    <p v-else-if="!current && !loading" class="text-subtle mb-0 mt-2 text-xs">尚未取得实际文件；标题范围不能作为包内集数。可独立重新读取。</p>
  </section>
</template>
