<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '@/api/client'
import { Permissions } from '@/auth/generated-permissions'
import type { FollowSummary } from '@/follows'
import { useAuthStore } from '@/stores/auth'
import type { ConnectionSummary, ListResponse, MediaLibraryDetail, ScheduleAction, ScheduleDefinition, ScheduleRun } from '@/types/api'

const auth = useAuthStore(); const schedules = ref<ScheduleDefinition[]>([]); const actions = ref<ScheduleAction[]>([]); const selectedId = ref(''); const runs = ref<ScheduleRun[]>([])
const loading = ref(true); const saving = ref(false); const error = ref(''); const notice = ref(''); const preview = ref<string[]>([]); const editorOpen = ref(false)
const libraries = ref<MediaLibraryDetail[]>([]); const connections = ref<ConnectionSummary[]>([]); const follows = ref<FollowSummary[]>([])
const mode = ref<'minutes'|'hourly'|'daily'|'weekly'|'monthly'|'advanced'>('daily'); const interval = ref(30); const hour = ref(3); const minute = ref(0); const weekday = ref(1); const monthday = ref(1)
const cronModes = [{id:'minutes',label:'分钟'},{id:'hourly',label:'小时'},{id:'daily',label:'每日'},{id:'weekly',label:'每周'},{id:'monthly',label:'每月'},{id:'advanced',label:'高级 Cron'}] as const
interface ScheduleForm { id:string; revision:number; name:string; action_type:string; target_type:string; target_id:string; cron_expression:string; timezone:string; enabled:boolean; misfire_policy:'skip'|'run_once'; overlap_policy:'skip'|'queue'; max_retries:number; retry_delay_seconds:number; max_runtime_seconds:number }
const form = ref<ScheduleForm>(emptyForm()); const selected = computed(() => schedules.value.find(item => item.id === selectedId.value) ?? null); const selectedAction = computed(() => actions.value.find(item => item.code === form.value.action_type))
const targetOptions = computed(() => {
  if (form.value.target_type === 'media_library') return libraries.value.map(item => ({ id: String(item.id), label: item.name }))
  if (form.value.target_type === 'connection') return connections.value.filter(item => item.provider === 'pan115').map(item => ({ id: String(item.id), label: item.name }))
  if (form.value.target_type === 'follow') return follows.value.map(item => ({ id: item.id, label: item.title }))
  return []
})
function emptyForm():ScheduleForm { return { id:'', revision:0, name:'', action_type:'media_library_scan', target_type:'media_library', target_id:'', cron_expression:'0 3 * * *', timezone:Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai', enabled:true, misfire_policy:'run_once', overlap_policy:'skip', max_retries:1, retry_delay_seconds:60, max_runtime_seconds:3600 } }
function toForm(item:ScheduleDefinition):ScheduleForm { return { id:item.id,revision:item.revision,name:item.name,action_type:item.action_type,target_type:item.target_type,target_id:item.target_id,cron_expression:item.cron_expression,timezone:item.timezone,enabled:item.enabled,misfire_policy:item.misfire_policy,overlap_policy:item.overlap_policy,max_retries:item.max_retries,retry_delay_seconds:item.retry_delay_seconds,max_runtime_seconds:item.max_runtime_seconds } }
async function load() {
  loading.value = true; error.value = ''
  try {
    const [items, catalog, libraryList, connectionList, followList] = await Promise.all([
      api<ListResponse<ScheduleDefinition>>('/api/v1/schedules'),
      api<{list: ScheduleAction[]}>('/api/v1/schedules/actions'),
      auth.can(Permissions.MediaLibrariesRead) ? api<ListResponse<MediaLibraryDetail>>('/api/v1/media-libraries').catch(() => ({ list: [], total: 0 })) : Promise.resolve({ list: [], total: 0 }),
      auth.can(Permissions.ConnectionsRead) ? api<ListResponse<ConnectionSummary>>('/api/v1/connections').catch(() => ({ list: [], total: 0 })) : Promise.resolve({ list: [], total: 0 }),
      auth.can(Permissions.FollowsReadOwn) || auth.can(Permissions.FollowsReadAll) ? api<ListResponse<FollowSummary>>('/api/v1/follows?page=1&page_size=200').catch(() => ({ list: [], total: 0 })) : Promise.resolve({ list: [], total: 0 }),
    ])
    schedules.value = items.list; actions.value = catalog.list; libraries.value = libraryList.list; connections.value = connectionList.list; follows.value = followList.list
    if (!selectedId.value && items.list.length) selectedId.value = items.list[0]!.id
  } catch (reason) { error.value = message(reason) } finally { loading.value = false }
}
watch(selected, item=>{if(item){form.value=toForm(item);editorOpen.value=false;void loadRuns(item.id)}},{immediate:true})
watch(selectedAction, item=>{if(item){form.value.target_type=item.target_type;if(item.target_type==='system')form.value.target_id='system';else if(!targetOptions.value.some(option=>option.id===form.value.target_id))form.value.target_id=targetOptions.value[0]?.id??''}})
watch([mode,interval,hour,minute,weekday,monthday],()=>{if(mode.value==='advanced')return;const m=Math.max(0,Math.min(59,minute.value));const h=Math.max(0,Math.min(23,hour.value));if(mode.value==='minutes')form.value.cron_expression=`*/${Math.max(1,Math.min(59,interval.value))} * * * *`;if(mode.value==='hourly')form.value.cron_expression=`${m} */${Math.max(1,Math.min(23,interval.value))} * * *`;if(mode.value==='daily')form.value.cron_expression=`${m} ${h} * * *`;if(mode.value==='weekly')form.value.cron_expression=`${m} ${h} * * ${Math.max(0,Math.min(6,weekday.value))}`;if(mode.value==='monthly')form.value.cron_expression=`${m} ${h} ${Math.max(1,Math.min(31,monthday.value))} * *`})
async function loadRuns(id:string){try{runs.value=(await api<ListResponse<ScheduleRun>>(`/api/v1/schedules/${id}/runs?limit=20`)).list}catch{runs.value=[]}}
function createNew(){form.value=emptyForm();selectedId.value='';editorOpen.value=true;preview.value=[]}
function editSelected(){if(selected.value){form.value=toForm(selected.value);editorOpen.value=true}}
async function previewCron(){error.value='';try{preview.value=(await api<{list:string[]}>('/api/v1/schedules/preview',{method:'POST',body:JSON.stringify({cron_expression:form.value.cron_expression,timezone:form.value.timezone,count:5})})).list}catch(reason){error.value=message(reason)}}
async function save(){saving.value=true;error.value='';try{const payload={...form.value};const item=form.value.id?await api<ScheduleDefinition>(`/api/v1/schedules/${form.value.id}`,{method:'PUT',body:JSON.stringify(payload)}):await api<ScheduleDefinition>('/api/v1/schedules',{method:'POST',body:JSON.stringify(payload)});notice.value='计划任务已保存';selectedId.value=item.id;editorOpen.value=false;await load()}catch(reason){error.value=message(reason)}finally{saving.value=false}}
async function remove(){if(!selected.value||!confirm(`删除计划任务“${selected.value.name}”？`))return;saving.value=true;try{await api(`/api/v1/schedules/${selected.value.id}`,{method:'DELETE',body:'{}'});selectedId.value='';notice.value='计划任务已删除';await load()}catch(reason){error.value=message(reason)}finally{saving.value=false}}
function time(value?:string|null){return value?new Date(value).toLocaleString():'—'} function message(reason:unknown){return reason instanceof Error?reason.message:'操作失败'}
onMounted(load)
</script>

<template>
  <section><div class="flex flex-wrap items-end justify-between gap-4"><div><h2 class="m-0 text-2xl font-800">计划任务</h2><p class="page-description mt-2">所有可配置调度统一使用标准五段 Cron；可视化选择只负责生成表达式，执行仍进入持久化任务队列。</p></div><button v-if="auth.can(Permissions.SettingsUpdate)" class="btn-primary" @click="createNew">新建计划</button></div>
    <p v-if="error" class="semantic-error mt-5 p-3 text-sm">{{ error }}</p><p v-if="notice" class="semantic-success mt-5 p-3 text-sm">{{ notice }}</p>
    <div v-if="loading" class="text-subtle mt-8">正在加载计划任务…</div><div v-else class="mt-7 grid gap-5 xl:grid-cols-[22rem_minmax(0,1fr)]">
      <div class="panel p-2"><button v-for="item in schedules" :key="item.id" class="semantic-list-item mb-1 w-full p-3 text-left" :class="{'semantic-list-item--selected':selectedId===item.id}" @click="selectedId=item.id"><div class="flex justify-between gap-2"><strong>{{ item.name }}</strong><span class="status-chip" :class="item.enabled?'status-chip--ready':''">{{ item.enabled?'启用':'停用' }}</span></div><div class="text-subtle mt-1 text-xs font-mono">{{ item.cron_expression }} · {{ item.timezone }}</div><div class="text-subtle mt-1 text-xs">下次 {{ time(item.next_run_at) }} · {{ item.last_status }}</div></button><p v-if="!schedules.length" class="text-subtle p-4 text-sm">尚无计划任务。</p></div>
      <div>
        <form v-if="editorOpen" class="panel" @submit.prevent="save"><div class="flex justify-between"><h3 class="m-0">{{ form.id?'编辑计划':'新建计划' }}</h3><button type="button" class="btn-secondary" @click="editorOpen=false">取消</button></div><div class="mt-5 grid gap-4 md:grid-cols-2">
          <div><label class="label">名称</label><input v-model="form.name" class="input" required maxlength="128"></div><div><label class="label">动作</label><select v-model="form.action_type" class="input"><option v-for="item in actions" :key="item.code" :value="item.code">{{ item.name }}</option></select></div>
          <div v-if="form.target_type!=='system'"><label class="label">{{ form.target_type==='follow'?'订阅':form.target_type==='connection'?'115 连接':'媒体库' }}</label><select v-model="form.target_id" class="input" required><option value="" disabled>请选择目标</option><option v-for="option in targetOptions" :key="option.id" :value="option.id">{{ option.label }}</option></select><p v-if="!targetOptions.length" class="semantic-warning-text mt-1 text-xs">当前账号没有可用于此动作的目标。</p></div><div><label class="label">时区</label><input v-model="form.timezone" class="input" required placeholder="Asia/Shanghai"></div>
          <div class="md:col-span-2"><label class="label">可视化周期</label><div class="flex flex-wrap gap-2"><button v-for="item in cronModes" :key="item.id" type="button" class="btn-secondary" :class="{'ring-2 ring-[var(--accent)]':mode===item.id}" @click="mode=item.id">{{ item.label }}</button></div></div>
          <div v-if="mode==='minutes'||mode==='hourly'"><label class="label">每隔</label><input v-model.number="interval" class="input" type="number" min="1" :max="mode==='minutes'?59:23"></div><div v-if="['hourly','daily','weekly','monthly'].includes(mode)"><label class="label">分钟</label><input v-model.number="minute" class="input" type="number" min="0" max="59"></div><div v-if="['daily','weekly','monthly'].includes(mode)"><label class="label">小时</label><input v-model.number="hour" class="input" type="number" min="0" max="23"></div><div v-if="mode==='weekly'"><label class="label">星期（0=周日）</label><input v-model.number="weekday" class="input" type="number" min="0" max="6"></div><div v-if="mode==='monthly'"><label class="label">日期</label><input v-model.number="monthday" class="input" type="number" min="1" max="31"></div>
          <div class="md:col-span-2"><label class="label">五段 Cron</label><div class="flex gap-2"><input v-model="form.cron_expression" class="input font-mono" required @input="mode='advanced'"><button type="button" class="btn-secondary" @click="previewCron">预览</button></div><p class="text-subtle mt-2 text-xs">分 时 日 月 周，不接受秒或年份字段。</p></div>
          <div><label class="label">错过执行</label><select v-model="form.misfire_policy" class="input"><option value="run_once">恢复后补跑一次</option><option value="skip">跳过</option></select></div><div><label class="label">重叠策略</label><select v-model="form.overlap_policy" class="input"><option value="skip">跳过重叠</option><option value="queue">排队</option></select></div><div><label class="label">最大重试次数</label><input v-model.number="form.max_retries" class="input" type="number" min="0" max="10"></div><div><label class="label">重试间隔（秒）</label><input v-model.number="form.retry_delay_seconds" class="input" type="number" min="10" max="86400"></div><div><label class="label">最大运行秒数</label><input v-model.number="form.max_runtime_seconds" class="input" type="number" min="30" max="86400"></div><label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox">启用</label>
        </div><div v-if="preview.length" class="semantic-divider mt-4 border-t pt-4 text-sm"><strong>未来运行</strong><ol class="text-subtle mb-0 mt-2"><li v-for="item in preview" :key="item">{{ time(item) }}</li></ol></div><button class="btn-primary mt-5" :disabled="saving">保存计划</button></form>
        <section v-else-if="selected" class="panel"><div class="flex flex-wrap justify-between gap-3"><div><h3 class="m-0">{{ selected.name }}</h3><p class="text-subtle mt-1 font-mono text-sm">{{ selected.cron_expression }} · {{ selected.timezone }}</p></div><div class="flex gap-2"><button v-if="auth.can(Permissions.SettingsUpdate)" class="btn-secondary" @click="editSelected">编辑</button><button v-if="auth.can(Permissions.SettingsUpdate)" class="btn-danger" @click="remove">删除</button></div></div><div class="mt-5 grid gap-3 text-sm md:grid-cols-3"><div>动作<br><strong>{{ actions.find(item=>item.code===selected?.action_type)?.name||selected.action_type }}</strong></div><div>下次运行<br><strong>{{ time(selected.next_run_at) }}</strong></div><div>最近状态<br><strong>{{ selected.last_status }}</strong></div></div><h4 class="mt-7">最近运行</h4><div class="mt-3 space-y-2"><div v-for="run in runs" :key="run.id" class="semantic-list-item p-3 text-sm"><div class="flex justify-between"><span>{{ time(run.scheduled_at) }}</span><strong>{{ run.status }}</strong></div><div v-if="run.error_code" class="semantic-danger-text mt-1 text-xs">{{ run.error_code }}</div></div><p v-if="!runs.length" class="text-subtle text-sm">尚无运行记录。</p></div></section><div v-else class="panel text-subtle">选择一个计划，或新建计划任务。</div>
      </div>
    </div>
  </section>
</template>
