<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '@/api/client'
import SubtitlePolicyEditor from '@/components/SubtitlePolicyEditor.vue'
import type { FollowPolicyDefaults } from '@/follows'
const props = defineProps<{ canUpdate: boolean }>()
const current = ref<FollowPolicyDefaults | null>(null)
const error = ref('')
const saving = ref(false)
async function load() { try { current.value = await api<FollowPolicyDefaults>('/api/v1/settings/follows'); error.value = '' } catch (reason) { error.value = reason instanceof Error ? reason.message : '默认订阅策略读取失败' } }
async function save() { if (!current.value || !props.canUpdate) return; saving.value = true; error.value = ''; try { current.value = await api<FollowPolicyDefaults>('/api/v1/settings/follows', { method: 'PATCH', body: JSON.stringify({ revision: current.value.revision, subtitles: current.value.subtitles }) }) } catch (reason) { error.value = reason instanceof Error ? reason.message : '保存失败，请刷新后重试' } finally { saving.value = false } }
onMounted(load)
</script>
<template>
  <form class="panel mt-6" @submit.prevent="save">
    <h2 class="m-0 text-lg">默认订阅策略</h2><p class="page-description mt-1 text-sm">只用于之后新建的订阅。已有订阅和正在执行的任务保留各自的策略。</p>
    <p v-if="error" class="semantic-error p-3" role="alert">{{ error }}</p>
    <template v-if="current"><SubtitlePolicyEditor v-model="current.subtitles" :disabled="!canUpdate || saving" /><button v-if="canUpdate" class="btn-primary mt-4" :disabled="saving">{{ saving ? '保存中…' : '保存默认订阅策略' }}</button></template>
    <button v-else class="btn-secondary" type="button" @click="load">重新读取</button>
  </form>
</template>
