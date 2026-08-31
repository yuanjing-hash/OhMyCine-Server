<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Permissions } from '@/auth/generated-permissions'
import {
  canInstallServerUpdate,
  checkServerUpdate,
  createUpdateRequestGuard,
  installServerUpdate,
  isUpdateBusy,
  loadServerUpdate,
  saveServerUpdateChannel,
  updateErrorLabel,
  updateManagedReasonLabel,
  updatePhaseLabel,
  waitForServerUpdateReconnect,
} from '@/server-update'
import { useAuthStore } from '@/stores/auth'
import { notify } from '@/toast'
import type { ServerUpdateChannel, ServerUpdateStatus } from '@/types/api'

const auth = useAuthStore()
const canAdmin = computed(() => auth.can(Permissions.SystemAdmin))
const status = ref<ServerUpdateStatus | null>(null)
const channel = ref<ServerUpdateChannel>('beta')
const loading = ref(true)
const action = ref<'check' | 'channel' | 'install' | ''>('')
const reconnecting = ref(false)
const loadError = ref('')
const reconnectError = ref('')
const requestGuard = createUpdateRequestGuard()
let disposed = false

const busy = computed(() => Boolean(action.value) || reconnecting.value || isUpdateBusy(status.value))
const channelDirty = computed(() => Boolean(status.value && channel.value !== status.value.channel))
const installAllowed = computed(() => canInstallServerUpdate(status.value) && !busy.value)
const phaseClass = computed(() => {
  if (!status.value) return 'status-chip'
  if (status.value.phase === 'failed' || status.value.phase === 'rolled_back') return 'status-chip status-chip--error'
  if (status.value.phase === 'succeeded') return 'status-chip status-chip--ready'
  if (isUpdateBusy(status.value) || status.value.phase === 'available') return 'status-chip status-chip--warning'
  return 'status-chip'
})

function applyStatus(next: ServerUpdateStatus) {
  status.value = next
  channel.value = next.channel
  loadError.value = ''
}

async function refresh(showError = true) {
  const generation = requestGuard.next()
  try {
    const next = await loadServerUpdate()
    if (!disposed && requestGuard.isCurrent(generation)) applyStatus(next)
    return next
  } catch (reason) {
    if (!disposed && requestGuard.isCurrent(generation) && showError) loadError.value = message(reason)
    throw reason
  }
}

async function runAction(kind: 'check' | 'channel', operation: () => Promise<ServerUpdateStatus>, success: string) {
  if (action.value || reconnecting.value) return
  action.value = kind
  loadError.value = ''
  const generation = requestGuard.next()
  try {
    const next = await operation()
    if (!disposed && requestGuard.isCurrent(generation)) {
      applyStatus(next)
      notify(success, 'success')
    }
  } catch (reason) {
    if (!disposed && requestGuard.isCurrent(generation)) {
      loadError.value = message(reason)
      notify(message(reason), 'error')
    }
  } finally {
    if (!disposed && requestGuard.isCurrent(generation)) action.value = ''
  }
}

function checkNow() {
  return runAction('check', () => checkServerUpdate(), 'Server 版本检查完成')
}

function saveChannel() {
  if (!status.value || !channelDirty.value) return
  const nextChannel = channel.value
  return runAction('channel', () => saveServerUpdateChannel(nextChannel, status.value!.revision), `更新通道已切换为 ${nextChannel === 'beta' ? 'Beta' : 'Stable'}`)
}

async function install() {
  const current = status.value
  if (!current || !installAllowed.value || !current.latest_version) return
  const targetVersion = current.latest_version
  action.value = 'install'
  loadError.value = ''
  reconnectError.value = ''
  const generation = requestGuard.next()
  try {
    const accepted = await installServerUpdate(targetVersion)
    if (disposed || !requestGuard.isCurrent(generation)) return
    applyStatus(accepted)
    action.value = ''
    reconnecting.value = true
    const recovered = await waitForServerUpdateReconnect(targetVersion, () => loadServerUpdate())
    if (disposed || !requestGuard.isCurrent(generation)) return
    if (!recovered) {
      reconnectError.value = '等待 Server 恢复超时。请刷新页面或通过原部署方式检查进程状态。'
      return
    }
    applyStatus(recovered)
    await auth.bootstrap(true)
    if (recovered.current_version === targetVersion && recovered.phase !== 'failed' && recovered.phase !== 'rolled_back') {
      notify(`Server 已更新到 ${targetVersion}`, 'success')
    } else {
      reconnectError.value = recovered.error_code
        ? updateErrorLabel(recovered.error_code)
        : 'Server 已恢复响应，但目标版本未生效。请查看运行日志。'
    }
  } catch (reason) {
    if (!disposed && requestGuard.isCurrent(generation)) {
      loadError.value = message(reason)
      notify(message(reason), 'error')
    }
  } finally {
    if (!disposed && requestGuard.isCurrent(generation)) {
      action.value = ''
      reconnecting.value = false
    }
  }
}

function dateTime(value: string | null) {
  if (!value) return '尚未检查'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '时间未知' : date.toLocaleString()
}

function message(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败' }

onMounted(() => {
  if (!canAdmin.value) { loading.value = false; return }
  void refresh().catch(() => undefined).finally(() => { loading.value = false })
})
onBeforeUnmount(() => { disposed = true; requestGuard.invalidate() })
</script>

<template>
  <section v-if="canAdmin" class="panel mt-6" aria-labelledby="server-update-title">
    <div class="flex flex-wrap items-start justify-between gap-4">
      <div>
        <h2 id="server-update-title" class="m-0 text-lg">Server 更新</h2>
        <p class="text-subtle mb-0 mt-1 text-sm">仅检查官方 OhMyCine-Server Release；更新只替换 Server 可执行文件，不会覆盖 <code>.runtime</code> 中的数据、配置、插件或缓存。</p>
      </div>
      <span v-if="status" :class="phaseClass" role="status">{{ reconnecting ? '正在等待 Server 重启' : updatePhaseLabel(status.phase) }}</span>
    </div>

    <p v-if="loading" class="text-subtle mb-0 mt-5" aria-live="polite">正在读取 Server 更新状态…</p>
    <div v-else-if="status" class="mt-5">
      <dl class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <div class="semantic-inset p-3"><dt class="text-subtle text-xs">当前版本</dt><dd class="m-0 mt-1 font-mono text-sm">{{ status.current_version || 'dev' }}</dd></div>
        <div class="semantic-inset p-3"><dt class="text-subtle text-xs">最新版本</dt><dd class="m-0 mt-1 font-mono text-sm">{{ status.latest_version || '当前通道暂无版本' }}</dd></div>
        <div class="semantic-inset p-3"><dt class="text-subtle text-xs">更新通道</dt><dd class="m-0 mt-1 text-sm">{{ status.channel === 'beta' ? 'Beta（含预发布）' : 'Stable（仅正式版）' }}</dd></div>
        <div class="semantic-inset p-3"><dt class="text-subtle text-xs">上次检查</dt><dd class="m-0 mt-1 text-sm">{{ dateTime(status.last_checked_at || null) }}</dd></div>
      </dl>

      <div v-if="status.deployment_managed" class="semantic-warning mt-4 p-4" role="status">
        <strong>由部署方式管理</strong>
        <p class="mb-0 mt-1 text-sm">{{ updateManagedReasonLabel(status.managed_reason) }}</p>
      </div>
      <div v-else-if="!status.official_build || !status.comparable" class="semantic-warning mt-4 p-4" role="status">
        <strong>当前版本不能在页面内更新</strong>
        <p class="mb-0 mt-1 text-sm">开发构建可以检查版本，但必须先手工安装首个支持自更新的正式 Server 包。</p>
      </div>
      <p v-else-if="status.last_checked_at && !status.update_available && !status.error_code" class="semantic-success mt-4 p-4 text-sm" role="status">当前 {{ status.channel === 'beta' ? 'Beta' : 'Stable' }} 通道没有需要安装的新版本。</p>
      <p v-if="status.error_code" class="semantic-warning mt-4 p-4 text-sm" role="alert">{{ updateErrorLabel(status.error_code) }}</p>
      <p v-if="reconnecting" class="semantic-warning mt-4 p-4 text-sm" aria-live="assertive">更新包已校验并交给本机更新助手。连接短暂中断是正常现象；页面正在等待健康检查完成，请勿重复启动 Server。</p>
      <p v-if="reconnectError" class="semantic-warning mt-4 p-4 text-sm" role="alert">{{ reconnectError }}</p>
      <p v-if="loadError" class="semantic-warning mt-4 p-4 text-sm" role="alert">{{ loadError }}</p>

      <div class="mt-5 grid items-end gap-3 sm:grid-cols-[minmax(12rem,1fr)_auto]">
        <label><span class="label">Release 通道</span><select v-model="channel" class="input" :disabled="busy"><option value="beta">Beta（预发布与正式版）</option><option value="stable">Stable（仅正式版）</option></select></label>
        <button class="btn-secondary" type="button" :disabled="busy || !channelDirty" @click="saveChannel">{{ action === 'channel' ? '正在保存…' : '保存通道' }}</button>
      </div>
      <p class="text-subtle mb-0 mt-2 text-xs">Stable 没有正式 Release 时会显示“暂无版本”，不会自动回退到 Beta。</p>

      <div class="mt-5 flex flex-wrap gap-3">
        <button class="btn-secondary" type="button" :disabled="busy" @click="checkNow">{{ action === 'check' ? '正在检查…' : '立即检查' }}</button>
        <button class="btn-primary" type="button" :disabled="!installAllowed" @click="install">{{ action === 'install' ? '正在准备更新…' : reconnecting ? '正在等待重启…' : `下载并更新${status.latest_version ? `到 ${status.latest_version}` : ''}` }}</button>
      </div>
      <p class="text-subtle mb-0 mt-3 text-xs">点击更新后，Server 会自动下载、校验 SHA-256、备份旧二进制、替换并重启；新版本健康检查失败时会自动回滚。</p>
    </div>
    <div v-else class="mt-5">
      <p class="semantic-warning p-4 text-sm" role="alert">{{ loadError || '无法读取 Server 更新状态。' }}</p>
      <button class="btn-secondary" type="button" @click="loading = true; refresh().catch(() => undefined).finally(() => { loading = false })">重新加载</button>
    </div>
  </section>
</template>
