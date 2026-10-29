<script setup lang="ts">
import { computed } from 'vue'
import type { SubtitlePolicy } from '@/follows'
const props = defineProps<{ modelValue?: SubtitlePolicy; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: SubtitlePolicy] }>()
const policy = computed<SubtitlePolicy>(() => props.modelValue ?? { mode: 'ignore', languages: [] })
const languages = [{ value: 'zh-Hans', label: '简体中文' }, { value: 'zh-Hant', label: '繁体中文' }, { value: 'en', label: '英语' }, { value: 'ja', label: '日语' }, { value: 'ko', label: '韩语' }]
function changeMode(event: Event) { const mode = (event.target as HTMLSelectElement).value as SubtitlePolicy['mode']; emit('update:modelValue', { mode, languages: policy.value.languages.length ? [...policy.value.languages] : mode === 'ignore' ? [] : ['zh-Hans', 'zh-Hant'] }) }
function toggleLanguage(value: string) { const current = policy.value.languages; emit('update:modelValue', { mode: policy.value.mode, languages: current.includes(value) ? current.filter(item => item !== value) : [...current, value] }) }
</script>
<template>
  <fieldset class="semantic-inset p-3" :disabled="disabled">
    <legend class="px-1 text-sm font-650">字幕偏好</legend>
    <label class="field-label">字幕策略<select class="field-input mt-1" :value="policy.mode" @change="changeMode"><option value="ignore">不限字幕</option><option value="prefer">优先对应字幕，缺字幕仍下载</option><option value="require">必须确认有对应字幕</option></select></label>
    <div v-if="policy.mode !== 'ignore'" class="mt-3 flex flex-wrap gap-3"><label v-for="language in languages" :key="language.value" class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="policy.languages.includes(language.value)" @change="toggleLanguage(language.value)">{{ language.label }}</label></div>
    <p class="mb-0 mt-2 text-xs text-muted">按勾选顺序优先；已选：{{ policy.languages.map(value => languages.find(item => item.value === value)?.label || value).join(' → ') || '不限' }}。内嵌字幕未探测时保留未知；发布名称声明不作为“必须有字幕”的证明。</p>
  </fieldset>
</template>
