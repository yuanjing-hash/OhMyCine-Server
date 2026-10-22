<script setup lang="ts">
import { shareContentGroupLabel, type ShareContentAnalysis } from '@/share-content'
defineProps<{ analysis?: ShareContentAnalysis; loading?: boolean; error?: string; received?: boolean }>()
</script>

<template>
  <section class="semantic-inset mt-3 p-3 text-sm" aria-label="分享文件分析" role="status">
    <template v-if="analysis">
      <strong>{{ analysis.complete ? '已读取分享内容' : '分享内容读取未完成' }}</strong>
      <span v-if="analysis.complete" class="text-subtle ml-2 text-xs">{{ analysis.video_count }} 个视频<template v-if="analysis.ignored_count"> · 忽略 {{ analysis.ignored_count }} 个非视频文件</template></span>
      <p v-for="(group, index) in analysis.groups" :key="`${group.tmdb_id}:${group.season}:${index}`" class="mb-0 mt-2">{{ shareContentGroupLabel(group) }}<span v-if="group.status !== 'matched'" class="text-subtle ml-2 text-xs">作品待确认</span></p>
      <p v-if="analysis.unresolved_count" class="text-subtle mb-0 mt-2 text-xs">另有 {{ analysis.unresolved_count }} 个视频的作品或集数待确认</p>
      <p v-if="analysis.message" class="text-subtle mb-0 mt-2 text-xs">{{ analysis.message }}</p>
    </template>
    <p v-else class="m-0" :class="error ? 'text-warning' : 'text-subtle'">{{ error || (loading ? '正在读取分享文件…' : received ? '当前 Server 未返回分享文件分析；标题仅供参考，请升级 Server 后重新检测。' : '等待读取分享文件…') }}</p>
  </section>
</template>
