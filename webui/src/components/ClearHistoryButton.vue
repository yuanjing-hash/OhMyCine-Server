<script setup lang="ts">
import { ref } from 'vue'
import { api } from '@/api/client'

const props = defineProps<{ scope: string; resourceId?: string; label?: string; disabled?: boolean }>()
const emit = defineEmits<{ cleared: [] }>()
const busy = ref(false)

interface PurgeResult { eligible: number; deleted: number; skipped: number }

async function clearHistory() {
  if (busy.value || props.disabled) return
  busy.value = true
  try {
    const body = JSON.stringify({ scope: props.scope, resource_id: props.resourceId || undefined })
    const preview = await api<PurgeResult>('/api/v1/management-history/purge/preview', { method: 'POST', body })
    if (preview.deleted === 0) {
      window.alert(preview.skipped ? `没有可安全清除的记录；${preview.skipped} 条仍在执行、等待处理或自动核验文件结果，本次已保留。结束后可再次清除。` : '当前没有可清除的历史记录。')
      return
    }
    const suffix = preview.skipped ? `；另有 ${preview.skipped} 条仍在执行、等待处理或自动核验文件结果，本次保留` : ''
    if (!window.confirm(`确认清除 ${preview.deleted} 条已结束的历史记录${suffix}？不会删除媒体文件、STRM 文件、播放历史、审计或运行日志。`)) return
    const result = await api<PurgeResult>('/api/v1/management-history/purge', { method: 'POST', body })
    window.alert(`已清除 ${result.deleted} 条历史记录${result.skipped ? `；${result.skipped} 条仍在执行、等待处理或自动核验文件结果，本次已保留，结束后可再次清除` : ''}。`)
    emit('cleared')
  } catch (reason) {
    window.alert(reason instanceof Error ? reason.message : '清除历史记录失败')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <button class="btn-danger" type="button" :disabled="busy || disabled" @click="clearHistory">
    {{ busy ? '正在检查…' : (label || '清除全部历史') }}
  </button>
</template>
