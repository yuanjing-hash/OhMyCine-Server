<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { api, APIError } from '@/api/client'
import { createLatestRequest } from '@/latest-request'
import { useJobLiveRefresh } from '@/use-job-live-refresh'
import { useAuthStore } from '@/stores/auth'
import { Permissions } from '@/auth/generated-permissions'
interface Notice { id: string; title: string; status: string; occurrence: number; read: boolean; resolved: boolean; link: string }
interface Page { list: Notice[]; total: number; page: number; has_more: boolean }
const emit = defineEmits<{ navigate: [path: string] }>()
const data = ref<Page>({ list: [], total: 0, page: 1, has_more: false })
const error = ref(''), loading = ref(false), busy = ref(false)
const reads = createLatestRequest()
const auth = useAuthStore()
let alive = true
async function load(page = data.value.page) {
  const request = reads.begin(); loading.value = true
  try {
    const result = await api<Page>(`/api/v1/notifications?page=${page}&page_size=24`, { signal: request.signal })
    if (request.isCurrent()) { data.value = result; error.value = '' }
  } catch (reason) {
    if (request.isCurrent()) {
      error.value = reason instanceof Error ? reason.message : '通知读取失败'
      if (reason instanceof APIError && [401, 403].includes(reason.status)) data.value = { list: [], total: 0, page: 1, has_more: false }
    }
  } finally { if (request.isCurrent()) loading.value = false; request.finish() }
}
async function acknowledge(item: Notice) {
  if (busy.value) return
  busy.value = true
  try { await api(`/api/v1/notifications/${encodeURIComponent(item.id)}/read`, { method: 'POST', body: JSON.stringify({ occurrence: item.occurrence }) }); if (alive) await load() }
  catch (reason) { if (alive) error.value = reason instanceof Error ? reason.message : '已读保存失败' }
  finally { if (alive) busy.value = false }
}
onMounted(() => { void load() })
useJobLiveRefresh(() => load(), () => auth.can(Permissions.JobsReadAll) || auth.can(Permissions.JobsReadOwn))
onUnmounted(() => { alive = false; reads.cancel() })
</script>
<template>
  <section class="space-y-3" :aria-busy="loading">
    <p class="text-sm text-muted">显示当前可访问的失败、待处理任务及已读任务的恢复状态。已读不会解决任务。</p>
    <p v-if="error" role="alert" class="semantic-error p-3">{{ error }} <button class="btn-secondary" @click="load()">重试</button></p>
    <article v-for="item in data.list" :key="item.id" class="panel space-y-2 p-3"><strong>{{ item.title }}</strong><p class="text-sm">{{ item.resolved ? '已恢复 / 不再待处理' : item.status === 'failed' ? '任务失败' : '需要人工处理' }} · {{ item.read ? '已读' : '未读' }}</p><div class="flex gap-2"><button class="btn-primary" @click="emit('navigate', item.link)">查看任务</button><button v-if="!item.read" class="btn-secondary" :disabled="busy" @click="acknowledge(item)">标为已读</button></div></article>
    <p v-if="!loading && !error && !data.list.length" class="text-muted">暂无需要处理的通知。</p>
    <footer class="flex items-center justify-between gap-2"><button class="btn-secondary" :disabled="loading || data.page <= 1" @click="load(data.page - 1)">上一页</button><span class="text-sm">第 {{ data.page }} 页 · 共 {{ data.total }} 项</span><button class="btn-secondary" :disabled="loading || !data.has_more" @click="load(data.page + 1)">下一页</button></footer>
  </section>
</template>
