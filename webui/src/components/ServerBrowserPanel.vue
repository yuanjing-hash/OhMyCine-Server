<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '@/api/client'

const props = defineProps<{ canInstall: boolean }>()
const accepted = ref(false)
const busy = ref(false)
const error = ref('')
const status = ref<{ state: string; installed: boolean; runtime_error?: string; tun_fake_ip_enabled?: boolean }>()
let generation = 0
let controller: AbortController | undefined
const labels: Record<string, string> = {
  ready: '组件已安装；实际启动结果以登录时检查为准', not_installed: '尚未安装',
  license_acceptance_required: '尚未接受浏览器许可', installing: '正在安装',
  install_failed: '安装失败，请重试', platform_unsupported: '当前平台不支持', unavailable: '组件服务不可用',
  launch_failed: '浏览器启动失败，请检查 Server 浏览器运行依赖与沙箱支持后重试',
}
const runtimeLabels: Record<string, string> = {
  browser_launch_failed: '浏览器启动失败，请检查 Server 浏览器运行依赖与沙箱支持后重试',
  browser_navigation_failed: '浏览器无法打开站点，请检查当前镜像连通性后重试登录',
  network_denied: '站点域名解析到了内网或受限地址，已阻止连接；请检查 Server 的 DNS 和网络配置',
  tun_fake_ip_requires_opt_in: '检测到 TUN 的 Fake-IP 地址，请由部署管理员启用下方兼容选项，无需关闭 TUN 或重新填写账号密码',
  network_timeout: '站点连接超时，请稍后重试登录或检查当前镜像连通性',
  launch_failed: '浏览器启动失败，请检查 Server 浏览器运行依赖与沙箱支持后重试',
  navigation_failed: '浏览器无法打开站点，请检查当前镜像连通性后重试登录',
  navigation_timeout: '站点打开超时，请稍后重试登录或检查当前镜像连通性',
  timeout: '浏览器操作超时，请检查站点连通性后重试',
}
const label = computed(() => status.value?.runtime_error
  ? runtimeLabels[status.value.runtime_error] ?? `${status.value.installed ? '组件已安装，但最近启动失败' : '组件服务启动失败'}。请检查完整 Server 安装与当前镜像连通性，然后重试。`
  : labels[status.value?.state ?? ''] ?? '尚未检查')
const tunLabel = computed(() => status.value?.tun_fake_ip_enabled === undefined
  ? '状态未知' : status.value.tun_fake_ip_enabled ? '已启用' : '未启用（默认）')
async function run(install = false) {
  if (busy.value || (install && (!props.canInstall || !accepted.value))) return
  const version = ++generation
  const active = new AbortController(); controller = active
  const timeout = setTimeout(() => active.abort(), install ? 600000 : 30000)
  busy.value = true; error.value = ''
  try {
    const result = await api<NonNullable<typeof status.value>>(`/api/v1/settings/browser/${install ? 'install' : 'status'}`, install
      ? { method: 'POST', body: JSON.stringify({ license_accepted: true }), signal: active.signal }
      : { signal: active.signal })
    if (version === generation) status.value = result
  } catch {
    if (version === generation) { status.value = undefined; error.value = '无法检查或安装 Server 浏览器。请确认完整安装包及 Node 运行依赖可用，然后重试。' }
  } finally { clearTimeout(timeout); if (version === generation) busy.value = false }
}
onMounted(() => { void run() })
onBeforeUnmount(() => { generation++; controller?.abort() })
</script>

<template>
  <section id="browser" class="panel mt-6" aria-label="Server 内置浏览器">
    <h3 class="m-0 text-base">内置浏览器 · CloakBrowser</h3>
    <p class="text-subtle text-sm">由 Server 统一安装和管理，所有需要浏览器的插件共用。安装后账号密码登录默认使用浏览器，无需在插件中重复安装。</p>
    <p v-if="error" class="semantic-error p-3 text-sm" role="alert">{{ error }}</p>
    <p v-else role="status" class="text-sm">{{ busy ? '正在处理，请稍候…' : label }}</p>
    <p v-if="status" class="text-subtle text-sm">TUN / Fake-IP 兼容：{{ tunLabel }}。仅当 Server 所在环境由可信 TUN 接管时，由部署管理员设置 <code>OMC_CLOAK_TUN_FAKE_IP=true</code> 并重启 Server。Docker 需重建容器以更新环境变量；仅浏览器页面所在电脑开启 TUN 不算。仍校验证书和站点域名，不放行内网地址。</p>
    <template v-if="canInstall && !status?.installed">
      <label class="flex items-center gap-2 text-sm"><input v-model="accepted" type="checkbox" :disabled="busy" />我已阅读并接受
        <a href="https://github.com/CloakHQ/CloakBrowser/blob/main/BINARY-LICENSE.md" target="_blank" rel="noopener noreferrer">CloakBrowser 浏览器许可</a>
      </label>
      <p class="text-subtle text-xs">确认后由此 Server 从官方获取浏览器。不会自动向 FlareSolverr 转交站点登录凭据。</p>
    </template>
    <div class="mt-3 flex flex-wrap gap-2">
      <button type="button" class="btn-secondary" :disabled="busy" @click="run()">检查组件状态</button>
      <button v-if="canInstall && !status?.installed" type="button" class="btn-primary" :disabled="busy || !accepted || status?.state === 'platform_unsupported'" @click="run(true)">接受许可并安装</button>
    </div>
  </section>
</template>
