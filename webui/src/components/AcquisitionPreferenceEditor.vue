<script setup lang="ts">
import type { AcquisitionPreference } from '@/follows'
defineProps<{ modelValue?: AcquisitionPreference; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [AcquisitionPreference] }>()
</script>

<template>
  <label class="block mt-3">
    <span class="field-label">资源获取顺序</span>
    <select class="field-input" :disabled="disabled" :value="modelValue ?? 'existing_order'" @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value as AcquisitionPreference)">
      <option value="transfer_first">优先网盘转存，其次其他渠道</option>
      <option value="existing_order">沿用覆盖范围与站点优先顺序</option>
    </select>
    <small class="text-muted mt-1 block">只在符合作品、缺集、质量及字幕条件的资源中排序。分享不可用时继续检查其他渠道；未确认的提交不会重复发送。</small>
  </label>
</template>
