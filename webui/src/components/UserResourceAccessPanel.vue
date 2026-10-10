<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api, APIError } from '@/api/client'
import { copyResourceAccess, defaultResourcePolicies, resourceAccessFingerprint, resourceAccessScopes, resourceDraftAllows, resourceSelectionLocked, resourceStatusLabel, resourceTypeLabel, setResourceMode } from '@/resource-access'
import type { ResourceAccessMode, ResourceAccessOption, ResourceAccessOptions, ResourceAccessPolicy, ResourceAccessScope, UserResourceAccess } from '@/types/api'

const props = defineProps<{ userId: number; editable: boolean; readOnlyReason?: string; refreshKey?: number; busy?: boolean }>()
const emit = defineEmits<{ options: [userId: number, options: ResourceAccessOptions]; saved: [userId: number] }>()
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const notice = ref('')
const conflicted = ref(false)
const draft = ref<UserResourceAccess>({ revision: 0, policies: defaultResourcePolicies() })
const initial = ref<UserResourceAccess | null>(null)
const options = ref<ResourceAccessOptions | null>(null)
const optionsStale = ref(false)
const searches = ref<Partial<Record<ResourceAccessScope, string>>>({})
const dirty = computed(() => initial.value !== null && resourceAccessFingerprint(draft.value) !== resourceAccessFingerprint(initial.value))
const locked = computed(() => !props.editable || loading.value || saving.value || Boolean(props.busy) || initial.value === null || options.value === null || conflicted.value)
let generation = 0
let request: AbortController | null = null
let disposed = false

function beginRequest() {
  request?.abort()
  const controller = new AbortController()
  request = controller
  return { controller, generation: ++generation, userId: props.userId }
}

function current(active: ReturnType<typeof beginRequest>) {
  return !disposed && active.generation === generation && active.userId === props.userId && !active.controller.signal.aborted
}

function validateAccess(value: UserResourceAccess) {
  const invalid = !value || !Number.isSafeInteger(value.revision) || value.revision < 0 || !Array.isArray(value.policies)
    || value.policies.length !== resourceAccessScopes.length
    || resourceAccessScopes.some(({ scope }) => value.policies.filter(policy => policy?.scope === scope).length !== 1)
    || value.policies.some(policy => !['all', 'allowlist', 'denylist'].includes(policy.mode)
      || !Array.isArray(policy.resource_ids) || policy.resource_ids.some(id => typeof id !== 'string')
      || (policy.mode === 'all' && policy.resource_ids.length))
  if (invalid) throw new Error('资源权限响应不完整，请重新加载。')
}

function validateOptions(value: ResourceAccessOptions) {
  if (!value || !Array.isArray(value.scopes) || resourceAccessScopes.some(item => {
    const scopes = value.scopes.filter(scope => scope?.scope === item.scope)
    if (scopes.length !== 1) return true
    const scope = scopes[0]!
    return scope.resource_type !== item.resourceType || !Array.isArray(scope.options)
      || scope.options.some(option => !option
        || [option.id, option.name, option.type, option.status, option.denial_reason].some(field => typeof field !== 'string')
        || [option.deleted, option.effective_allowed, option.can_grant].some(field => typeof field !== 'boolean'))
  })) throw new Error('资源选项响应不完整，请重新加载。')
}

async function load() {
  const active = beginRequest()
  loading.value = true
  saving.value = false
  error.value = ''
  notice.value = ''
  conflicted.value = false
  initial.value = null
  options.value = null
  optionsStale.value = false
  searches.value = {}
  try {
    const [value, choices] = await Promise.all([
      api<UserResourceAccess>(`/api/v1/users/${active.userId}/resource-access`, { signal: active.controller.signal }),
      api<ResourceAccessOptions>(`/api/v1/users/${active.userId}/resource-access/options`, { signal: active.controller.signal }),
    ])
    if (!current(active)) return
    validateAccess(value)
    validateOptions(choices)
    if (value.revision !== choices.revision) throw new Error('读取期间用户权限已更新，请重新加载。')
    initial.value = copyResourceAccess(value)
    draft.value = copyResourceAccess(value)
    options.value = choices
    emit('options', active.userId, choices)
  } catch (reason) {
    if (current(active)) error.value = reason instanceof Error ? reason.message : '资源权限读取失败'
  } finally {
    if (current(active)) { loading.value = false; request = null }
  }
}

async function save() {
  if (locked.value || !dirty.value) return
  const active = beginRequest()
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    const value = await api<UserResourceAccess>(`/api/v1/users/${active.userId}/resource-access`, { method: 'PUT', body: JSON.stringify(copyResourceAccess(draft.value)), signal: active.controller.signal })
    if (!current(active)) return
    validateAccess(value)
    initial.value = copyResourceAccess(value)
    draft.value = copyResourceAccess(value)
    notice.value = '资源名单已保存，下一次请求和订阅运行使用新权限。已提交任务继续执行。'
    emit('saved', active.userId)
    try {
      const choices = await api<ResourceAccessOptions>(`/api/v1/users/${active.userId}/resource-access/options`, { signal: active.controller.signal })
      if (!current(active)) return
      validateOptions(choices)
      if (choices.revision !== value.revision) {
        conflicted.value = true
        error.value = '保存后权限又有更新，请重新加载以查看当前有效权限。'
        options.value = null
      } else {
        options.value = choices
        optionsStale.value = false
        emit('options', active.userId, choices)
      }
    } catch (reason) {
      if (current(active)) { options.value = null; error.value = `名单已保存，但当前有效权限读取失败：${reason instanceof Error ? reason.message : '请重新加载'}` }
    }
  } catch (reason) {
    if (!current(active)) return
    if (reason instanceof APIError && reason.errorCode === 'CONFLICT') {
      conflicted.value = true
      optionsStale.value = true
      error.value = '此用户的权限已被更新。你的名单改动仍保留，重新加载会放弃这些改动。'
    } else error.value = reason instanceof Error ? reason.message : '资源权限保存失败'
  } finally {
    if (current(active)) { saving.value = false; request = null }
  }
}

function policyFor(scope: ResourceAccessScope): ResourceAccessPolicy {
  return draft.value.policies.find(policy => policy.scope === scope)!
}
function allOptions(scope: ResourceAccessScope): ResourceAccessOption[] {
  return options.value?.scopes.find(item => item.scope === scope)?.options ?? []
}
function visibleOptions(scope: ResourceAccessScope) {
  const search = (searches.value[scope] ?? '').trim().toLocaleLowerCase()
  return allOptions(scope).filter(item => !search || `${item.name} ${resourceTypeLabel(item.type)}`.toLocaleLowerCase().includes(search))
}
function changeMode(scope: ResourceAccessScope, event: Event) {
  if (locked.value) return
  setResourceMode(policyFor(scope), (event.target as HTMLSelectElement).value as ResourceAccessMode)
}
function selectionLocked(scope: ResourceAccessScope, option: ResourceAccessOption) {
  return locked.value || resourceSelectionLocked(policyFor(scope), initial.value?.policies.find(item => item.scope === scope), option)
}
function toggleResource(scope: ResourceAccessScope, option: ResourceAccessOption, event: Event) {
  if (selectionLocked(scope, option)) return
  const policy = policyFor(scope)
  const selected = (event.target as HTMLInputElement).checked
  policy.resource_ids = selected ? [...new Set([...policy.resource_ids, option.id])] : policy.resource_ids.filter(id => id !== option.id)
}
function currentAllowedCount(scope: ResourceAccessScope) { return allOptions(scope).filter(item => !item.deleted && item.effective_allowed).length }
function draftAllowedCount(scope: ResourceAccessScope) { return allOptions(scope).filter(item => !item.deleted && resourceDraftAllows(draft.value.policies, scope, item.id)).length }

watch(() => props.userId, () => { void load() }, { immediate: true })
watch(() => props.refreshKey, () => {
  if (dirty.value) {
    conflicted.value = true
    optionsStale.value = true
    error.value = '角色或直接授权已更新。你的名单改动仍保留，重新加载后再继续保存。'
  } else void load()
})
onBeforeUnmount(() => { disposed = true; generation++; request?.abort(); request = null })
</script>

<template>
  <div>
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div><h4 class="m-0 text-base">资源名单</h4><p class="text-subtle mb-0 mt-2 text-sm">默认全部开放。名单只限制资源，功能权限仍由角色和直接授权决定，拒绝规则优先。</p></div>
      <button type="button" class="btn-secondary shrink-0" :disabled="loading || saving || busy" @click="load">{{ dirty || conflicted ? '重新加载（放弃名单改动）' : '重新加载' }}</button>
    </div>
    <p v-if="readOnlyReason" class="text-subtle mt-3 text-sm" role="note">{{ readOnlyReason }}</p>
    <p v-if="error" class="semantic-error mt-4 p-3 text-sm" role="alert">{{ error }}</p>
    <p v-if="notice" class="semantic-success mt-4 p-3 text-sm" role="status">{{ notice }}</p>
    <p v-if="loading" class="text-subtle mt-5" role="status">正在读取资源权限…</p>
    <form v-else-if="initial" class="mt-5" @submit.prevent="save">
      <p class="text-subtle mb-4 text-xs">“当前有效”是已保存的实际权限；“名单范围”是待保存的选择，仍需满足功能权限和拒绝规则。入库范围同时参考媒体库访问名单。</p>
      <div class="grid gap-4 2xl:grid-cols-2">
        <section v-for="item in resourceAccessScopes" :key="item.scope" class="rounded-lg border border-[var(--border-subtle)] p-4" :aria-labelledby="`policy-title-${item.scope}`">
          <h5 :id="`policy-title-${item.scope}`" class="m-0 text-sm font-700">{{ item.title }}</h5>
          <p class="text-subtle mt-1 text-xs">{{ item.description }}</p>
          <label class="label" :for="`policy-mode-${item.scope}`">名单模式</label>
          <select :id="`policy-mode-${item.scope}`" class="input" :value="policyFor(item.scope).mode" :disabled="locked" @change="changeMode(item.scope, $event)">
            <option value="all">全部开放</option><option value="allowlist">白名单 · 仅允许选中项</option><option value="denylist">黑名单 · 禁止选中项</option>
          </select>
          <p v-if="policyFor(item.scope).mode === 'all'" class="text-subtle mt-2 text-xs">现在和以后新增的资源均不受此名单限制。</p>
          <p v-else-if="policyFor(item.scope).mode === 'allowlist'" class="mt-2 text-xs" :class="policyFor(item.scope).resource_ids.length ? 'text-subtle' : 'semantic-warning p-2'">{{ policyFor(item.scope).resource_ids.length ? `已选 ${policyFor(item.scope).resource_ids.length} 项；新增资源默认禁止。` : '空白名单会禁止此类全部资源，包括以后新增的资源。' }}</p>
          <p v-else class="text-subtle mt-2 text-xs">已选 {{ policyFor(item.scope).resource_ids.length }} 项禁止；{{ policyFor(item.scope).resource_ids.length ? '' : '空黑名单不限制任何资源，' }}新增资源默认允许。</p>
          <div class="text-subtle mb-3 flex flex-wrap gap-x-4 gap-y-1 text-xs"><span>当前有效：{{ optionsStale ? '需要重新读取' : options ? `${currentAllowedCount(item.scope)} 项允许` : '尚未读取' }}</span><span>名单范围：{{ options ? `${draftAllowedCount(item.scope)} 项` : '尚未读取' }}</span></div>
          <input v-if="allOptions(item.scope).length > 8" v-model="searches[item.scope]" class="input mb-2" type="search" :aria-label="`搜索${item.title}资源名称`" placeholder="按名称筛选资源" />
          <div v-if="allOptions(item.scope).length" class="max-h-60 space-y-2 overflow-y-auto">
            <label v-for="option in visibleOptions(item.scope)" :key="option.id" class="flex items-start gap-2 rounded border border-[var(--border-subtle)] p-2" :class="{ 'cursor-pointer': policyFor(item.scope).mode !== 'all' && !selectionLocked(item.scope, option) }">
              <input v-if="policyFor(item.scope).mode !== 'all'" type="checkbox" class="mt-1 shrink-0" :checked="policyFor(item.scope).resource_ids.includes(option.id)" :disabled="selectionLocked(item.scope, option)" :aria-label="`${policyFor(item.scope).mode === 'allowlist' ? '允许' : '禁止'}${option.name}`" @change="toggleResource(item.scope, option, $event)" />
              <span class="min-w-0 flex-1">
                <span class="flex flex-wrap items-center gap-2"><span class="break-words text-sm font-600">{{ option.name }}</span><span class="text-subtle text-xs">{{ resourceTypeLabel(option.type) }} · {{ resourceStatusLabel(option) }}</span></span>
                <span class="mt-1 flex flex-wrap items-center gap-2 text-xs"><span v-if="!optionsStale" :class="option.effective_allowed ? 'status-chip status-chip--ready' : 'status-chip'">当前有效：{{ option.effective_allowed ? '允许' : '禁止' }}</span><span v-if="dirty">待保存名单：{{ resourceDraftAllows(draft.policies, item.scope, option.id) ? '允许' : '禁止' }}</span></span>
                <span v-if="option.denial_reason && !optionsStale" class="text-subtle mt-1 block text-xs">{{ option.denial_reason }}</span>
                <span v-if="item.scope === 'library_ingest' && policyFor(item.scope).mode === 'allowlist' && policyFor(item.scope).resource_ids.includes(option.id) && !resourceDraftAllows(draft.policies, item.scope, option.id)" class="semantic-warning mt-1 block p-1 text-xs">还需在“媒体库访问”中允许此库。</span>
                <span v-if="editable && !option.can_grant && !option.deleted" class="text-subtle mt-1 block text-xs">超出你的可授予范围，可保留或收紧已有权限。</span>
              </span>
            </label>
            <p v-if="!visibleOptions(item.scope).length" class="text-subtle text-sm">没有匹配的资源名称。</p>
          </div>
          <p v-else class="text-subtle mb-0 text-sm">{{ options ? '目前没有可列出的资源，名单模式仍适用于以后新增的资源。' : '当前有效权限尚未读取，请重新加载。' }}</p>
        </section>
      </div>
      <div v-if="editable" class="mt-5 flex flex-wrap items-center gap-3"><button class="btn-primary" :disabled="locked || !dirty">{{ saving ? '正在保存…' : '保存资源名单' }}</button><span class="text-subtle text-sm">{{ dirty ? '名单有未保存改动' : '名单与已保存配置一致' }}</span></div>
    </form>
  </div>
</template>
