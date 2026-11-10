<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'

const props = withDefaults(defineProps<{ src?: string; title: string; fallback?: string; loading?: 'lazy' | 'eager' }>(), { src: '', fallback: '暂无海报', loading: 'lazy' })
const failed = ref(false)
const attempt = ref(0)
const image = ref<HTMLImageElement>()
let retryTimer: number | undefined

function imageFailed(event: Event) {
  if (event.currentTarget !== image.value) return
  failed.value = true
  if (attempt.value !== 0) return
  retryTimer = window.setTimeout(() => {
    attempt.value++
    failed.value = false
  }, 1000)
}

watch(() => props.src, () => {
  window.clearTimeout(retryTimer)
  failed.value = false
  attempt.value = 0
})
onBeforeUnmount(() => window.clearTimeout(retryTimer))
</script>

<template>
  <img v-if="src && !failed" :key="`${src}:${attempt}`" ref="image" :src="src" :alt="`${title} 海报`" :loading="loading" referrerpolicy="no-referrer" @error="imageFailed">
  <span v-else>{{ failed ? '海报暂不可用' : fallback }}</span>
</template>
