<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import SecretInput from '@/components/SecretInput.vue'
import {
  pluginResourceCaptchaAssetPath,
  type PluginConnectionSummary,
  type ResourceCaptchaPoint,
  type ResourceHealthResponse,
  type ResourceLoginResponse,
} from '@/plugins'

const props = withDefaults(defineProps<{
  pluginId: string
  connection: PluginConnectionSummary
  canManage?: boolean
  response?: ResourceLoginResponse
  healthResponse?: ResourceHealthResponse
  busy?: boolean
  error?: string
}>(), { canManage: true, response: undefined, healthResponse: undefined, error: '' })

const emit = defineEmits<{
  login: [credentials: { username: string, password: string }]
  cookie: [cookie: string]
  captcha: [payload: { challengeId: string, points: ResourceCaptchaPoint[] }]
  health: []
}>()

const mode = ref<'password' | 'cookie'>('password')
const username = ref('')
const password = ref('')
const cookie = ref('')
const points = ref<ResourceCaptchaPoint[]>([])

const challenge = computed(() => props.response?.state === 'captcha_required' ? props.response.challenge : undefined)
const actionDisabled = computed(() => props.canManage === false || Boolean(props.busy) || !props.connection.enabled)
const captchaURL = computed(() => challenge.value
  ? pluginResourceCaptchaAssetPath(props.pluginId, props.connection.id, challenge.value.imageAssetRef)
  : '')

watch(() => challenge.value?.challengeId, () => { points.value = [] })

function submitLogin() {
  const secret = password.value
  password.value = ''
  emit('login', { username: username.value, password: secret })
}

function submitCookie() {
  const secret = cookie.value
  cookie.value = ''
  emit('cookie', secret)
}

function selectCaptchaPoint(event: MouseEvent) {
  const current = challenge.value
  if (!current || points.value.length >= current.maxPoints) return
  const image = event.currentTarget as HTMLImageElement
  const bounds = image.getBoundingClientRect()
  if (bounds.width <= 0 || bounds.height <= 0) return
  const x = Math.max(0, Math.min(current.width - 1, Math.round((event.clientX - bounds.left) * current.width / bounds.width)))
  const y = Math.max(0, Math.min(current.height - 1, Math.round((event.clientY - bounds.top) * current.height / bounds.height)))
  points.value = [...points.value, { x, y }]
}

function submitCaptcha() {
  const current = challenge.value
  if (!current || points.value.length === 0) return
  emit('captcha', { challengeId: current.challengeId, points: [...points.value] })
}

function healthLabel() {
  if (props.healthResponse?.status === 'healthy') return '入口可用，登录有效'
  if (props.healthResponse?.status === 'auth_required') return '入口可用，需要重新登录'
  if (props.healthResponse?.status === 'rate_limited') return '站点正在限流，请稍后再试'
  if (props.healthResponse?.status === 'unavailable') return '当前入口不可用'
  if (!props.connection.enabled) return '连接已停用'
  if (props.connection.health_status === 'healthy') return '入口可用，登录有效'
  if (props.connection.health_status === 'auth_pending') return '凭据已保存，等待验证'
  if (props.connection.health_status === 'auth_expired' || props.connection.health_status === 'auth_required') return '登录已过期，请重新登录'
  if (props.connection.health_status === 'rate_limited') return '站点正在限流，请稍后再试'
  if (props.connection.health_status === 'unavailable' || props.connection.health_status === 'error') return '当前入口不可用'
  return '尚未检测入口状态'
}

function healthClass() {
  const status = props.healthResponse?.status ?? props.connection.health_status
  if (status === 'healthy') return 'status-chip status-chip--ready'
  if (status === 'unavailable' || status === 'error') return 'status-chip status-chip--error'
  return 'status-chip status-chip--warning'
}
</script>

<template>
  <section class="semantic-inset mt-3 grid gap-4 p-4" :aria-label="`${connection.name} 资源站登录`">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h4 class="m-0 text-sm">BT 资源站登录</h4>
        <p class="text-subtle mb-0 mt-1 text-xs">连接固定使用一个镜像；切换镜像后必须重新登录。</p>
      </div>
      <span :class="healthClass()">{{ healthLabel() }}</span>
    </div>

    <div v-if="canManage !== false" class="flex justify-end">
      <button type="button" class="btn-secondary" :disabled="actionDisabled" @click="emit('health')">{{ busy ? '正在检测…' : '检测入口与登录' }}</button>
    </div>

    <dl class="m-0 grid gap-3 text-sm sm:grid-cols-2">
      <div><dt class="text-subtle text-xs">固定镜像</dt><dd class="m-0 mt-1 break-all font-mono text-xs">{{ connection.entry_origin || '未配置' }}</dd></div>
      <div><dt class="text-subtle text-xs">当前账号</dt><dd class="m-0 mt-1">{{ healthResponse?.accountName || connection.login_account_label || (connection.credential_configured ? '已保存登录凭据' : '尚未登录') }}</dd></div>
    </dl>

    <div v-if="error" class="semantic-error p-3 text-sm" role="alert">{{ error }}</div>
    <div v-if="response?.state === 'authenticated'" class="semantic-success p-3 text-sm" role="status">{{ mode === 'cookie' ? 'Cookie 验证成功' : '登录验证成功' }}{{ response.accountName ? `：${response.accountName}` : '' }}。密码和粘贴的 Cookie 均未保留在浏览器中。</div>

    <section v-if="challenge && canManage !== false" class="grid gap-3 rounded border border-[var(--line)] bg-[var(--surface)] p-3">
      <div>
        <strong class="text-sm">需要手动完成验证码</strong>
        <p class="text-subtle mb-0 mt-1 text-xs">{{ challenge.prompt || '请按图片提示依次点击字符。' }} 最多 {{ challenge.maxPoints }} 个点。</p>
      </div>
      <div class="relative mx-auto w-fit max-w-full">
        <img
          :src="captchaURL"
          :width="challenge.width"
          :height="challenge.height"
          alt="资源站登录验证码，请按提示依次点击"
          class="block max-w-full cursor-crosshair select-none rounded bg-white"
          draggable="false"
          referrerpolicy="no-referrer"
          @click="selectCaptchaPoint"
        />
        <span
          v-for="(point, index) in points"
          :key="`${point.x}:${point.y}:${index}`"
          class="pointer-events-none absolute grid h-6 w-6 -translate-x-1/2 -translate-y-1/2 place-items-center rounded-full bg-[var(--accent)] text-xs font-bold text-white shadow"
          :style="{ left: `${point.x / challenge.width * 100}%`, top: `${point.y / challenge.height * 100}%` }"
          aria-hidden="true"
        >{{ index + 1 }}</span>
      </div>
      <p class="text-subtle m-0 text-xs">已选择 {{ points.length }} 个位置。坐标只用于本次挑战，页面不会保存验证码图片。</p>
      <div class="flex flex-wrap justify-end gap-2">
        <button type="button" class="btn-secondary" :disabled="actionDisabled || points.length === 0" @click="points = points.slice(0, -1)">撤销一点</button>
        <button type="button" class="btn-secondary" :disabled="actionDisabled || points.length === 0" @click="points = []">重新选择</button>
        <button type="button" class="btn-primary" :disabled="actionDisabled || points.length === 0" @click="submitCaptcha">{{ busy ? '正在验证…' : '提交验证码' }}</button>
      </div>
    </section>

    <template v-else-if="canManage !== false">
      <nav class="management-tabs" aria-label="资源站登录方式">
        <button type="button" class="management-tab" :class="{ 'management-tab--active': mode === 'password' }" @click="mode = 'password'">账号密码</button>
        <button type="button" class="management-tab" :class="{ 'management-tab--active': mode === 'cookie' }" @click="mode = 'cookie'">粘贴 Cookie</button>
      </nav>

      <form v-if="mode === 'password'" class="grid gap-3" @submit.prevent="submitLogin">
        <div><label class="label">用户名或邮箱</label><input v-model="username" class="input" minlength="2" maxlength="128" required autocomplete="username" :disabled="actionDisabled" /></div>
        <div><label class="label">密码</label><SecretInput v-model="password" class="input" required autocomplete="current-password" :disabled="actionDisabled" /></div>
        <p class="text-subtle m-0 text-xs">密码只用于这一次登录请求；提交后立即从表单清除，Server 不会持久化密码。</p>
        <button class="btn-primary" :disabled="actionDisabled || username.trim().length < 2 || password.length < 6">{{ busy ? '正在登录…' : '登录' }}</button>
      </form>

      <form v-else class="grid gap-3" @submit.prevent="submitCookie">
        <div><label class="label">站点 Cookie</label><SecretInput v-model="cookie" class="input min-h-24 font-mono text-xs" multiline required autocomplete="off" spellcheck="false" :disabled="actionDisabled" /></div>
        <p class="text-subtle m-0 text-xs">Cookie 将直接交给 Server 加密保存，不会写入插件普通配置、浏览器存储或日志。</p>
        <button class="btn-primary" :disabled="actionDisabled || !cookie.trim()">{{ busy ? '正在保存…' : '保存 Cookie 并登录' }}</button>
      </form>
    </template>
    <p v-else class="text-subtle m-0 text-xs">当前账户没有管理权限，只能查看资源站连接状态。</p>
  </section>
</template>
