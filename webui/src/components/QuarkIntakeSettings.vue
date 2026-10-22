<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import type { DownloaderSummary, ListResponse } from '@/types/api'

interface Settings { enabled: boolean; downloader_id: string; directory: string }
const props = defineProps<{ libraryId: number }>()
const auth = useAuthStore()
const canEdit = computed(() => auth.can(Permissions.MediaLibrariesUpdate) && auth.can(Permissions.DownloadsCreate) && auth.can(Permissions.DownloadersRead))
const enabled = ref(false), downloaderId = ref(''), directory = ref(''), loading = ref(false), saving = ref(false), error = ref(''), notice = ref('')
const downloaders = ref<DownloaderSummary[]>([])
let generation = 0
let controller: AbortController | null = null
function retire() { generation++; controller?.abort(); controller = null }
function assign(value: Settings) { enabled.value = value.enabled; downloaderId.value = value.downloader_id; directory.value = value.directory }
async function load() {
  retire(); const version = generation, id = props.libraryId
  controller = new AbortController(); const signal = controller.signal
  loading.value = true; saving.value = false; error.value = ''; notice.value = ''; assign({ enabled: false, downloader_id: '', directory: '' }); downloaders.value = []
  try {
    const [value, list] = await Promise.all([
      api<Settings>(`/api/v1/media-libraries/${id}/quark-intake`, { signal }),
      canEdit.value ? api<ListResponse<DownloaderSummary>>('/api/v1/downloaders', { signal }) : Promise.resolve({ list: [], total: 0 }),
    ])
    if (version !== generation) return
    assign(value); downloaders.value = list.list.filter(item => item.type === 'quark_share' && item.enabled)
  } catch (reason) { if (version === generation) error.value = reason instanceof Error ? reason.message : '无法读取自动接管设置' }
  finally { if (version === generation) loading.value = false }
}
async function save() {
  if (!canEdit.value || saving.value || loading.value || (enabled.value && !downloaderId.value)) return
  const version = generation, id = props.libraryId
  saving.value = true; error.value = ''; notice.value = ''
  try {
    const value = await api<Settings>(`/api/v1/media-libraries/${id}/quark-intake`, { method: 'PUT', body: JSON.stringify({ enabled: enabled.value, downloader_id: downloaderId.value }), signal: controller?.signal })
    if (version !== generation) return
    assign(value); notice.value = value.enabled ? '已启用接收目录自动接管' : '已停用自动接管'
  } catch (reason) { if (version === generation) error.value = reason instanceof Error ? reason.message : '保存失败，请重试' }
  finally { if (version === generation) saving.value = false }
}
watch(() => [props.libraryId, canEdit.value], () => void load(), { immediate: true })
onBeforeUnmount(retire)
</script>

<template>
  <section class="semantic-inset mt-4 p-4" aria-label="夸克接收目录自动接管">
    <h3 class="m-0 text-base">接收目录自动接管</h3>
    <p class="text-subtle mt-2 text-sm">手动放入最终媒体库的文件只会被识别和索引。开启此项后，Server 会把接收目录中已稳定的内容按本库规则整理入库；接收目录与最终库不能重叠。</p>
    <p v-if="loading" role="status">正在读取设置…</p>
    <template v-else>
      <label class="flex items-center gap-2"><input v-model="enabled" type="checkbox" :disabled="!canEdit || saving">自动接管并整理到此媒体库</label>
      <label v-if="canEdit" class="mt-3 block text-sm">接收目录对应的夸克下载器
        <select v-model="downloaderId" class="input mt-1 w-full" :disabled="!enabled || saving">
          <option value="">请选择同一夸克连接的分享接收器</option>
          <option v-for="item in downloaders" :key="item.id" :value="item.id">{{ item.name }} · {{ item.provider_directory_path }}</option>
        </select>
      </label>
      <p v-if="directory" class="text-subtle text-sm break-all">已绑定目录：{{ directory }}</p>
      <p class="text-subtle text-sm">两次完整检查且至少静默 60 秒后才接管，系统任务目录和验证文件会跳过。停用后不再接管新内容，已创建的整理任务仍按原计划执行。</p>
      <button v-if="canEdit" type="button" class="button button-primary" :disabled="saving || (enabled && !downloaderId)" @click="save">{{ saving ? '正在保存…' : '保存自动接管设置' }}</button>
    </template>
    <p v-if="error" class="semantic-error mt-3 p-3" role="alert">{{ error }} <button type="button" class="button" :disabled="saving" @click="load">重新读取</button></p>
    <p v-if="notice" role="status" class="mt-3">{{ notice }}</p>
  </section>
</template>
