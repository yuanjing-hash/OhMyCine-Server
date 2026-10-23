<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

const props = defineProps<{ open: boolean; path: string }>()
const emit = defineEmits<{ close: []; confirm: [] }>()
const dialog = ref<HTMLElement | null>(null)
const cancelButton = ref<HTMLButtonElement | null>(null)
let previousFocus: HTMLElement | null = null

watch(() => props.open, async open => {
  if (!open) { await nextTick(); previousFocus?.focus(); previousFocus = null; return }
  previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
  await nextTick(); cancelButton.value?.focus()
}, { immediate: true })
onBeforeUnmount(() => previousFocus?.focus())

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') { event.preventDefault(); emit('close'); return }
  if (event.key !== 'Tab' || !dialog.value) return
  const buttons = [...dialog.value.querySelectorAll<HTMLButtonElement>('button:not([disabled])')]
  const first = buttons[0], last = buttons[buttons.length - 1]
  if (!first || !last) return
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="modal-backdrop fixed inset-0 z-100 flex items-center justify-center p-4" @mousedown.self="emit('close')">
      <section ref="dialog" role="dialog" aria-modal="true" aria-labelledby="quark-root-recovery-title" aria-describedby="quark-root-recovery-description" class="panel max-h-[90vh] w-full max-w-xl overflow-y-auto" @keydown="onKeydown">
        <h2 id="quark-root-recovery-title" class="m-0 text-xl">开始一次新的目录验证？</h2>
        <div id="quark-root-recovery-description" class="mt-4 space-y-3 text-sm">
          <p class="text-muted m-0">目录：<strong class="break-all">{{ path }}</strong></p>
          <p class="semantic-warning m-0 p-3">上次验证结果尚未确认。Server 会保留原记录，在同一目录重新验证一次。</p>
          <p class="text-muted m-0">本次会上传另一个极小验证文件，由播放授权独立读回，验证结果保存后清理已确认的文件。无需重新登录。</p>
          <p class="text-subtle m-0">旧验证文件若仍无法确认，会继续保留待核对记录。</p>
        </div>
        <div class="mt-5 flex flex-wrap justify-end gap-2">
          <button ref="cancelButton" type="button" class="btn-secondary" @click="emit('close')">取消</button>
          <button type="button" class="btn-primary" @click="emit('confirm')">确认并开始新验证</button>
        </div>
      </section>
    </div>
  </Teleport>
</template>
