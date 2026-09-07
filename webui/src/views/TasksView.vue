<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import JobRepairDetails from '@/components/JobRepairDetails.vue';
import { useRoute, useRouter } from 'vue-router';
import { createLatestRequest } from '@/latest-request';
import { useJobLiveRefresh } from '@/use-job-live-refresh';
import { Permissions } from "@/auth/generated-permissions";
import { useAuthStore } from "@/stores/auth";
import { APIError } from "@/api/client";
import {
  controlJob,
  getAttempts,
  getJob,
  getTimeline,
  listJobs,
  reorderLane,
  respondAction,
  statusLabels,
  unknown,
  type Job,
  type JobAttempt,
  type JobEvent,
  type JobStatus,
} from "@/jobs";
const auth = useAuthStore(),
  jobs = ref<Job[]>([]),
  total = ref(0),
  page = ref(1),
  pageSize = ref(50),
  loading = ref(true),
  error = ref(""),
  notice = ref(""),
  status = ref(""),
  jobType = ref(""),
  priority = ref(""),
  provider = ref(""),
  laneOnly = ref(false),
  selected = ref<Job | null>(null),
  attempts = ref<JobAttempt[]>([]),
  timeline = ref<JobEvent[]>([]),
  dragged = ref<number | null>(null);
let drawerTrigger: HTMLElement | null = null;
const attemptPage = ref(1), attemptTotal = ref(0), timelinePage = ref(1), timelineTotal = ref(0);
const route = useRoute(), router = useRouter();
const listRequest = createLatestRequest(), detailRequest = createLatestRequest();
const detailID = ref(''), detailError = ref(''), detailLoading = ref(false), ordering = ref(false);
let alive = true;
const canControl = computed(() =>
    auth.canAny([Permissions.JobsControlOwn, Permissions.JobsControlAll]),
  ),
  canRespond = computed(() => auth.can(Permissions.JobsRespond)),
  canReorder = computed(() => auth.can(Permissions.JobsReorder)),
  counts = computed(() =>
    jobs.value.reduce<Record<string, number>>((r, j) => {
      r[j.status] = (r[j.status] ?? 0) + 1;
      return r;
    }, {}),
  ),
  laneReady = computed(
    () =>
      laneOnly.value &&
      status.value === "queued" &&
      jobType.value !== "" &&
      priority.value !== "" &&
      jobs.value.length > 1,
  );
const completeLane = computed(() => !ordering.value && !loading.value && laneReady.value && total.value === jobs.value.length);
async function load(quiet = false) {
  const request = listRequest.begin();
  if (!quiet) loading.value = true;
  error.value = "";
  try {
    const q = new URLSearchParams({ page: String(page.value), page_size: String(laneOnly.value ? 200 : pageSize.value) });
    if (status.value) q.set("status", status.value);
    if (jobType.value) q.set("job_type", jobType.value);
    if (priority.value) q.set("priority", priority.value);
    if (provider.value) q.set("provider", provider.value);
    const r = await listJobs(q, request.signal);
    if (!request.isCurrent()) return;
    jobs.value = r.list;
    total.value = r.total;
  } catch (c) {
    if (!request.isCurrent()) return;
    if (c instanceof APIError && [401, 403].includes(c.status)) jobs.value = [];
    error.value = c instanceof Error ? c.message : "任务加载失败";
  } finally {
    if (request.isCurrent()) loading.value = false;
    request.finish();
  }
}
async function loadDetail(id: string, quiet = false) {
  const request = detailRequest.begin();
  detailError.value = '';
  if (!quiet) { selected.value = null; attempts.value = []; timeline.value = []; detailLoading.value = true; }
  try {
    const [detail, a, t] = await Promise.all([getJob(id, request.signal), getAttempts(id, request.signal, attemptPage.value), getTimeline(id, request.signal, timelinePage.value)]);
    if (!request.isCurrent()) return;
    selected.value = detail;
    attempts.value = a.list;
    timeline.value = t.list;
    attemptTotal.value = a.total ?? a.list.length;
    timelineTotal.value = t.total ?? t.list.length;
  } catch (reason) {
    if (!request.isCurrent()) return;
    detailError.value = reason instanceof Error ? reason.message : '任务详情读取失败';
    if (reason instanceof APIError && [401, 403, 404].includes(reason.status)) { selected.value = null; attempts.value = []; timeline.value = []; }
  } finally {
    if (request.isCurrent()) detailLoading.value = false;
    request.finish();
    if (!quiet) { await nextTick(); if (request.isCurrent()) document.querySelector<HTMLElement>('.task-drawer .icon-button')?.focus(); }
  }
}
function open(job: Job) {
  attemptPage.value = 1; timelinePage.value = 1;
  drawerTrigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  void router.replace({ query: { ...route.query, job_id: job.id } });
}
function closeDrawer() {
  detailRequest.cancel(); detailID.value = ''; selected.value = null;
  const query = { ...route.query }; delete query.job_id;
  void router.replace({ query });
  void nextTick(() => { if (alive) drawerTrigger?.focus() });
}
function handleEscape(event: KeyboardEvent) {
  if (!detailID.value) return;
  if (event.key === 'Escape') { closeDrawer(); return; }
  if (event.key !== 'Tab') return;
  const drawer = document.querySelector<HTMLElement>('.task-drawer');
  const controls = Array.from(drawer?.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex="0"]') ?? []);
  const first = controls[0], last = controls[controls.length - 1];
  if (!first || !last) return;
  if (!drawer?.contains(document.activeElement) || (!event.shiftKey && document.activeElement === last)) { event.preventDefault(); first.focus(); }
  else if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
}
function queryText(value: unknown, pattern: RegExp) { return typeof value === 'string' && pattern.test(value) ? value : '' }
function applyFilters() { page.value = 1; changePage(1) }
function changePage(value: number) {
  const query: Record<string, string> = { page: String(value) };
  if (status.value) query.status = status.value;
  if (jobType.value) query.job_type = jobType.value;
  if (priority.value) query.priority = priority.value;
  if (provider.value) query.provider = provider.value;
  if (laneOnly.value) query.lane = '1';
  if (detailID.value) query.job_id = detailID.value;
  void router.replace({ query });
}
let previousListKey = '';
watch(() => route.fullPath, () => {
  if (route.path !== '/automation/tasks') return;
  status.value = Object.hasOwn(statusLabels, String(route.query.status)) ? String(route.query.status) : '';
  jobType.value = queryText(route.query.job_type, /^[a-zA-Z0-9_-]{1,64}$/);
  priority.value = queryText(route.query.priority, /^-?\d{1,6}$/);
  provider.value = queryText(route.query.provider, /^[a-zA-Z0-9_-]{1,64}$/);
  laneOnly.value = route.query.lane === '1';
  page.value = Math.max(1, Math.min(100000, Number(queryText(route.query.page, /^\d{1,6}$/)) || 1));
  if (laneOnly.value) { status.value = 'queued'; page.value = 1; }
  const key = JSON.stringify([status.value, jobType.value, priority.value, provider.value, laneOnly.value, page.value]);
  if (key !== previousListKey) { previousListKey = key; void load() }
  const id = queryText(route.query.job_id, /^[a-zA-Z0-9_-]{1,128}$/);
  if (id !== detailID.value) {
    detailRequest.cancel(); detailID.value = id; selected.value = null; attemptPage.value = 1; timelinePage.value = 1;
    if (id) void loadDetail(id);
  }
}, { immediate: true });
async function refresh() {
  if (!alive) return;
  await Promise.all([load(true), ...(detailID.value ? [loadDetail(detailID.value, true)] : [])]);
}
useJobLiveRefresh(async () => {
  await Promise.all([...(listRequest.pending ? [] : [load(true)]), ...(detailID.value && !detailRequest.pending ? [loadDetail(detailID.value, true)] : [])]);
});
async function action(job: Job, name: "pause" | "resume" | "cancel" | "retry") {
  if (
    (name === "cancel" || name === "retry") &&
    !window.confirm(
      `确认${name === "cancel" ? "取消" : "重试"}“${job.display_name}”？取消不会删除真实文件。`,
    )
  )
    return;
  try {
    await controlJob(job.id, name);
    await refresh();
  } catch (c) {
    if (alive) error.value = c instanceof Error ? c.message : "操作失败";
  }
}
async function respond(job: Job, response: string) {
  try {
    await respondAction(job, response);
    await refresh();
    if (alive && detailID.value === job.id) closeDrawer();
  } catch (c) {
    if (alive) error.value = c instanceof Error ? c.message : "响应失败";
  }
}
async function move(index: number, offset: number) {
  if (!completeLane.value) return;
  const next = index + offset;
  if (next < 0 || next >= jobs.value.length) return;
  const ordered = [...jobs.value];
  [ordered[index], ordered[next]] = [ordered[next], ordered[index]];
  ordering.value = true;
  try {
    await reorderLane(ordered[0].job_type, ordered[0].priority, ordered);
    if (!alive) return;
    await refresh();
    notice.value = "队列顺序已保存";
  } catch (c) {
    if (!alive) return;
    notice.value =
      c instanceof APIError && c.errorCode === "queue_order_conflict"
        ? "队列已变化，已刷新真实顺序"
        : "";
    error.value = notice.value
      ? ""
      : c instanceof Error
        ? c.message
        : "排序失败";
    if (alive) await refresh();
  } finally {
    if (alive) ordering.value = false;
  }
}
async function drop(index: number) {
  if (!completeLane.value || dragged.value === null || dragged.value === index) return;
  const ordered = [...jobs.value],
    item = ordered.splice(dragged.value, 1)[0];
  ordered.splice(index, 0, item);
  dragged.value = null;
  ordering.value = true;
  try {
    await reorderLane(ordered[0].job_type, ordered[0].priority, ordered);
    if (alive) await refresh();
  } catch {
    if (!alive) return;
    notice.value = "队列已变化，已刷新真实顺序";
    if (alive) await refresh();
  } finally {
    if (alive) ordering.value = false;
  }
}
onMounted(()=>window.addEventListener("keydown",handleEscape));
onBeforeUnmount(() => { alive = false; listRequest.cancel(); detailRequest.cancel(); window.removeEventListener("keydown",handleEscape); });
</script>
<template>
  <section class="mx-auto max-w-[96rem]">
    <header class="flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 class="m-0 text-3xl font-800">任务中心</h1>
        <p class="page-description mt-2">
          持久化任务的全局观察与控制面；媒体库 watcher 和 reconciliation
          不进入此队列。
        </p>
      </div>
      <button class="btn-secondary" @click="refresh">刷新</button>
    </header>
    <div class="task-summary mt-6">
      <article
        v-for="item in [
          'queued',
          'running',
          'waiting_user_action',
          'retry_wait',
          'failed',
        ]"
        :key="item"
        class="panel"
      >
        <small>本页 · {{ statusLabels[item as JobStatus] }}</small><strong>{{ counts[item] ?? 0 }}</strong>
      </article>
    </div>
    <div class="panel mt-4 task-filters">
      <label><span class="label">状态</span><select v-model="status" class="input" @change="applyFilters">
        <option value="">全部</option>
        <option v-for="(label, key) in statusLabels" :key="key" :value="key">
          {{ label }}
        </option>
      </select></label><label><span class="label">任务类型</span><input
        v-model.trim="jobType"
        class="input"
        placeholder="download / transfer"
        @change="applyFilters"
      /></label><label><span class="label">优先级</span><input
        v-model.trim="priority"
        class="input"
        inputmode="numeric"
        placeholder="10"
        @change="applyFilters"
      /></label><label><span class="label">Provider</span><input
        v-model.trim="provider"
        class="input"
        placeholder="provider"
        @change="applyFilters"
      /></label><label class="task-checkbox"><input
        v-model="laneOnly"
        type="checkbox"
        @change="
          status = laneOnly ? 'queued' : status;
          applyFilters();
        "
      />
        单 lane 排序</label>
    </div>
    <p v-if="notice" class="semantic-warning mt-4 p-3" role="status">
      {{ notice }}
    </p>
    <p v-if="error" class="semantic-error mt-4 p-3" role="alert">{{ error }}</p>
    <div class="panel mt-4 overflow-x-auto p-0">
      <p v-if="laneOnly && !laneReady" class="semantic-warning m-3 p-3">
        排序模式需要排队状态、任务类型和优先级三个精确筛选。
      </p>
      <p v-else-if="laneReady && !completeLane" class="semantic-warning m-3 p-3">
        当前 lane 超过 200 条，无法加载完整顺序，因此已禁用排序。
      </p>
      <p v-if="loading" class="p-6 text-muted">正在读取持久化队列…</p>
      <p v-else-if="!jobs.length" class="p-6 text-muted">
        {{ error ? '任务读取失败，请重试。' : '查询成功，当前筛选范围内没有任务。' }}
      </p>
      <table v-else class="semantic-table task-table w-full">
        <thead>
          <tr>
            <th>顺序</th>
            <th>任务</th>
            <th>状态</th>
            <th>进度 / 处理量</th>
            <th>速度 / ETA</th>
            <th>错误 / 重试</th>
            <th>更新</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="(job, index) in jobs"
            :key="job.id"
            :class="{
              'task-action-required': job.status === 'waiting_user_action',
            }"
            :draggable="canReorder && completeLane"
            @dragstart="dragged = index"
            @dragover.prevent
            @drop="drop(index)"
          >
            <td>
              #{{ job.lane_rank ?? "—" }}
              <span v-if="canReorder && completeLane" aria-hidden="true">⠿</span>
              <div v-if="canReorder && completeLane" class="flex gap-1">
                <button
                  class="task-order"
                  :disabled="index === 0"
                  :aria-label="`上移 ${job.display_name}`"
                  @click="move(index, -1)"
                >
                  ↑</button><button
                  class="task-order"
                  :disabled="index === jobs.length - 1"
                  :aria-label="`下移 ${job.display_name}`"
                  @click="move(index, 1)"
                >
                  ↓
                </button>
              </div>
            </td>
            <td>
              <button class="semantic-link text-left" @click="open(job)">
                <strong>{{ job.display_name }}</strong><small class="block text-subtle">{{ job.job_type }} · P{{ job.priority }} ·
                  {{ job.provider || job.resource_key || "无资源标签" }}</small>
              </button>
            </td>
            <td>
              <span
                class="status-chip"
                :class="{
                  'status-chip--warning': [
                    'waiting_user_action',
                    'retry_wait',
                  ].includes(job.status),
                  'status-chip--error': job.status === 'failed',
                  'status-chip--ready': job.status === 'completed',
                }"
              >{{ statusLabels[job.status] }}</span><small
                v-if="job.interrupt_pending"
                class="block mt-1 semantic-warning-text"
              >控制请求处理中</small>
              <small v-if="job.wait_reason && ['queued', 'retry_wait'].includes(job.status)" class="block mt-1 semantic-warning-text">等待原因：{{ job.wait_reason.message }}</small>
            </td>
            <td>
              {{ unknown(job.progress, "%")
              }}<small class="block text-subtle">{{ unknown(job.processed_items) }} /
                {{ unknown(job.total_items) }}</small>
            </td>
            <td>
              {{ unknown(job.speed, "/s")
              }}<small class="block text-subtle">ETA {{ unknown(job.eta_seconds, "s") }}</small>
            </td>
            <td>
              {{ job.last_error_message || "—"
              }}<small class="block text-subtle">{{
                job.next_attempt_at
                  ? new Date(job.next_attempt_at).toLocaleString()
                  : "—"
              }}</small>
            </td>
            <td>{{ new Date(job.updated_at).toLocaleString() }}</td>
            <td>
              <div v-if="canControl" class="flex flex-wrap gap-1">
                <button
                  v-if="
                    ['queued', 'running', 'retry_wait'].includes(job.status)
                  "
                  class="btn-secondary"
                  @click="action(job, 'pause')"
                >
                  暂停</button><button
                  v-if="job.status === 'paused'"
                  class="btn-secondary"
                  @click="action(job, 'resume')"
                >
                  恢复</button><button
                  v-if="job.status === 'failed'"
                  class="btn-secondary"
                  @click="action(job, 'retry')"
                >
                  重试</button><button
                  v-if="!['completed', 'cancelled'].includes(job.status)"
                  class="btn-danger"
                  @click="action(job, 'cancel')"
                >
                  取消
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      <footer
        v-if="total || page > 1"
        class="border-t border-[var(--border)] p-3 text-sm text-muted"
      >
        显示 {{ jobs.length }} / {{ total }} 条
        <span v-if="!laneOnly" class="ml-4 inline-flex items-center gap-2">
          <button class="btn-secondary" :disabled="page === 1" @click="changePage(page - 1)">上一页</button>
          <span>第 {{ page }} 页</span>
          <button class="btn-secondary" :disabled="page * pageSize >= total" @click="changePage(page + 1)">下一页</button>
        </span>
      </footer>
    </div>
    <div
      v-if="detailID"
      class="task-drawer-backdrop"
      @click.self="closeDrawer()"
    >
      <aside
        class="task-drawer"
        role="dialog"
        aria-modal="true"
        :aria-label="`${selected?.display_name || '任务'} 详情`"
      >
        <header>
          <div>
            <small>{{ selected?.job_type }}</small>
            <h2>{{ selected?.display_name || '任务详情' }}</h2>
            <span v-if="selected" class="status-chip">{{ statusLabels[selected.status] }}</span>
          </div>
          <button
            class="icon-button"
            aria-label="关闭详情"
            @click="closeDrawer()"
          >
            ×
          </button>
        </header>
        <p v-if="detailLoading" class="p-4" role="status">正在读取任务详情…</p>
        <p v-if="selected?.wait_reason && ['queued', 'retry_wait'].includes(selected.status)" class="semantic-warning m-4 p-4" role="status">等待原因：{{ selected.wait_reason.message }}</p>
        <p v-if="detailError" class="semantic-error m-4 p-4" role="alert">{{ detailError }} <button class="btn-secondary" @click="loadDetail(detailID)">重试</button></p>
        <section
          v-if="selected?.action_request"
          class="semantic-warning m-4 p-4"
        >
          <strong>{{ selected.action_request.prompt }}</strong>
          <div class="mt-3 flex flex-wrap gap-2">
            <button
              v-for="option in selected.action_request.options"
              :key="option"
              class="btn-primary"
              :disabled="!canRespond"
              @click="respond(selected, option)"
            >
              {{ option }}
            </button>
          </div>
        </section>
        <section class="p-4">
          <JobRepairDetails v-if="selected?.job_type === 'media_library_repair'" :job-id="selected.id" :revision="selected.revision" :active="selected.status === 'running'" />
          <h3>状态时间线</h3>
          <ol class="task-timeline">
            <li v-for="event in timeline" :key="event.id">
              <strong>任务状态更新</strong><span>{{ statusLabels[event.from_status as JobStatus] || "开始" }} →
                {{ statusLabels[event.to_status as JobStatus] || "—" }}</span><time>{{ new Date(event.created_at).toLocaleString() }}</time>
            </li>
          </ol>
          <div v-if="selected" class="flex items-center gap-3"><button class="btn-secondary" :disabled="detailLoading || timelinePage <= 1" @click="timelinePage--; loadDetail(detailID, true)">上一页时间线</button><span>第 {{ timelinePage }} 页</span><button class="btn-secondary" :disabled="detailLoading || timelinePage * 50 >= timelineTotal" @click="timelinePage++; loadDetail(detailID, true)">下一页时间线</button></div>
          <h3 class="mt-6">执行尝试</h3>
          <ol class="task-timeline">
            <li v-for="attempt in attempts" :key="attempt.id">
              <strong>第 {{ attempt.attempt_number }} 次 ·
                {{ statusLabels[attempt.status as JobStatus] || '执行结束' }}</strong><span>{{ attempt.error_message || "无错误" }}</span><time>{{ new Date(attempt.started_at).toLocaleString() }}</time>
            </li>
          </ol>
          <div v-if="selected" class="flex items-center gap-3"><button class="btn-secondary" :disabled="detailLoading || attemptPage <= 1" @click="attemptPage--; loadDetail(detailID, true)">上一页尝试</button><span>第 {{ attemptPage }} 页</span><button class="btn-secondary" :disabled="detailLoading || attemptPage * 50 >= attemptTotal" @click="attemptPage++; loadDetail(detailID, true)">下一页尝试</button></div>
        </section>
      </aside>
    </div>
  </section>
</template>

