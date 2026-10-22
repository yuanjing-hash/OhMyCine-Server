<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api/client'
import { discoveryTypeOptions, normalizeCloudSiteConfig, panSouScopePath, type CloudSiteConfig, type PanSouScope } from '@/sites'

const props = defineProps<{ modelValue: CloudSiteConfig; baseURL: string; username: string; password: string; authEnabled: boolean; siteID?: number; revision?: number }>()
const emit = defineEmits<{ 'update:modelValue': [value: CloudSiteConfig] }>()
const scope = ref<PanSouScope | null>(null)
const scopeLoading = ref(false)
const scopeError = ref('')
let generation = 0
let controller: AbortController | undefined
const config = computed(() => normalizeCloudSiteConfig(props.modelValue))
function patch(value: Partial<CloudSiteConfig>) { emit('update:modelValue', { ...config.value, ...value }) }
const sourceScope = computed({ get: () => config.value.source_scope!, set: value => patch({ source_scope: value }) })
const channels = computed({ get: () => config.value.channels.join('\n'), set: value => patch({ channels: value.split(/[,，\n]/).map(item => item.trim()).filter(Boolean) }) })
const plugins = computed({ get: () => config.value.plugins?.join(', ') ?? '', set: value => patch({ plugins: [...new Set(value.split(/[,，\s]+/).map(item => item.trim()).filter(Boolean))] }) })
const typePolicy = computed({ get: () => config.value.type_policy!, set: value => patch({ type_policy: value }) })
const cloudTypes = computed({ get: () => config.value.cloud_types ?? [], set: value => patch({ cloud_types: value }) })
const includesTG = computed(() => sourceScope.value !== 'plugin')
const includesPlugins = computed(() => sourceScope.value !== 'tg')
const fingerprint = computed(() => JSON.stringify([props.baseURL, props.username, props.password, props.authEnabled, props.siteID, props.revision, config.value]))
watch(fingerprint, () => {
  generation++
  controller?.abort()
  scope.value = null
  scopeError.value = ''
  scopeLoading.value = false
})
async function loadScope() {
  if (!includesTG.value || !props.baseURL.trim()) return
  controller?.abort()
  const request = new AbortController()
  controller = request
  const current = ++generation
  scopeLoading.value = true
  scopeError.value = ''
  let timedOut = false
  const timeout = window.setTimeout(() => { timedOut = true; request.abort() }, 20_000)
  try {
    const result = await api<PanSouScope>(panSouScopePath, {
      method: 'POST', signal: request.signal,
      body: JSON.stringify({ refresh: true, base_url: props.baseURL.trim(), cloud_config: { ...config.value, auth_enabled: props.authEnabled }, username: props.username || undefined, password: props.password || undefined, site_id: props.siteID, revision: props.revision }),
    })
    if (current !== generation || request.signal.aborted) return
    scope.value = result
    if (result.default_error_code) scopeError.value = '默认频道读取失败；搜索时会标明 TG 范围未完成，已启用插件仍可搜索。'
  } catch (reason) {
    if (current === generation && (!request.signal.aborted || timedOut)) scopeError.value = timedOut ? '读取频道范围超时，请确认 PanSou 地址可从 Server 访问后重试。' : reason instanceof Error ? reason.message : '无法读取频道范围，请重试。'
  } finally { window.clearTimeout(timeout); if (current === generation) scopeLoading.value = false }
}
onBeforeUnmount(() => { generation++; controller?.abort() })
</script>

<template>
  <section class="space-y-4 sm:col-span-2" aria-label="PanSou 搜索范围">
    <div>
      <label class="label" for="pansou-source-scope">搜索来源</label>
      <select id="pansou-source-scope" v-model="sourceScope" class="input">
        <option value="all">TG 频道＋已启用插件</option><option value="tg">仅 TG 频道</option><option value="plugin">仅 PanSou 插件</option>
      </select>
      <p class="text-subtle mb-0 mt-2 text-xs">插件开关由你连接的 PanSou 管理，与 OhMyCine 插件管理中的开关独立。</p>
    </div>
    <div v-if="includesTG">
      <div class="mb-2 flex flex-wrap items-end justify-between gap-2">
        <label class="label mb-0" for="site-channels">自定义 TG 频道</label>
        <div class="flex gap-1" role="group" aria-label="频道范围">
          <button type="button" class="btn-secondary text-xs" :aria-pressed="config.channel_mode === 'custom_only'" @click="patch({ channel_mode: 'custom_only' })">仅自定义</button>
          <button type="button" class="btn-secondary text-xs" :aria-pressed="config.channel_mode === 'defaults_plus_custom'" @click="patch({ channel_mode: 'defaults_plus_custom' })">默认＋自定义</button>
        </div>
      </div>
      <textarea id="site-channels" v-model="channels" class="input min-h-28 font-mono text-sm" :required="config.channel_mode === 'custom_only'" placeholder="每行一个；支持 @频道名或 https://t.me/频道名" />
      <div class="mt-2 flex flex-wrap items-center justify-between gap-2">
        <p class="text-subtle m-0 text-xs">{{ config.channel_mode === 'custom_only' ? '只搜索这里填写的频道。' : '合并本实例默认频道与自定义频道，自动去重；留空即可使用默认频道。' }}去重后最多 1024 个。</p>
        <button class="btn-secondary text-xs" type="button" :disabled="scopeLoading || !baseURL.trim()" @click="loadScope">{{ scopeLoading ? '正在读取…' : scope ? '刷新频道范围' : '读取频道范围' }}</button>
      </div>
      <p v-if="scope && !scope.default_error_code" class="semantic-inset mb-0 mt-2 p-3 text-xs" role="status"><template v-if="config.channel_mode === 'custom_only'">仅自定义：规范化后共 {{ scope.effective_count }} 个频道。</template><template v-else>默认 {{ scope.default_count }} 个＋自定义 {{ scope.custom_count }} 个，去重后共 {{ scope.effective_count }} 个频道。</template></p>
      <p v-else-if="!scopeError" class="text-subtle mb-0 mt-2 text-xs" role="status">默认频道来自当前 PanSou 实例，读取后显示实际数量；配置变化后需要重新读取。</p>
      <p v-if="scopeError" class="semantic-warning mb-0 mt-2 p-3 text-xs" role="alert">{{ scopeError }}</p>
    </div>
    <div v-if="includesPlugins">
      <label class="label" for="pansou-plugins">插件范围（可选）</label>
      <input id="pansou-plugins" v-model="plugins" class="input font-mono text-sm" placeholder="留空搜索 PanSou 已启用的全部插件；多个名称用逗号分隔" />
      <p v-if="!includesTG" class="text-subtle mb-0 mt-2 text-xs">仅搜索插件，无须填写 TG 频道，也不读取默认频道。</p>
    </div>
    <div>
      <label class="label" for="pansou-type-policy">默认结果类型</label>
      <select id="pansou-type-policy" v-model="typePolicy" class="input"><option value="auto">自动：当前用户可用的网盘＋磁力</option><option value="all">全部类型</option><option value="manual">指定类型</option></select>
      <fieldset v-if="typePolicy === 'manual'" class="mt-3 flex flex-wrap gap-3"><legend class="label">显示类型</legend><label v-for="option in discoveryTypeOptions" :key="option[0]" class="text-sm"><input v-model="cloudTypes" type="checkbox" :value="option[0]" /> {{ option[1] }}</label></fieldset>
      <p class="text-subtle mb-0 mt-2 text-xs">自动模式始终搜索磁力；115／夸克按当前用户可用配置选择。其他网盘可以展示，是否能入库由实际接收能力决定。夸克目前仅支持分享转存。</p>
    </div>
    <p v-if="authEnabled && /^http:\/\//i.test(baseURL.trim())" class="semantic-warning m-0 p-3 text-xs">HTTP 会明文传输 PanSou 登录信息和搜索请求；该地址仍可保存和使用。</p>
  </section>
</template>

<style scoped>
.btn-secondary[aria-pressed="true"] { border-color: var(--accent); background: var(--accent-soft); color: var(--accent); }
</style>
