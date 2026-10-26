<script setup lang="ts">
import type { DashboardSection } from './activity'
import { progressPercent, statusLabels, type Job } from '@/jobs'
defineProps<{ section?: DashboardSection<Job>; empty: string; error?: string }>()
const typeLabels: Record<string, string> = {
  download: '下载', transfer: '整理入库', 'follow-search': '订阅搜索',
  unified_schedule: '计划任务', media_library_recognition: '识别媒体',
  media_library_structure_diagnosis: '检查媒体库', media_library_repair: '整理媒体库',
  media_library_retirement: '移除媒体库', media_reorganization: '整理媒体',
  media_artifact: '生成媒体文件', strm_reconcile: '同步 STRM',
  media_server_refresh: '刷新媒体服务器', seeding: '做种管理',
  subscription_calendar_refresh: '更新播出计划',
}
</script>
<template>
  <p v-if="!section && error" class="dashboard-empty semantic-error" role="alert">任务暂时无法读取，请稍后刷新。</p>
  <p v-else-if="!section" class="dashboard-empty">正在读取任务…</p>
  <p v-else-if="section.status === 'unavailable'" class="dashboard-empty semantic-error" role="alert">任务暂时无法读取，请稍后刷新。</p>
  <ul v-else-if="section.list.length" class="task-list">
    <li v-for="task in section.list" :key="task.id">
      <div class="task-row"><RouterLink :to="{ path: '/automation/tasks', query: { job_id: task.id } }" class="task-title">{{ task.display_name }}</RouterLink><span class="status-chip">{{ statusLabels[task.status] }}</span></div>
      <div class="task-meta"><span>{{ typeLabels[task.job_type] ?? '后台任务' }}</span><span v-if="task.progress != null">{{ progressPercent(task.progress) }}</span><span v-if="task.processed_items != null && task.total_items != null">{{ task.processed_items }} / {{ task.total_items }}</span></div>
      <p v-if="task.wait_reason?.message || task.last_error_message" class="task-message">{{ task.wait_reason?.message || task.last_error_message }}</p>
    </li>
  </ul>
  <p v-else class="dashboard-empty">{{ empty }}</p>
  <RouterLink v-if="section?.has_more" class="more-link" to="/automation/tasks">查看全部任务</RouterLink>
</template>
<style scoped>
.task-list { list-style: none; margin: 0; padding: 0; }
.task-list li { padding: .85rem 0; border-bottom: 1px solid var(--border); }
.task-list li:last-child { border-bottom: 0; }
.task-row { display: flex; align-items: flex-start; justify-content: space-between; gap: .7rem; }
.task-title { min-width: 0; color: var(--text); font-size: .88rem; font-weight: 650; overflow-wrap: anywhere; }
.task-meta { display: flex; flex-wrap: wrap; gap: .65rem; margin-top: .4rem; color: var(--text-muted); font-size: .75rem; }
.task-message { margin: .4rem 0 0; font-size: .75rem; color: var(--warning); overflow-wrap: anywhere; }
.dashboard-empty { padding: 1.4rem .2rem; color: var(--text-muted); font-size: .85rem; }
.more-link { display: block; margin-top: .6rem; font-size: .8rem; }
</style>
