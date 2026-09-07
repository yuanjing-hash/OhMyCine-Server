<script setup lang="ts">
import type { MediaLibraryDetail } from '@/types/api'
defineProps<{ library: Pick<MediaLibraryDetail, 'ready' | 'readiness_status'> }>()
const labels: Record<string, string> = { ready: '已准备好', disabled: '未准备好 · 已停用', unavailable: '未准备好 · 数据源不可用', credentials_required: '未准备好 · 等待更新登录凭据', repairing: '未准备好 · 整理修复中', repair_failed: '未准备好 · 整理待恢复', busy: '未准备好 · 文件操作待确认', initializing: '未准备好 · 初始化中', checking: '未准备好 · 首次检查中' }
</script>
<template>
  <span class="status-chip" :class="library.ready === true ? 'status-chip--ready' : 'status-chip--warning'" role="status">{{ labels[library.readiness_status ?? ''] ?? '准备状态待确认' }}</span>
</template>
