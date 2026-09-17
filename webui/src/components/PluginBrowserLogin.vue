<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api, APIError } from '@/api/client'
import type { ResourceLoginResponse } from '@/plugins'
import SecretInput from '@/components/SecretInput.vue'

const props = defineProps<{ pluginId: string; connectionId: string; origin: string; disabled?: boolean }>()
const emit = defineEmits<{ result: [response: ResourceLoginResponse]; cancel: [] }>()
const opened = ref(true)
const busy = ref(false)
const state = ref('unknown')
const session = ref('')
const image = ref('')
const width = ref(1)
const height = ref(1)
const input = ref('')
const error = ref('')
const resourceWarning = ref('')
const success = ref(false)
let generation = 0
let controller: AbortController | undefined
let handedOff = false
let sessionBase = ''
const base = computed(() => `/api/v1/plugins/${encodeURIComponent(props.pluginId)}/connections/${encodeURIComponent(props.connectionId)}/resource/browser`)
const inputID = computed(() => `browser-login-text-${props.connectionId}`)
const locked = computed(() => busy.value || props.disabled)

function reset() {
  if (session.value && sessionBase && !handedOff) closeSession(sessionBase, session.value)
  generation++
  controller?.abort()
  image.value = ''; input.value = ''; session.value = ''; error.value = ''; resourceWarning.value = ''; success.value = false
  opened.value = true; busy.value = false; state.value = 'unknown'
  sessionBase = ''
}
function closeSession(path: string, id: string) {
  void api(`${path}/close`, { method: 'POST', body: JSON.stringify({ session_id: id }) }).catch(() => {})
}
onBeforeUnmount(reset)

async function run(operation: string, payload: Record<string, unknown> = {}) {
  if (locked.value) return
  const version = ++generation
  const path = base.value
  controller = new AbortController()
  const signal = controller.signal
  const activeController = controller
  const timeout = setTimeout(() => activeController.abort(), operation === 'confirm' ? 180000 : 45000)
  busy.value = true; error.value = ''
  try {
    const result = await api<Record<string, unknown>>(`${path}/${operation}`, operation === 'status'
      ? { signal }
      : { method: 'POST', body: JSON.stringify(payload), signal })
    if (version !== generation) {
      if (operation === 'status' && typeof result.session_id === 'string' && result.session_id) {
        closeSession(path, result.session_id)
      }
      return
    }
    if (typeof result.state === 'string') state.value = result.state
    if (operation === 'status') {
      const restored = typeof result.session_id === 'string' ? result.session_id : ''
      if (restored !== session.value) {
        image.value = ''; input.value = ''; resourceWarning.value = ''; success.value = false
      }
      session.value = restored
      sessionBase = restored ? path : ''
    }
    if (operation === 'start' && typeof result.session_id === 'string') {
      session.value = result.session_id; success.value = false
    }
    if (operation === 'confirm') {
      const response = result as unknown as ResourceLoginResponse
      if (response.state !== 'authenticated' && response.state !== 'captcha_required') throw new Error('not_authenticated')
      handedOff = true
      success.value = response.state === 'authenticated'; image.value = ''; resourceWarning.value = ''; emit('result', response)
    }
    if (operation === 'close') { session.value = ''; image.value = ''; resourceWarning.value = ''; success.value = false; emit('cancel') }
    if (operation === 'snapshot') {
      if (result.mime_type !== 'image/png' || typeof result.image_base64 !== 'string' || result.image_base64.length > 8 * 1024 * 1024
        || !/^[A-Za-z0-9+/]*={0,2}$/.test(result.image_base64)
        || typeof result.width !== 'number' || result.width <= 0 || result.width > 4096
        || typeof result.height !== 'number' || result.height <= 0 || result.height > 4096) throw new Error('invalid_snapshot')
      width.value = result.width; height.value = result.height
      image.value = `data:image/png;base64,${result.image_base64}`
      const resourceMessages: Record<string, string> = {
        resource_network_denied: '部分网页资源被网络安全检查阻止，页面样式或验证可能不完整。',
        resource_network_timeout: '部分网页资源连接超时，页面样式或验证可能尚未加载。',
        resource_network_failed: '部分公网资源连接失败，页面样式或验证可能不完整。',
        resource_limit_exceeded: '网页资源请求达到本次安全限制，部分内容未加载。',
        tun_fake_ip_requires_opt_in: '网页资源使用 TUN 虚拟地址，请在 Server 系统设置查看 TUN 兼容配置。',
      }
      const warning = typeof result.network_error_code === 'string' ? resourceMessages[result.network_error_code] : undefined
      const count = typeof result.blocked_resource_count === 'number' && Number.isSafeInteger(result.blocked_resource_count) && result.blocked_resource_count >= 0 && result.blocked_resource_count <= 10000 ? result.blocked_resource_count : undefined
      resourceWarning.value = warning ? `${warning}${count ? ` 本次未加载 ${count} 项。` : ''}这不代表账号密码错误或登录失效；可检查网络后重新加载网页。` : ''
    }
  } catch (reason) {
    if (version !== generation) return
    const code = reason instanceof APIError ? reason.errorCode : ''
    if (code === 'resource_browser_session_expired') {
      session.value = ''; image.value = ''; input.value = ''; resourceWarning.value = ''; success.value = false
    }
    error.value = code === 'resource_browser_verification_required' ? '站点仍要求验证，请在浏览器画面内手动完成。'
      : code === 'resource_auth_required' || code === 'resource_auth_failed' ? '站点尚未确认登录，请先在画面内登录。'
      : code === 'resource_browser_session_expired' ? '本次验证已过期，请取消后重新提交登录。已保存的登录状态不因此删除。'
      : code === 'resource_browser_login_expired' ? '保存的本次密码请求已过期，网页仍可使用；请在画面内登录后再次确认，或取消后重新输入账号密码。'
      : code === 'resource_entry_unavailable' ? '插件未能确认站点的登录状态（站点响应不可用或无法识别），不代表账号密码错误；网页已保留，可稍后再次确认。'
      : code === 'resource_browser_request_failed' ? '浏览器中的站点请求失败，可能是网络或不支持的跳转；不是密码错误。网页已保留，可稍后再次确认。'
      : code === 'resource_browser_request_timeout' ? '浏览器中的站点请求超时；网页已保留，请稍后再次确认。'
      : code === 'resource_browser_request_denied' ? '站点请求被浏览器网络安全策略拒绝，不代表密码错误；请检查 Server 网络配置。'
      : code === 'resource_browser_tun_required' ? '浏览器检测到 TUN Fake-IP；请在 Server 部署中启用 OMC_CLOAK_TUN_FAKE_IP=true 并重启 Server，无需反复输入密码。'
      : code === 'resource_browser_unavailable' || code === 'resource_browser_start_failed' ? 'Server 浏览器启动失败，请到系统设置的内置浏览器中检查运行状态。'
      : code === 'resource_browser_reload_failed' ? '网页重新加载未完成，请检查网络或更新画面查看状态；不代表登录失效。'
      : code === 'resource_browser_reload_post_denied' ? '当前页面来自表单提交，为避免重复提交，不能直接重新加载。可更新画面或继续手动验证。'
      : '操作未完成。请检查组件状态或更新画面后重试；未确认登录成功。'
  } finally {
    clearTimeout(timeout)
    if (version === generation) busy.value = false
  }
}
async function open() { await run('status'); if (session.value) await snapshot() }
async function snapshot() { await run('snapshot', { session_id: session.value }) }
async function reload() {
  const currentGeneration = generation, currentSession = session.value
  await run('reload', { session_id: currentSession })
  if (generation === currentGeneration + 1 && session.value === currentSession && !error.value) await snapshot()
}
async function action(payload: Record<string, unknown>) {
  await run('input', { session_id: session.value, ...payload })
  if (!error.value && session.value) await snapshot()
}
async function clickImage(event: MouseEvent) {
  if (!session.value || locked.value) return
  const rect = (event.currentTarget as HTMLImageElement).getBoundingClientRect()
  if (rect.width <= 0 || rect.height <= 0) return
  await action({ action: 'click', x: Math.max(0, Math.min(width.value - 1, Math.floor((event.clientX - rect.left) * width.value / rect.width))),
    y: Math.max(0, Math.min(height.value - 1, Math.floor((event.clientY - rect.top) * height.value / rect.height))) })
}
async function sendText() { const text = input.value; input.value = ''; await action({ action: 'text', text }) }
watch(() => [props.pluginId, props.connectionId, props.origin, props.disabled], () => {
  reset(); handedOff = false
  if (!props.disabled) void open()
}, { immediate: true })
</script>

<template>
  <section class="semantic-inset mt-3 grid gap-3 p-3" aria-label="内置浏览器登录">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <strong class="text-sm">需要手动完成站点验证</strong>
    </div>
    <template v-if="opened">
      <p class="text-subtle m-0 text-xs" role="status">{{ busy ? '正在处理…' : '请在下方画面完成站点要求的验证，然后继续登录' }}。当前镜像：{{ origin }}</p>
      <p v-if="error" class="semantic-error p-2 text-sm" role="alert">{{ error }}</p>
      <p v-if="resourceWarning" class="semantic-warning p-2 text-sm" role="status">{{ resourceWarning }}</p>
      <p v-if="success" class="semantic-success p-2 text-sm" role="status">已通过插件登录检测。</p>
      <template v-if="!session">
        <p v-if="!busy && !error" class="text-subtle m-0 text-xs">没有可继续的验证，请取消后重新提交登录。</p>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn-secondary" :disabled="locked" @click="emit('cancel')">取消验证</button>
        </div>
      </template>
      <template v-else>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn-secondary" :disabled="locked" @click="snapshot">更新画面</button>
          <button type="button" class="btn-secondary" :disabled="locked" @click="reload">重新加载网页</button>
          <button type="button" class="btn-primary" :disabled="locked" @click="run('confirm', { session_id: session })">验证完成，继续登录</button>
          <button type="button" class="btn-secondary" :disabled="locked" @click="run('close', { session_id: session })">取消验证</button>
        </div>
        <img v-if="image" :src="image" :width="width" :height="height" alt="站点浏览器画面，点击输入框或手动验证位置" class="max-w-full cursor-crosshair border border-[var(--line)]" draggable="false" @click="clickImage" />
        <div class="flex flex-wrap gap-2">
          <label class="label" :for="inputID">点击画面中的输入框后输入（提交后清空）</label>
          <SecretInput :id="inputID" v-model="input" class="input" autocomplete="off" maxlength="2048" :disabled="locked" @keydown.enter.prevent="sendText" />
          <button type="button" class="btn-secondary" :disabled="locked || !input" @click="sendText">输入到当前焦点</button>
          <button v-for="key in ['Tab', 'Enter', 'Backspace']" :key="key" type="button" class="btn-secondary" :disabled="locked" @click="action({ action: 'key', key })">{{ key }}</button>
          <button type="button" class="btn-secondary" :disabled="locked" @click="action({ action: 'scroll', delta_y: -500 })">向上滚动</button>
          <button type="button" class="btn-secondary" :disabled="locked" @click="action({ action: 'scroll', delta_y: 500 })">向下滚动</button>
        </div>
      </template>
    </template>
  </section>
</template>
