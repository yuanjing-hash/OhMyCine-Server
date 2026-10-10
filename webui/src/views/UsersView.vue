<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import SecretInput from '@/components/SecretInput.vue'
import UserResourceAccessPanel from '@/components/UserResourceAccessPanel.vue'
import { authorizationResourceOptions, resourceStatusLabel, resourceTypeLabel } from '@/resource-access'
import { useAuthStore } from '@/stores/auth'
import type { AuthorizationRule, ListResponse, PermissionDefinition, ResourceAccessOptions, RoleSummary, UserSummary } from '@/types/api'

const auth = useAuthStore()
const users = ref<UserSummary[]>([]); const roles = ref<RoleSummary[]>([]); const selectedID = ref<number | null>(null)
const loading = ref(true); const saving = ref(false); const error = ref(''); const notice = ref('')
const createOpen = ref(false); const createForm = ref({ username: '', displayName: '', password: '', roleIDs: [] as number[] })
const editDisplayName = ref(''); const editRoleIDs = ref<number[]>([]); const resetPassword = ref(''); const currentPassword = ref('')
const permissionCatalog = ref<PermissionDefinition[]>([]); const editRules = ref<AuthorizationRule[]>([])
const resourceOptions = ref<ResourceAccessOptions | null>(null); const resourceRefreshKey = ref(0)
const selected = computed(() => users.value.find(user => user.id === selectedID.value) ?? null)
const canEditAuthorization = computed(() => auth.can(Permissions.RolesAssign) && Boolean(selected.value) && !selected.value?.is_owner && selected.value?.id !== auth.user?.id)
const authorizationReadOnlyReason = computed(() => selected.value?.is_owner ? 'Owner 的授权受到保护，此处仅供查看。' : selected.value?.id === auth.user?.id ? '不能修改自己的授权，请由其他管理员调整。' : !auth.can(Permissions.RolesAssign) ? '当前账户仅可查看用户权限。' : '')

watch(selected, user => { editDisplayName.value = user?.display_name ?? ''; editRoleIDs.value = user?.roles.map(role => role.id) ?? []; editRules.value = (user?.authorization_rules ?? []).map(rule => ({ ...rule })); resetPassword.value = ''; currentPassword.value = '' }, { immediate: true })
watch(selectedID, () => { resourceOptions.value = null })

async function load() {
  loading.value = users.value.length === 0; error.value = ''
  try {
    const userData = await api<ListResponse<UserSummary>>('/api/v1/users')
    users.value = userData.list
    roles.value = auth.can(Permissions.RolesRead) ? (await api<ListResponse<RoleSummary>>('/api/v1/roles')).list : []
    permissionCatalog.value = auth.can(Permissions.RolesRead) ? await api<PermissionDefinition[]>('/api/v1/permissions') : []
    if (!selectedID.value && users.value.length) selectedID.value = users.value[0]?.id ?? null
  } catch (reason) { error.value = message(reason) } finally { loading.value = false }
}

async function createUser() {
  saving.value = true; error.value = ''
  try {
    await api('/api/v1/users', { method: 'POST', body: JSON.stringify({ username: createForm.value.username, display_name: createForm.value.displayName, password: createForm.value.password, role_ids: createForm.value.roleIDs }) })
    createForm.value = { username: '', displayName: '', password: '', roleIDs: [] }; createOpen.value = false; notice.value = '用户已创建'; await load()
  } catch (reason) { error.value = message(reason) } finally { saving.value = false }
}

async function saveProfile() { if (!selected.value) return; await run(async () => { await api(`/api/v1/users/${selected.value?.id}`, { method: 'PATCH', body: JSON.stringify({ display_name: editDisplayName.value }) }); notice.value = '用户资料已保存' }) }
async function saveRoles() { if (!selected.value || !canEditAuthorization.value) return; const userID = selected.value.id; await run(async () => { await api(`/api/v1/users/${userID}/roles`, { method: 'PUT', body: JSON.stringify({ role_ids: editRoleIDs.value }) }); notice.value = '角色分配已更新'; if (selectedID.value === userID) resourceRefreshKey.value++ }) }
function addAuthorizationRule() { const code = permissionCatalog.value[0]?.code; if (!code) return; editRules.value.push({ permission_code: code, effect: 'allow', resource_type: '', resource_id: '' }) }
function removeAuthorizationRule(index: number) { editRules.value.splice(index, 1) }
async function saveAuthorizationRules() { if (!selected.value || !canEditAuthorization.value) return; const userID = selected.value.id; await run(async () => { await api(`/api/v1/users/${userID}/authorization-rules`, { method: 'PUT', body: JSON.stringify({ rules: editRules.value.map(rule => ({ ...rule, resource_id: rule.resource_type ? rule.resource_id : '' })) }) }); notice.value = '用户直接授权已更新，拒绝规则优先于角色和允许规则'; if (selectedID.value === userID) resourceRefreshKey.value++ }) }
function onResourceOptions(userID: number, options: ResourceAccessOptions) { if (selectedID.value === userID) resourceOptions.value = options }
function ruleOptions(rule: AuthorizationRule) { return rule.resource_type ? authorizationResourceOptions(resourceOptions.value, rule.resource_type, rule.resource_id) : [] }
function changeRuleResourceType(rule: AuthorizationRule) { rule.resource_id = '' }
function canChooseRuleResource(rule: AuthorizationRule, id: string, deleted: boolean, status: string) { return id === rule.resource_id || (!deleted && !['restricted', 'unavailable'].includes(status)) }
async function toggleEnabled() { if (!selected.value) return; const action = selected.value.status === 'active' ? 'disable' : 'enable'; await run(async () => { await api(`/api/v1/users/${selected.value?.id}/${action}`, { method: 'POST', body: '{}' }); notice.value = action === 'disable' ? '用户已停用' : '用户已启用' }) }
async function resetUserPassword() { if (!selected.value || !resetPassword.value) return; await run(async () => { await api(`/api/v1/users/${selected.value?.id}/reset-password`, { method: 'POST', body: JSON.stringify({ password: resetPassword.value, current_password: currentPassword.value }) }); notice.value = '密码已重置，目标用户现有会话已撤销'; resetPassword.value = ''; currentPassword.value = '' }) }
async function deleteUser() { if (!selected.value || !window.confirm(`确认删除用户 ${selected.value.username}？此操作不可撤销。`)) return; const id = selected.value.id; await run(async () => { await api(`/api/v1/users/${id}`, { method: 'DELETE', body: '{}' }); selectedID.value = null; notice.value = '用户已删除' }) }

async function run(action: () => Promise<void>) { saving.value = true; error.value = ''; try { await action(); await load() } catch (reason) { error.value = message(reason) } finally { saving.value = false } }
function message(reason: unknown) { return reason instanceof Error ? reason.message : '操作失败' }
onMounted(load)
</script>

<template>
  <section>
    <div class="flex flex-wrap items-end justify-between gap-4"><div><h2 class="m-0 text-2xl font-800">账户</h2><p class="page-description mt-2">选择用户后，可单独调整角色、资源名单和直接授权。新用户默认全部资源开放，仍需具备相应功能权限。</p></div><button v-if="auth.can(Permissions.UsersCreate)" class="btn-primary" @click="createOpen = !createOpen">{{ createOpen ? '取消创建' : '创建用户' }}</button></div>
    <p v-if="error" class="semantic-error mt-5 p-3 text-sm" role="alert">{{ error }}</p><p v-if="notice" class="semantic-success mt-5 p-3 text-sm" role="status">{{ notice }}</p>
    <form v-if="createOpen" class="panel mt-6 grid gap-4 md:grid-cols-2 xl:grid-cols-4" @submit.prevent="createUser">
      <div><label class="label">用户名</label><input v-model="createForm.username" class="input" required minlength="3" maxlength="64" /></div>
      <div><label class="label">显示名称</label><input v-model="createForm.displayName" class="input" maxlength="128" /></div>
      <div><label class="label">初始密码</label><SecretInput v-model="createForm.password" class="input" required minlength="12" maxlength="128" autocomplete="new-password" /></div>
      <div><label class="label">角色</label><select v-if="auth.can(Permissions.RolesRead)" v-model="createForm.roleIDs" class="input min-h-11" multiple><option v-for="role in roles.filter(item => item.active)" :key="role.id" :value="role.id">{{ role.name }}</option></select><p v-else class="text-subtle m-0 text-sm">未授予角色查看权限时，新账户使用默认只读角色。</p></div>
      <button class="btn-primary md:col-span-2 xl:col-span-4" :disabled="saving">创建账户</button>
    </form>
    <div v-if="loading" class="text-subtle mt-8">正在加载用户…</div>
    <div v-else-if="users.length === 0" class="text-subtle panel mt-8">尚无用户。</div>
    <div v-else class="mt-7 grid gap-5 xl:grid-cols-[minmax(16rem,.55fr)_minmax(32rem,1.45fr)]">
      <div class="panel p-2"><button v-for="user in users" :key="user.id" class="semantic-list-item mb-1 w-full p-3 text-left" :class="{ 'semantic-list-item--selected': selectedID === user.id }" @click="selectedID = user.id"><div class="flex items-center justify-between gap-3"><strong>{{ user.display_name }}</strong><span class="status-chip" :class="user.status === 'active' ? 'status-chip--ready' : ''">{{ user.is_owner ? 'OWNER' : user.status }}</span></div><div class="text-subtle mt-1 text-xs">@{{ user.username }} · {{ user.roles.map(role => role.name).join('、') || '无角色' }}</div></button></div>
      <section v-if="selected" class="panel">
        <div class="flex items-start justify-between gap-4"><div><h2 class="m-0">{{ selected.display_name }}</h2><p class="text-subtle mt-1 text-sm">@{{ selected.username }} · 创建于 {{ new Date(selected.created_at).toLocaleString() }}</p></div><span v-if="selected.is_owner" class="status-chip status-chip--planned">实例 Owner</span></div>
        <div class="mt-6 grid gap-5 md:grid-cols-2">
          <form class="space-y-3" @submit.prevent="saveProfile"><div><label class="label">显示名称</label><input v-model="editDisplayName" class="input" :disabled="!auth.can(Permissions.UsersUpdate)" /></div><button v-if="auth.can(Permissions.UsersUpdate)" class="btn-secondary w-full" :disabled="saving">保存资料</button></form>
          <form v-if="auth.can(Permissions.RolesRead)" class="space-y-3" @submit.prevent="saveRoles"><div><label class="label">角色（功能权限并集）</label><select v-model="editRoleIDs" class="input min-h-28" multiple :disabled="!canEditAuthorization || saving"><option v-for="role in roles.filter(item => item.active)" :key="role.id" :value="role.id">{{ role.name }} · {{ role.code }}</option></select></div><button v-if="auth.can(Permissions.RolesAssign)" class="btn-secondary w-full" :disabled="saving || !canEditAuthorization">保存角色</button></form>
        </div>
        <section class="semantic-divider mt-6 border-t pt-5" aria-labelledby="user-permissions-title">
          <h3 id="user-permissions-title" class="mb-4 mt-0 text-lg">权限设置</h3>
          <UserResourceAccessPanel :key="selected.id" :user-id="selected.id" :editable="canEditAuthorization" :read-only-reason="authorizationReadOnlyReason" :refresh-key="resourceRefreshKey" :busy="saving" @options="onResourceOptions" />
        </section>
        <form class="semantic-divider mt-6 border-t pt-5" @submit.prevent="saveAuthorizationRules">
          <div class="flex flex-wrap items-start justify-between gap-3"><div><h3 class="m-0 text-sm">直接功能授权（高级）</h3><p class="text-subtle mt-1 text-xs">保留原有允许与拒绝规则。资源名单不授予功能权限，直接允许也不能绕过名单；拒绝始终优先。</p></div><button v-if="auth.can(Permissions.RolesAssign)" class="btn-secondary" type="button" :disabled="!canEditAuthorization || saving || !permissionCatalog.length" @click="addAuthorizationRule">添加规则</button></div>
          <div v-if="editRules.length" class="mt-4 space-y-3">
            <fieldset v-for="(rule, index) in editRules" :key="index" class="m-0 grid min-w-0 gap-2 rounded-lg border border-[var(--border-subtle)] p-3 md:grid-cols-2" :disabled="!canEditAuthorization || saving">
              <legend class="text-subtle px-1 text-xs">规则 {{ index + 1 }}</legend>
              <label><span class="label">功能权限</span><select v-model="rule.permission_code" class="input"><option v-if="!permissionCatalog.some(item => item.code === rule.permission_code)" :value="rule.permission_code">{{ rule.permission_code }}</option><option v-for="permission in permissionCatalog" :key="permission.code" :value="permission.code">{{ permission.name }} · {{ permission.code }}</option></select></label>
              <label><span class="label">授权效果</span><select v-model="rule.effect" class="input"><option value="allow">允许</option><option value="deny">拒绝</option></select></label>
              <label><span class="label">资源范围</span><select v-model="rule.resource_type" class="input" @change="changeRuleResourceType(rule)"><option value="">全局</option><option value="media_library">媒体库</option><option value="downloader">下载器</option><option value="site">站点</option></select></label>
              <label v-if="rule.resource_type"><span class="label">资源名称</span><select v-model="rule.resource_id" class="input" required :disabled="!resourceOptions"><option value="" disabled>{{ resourceOptions ? '请选择资源' : '正在读取资源名称…' }}</option><option v-for="option in ruleOptions(rule)" :key="option.id" :value="option.id" :disabled="!canChooseRuleResource(rule, option.id, option.deleted, option.status)">{{ option.name }} · {{ resourceTypeLabel(option.type) }} · {{ resourceStatusLabel(option) }}</option></select></label>
              <p v-else class="text-subtle mb-0 mt-7 text-xs">此规则适用于该功能的全部资源。</p>
              <button v-if="auth.can(Permissions.RolesAssign)" class="btn-danger justify-self-start" type="button" @click="removeAuthorizationRule(index)">移除规则</button>
            </fieldset>
          </div><p v-else class="text-subtle mt-4 text-sm">未配置直接规则，完全继承角色权限。</p>
          <button v-if="auth.can(Permissions.RolesAssign)" class="btn-secondary mt-4" :disabled="saving || !canEditAuthorization || editRules.some(rule => Boolean(rule.resource_type) && !rule.resource_id)">保存直接授权</button>
        </form>
        <form v-if="auth.can(Permissions.UsersUpdate) && (!selected.is_owner || selected.id === auth.user?.id)" class="semantic-divider mt-6 border-t pt-5" @submit.prevent="resetUserPassword"><h3 class="mt-0 text-sm">重置密码</h3><div class="grid gap-3 md:grid-cols-2"><SecretInput v-model="resetPassword" class="input" minlength="12" maxlength="128" required autocomplete="new-password" placeholder="至少 12 位新密码" /><SecretInput v-model="currentPassword" class="input" required autocomplete="current-password" placeholder="当前操作者密码（重新认证）" /></div><button class="btn-secondary mt-3" :disabled="saving">重置并撤销目标会话</button></form>
        <div class="semantic-divider mt-6 flex flex-wrap gap-3 border-t pt-5"><button v-if="selected.status === 'active' && auth.can(Permissions.UsersDisable)" class="btn-danger" :disabled="saving || selected.is_owner || selected.id === auth.user?.id" @click="toggleEnabled">停用账户</button><button v-else-if="selected.status === 'disabled' && auth.can(Permissions.UsersUpdate)" class="btn-secondary" :disabled="saving || selected.id === auth.user?.id" @click="toggleEnabled">启用账户</button><button v-if="auth.can(Permissions.UsersDelete)" class="btn-danger" :disabled="saving || selected.is_owner || selected.id === auth.user?.id" @click="deleteUser">删除账户</button></div>
      </section>
    </div>
  </section>
</template>
