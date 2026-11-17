<script setup lang="ts">
defineProps<{ busy: boolean; count?: number; sourceIndexes?: number[] }>()
defineEmits<{ continue: []; chooseOther: [] }>()
</script>

<template>
  <div class="semantic-warning mt-4 p-4" role="alert" aria-label="历史离线超时提示">
    <strong>{{ count && count > 1 ? `所选 ${count} 个资源在本次路线有离线超时记录` : '此资源在本次路线有离线超时记录' }}</strong>
    <p class="mb-0 mt-2 text-sm">此前云端离线在 30 分钟内未完成，已取消获取。可以继续尝试本次选择，或选择其他资源。</p>
    <p v-if="sourceIndexes?.length" class="mb-0 mt-2 text-sm">本次仅确认输入中的第 {{ sourceIndexes.join('、') }} 项。其他未入队项目继续保留。</p>
    <div class="mt-4 flex flex-wrap justify-end gap-3">
      <button class="btn-secondary" type="button" :disabled="busy" @click="$emit('chooseOther')">选择其他资源</button>
      <button class="btn-primary" type="button" :disabled="busy" @click="$emit('continue')">继续下载</button>
    </div>
    <p v-if="busy" class="mb-0 mt-2 text-sm" role="status">正在提交本次选择…</p>
  </div>
</template>
