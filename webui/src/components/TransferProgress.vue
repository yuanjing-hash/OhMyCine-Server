<script setup lang="ts">
import { computed } from 'vue'
import { formatBytes } from '@/downloads'
import { formatTransferProgress, transferTargetCheckSummary, transferUploadMetrics, type TransferSummary } from '@/transfers'

const props = defineProps<{ item: TransferSummary; now: number }>()
const metrics = computed(() => transferUploadMetrics(props.item, props.now))
const targetCheck = computed(() => transferTargetCheckSummary(props.item))
</script>

<template>
  <div class="transfer-progress">
    <span>文件 {{ formatTransferProgress(item) }}</span>
    <template v-if="item.execution_location === 'node'">
      <template v-if="targetCheck">
        <span class="text-subtle text-xs">{{ targetCheck.reason }}</span>
        <small v-if="targetCheck.complete === false" class="text-subtle">目标核对尚未覆盖完整范围</small>
        <small v-if="targetCheck.counts" class="text-subtle">{{ targetCheck.counts }}</small>
        <time v-if="targetCheck.retryAt" class="text-subtle text-xs" :datetime="targetCheck.retryAt">下次检查 {{ new Date(targetCheck.retryAt).toLocaleString('zh-CN', { hour12: false }) }}</time>
      </template>
      <span class="text-subtle text-xs">上传数据 {{ formatBytes(metrics.transferred) }} / {{ formatBytes(metrics.total) }}<template v-if="metrics.percentage !== null"> · {{ metrics.percentage.toFixed(1) }}%</template></span>
      <progress v-if="metrics.percentage !== null" :value="metrics.percentage" max="100" aria-label="上传数据进度"></progress>
      <span class="text-subtle text-xs">上传速度 {{ formatBytes(metrics.speed, '/s') }}</span>
      <small v-if="metrics.sampleState === 'stale'" class="text-subtle">速度采样已过期</small>
      <small v-else-if="metrics.sampleState === 'unknown'" class="text-subtle">尚无有效上传速度采样</small>
      <time v-if="metrics.sampledAt" class="text-subtle text-xs" :datetime="metrics.sampledAt">上次采样 {{ new Date(metrics.sampledAt).toLocaleTimeString('zh-CN', { hour12: false }) }}</time>
    </template>
  </div>
</template>

<style scoped>
.transfer-progress { display: grid; gap: .3rem; min-width: 0; overflow-wrap: anywhere; }
.transfer-progress progress { width: 100%; max-width: 16rem; height: .45rem; accent-color: var(--accent); }
</style>
