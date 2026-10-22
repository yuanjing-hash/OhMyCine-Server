<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import DirectoryPickerDialog from '@/components/DirectoryPickerDialog.vue'
import { useAuthStore } from '@/stores/auth'
import { quarkAuthorizationLabel, quarkAuthorizationURL, quarkPollDelay, quarkQRDataURL } from '@/quark'
import type { QuarkAuthPurpose, QuarkAuthSession, QuarkConnectionStatus } from '@/quark'
import type { ConnectionSummary, StorageSummary } from '@/types/api'

const props = defineProps<{ connection: ConnectionSummary; storage?: StorageSummary | null }>()
const emit = defineEmits<{ changed: []; created: [storage: StorageSummary] }>()
const auth = useAuthStore()
const status = ref<QuarkConnectionStatus | null>(null)
const session = ref<QuarkAuthSession | null>(null)
const error = ref(''), notice = ref(''), busy = ref(false), pickerOpen = ref(false)
const name = ref(''), enabled = ref(true)
const directory = ref<{ path: string; token: string } | null>(null)
const canAuthorize = computed(() => auth.can(Permissions.ConnectionsUpdate))
const canSave = computed(() => props.storage ? auth.can(Permissions.StoragesUpdate) : canAuthorize.value)
const canSelect = computed(() => auth.can(Permissions.ConnectionsRead) && auth.can(Permissions.StoragesBrowse) && auth.can(props.storage ? Permissions.StoragesUpdate : Permissions.StoragesCreate))
const ready = computed(() => Boolean(status.value?.playback.ready && status.value?.management.ready))
const qr = computed(() => quarkQRDataURL(session.value?.qr_data_url))
const authorizationURL = computed(() => quarkAuthorizationURL(session.value?.authorization_url))
let generation = 0
let request: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | undefined
function stop() { generation++; request?.abort(); request = null; if (timer) clearTimeout(timer); timer = undefined }
async function boundedAPI<T>(path: string, options: RequestInit = {}, timeout = 50_000): Promise<T> {
  const controller = new AbortController(); request = controller
  const timeoutID = setTimeout(() => controller.abort(), timeout)
  try { return await api<T>(path, { ...options, signal: controller.signal }) }
  finally { clearTimeout(timeoutID); if (request === controller) request = null }
}
function message(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败，请重试' }
function endpoint(id = props.connection.id) { return `/api/v1/connections/${id}/quark` }
async function refreshStatus() {
  const version = generation, id = props.connection.id
  try { const value = await boundedAPI<QuarkConnectionStatus>(`${endpoint(id)}/status`); if (version === generation && id === props.connection.id) status.value = value }
  catch (reason) { if (version === generation) error.value = message(reason) }
}
function cancelSavedSession(id: number, key?: string) {
  if (key) void api(`${endpoint(id)}/auth/${key}/cancel`, { method: 'POST', body: '{}' }).catch(() => {})
}
watch(() => [props.connection.id, props.storage?.id], (_, previous) => {
  if (previous && session.value) cancelSavedSession(Number(previous[0]), session.value.session_id)
  stop(); session.value = null; status.value = null; error.value = ''; notice.value = ''; directory.value = null; busy.value = false
  name.value = props.storage?.name ?? props.connection.name; enabled.value = props.storage?.enabled ?? props.connection.enabled
  void refreshStatus()
}, { immediate: true })
async function start(purpose: QuarkAuthPurpose) {
  if (!canAuthorize.value || busy.value || session.value) return
  stop(); const version = generation, id = props.connection.id; busy.value = true; error.value = ''; notice.value = ''
  try {
    const value = await boundedAPI<QuarkAuthSession>(`${endpoint(id)}/auth`, { method: 'POST', body: JSON.stringify({ purpose }) })
    if (version !== generation) { cancelSavedSession(id, value.session_id); return }
    session.value = value; schedule(version, id)
  } catch (reason) { if (version === generation) error.value = message(reason) }
  finally { if (version === generation) busy.value = false }
}
function schedule(version: number, id: number) {
  if (!session.value || version !== generation) return
  if (Date.now() >= Date.parse(session.value.expires_at)) { notice.value = '本次授权等待已结束，请重新开始'; cancelAuthorization(); return }
  timer = setTimeout(() => void poll(version, id), quarkPollDelay(session.value.poll_after_seconds))
}
async function poll(version: number, id: number) {
  const current = session.value
  if (!current || version !== generation) return
  try {
    const value = await boundedAPI<QuarkAuthSession>(`${endpoint(id)}/auth/${current.session_id}/poll`, { method: 'POST', body: '{}' })
    if (version !== generation) return
    session.value = value
    if (value.state === 'complete') {
      notice.value = value.message; session.value = null; await refreshStatus(); if (version === generation) emit('changed'); return
    }
    if (value.state === 'expired' || value.state === 'cancelled') { notice.value = value.message; session.value = null; return }
    // A failed provider call is explicitly retryable; never spin forever or
    // exchange another code until the administrator chooses to continue.
    if (value.state === 'retryable') { error.value = value.message; await refreshStatus(); return }
    schedule(version, id)
  } catch (reason) { if (version === generation) error.value = message(reason) }
}
function retryAuthorization() { error.value = ''; void poll(generation, props.connection.id) }
function cancelAuthorization() {
  const id = props.connection.id, key = session.value?.session_id
  stop(); session.value = null; busy.value = false; cancelSavedSession(id, key)
}
async function save() {
  if (!canSave.value || !name.value.trim() || busy.value || session.value) return
  const version = generation, id = props.connection.id, storage = props.storage
  busy.value = true; error.value = ''; notice.value = ''
  try {
    if (storage) {
      await boundedAPI(`/api/v1/storages/${storage.id}`, { method: 'PATCH', body: JSON.stringify({ name: name.value, enabled: enabled.value, connection_id: id, provider_picker_token: directory.value?.token }) }, 120_000)
    } else {
      await boundedAPI(`/api/v1/connections/${id}`, { method: 'PATCH', body: JSON.stringify({ name: name.value, enabled: enabled.value, revision: props.connection.revision }) })
    }
    if (version !== generation) return
    directory.value = null; notice.value = '配置已保存'; emit('changed')
  } catch (reason) { if (version === generation) error.value = message(reason) }
  finally { if (version === generation) busy.value = false }
}
async function createStorage() {
  if (!directory.value || !ready.value || !canSelect.value || busy.value || session.value) return
  const version = generation, id = props.connection.id, selection = directory.value
  busy.value = true; error.value = ''; notice.value = '正在验证所选目录的播放与管理访问能力…'
  try {
    await boundedAPI(`${endpoint(id)}/verify-root`, { method: 'POST', body: JSON.stringify({ provider_picker_token: selection.token }) }, 120_000)
    if (version !== generation) return
    const value = await boundedAPI<StorageSummary>('/api/v1/storages', { method: 'POST', body: JSON.stringify({ name: name.value, type: 'quark', connection_id: id, provider_picker_token: selection.token, enabled: enabled.value }) }, 120_000)
    if (version !== generation) return
    directory.value = null; notice.value = '夸克数据源已添加'; emit('created', value)
  } catch (reason) { if (version === generation) { error.value = message(reason); notice.value = '' } }
  finally { if (version === generation) busy.value = false }
}
onBeforeUnmount(() => { const key = session.value?.session_id; stop(); cancelSavedSession(props.connection.id, key) })
</script>

<template>
  <section class="panel">
    <h2 class="m-0">{{ storage?.name ?? connection.name }}</h2>
    <p class="text-subtle mt-2 text-sm">夸克网盘 · 原画播放与文件管理使用独立授权，日常续期由 Server 自动处理。</p>
    <div class="mt-4 grid gap-3 sm:grid-cols-2">
      <div v-for="purpose in (['playback', 'management'] as const)" :key="purpose" class="semantic-inset p-4">
        <strong>{{ purpose === 'playback' ? '原画播放' : '文件管理与分享转存' }}</strong>
        <p class="text-muted my-2 text-sm">{{ quarkAuthorizationLabel(status?.[purpose]) }}</p>
        <button v-if="canAuthorize" type="button" class="btn-secondary" :disabled="busy || Boolean(session) || !connection.enabled" @click="start(purpose)">{{ status?.[purpose].ready ? '重新登录' : '登录授权' }}</button>
      </div>
    </div>
    <section v-if="session" class="semantic-inset mt-4 p-4" aria-live="polite">
      <h3 class="mt-0">{{ session.purpose === 'playback' ? '原画播放授权' : '文件管理授权' }}</h3>
      <img v-if="qr" :src="qr" alt="夸克授权二维码" class="mx-auto block w-full max-w-64" />
      <p class="text-muted text-sm">{{ session.message }}</p>
      <a v-if="authorizationURL" :href="authorizationURL" target="_blank" rel="noreferrer noopener" class="btn-secondary">打开夸克官方授权页面</a>
      <div class="mt-3 flex flex-wrap gap-2"><button v-if="error" type="button" class="btn-primary" @click="retryAuthorization">重新检查授权</button><button type="button" class="btn-secondary" @click="cancelAuthorization">取消本次授权</button></div>
    </section>
    <div class="mt-5 grid gap-3">
      <label class="label">{{ storage ? '数据源名称' : '账号名称' }}<input v-model="name" class="input mt-2" maxlength="128" :disabled="!canSave || busy" /></label>
      <label class="text-muted flex items-center gap-2 text-sm"><input v-model="enabled" type="checkbox" :disabled="!canSave || busy" />启用{{ storage ? '数据源' : '账号' }}</label>
      <label class="label">云端根目录<input class="input mt-2" :value="directory?.path ?? storage?.root_display_path ?? '尚未选择'" readonly /></label>
      <p v-if="!storage" class="text-subtle m-0 text-sm">完成两项核心授权后选择目录。Server 会在所选目录写入一个极小验证文件，并从播放授权独立读回，验证这两项授权能共同访问此范围。</p>
      <div class="flex flex-wrap gap-2">
        <button v-if="canSelect" type="button" class="btn-secondary" :disabled="busy || Boolean(session) || !ready" @click="pickerOpen = true">{{ storage ? '更换目录' : '选择云端目录' }}</button>
        <button v-if="canSave" type="button" class="btn-secondary" :disabled="busy || Boolean(session) || !name.trim()" @click="save">保存{{ storage ? '配置' : '账号' }}</button>
        <button v-if="!storage && canSelect" type="button" class="btn-primary" :disabled="busy || Boolean(session) || !ready || !directory || !name.trim()" @click="createStorage">验证目录并添加数据源</button>
      </div>
    </div>
    <p v-if="error" role="alert" class="semantic-danger-text mt-4 text-sm">{{ error }}</p>
    <p v-if="notice" role="status" class="text-muted mt-4 text-sm">{{ notice }}</p>
    <DirectoryPickerDialog :open="pickerOpen" :provider-connection-id="connection.id" @close="pickerOpen = false" @select="directory = $event" />
  </section>
</template>
