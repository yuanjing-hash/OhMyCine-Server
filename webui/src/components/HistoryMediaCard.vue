<script setup lang="ts">
import { historyProgress, type UserHistoryItem } from '@/user-media-overview'

defineProps<{ item: UserHistoryItem; showUpdatedAt?: boolean; showRecords?: boolean }>()
const emit = defineEmits<{ open: [item: UserHistoryItem]; menu: [event: MouseEvent | KeyboardEvent, item: UserHistoryItem]; records: [item: UserHistoryItem] }>()

function menuKey(event: KeyboardEvent, item: UserHistoryItem) {
  if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
    event.preventDefault()
    emit('menu', event, item)
  }
}
function formatDate(value: number) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '时间未知' : new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date)
}
</script>

<template>
  <article class="history-card">
    <button type="button" class="discovery-poster history-card__poster" :aria-disabled="!item.playable" @click="emit('open', item)" @contextmenu.prevent="emit('menu', $event, item)" @keydown="menuKey($event, item)">
      <span class="discovery-poster__image"><img v-if="item.poster_url" :src="item.poster_url" :alt="`${item.title} 海报`" loading="lazy"><span v-else>暂无海报</span></span>
      <strong :title="item.title">{{ item.title }}</strong>
      <small :title="item.subtitle">{{ item.subtitle || (item.completed ? '已看完' : '继续观看') }}</small>
      <small>{{ item.source_name }}</small>
      <small v-if="showUpdatedAt">{{ formatDate(item.updated_at) }} · {{ item.completed ? '已看完' : '未看完' }}</small>
      <span class="history-progress" aria-hidden="true"><i :style="{ width: `${historyProgress(item)}%` }"></i></span>
    </button>
    <div class="history-card__actions">
      <button v-if="showRecords && (item.record_count ?? 0) > 1" type="button" class="history-card__action" @click="emit('records', item)">分集记录 · {{ item.record_count }}</button>
      <button v-if="item.history_id" type="button" class="history-card__action history-card__more" :aria-label="`${item.title}观看记录操作`" aria-haspopup="menu" @click="emit('menu', $event, item)">更多操作</button>
    </div>
  </article>
</template>

<style scoped>
.history-card { min-width: 0; }
.history-card__poster { display: block; width: 100%; }
.history-progress { display: block; height: .22rem; margin-top: .55rem; overflow: hidden; border-radius: 999px; background: var(--surface-muted); }
.history-progress i { display: block; height: 100%; border-radius: inherit; background: var(--accent); }
.history-card__actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: .25rem; margin-top: .4rem; }
.history-card__action { padding: .3rem .1rem; border: 0; border-radius: .2rem; background: transparent; color: var(--text-muted); font: inherit; font-size: .72rem; cursor: pointer; }
.history-card__more { margin-left: auto; }
.history-card__action:hover { color: var(--accent); }
.history-card__action:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
</style>
