<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import { useAuthStore } from '@/stores/auth'
import { notify } from '@/toast'
import { transferNodeDockerEnvironment, transferNodeTransport } from '@/transfer-node-installation'
import type { CreateTransferNodeResult, ListResponse, TransferNodeInstallation, TransferNodeSettings, TransferNodeSummary } from '@/types/api'

const auth = useAuthStore()
const nodes = ref<TransferNodeSummary[]>([])
const settings = ref<TransferNodeSettings | null>(null)
const loading = ref(true)
const saving = ref(false)
const createOpen = ref(false)
const createdSecret = ref<{ nodeID: string; token: string; transport: 'http' | 'https'; expiresIn: number; installation: TransferNodeInstallation } | null>(null)
const form = ref({ name: '', apiURL: '', platform: 'linux' as 'linux' | 'windows', architecture: 'amd64' as 'amd64' | 'arm64' })
const onlineNodes = computed(() => nodes.value.filter(node => node.status === 'online'))

function errorMessage(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败，请查看 Server 日志' }
function statusLabel(status: TransferNodeSummary['status']) { return ({ pending: '等待安装', online: '在线', offline: '离线', disabled: '已停用', revoked: '已撤销' } as const)[status] }
function statusClass(status: TransferNodeSummary['status']) { return status === 'online' ? 'status-chip status-chip--ready' : status === 'offline' || status === 'revoked' ? 'status-chip status-chip--error' : 'status-chip status-chip--warning' }
function capacity(node: TransferNodeSummary) { return node.free_bytes_known && node.free_bytes != null ? `${(node.free_bytes / 1024 / 1024 / 1024).toFixed(1)} GiB 可用` : '空间未知' }

async function load() {
  loading.value = true
  try {
    const [nodePage, nodeSettings] = await Promise.all([
      api<ListResponse<TransferNodeSummary>>('/api/v1/transfer-nodes'),
      api<TransferNodeSettings>('/api/v1/transfer-nodes/settings'),
    ])
    nodes.value = nodePage.list
    settings.value = nodeSettings
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { loading.value = false }
}

async function createNode() {
  saving.value = true
  try {
    const result = await api<CreateTransferNodeResult>('/api/v1/transfer-nodes', { method: 'POST', body: JSON.stringify({ name: form.value.name, api_url: form.value.apiURL, platform: form.value.platform, architecture: form.value.architecture }) })
    createdSecret.value = { nodeID: result.node.id, token: result.enrollment_token, transport: transferNodeTransport(result.node.api_url), expiresIn: result.expires_in_seconds, installation: result.installation }
    form.value = { name: '', apiURL: '', platform: 'linux', architecture: 'amd64' }
    createOpen.value = false
    notify('节点已预创建。完整安装命令只显示这一次，请立即复制。', 'success')
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

async function copyInstallation() {
  if (!createdSecret.value) return
  await navigator.clipboard.writeText(createdSecret.value.installation.command || createdSecret.value.token)
  notify(createdSecret.value.installation.available ? '完整安装命令已复制' : '一次性安装令牌已复制', 'success')
}
async function copyNodeID() {
  if (!createdSecret.value) return
  await navigator.clipboard.writeText(createdSecret.value.nodeID)
  notify('Node ID 已复制', 'success')
}
async function copyDockerConfig() {
  if (!createdSecret.value) return
  const env = transferNodeDockerEnvironment(createdSecret.value)
  await navigator.clipboard.writeText(env)
  notify('Docker 配置参数已复制（仅显示本次令牌）', 'success')
}

async function regenerate(node: TransferNodeSummary) {
  saving.value = true
  try {
    const result = await api<{ enrollment_token: string; expires_at: string; installation: TransferNodeInstallation }>(`/api/v1/transfer-nodes/${node.id}/enrollment`, { method: 'POST', body: '{}' })
    createdSecret.value = { nodeID: node.id, token: result.enrollment_token, transport: transferNodeTransport(node.api_url), expiresIn: Math.max(0, Math.round((Date.parse(result.expires_at) - Date.now()) / 1000)), installation: result.installation }
    notify('旧安装令牌已作废，新的完整安装命令只显示这一次。', 'success')
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

async function pair(node: TransferNodeSummary) { await nodeAction(node, 'enroll', '已完成安全配对并测试节点') }
async function test(node: TransferNodeSummary) { await nodeAction(node, 'test', '节点测试已完成') }
async function nodeAction(node: TransferNodeSummary, action: 'enroll' | 'test', successMessage: string) {
  saving.value = true
  try {
    await api(`/api/v1/transfer-nodes/${node.id}/${action}`, { method: 'POST', body: '{}' })
    notify(successMessage, 'success')
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

async function toggle(node: TransferNodeSummary) {
  saving.value = true
  try {
    await api(`/api/v1/transfer-nodes/${node.id}`, { method: 'PATCH', body: JSON.stringify({ enabled: node.status === 'disabled', revision: node.revision }) })
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

async function updateDefault(event: Event) {
  if (!settings.value) return
  const value = (event.target as HTMLSelectElement).value
  saving.value = true
  try {
    settings.value = await api<TransferNodeSettings>('/api/v1/transfer-nodes/settings', { method: 'PATCH', body: JSON.stringify({ default_node_id: value || null, revision: settings.value.revision }) })
    notify(value ? '默认跨库传输节点已保存' : '已取消默认传输节点', 'success')
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error'); await load() } finally { saving.value = false }
}

async function revoke(node: TransferNodeSummary) {
  if (!window.confirm(`确认撤销“${node.name}”？它将不能承接新任务，已有任务只保留恢复和审计事实。`)) return
  saving.value = true
  try {
    await api(`/api/v1/transfer-nodes/${node.id}/revoke`, { method: 'POST', body: '{}' })
    notify('节点身份已撤销', 'success')
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

async function remove(node: TransferNodeSummary) {
  if (!window.confirm(`确认删除“${node.name}”的节点配置？只有已撤销且没有任何关联事实的节点才允许删除。`)) return
  saving.value = true
  try {
    await api(`/api/v1/transfer-nodes/${node.id}`, { method: 'DELETE', body: '{}' })
    notify('节点配置已删除', 'success')
    await load()
  } catch (reason) { notify(errorMessage(reason), 'error') } finally { saving.value = false }
}

onMounted(load)
</script>

<template>
  <section id="download-panel-nodes" role="tabpanel" aria-labelledby="download-tab-nodes">
    <div class="flex flex-wrap items-end justify-between gap-4"><div><h2 class="m-0 text-xl">传输节点</h2><p class="text-subtle mb-0 mt-1 text-xs">主 Server 负责识别和任务决策；公网节点只执行冻结的下载、回传与跨库上传计划，不会自动安装下载器。</p></div><button v-if="auth.can(Permissions.TransferNodesCreate)" class="btn-primary" type="button" @click="createOpen = !createOpen">{{ createOpen ? '取消添加' : '添加传输节点' }}</button></div>

    <form v-if="createOpen" class="panel mt-5 grid gap-4 md:grid-cols-2" @submit.prevent="createNode"><div><label class="label">节点名称</label><input v-model="form.name" class="input" required maxlength="128" /></div><div><label class="label">公网 HTTP / HTTPS 地址</label><input v-model="form.apiURL" class="input" required placeholder="https://node.example.com:4433" /><p class="text-subtle mb-0 mt-2 text-xs">必须能由主 Server 主动访问；支持 HTTP 和 HTTPS，内网及 localhost 地址不可用。</p><p v-if="form.apiURL.trim().toLowerCase().startsWith('http://')" class="semantic-warning-text mb-0 mt-2 text-xs">HTTP 传输不加密；节点仍会验证身份和请求签名。</p></div><div><label class="label">平台</label><select v-model="form.platform" class="input"><option value="linux">Linux</option><option value="windows">Windows</option></select></div><div><label class="label">架构</label><select v-model="form.architecture" class="input"><option value="amd64">amd64</option><option v-if="form.platform === 'linux'" value="arm64">arm64</option></select></div><button class="btn-primary md:col-span-2" :disabled="saving">{{ saving ? '正在创建…' : '生成完整安装命令' }}</button></form>

    <div v-if="createdSecret" class="semantic-warning mt-5 p-4"><div class="flex flex-wrap items-center justify-between gap-3"><div><strong>{{ createdSecret.installation.available ? `完整 ${createdSecret.installation.shell} 安装命令` : '一次性安装令牌' }}</strong><p class="mb-0 mt-1 text-xs">约 {{ Math.ceil(createdSecret.expiresIn / 60) }} 分钟后失效；执行安装后还需回到这里完成配对。命令会先验证 Release 签名和 SHA-256，再修改系统；只安装 OhMyCine Node。</p></div><div class="flex gap-2"><button class="btn-secondary" type="button" @click="copyInstallation">{{ createdSecret.installation.available ? '复制完整命令' : '复制令牌' }}</button><button class="btn-secondary" type="button" @click="copyDockerConfig">复制 Docker 配置</button></div></div><div class="mt-3 flex items-center gap-2 text-xs"><span>Node ID：<code>{{ createdSecret.nodeID }}</code></span><button class="btn-secondary" type="button" @click="copyNodeID">复制 ID</button></div><pre v-if="createdSecret.installation.available" class="mt-3 max-h-72 overflow-auto whitespace-pre-wrap rounded bg-black/10 p-3 text-xs">{{ createdSecret.installation.command }}</pre><div v-else class="semantic-inset mt-3 p-3 text-xs">当前是开发构建，没有注入官方 Node Release 信任根；正式 Beta 会在这里显示完整安装命令。</div></div>

    <div v-if="settings" class="panel mt-5"><label class="label" for="default-transfer-node">默认跨库传输节点</label><select id="default-transfer-node" class="input mt-2" :value="settings.default_node_id || ''" :disabled="saving || !auth.can(Permissions.TransferNodesUpdate)" @change="updateDefault"><option value="">不自动选择</option><option v-for="node in onlineNodes" :key="node.id" :value="node.id">{{ node.name }} · {{ capacity(node) }}</option></select><p class="text-subtle mb-0 mt-2 text-xs">这里只影响新任务的默认选择；任务提交后节点和最终媒体库会被冻结，不会自动漂移或回退主 Server。</p></div>

    <div v-if="loading" class="panel mt-5">正在读取节点状态…</div><div v-else-if="nodes.length === 0" class="panel mt-5">还没有传输节点。</div>
    <div v-else class="node-grid mt-5"><article v-for="node in nodes" :key="node.id" class="panel"><header class="flex items-start justify-between gap-3"><div><div class="flex flex-wrap items-center gap-2"><h3 class="m-0 text-lg">{{ node.name }}</h3><span :class="statusClass(node.status)">{{ statusLabel(node.status) }}</span><span v-if="node.default" class="status-chip status-chip--ready">默认</span></div><p class="text-subtle mb-0 mt-1 text-xs">{{ node.platform }} {{ node.architecture }} · {{ node.agent_version || '尚未读取版本' }}</p></div></header><dl class="mt-4 grid grid-cols-2 gap-3 text-sm"><div><dt class="text-subtle text-xs">协议</dt><dd class="m-0 mt-1">v{{ node.protocol_min }}–v{{ node.protocol_max }}</dd></div><div><dt class="text-subtle text-xs">受管空间</dt><dd class="m-0 mt-1">{{ capacity(node) }}</dd></div><div><dt class="text-subtle text-xs">并发上限</dt><dd class="m-0 mt-1">{{ node.capabilities.max_concurrent || '未知' }}</dd></div><div><dt class="text-subtle text-xs">最近心跳</dt><dd class="m-0 mt-1">{{ node.last_heartbeat_at ? new Date(node.last_heartbeat_at).toLocaleString() : '从未' }}</dd></div></dl><div class="semantic-inset mt-4 p-3"><span class="text-subtle block text-xs">公网 API</span><span class="mt-1 block truncate font-mono text-xs">{{ node.api_url }}</span><span v-if="node.last_error_code" class="semantic-warning-text mt-2 block text-xs">{{ node.last_error_code }}</span></div><div class="mt-4 flex flex-wrap gap-2"><button v-if="node.status === 'pending' && auth.can(Permissions.TransferNodesEnroll)" class="btn-secondary" type="button" :disabled="saving" @click="pair(node)">完成配对</button><button v-if="node.status === 'pending' && auth.can(Permissions.TransferNodesEnroll)" class="btn-secondary" type="button" :disabled="saving" @click="regenerate(node)">重新生成令牌</button><button v-if="node.status !== 'pending' && node.status !== 'revoked' && auth.can(Permissions.TransferNodesTest)" class="btn-secondary" type="button" :disabled="saving" @click="test(node)">测试</button><button v-if="node.status !== 'pending' && node.status !== 'revoked' && auth.can(Permissions.TransferNodesUpdate)" class="btn-secondary" type="button" :disabled="saving" @click="toggle(node)">{{ node.status === 'disabled' ? '启用' : '停用' }}</button><button v-if="node.status !== 'revoked' && auth.can(Permissions.TransferNodesRevoke)" class="btn-danger" type="button" :disabled="saving" @click="revoke(node)">撤销</button><button v-if="node.status === 'revoked' && auth.can(Permissions.TransferNodesDelete)" class="btn-danger" type="button" :disabled="saving" @click="remove(node)">删除</button></div></article></div>
  </section>
</template>

<style scoped>
.node-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 22rem), 1fr)); gap: 1rem; }
</style>
