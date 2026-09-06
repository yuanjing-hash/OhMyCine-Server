<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
import type { MediaLibraryStructurePreviewItemPage, MediaLibraryStructureSelectionPreview } from '@/types/api'
const props = defineProps<{ libraryId: number; preview: MediaLibraryStructureSelectionPreview }>()
const emit = defineEmits<{ ready: [value: boolean] }>()
const request = createLatestRequest()
const items = ref<MediaLibraryStructurePreviewItemPage | null>(null)
const loading = ref(false), error = ref('')
async function load(page: number) {
  const current = request.begin()
  loading.value = true; error.value = ''; emit('ready', false)
  try {
    const data = await api<MediaLibraryStructurePreviewItemPage>(`/api/v1/media-libraries/${props.libraryId}/structure/selection-preview/items`, { method: 'POST', body: JSON.stringify({ confirmation_token: props.preview.confirmation_token, page, page_size: 50 }), signal: current.signal })
    if (!current.isCurrent()) return
    items.value = data
    emit('ready', true)
  } catch (reason) { if (current.isCurrent()) error.value = reason instanceof Error ? reason.message : '请求失败' }
  finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
watch(() => props.preview, preview => {
  request.cancel(); loading.value = false; error.value = ''; items.value = preview.items ?? null
  emit('ready', Boolean(items.value))
  if (!items.value) void load(1)
}, { immediate: true })
onBeforeUnmount(() => request.cancel())
</script>
<template>
  <section class="semantic-inset mt-3 p-3" aria-label="文件变更预览">
    <strong>文件最终名称预览</strong>
    <p class="text-subtle text-xs">以下为本次冻结的操作清单，不是已执行结果。请核对每份文件；分页可查看全部最终名称。</p>
    <p v-if="error" class="semantic-error p-2" role="alert">文件清单读取失败：{{ error }} <button class="btn-secondary" type="button" @click="load(items?.page ?? 1)">重试读取清单</button></p>
    <div v-if="items" class="max-h-72 overflow-auto"><table class="semantic-table w-full text-left text-xs"><thead><tr><th>动作</th><th>当前相对路径</th><th>最终相对路径</th></tr></thead><tbody><tr v-for="(item, index) in items.list" :key="`${items.page}-${index}`"><td>{{ item.action === 'move' ? '整理' : item.action === 'keep' ? '原地保留' : '可恢复回收' }}</td><td class="break-all font-mono">{{ item.current_path }}</td><td class="break-all font-mono">{{ item.action === 'recycle' ? '可恢复回收站（不永久删除）' : item.expected_path }}</td></tr><tr v-if="items.total === 0"><td colspan="3">没有文件移动或回收操作。</td></tr></tbody></table></div>
    <p v-if="loading" class="text-subtle" role="status">正在读取文件清单…</p>
    <div v-if="items && items.total > items.page_size" class="mt-2 flex items-center justify-end gap-2"><button class="btn-secondary" type="button" :disabled="loading || items.page <= 1" @click="load(items.page - 1)">预览上一页</button><span>第 {{ items.page }} / {{ Math.ceil(items.total / items.page_size) }} 页 · {{ items.total }} 个动作</span><button class="btn-secondary" type="button" :disabled="loading || items.page * items.page_size >= items.total" @click="load(items.page + 1)">预览下一页</button></div>
  </section>
</template>
