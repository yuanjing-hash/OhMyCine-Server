<script setup lang="ts">
import { computed } from 'vue'
import type { ProviderSynchronization } from '@/types/api'

const props = defineProps<{ synchronization?: ProviderSynchronization; enabled: boolean }>()
const waiting = computed(() => {
  const until = props.synchronization?.cooldown_until
  return Boolean(until && Date.parse(until) > Date.now())
})
const failure = computed(() => props.synchronization?.error_code === 'observation_publication_failed' ? '变化已保存，媒体目录更新正在等待重试' : props.synchronization?.error_code ? '目录读取暂未完成，已确认的内容仍保留' : '')
function date(value: string | null | undefined) {
  if (!value) return '尚无记录'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? '尚无记录' : parsed.toLocaleString()
}
</script>

<template>
  <section class="semantic-inset mt-4 p-4" aria-label="夸克目录同步状态">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h3 class="m-0 text-base">目录变化巡检</h3>
      <span class="status-chip" :class="!enabled || waiting || synchronization?.delayed ? 'status-chip--warning' : ''">{{ !enabled ? '已停用' : waiting ? '限流等待中' : synchronization?.delayed ? '覆盖延期' : synchronization ? '按目录轮转' : '正在读取同步状态' }}</span>
    </div>
    <p class="text-subtle mt-2 text-sm">本系统确认完成的文件立即更新。夸克 App 中的新增、改名和移动由目录巡检发现；缺失文件经过独立复查后才下架。</p>
    <dl v-if="synchronization" class="m-0 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <div><dt class="text-subtle text-xs">上次完整核对</dt><dd class="m-0 mt-1 text-sm">{{ date(synchronization.last_complete_at) }}</dd></div>
      <div><dt class="text-subtle text-xs">待检查目录</dt><dd class="m-0 mt-1">{{ synchronization.pending_directories }}</dd></div>
      <div><dt class="text-subtle text-xs">待确认缺失</dt><dd class="m-0 mt-1">{{ synchronization.missing_candidates }}</dd></div>
      <div><dt class="text-subtle text-xs">待发布批次</dt><dd class="m-0 mt-1">{{ synchronization.pending_publications ?? 0 }}</dd></div>
      <div><dt class="text-subtle text-xs">待更新目录路径</dt><dd class="m-0 mt-1">{{ synchronization.path_repairs ?? 0 }}</dd></div>
      <div><dt class="text-subtle text-xs">待完成确认</dt><dd class="m-0 mt-1">{{ synchronization.pending_confirmations ?? 0 }}</dd></div>
      <div><dt class="text-subtle text-xs">最早待检查时间</dt><dd class="m-0 mt-1 text-sm">{{ date(synchronization.oldest_pending_at) }}</dd></div>
      <div><dt class="text-subtle text-xs">预计覆盖时间</dt><dd class="m-0 mt-1 text-sm">{{ synchronization.estimated_coverage_at ? `${synchronization.estimate_is_lower_bound ? '不早于 ' : '约 '}${date(synchronization.estimated_coverage_at)}` : '数据不足，暂不能估算' }}</dd></div>
      <div v-if="waiting"><dt class="text-subtle text-xs">限流后重试时间</dt><dd class="m-0 mt-1 text-sm">{{ date(synchronization.cooldown_until) }}</dd></div>
    </dl>
    <p v-if="failure" class="semantic-warning mt-4 p-3" role="status">{{ failure }}。下次重试：{{ date(synchronization?.next_retry_at) }}</p>
    <p v-if="synchronization?.delayed" class="semantic-warning mt-4 mb-0 p-3 text-sm">目录数量或限流已使本轮覆盖延期。尚未检查的目录不会标记为已同步，已确认的媒体仍可使用。</p>
  </section>
</template>
