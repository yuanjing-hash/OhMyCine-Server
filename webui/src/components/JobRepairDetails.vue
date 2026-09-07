<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { getRepairDetails, type RepairDetails } from '@/jobs'
import { createLatestRequest } from '@/latest-request'
import { APIError } from '@/api/client'
const props = defineProps<{ jobId: string; revision: number; active?: boolean }>()
const page = ref(1), status = ref(''), details = ref<RepairDetails | null>(null), loading = ref(false), error = ref('')
const request = createLatestRequest()
let accessDenied = false
const states: Record<string, string> = { pending: '待执行', running: '处理中', succeeded: '已完成', failed: '失败', blocked: '等待前置操作' }
const phases: Record<string, string> = { queued: '等待执行', executing: '正在整理', reconciling: '正在核对整理结果', completed: '整理完成', partial_failed: '部分项目待重试', failed: '整理待恢复' }
const actions: Record<string, string> = { move: '移动 / 重命名', recycle: '移入回收站' }
async function load() {
  const current = request.begin(); loading.value = true; error.value = ''
  try { const result = await getRepairDetails(props.jobId, page.value, status.value, current.signal); if (current.isCurrent()) { accessDenied = false; details.value = result } }
  catch (reason) { if (current.isCurrent()) { accessDenied = reason instanceof APIError && [401, 403, 404].includes(reason.status); if (accessDenied) details.value = null; error.value = reason instanceof Error ? reason.message : '明细加载失败' } }
  finally { if (current.isCurrent()) loading.value = false; current.finish() }
}
watch(() => props.jobId, () => { accessDenied = false; page.value = 1; status.value = ''; details.value = null; void load() }, { immediate: true })
watch(() => props.revision, () => { if (!accessDenied && !document.hidden) void load() })
function refreshVisible() { if (props.active && !accessDenied && !request.pending && !document.hidden) void load() }
const timer = setInterval(refreshVisible, 5000)
document.addEventListener('visibilitychange', refreshVisible)
function filter() { page.value = 1; void load() }
function turn(delta: number) { page.value += delta; void load() }
onBeforeUnmount(() => { clearInterval(timer); document.removeEventListener('visibilitychange', refreshVisible); request.cancel() })
</script>
<template>
  <section class="p-4" aria-label="整理执行明细">
    <h3>整理执行明细</h3>
    <p v-if="error" role="alert">{{ error }} <button class="btn-secondary" @click="load">重试</button></p>
    <template v-if="details">
      <p>{{ phases[details.phase] ?? '等待状态更新' }}</p>
      <p v-if="details.current_batch_size">正在{{ actions[details.current_action] ?? '处理' }}本批 {{ details.current_batch_size }} 项 · {{ details.current_item }}（批次返回后核验结果）</p>
      <p>已完成 {{ details.counts.succeeded ?? 0 }} · 失败 {{ details.counts.failed ?? 0 }} · 等待前置操作 {{ details.counts.blocked ?? 0 }} · 待执行 {{ details.counts.pending ?? 0 }}</p>
    </template>
    <label>执行结果 <select v-model="status" class="input" @change="filter"><option value="">全部</option><option v-for="(label, key) in states" :key="key" :value="key">{{ label }}</option></select></label>
    <p v-if="loading" role="status">正在读取执行明细…</p>
    <ol v-if="details" class="task-timeline"><li v-for="item in details.list" :key="item.ordinal"><strong>{{ actions[item.action] ?? '文件操作' }} · {{ states[item.status] ?? '等待确认' }}</strong><span class="break-all">{{ item.source_path }}<template v-if="item.target_path"> → {{ item.target_path }}</template></span><span v-if="item.error_message">{{ item.error_message }}</span></li></ol>
    <div class="flex items-center gap-3"><button class="btn-secondary" :disabled="loading || page <= 1" @click="turn(-1)">上一页</button><span>第 {{ page }} 页 · 共 {{ details?.total ?? 0 }} 项</span><button class="btn-secondary" :disabled="loading || !details || page * 50 >= details.total" @click="turn(1)">下一页</button></div>
  </section>
</template>
