<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
const props = defineProps<{ libraryId: number }>()
type Page = { list: { name: string; reason: string; created_at: string }[]; total: number; page: number; page_size: number }
const data = ref<Page>()
const busy = ref(false)
const error = ref('')
const request = createLatestRequest()
async function load(page = 1) {
  const current = request.begin()
  const timer = setTimeout(() => {
    if (current.isCurrent()) { error.value = '待检查事件读取超时，请重试。'; busy.value = false; request.cancel() }
  }, 15000)
  busy.value = true; error.value = ''
  try {
    const result = await api<Page>(`/api/v1/media-libraries/${props.libraryId}/provider-events?page=${page}`, { signal: current.signal })
    if (current.isCurrent()) data.value = result
  } catch {
    if (current.isCurrent()) error.value = '待检查事件读取失败，请重试。'
  } finally {
    clearTimeout(timer)
    if (current.isCurrent()) busy.value = false
    current.finish()
  }
}
watch(() => props.libraryId, () => { request.cancel(); data.value = undefined; void load() }, { immediate: true })
onBeforeUnmount(() => request.cancel())
</script>

<template>
  <section class="panel mt-4" aria-label="待检查事件">
    <div class="flex items-center justify-between gap-3"><h3 class="m-0 text-base">待检查事件<span v-if="data">（{{ data.total }}）</span></h3><button type="button" class="btn-secondary" :disabled="busy" @click="load(data?.page)">刷新</button></div>
    <p class="text-subtle text-sm">这里只显示已暂停自动处理的通知，不代表系统已完成对应的库内记录或 STRM 清理。请先核对媒体库来源；需要补充目录信息时，请使用上方的“完整核对”，如有残留 STRM，再到 STRM 管理执行“完整核对”。系统不会猜测删除或反复重试这些通知。</p>
    <p v-if="busy" role="status">正在读取…</p><p v-if="error" class="semantic-error p-3" role="alert">{{ error }}</p>
    <table v-if="data?.list.length" class="semantic-table w-full text-left text-sm"><thead><tr><th>文件或目录</th><th>暂停原因</th><th>收到时间</th></tr></thead><tbody><tr v-for="(item, index) in data.list" :key="index"><td class="break-all">{{ item.name }}</td><td>{{ item.reason }}</td><td>{{ new Date(item.created_at).toLocaleString() }}</td></tr></tbody></table>
    <p v-else-if="data && !busy" class="text-subtle">没有待检查事件。</p>
    <div v-if="data && data.total > data.page_size" class="mt-3 flex items-center gap-3"><button type="button" class="btn-secondary" :disabled="busy || data.page <= 1" @click="load(data.page - 1)">上一页</button><span>第 {{ data.page }} 页，共 {{ Math.ceil(data.total / data.page_size) }} 页</span><button type="button" class="btn-secondary" :disabled="busy || data.page * data.page_size >= data.total" @click="load(data.page + 1)">下一页</button></div>
  </section>
</template>
