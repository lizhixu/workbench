<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton, NDataTable, NEmpty, NIcon, NInput, NModal,
  NSpace, NSpin, NTag, NCheckbox, NAlert, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, MoveOutline, RocketOutline, GitCommitOutline, RefreshOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import {
  deleteApp, deployApp, getAppStatus, listApps, restartApp, startApp, stopApp,
  type AppEntity, type AppStatus,
} from '../../api/apps'
import { listHosts } from '../../api/hosts'
import { useWorkspaceStore } from '../../stores/workspace'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTimeMinute } from '../../utils/time'
import AppMigrateModal from '../../components/apps/AppMigrateModal.vue'

defineOptions({ name: 'AppList' })

const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()
const auth = useAuthStore()

const apps = ref<AppEntity[]>([])
const loading = ref(true)
const searchKeyword = ref('')
const statuses = ref<Record<string, AppStatus>>({})
// 状态首轮拉取未完成前，状态格不得渲染「未知」等确定性文案（见 stateTag）。
const statusesLoading = ref(false)
// host_id → hostname. /apps does not join the host record; the list maps it
// client-side from the host inventory (same pattern as AppDetail).
const hostnameById = ref<Map<string, string>>(new Map())
let statusTimer: ReturnType<typeof setInterval> | null = null

const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')

// ---- 批量操作：多选 + 前端并发编排（3 个同时），零后端改动，复用单机接口
// （与 NetworkList.vue 同款范式：勾选按 id 跨页保留、状态列逐台显示进度、
// 失败可重试、完成后给成功/失败汇总；审计走既有单机接口，无需新埋点）。
// NDataTable 的 checked-row-keys 由本页全量数据驱动，翻页不丢勾选。
const checkedRowKeys = ref<string[]>([])
const BATCH_CONCURRENCY = 3
type BatchOpPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchOpPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchKind = ref<'start' | 'stop' | 'restart' | 'deploy' | 'delete' | null>(null)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)

const selectedApps = computed(() => apps.value.filter((a) => checkedRowKeys.value.includes(a.id)))
// 状态按需拉取、可能尚未就绪：状态未知时先放行、由服务端裁决，失败逐台记录；
// 部署中（deploying）的一律排除，避免与在途部署冲突。
const appState = (id: string) => statuses.value[id]
const batchStartable = computed(() =>
  selectedApps.value.filter((a) => {
    const st = appState(a.id)
    if (st?.deploying) return false
    return !st || st.state === 'exited' || st.state === 'dead'
  }),
)
const batchStoppable = computed(() =>
  selectedApps.value.filter((a) => {
    const st = appState(a.id)
    if (st?.deploying) return false
    return !st || st.state === 'running'
  }),
)
const batchRestartable = computed(() => batchStoppable.value)
const batchDeployable = computed(() =>
  selectedApps.value.filter((a) => !appState(a.id)?.deploying),
)

function clearSelection() {
  checkedRowKeys.value = []
  opStatus.value = {}
  batchSummary.value = null
  batchKind.value = null
}

// NDataTable 的勾选回调给 string | number 混型 key，本页 row-key 全是 string。
function onCheckedRowKeysChange(keys: (string | number)[]) {
  checkedRowKeys.value = keys.map(String)
}

function isOpBusy(id: string) {
  const st = opStatus.value[id]
  return !!st && (st.phase === 'queued' || st.phase === 'running')
}

// 批量执行中的行：操作列整体置灰禁用，防单机操作与批量并发冲突
function rowClassName(row: AppEntity) {
  return isOpBusy(row.id) ? 'batch-row-busy' : ''
}

// 逐台并发执行器：启停/重启/重新部署/删除共用。每台独立链路，失败一台不影响其他。
async function runBatchPerApp(
  kind: 'start' | 'stop' | 'restart' | 'deploy' | 'delete',
  targets: AppEntity[],
  fn: (app: AppEntity) => Promise<unknown>,
  // skipped 由调用方按「已选 − 本轮目标」传入；重试只重跑失败应用、传 0，
  // 不把上一轮已成功的计为跳过。
  skipped = 0,
) {
  if (targets.length === 0) {
    message.warning('所选应用中没有可执行的目标')
    return
  }
  batchRunning.value = true
  batchKind.value = kind
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((a) => {
    opStatus.value[a.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...targets]
  async function worker() {
    while (queue.length > 0) {
      const app = queue.shift()!
      opStatus.value[app.id] = { phase: 'running' }
      try {
        await fn(app)
        opStatus.value[app.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[app.id] = { phase: 'failed', message: e.message || '执行出错' }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, queue.length) }, () => worker()),
  )
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped }
  const verb = { start: '启动', stop: '停止', restart: '重启', deploy: '重新部署', delete: '删除' }[kind]
  const skipText = skipped > 0 ? `，跳过 ${skipped} 个` : ''
  if (failed === 0) {
    message.success(`批量${verb}完成：成功 ${done} 个${skipText}`)
  } else {
    message.warning(`批量${verb}完成：成功 ${done} 个，失败 ${failed} 个${skipText}，可点「重试失败」只重跑失败的应用`)
  }
  // 刷新列表与状态；进度与汇总保留在行内/批量栏，手动刷新时再清空。
  await loadData()
  await refreshStatuses()
}

function retryBatchFailed() {
  if (!batchKind.value || !batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  const targets = apps.value.filter((a) => failedIds.includes(a.id))
  if (targets.length === 0) return
  const kind = batchKind.value
  if (kind === 'start') runBatchPerApp('start', targets, (a) => startApp(a.id))
  else if (kind === 'stop') runBatchPerApp('stop', targets, (a) => stopApp(a.id))
  else if (kind === 'restart') runBatchPerApp('restart', targets, (a) => restartApp(a.id))
  else if (kind === 'deploy') runBatchPerApp('deploy', targets, (a) => deployApp(a.id))
  else if (kind === 'delete') runBatchPerApp('delete', targets, (a) => deleteApp(a.id, lastBatchPurge.value))
}

// ---- 批量删除：危险操作，弹窗内就地二次确认（DESIGN.md §9.17），
// 复用单机删除接口与「同时删除应用内容」选项。
const showBatchDeleteModal = ref(false)
const batchPurgeContent = ref(false)
const lastBatchPurge = ref(false)

function openBatchDeleteModal() {
  batchPurgeContent.value = false
  showBatchDeleteModal.value = true
}

async function confirmBatchDelete() {
  lastBatchPurge.value = batchPurgeContent.value
  showBatchDeleteModal.value = false
  await runBatchPerApp('delete', selectedApps.value, (a) => deleteApp(a.id, lastBatchPurge.value))
}

const hostLabel = (row: AppEntity) => hostnameById.value.get(row.host_id) || row.host_id || '-'

// Source cell: the old "repo_url @ branch" column rendered " @ main" for every
// non-Git app. Show what each source actually is.
function sourceLabel(row: AppEntity): string {
  switch (row.source_type) {
    case 'git':
      return `${row.repo_url ?? ''}${row.auto_deploy ? ' · 自动部署' : ''}`
    case 'image':
      return row.image || '镜像'
    case 'template':
      return `模板 ${row.template_id ?? ''}`.trim()
    case 'raw_compose':
      return 'Docker Compose'
    default:
      return row.source_type || '-'
  }
}

// 当前版本的真实数据字段按来源区分（后端只持久化这些，见 server
// internal/apps/types.go: CurrentCommit 与 deploy.go: finishDeploySuccess）：
//   git            → current_commit（部署时取 rev-parse --short HEAD 的短哈希）
//   image/template → image 字段本身就是已部署的镜像引用，版本号是它的 tag
//   raw_compose    → 没有任何版本字段（CurrentCommit 只在 git 路径赋值），
//                    不编造版本号，显示占位。
function imageTag(image?: string): string | null {
  if (!image) return null
  const slash = image.lastIndexOf('/')
  const colon = image.lastIndexOf(':')
  // registry 端口里的冒号在最后一个 / 之前，不算 tag 分隔符。
  if (colon > slash) return image.slice(colon + 1) || null
  return image
}

function versionCell(row: AppEntity) {
  if (row.source_type === 'git') {
    return row.current_commit
      ? h(NTag, { size: 'small', bordered: false, type: 'info' }, { icon: () => h(NIcon, null, { default: () => h(GitCommitOutline) }), default: () => row.current_commit })
      : h('span', { style: 'color: var(--text-secondary)' }, '-')
  }
  if (row.source_type === 'image' || row.source_type === 'template') {
    const tag = imageTag(row.image)
    return tag
      ? h('span', { title: row.image }, tag)
      : h('span', { style: 'color: var(--text-secondary)' }, '-')
  }
  return h('span', { style: 'color: var(--text-secondary)' }, '-')
}

const filteredApps = computed(() => {
  const q = searchKeyword.value.trim().toLowerCase()
  if (!q) return apps.value
  return apps.value.filter((a) =>
    a.name.toLowerCase().includes(q) ||
    hostLabel(a).toLowerCase().includes(q) ||
    (a.repo_url ?? '').toLowerCase().includes(q) ||
    (a.branch ?? '').toLowerCase().includes(q) ||
    (a.image ?? '').toLowerCase().includes(q) ||
    (a.current_commit ?? '').toLowerCase().includes(q),
  )
})

const rowCount = computed(() => filteredApps.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, pageSizes: [10, 20, 50], rowCount })
// 筛选条件收紧可能让当前页越界：回第一页（越界夹回另由 useTablePagination 兜底）。
watch(searchKeyword, () => resetPage())

const stateTag = (a: AppEntity) => {
  // 批量进度：执行期间状态格逐台显示当前阶段，失败时显示原因（悬停看全文）
  const op = opStatus.value[a.id]
  if (op) {
    const text = op.phase === 'queued'
      ? '排队中…'
      : op.phase === 'running'
        ? '执行中…'
        : op.phase === 'done'
          ? '已完成'
          : `失败：${op.message || '执行出错'}`
    const color = op.phase === 'done' ? '#059669' : op.phase === 'failed' ? '#dc2626' : '#3b82f6'
    return h('span', {
      style: `font-size: 12px; color: ${color}; max-width: 100%; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; display: inline-block; vertical-align: middle;`,
      title: op.message || '',
    }, text)
  }
  const st = statuses.value[a.id]
  if (!st) {
    // 加载期内不渲染确定性文案：状态尚未取回时「未知」是误导（走查：
    // 初载先闪「未知」再变「运行中」），先给加载态，取回后再显示真实状态。
    if (loading.value || statusesLoading.value) {
      return h(NSpin, { size: 'small' })
    }
    return h(NTag, { size: 'small', bordered: false }, { default: () => '未知' })
  }
  if (st.deploying) {
    return h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => '部署中' })
  }
  if (!st.agent_online) {
    return h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => '主机离线' })
  }
  switch (st.state) {
    case 'running':
      return h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '运行中' })
    case 'undeployed':
      return h(NTag, { size: 'small', bordered: false }, { default: () => '未部署' })
    case 'exited':
    case 'dead':
      return h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => '已停止' })
    default:
      return h(NTag, { size: 'small', bordered: false }, { default: () => st.state })
  }
}

const columns = computed<DataTableColumns<AppEntity>>(() => [
  ...(canWrite.value
    ? [{
        type: 'selection' as const,
        width: 40,
        disabled: () => batchRunning.value,
      }]
    : []),
  {
    title: '应用名称',
    key: 'name',
    width: 180,
    ellipsis: { tooltip: true },
    render: (row) =>
      h('span', { style: 'font-weight: 600; cursor: pointer;', onClick: () => openDetail(row) }, row.name),
  },
  {
    title: '服务器',
    key: 'host',
    width: 170,
    ellipsis: { tooltip: true },
    render: (row) => h('span', hostLabel(row)),
  },
  {
    title: '来源',
    key: 'source_type',
    // 此列原先无 width，独自吸走全部剩余宽度（约 600px 空白的根因）；
    // 各列现全部定宽，合计 1350，配 :scroll-x="1350"。
    width: 240,
    // 仓库 URL 是无空格长 token：弹层限宽 + 任意断行，否则单行铺开被视口裁掉（DESIGN §9.4）。
    ellipsis: {
      tooltip: {
        style: { maxWidth: '480px' },
        contentStyle: { wordBreak: 'break-all' },
      },
    },
    render: (row) =>
      h('span', { style: 'font-size: 12px; color: var(--n-text-color-disabled, #999)' },
        sourceLabel(row)),
  },
  {
    title: '当前版本',
    key: 'current_commit',
    width: 170,
    ellipsis: { tooltip: true },
    render: (row) => versionCell(row),
  },
  { title: '状态', key: 'state', width: 100, render: (row) => stateTag(row) },
  {
    title: '最近部署',
    key: 'last_deploy_at',
    width: 150,
    render: (row) => row.last_deploy_at ? formatTime(row.last_deploy_at) : '-',
  },
  {
    title: '最近更新',
    key: 'updated_at',
    width: 150,
    render: (row) => row.updated_at ? formatTime(row.updated_at) : (row.created_at ? formatTime(row.created_at) : '-'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    render: (row) =>
      h(NSpace, { size: 8 }, {
        default: () => {
          const buttons = [
            h(
              NButton,
              { size: 'tiny', secondary: true, onClick: () => openDetail(row) },
              { default: () => '详情' },
            ),
          ]
          if (canWrite.value) {
            buttons.push(
              h(
                NButton,
                { size: 'tiny', secondary: true, onClick: () => openMigrate(row) },
                { default: () => h(NIcon, null, { default: () => h(MoveOutline) }) },
              ),
            )
            buttons.push(
              h(
                NButton,
                { size: 'tiny', secondary: true, type: 'error', onClick: () => openDeleteModal(row) },
                { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) },
              ),
            )
          }
          return buttons
        },
      }),
  },
])

function formatTime(v: string) {
  return fmtDateTimeMinute(v)
}

async function loadData() {
  loading.value = true
  statusesLoading.value = true
  try {
    // Hosts come from the inventory in parallel: the apps response carries
    // only host_id, and a failed host fetch must not blank the app list.
    // listHosts() resolves the whole {data,total} envelope, unlike listApps().
    const [appList, hostResp] = await Promise.all([
      listApps(),
      listHosts().catch(() => ({ data: [] as { id: string; hostname: string }[], total: 0 })),
    ])
    apps.value = appList
    // 刷新后清掉已不存在的应用勾选，避免批量操作带上幽灵 ID
    const alive = new Set(appList.map((a) => a.id))
    checkedRowKeys.value = checkedRowKeys.value.filter((id) => alive.has(id))
    const map = new Map<string, string>()
    for (const h of hostResp.data) map.set(h.id, h.hostname)
    hostnameById.value = map
  } catch (e: any) {
    message.error(e.message || '获取应用列表失败')
  } finally {
    loading.value = false
  }
}

async function refreshStatuses() {
  try {
    for (const a of apps.value) {
      try {
        statuses.value[a.id] = await getAppStatus(a.id)
      } catch {
        statuses.value[a.id] = { app_id: a.id, deploying: false, agent_online: false, state: 'unknown' }
      }
    }
  } finally {
    statusesLoading.value = false
  }
}

async function refreshAll() {
  await loadData().then(refreshStatuses)
  // 手动刷新拿到的是最新状态，旧的操作进度（已完成/失败）不再对应当前行；
  // 批量进行中时不动，等本轮跑完再由执行器收尾。
  if (!batchRunning.value) {
    opStatus.value = {}
    batchSummary.value = null
  }
}

function openDetail(app: AppEntity) {
  workspace.openTab({
    key: `/apps/${app.id}`,
    title: `应用 ${app.name}`,
    path: `/apps/${app.id}`,
    viewName: 'AppDetail',
  })
  router.push(`/apps/${app.id}`)
}

// 迁移快捷入口：复用详情页同一个弹窗组件，成功后跳到详情页的部署历史
// 看实时进度（列表页没有进度流）。
const showMigrate = ref(false)
const migrateTarget = ref<AppEntity | null>(null)

function openMigrate(app: AppEntity) {
  migrateTarget.value = app
  showMigrate.value = true
}

function onMigrated() {
  const target = migrateTarget.value
  showMigrate.value = false
  loadData().then(refreshStatuses)
  if (target) openDetail(target)
}

function openCreate() {
  workspace.openTab({
    key: '/apps/create',
    title: '新建应用',
    path: '/apps/create',
    viewName: 'AppCreate',
  })
  router.push('/apps/create')
}

// 删除确认（含"是否删除内容"选项）
const showDeleteModal = ref(false)
const deleteTarget = ref<AppEntity | null>(null)
const purgeContent = ref(false)
const deleting = ref(false)

function openDeleteModal(app: AppEntity) {
  deleteTarget.value = app
  purgeContent.value = false
  showDeleteModal.value = true
}

async function confirmDelete() {
  const app = deleteTarget.value
  if (!app) return
  deleting.value = true
  try {
    const res = await deleteApp(app.id, purgeContent.value)
    if (res.purged) {
      message.success(`已删除应用「${app.name}」及其部署内容`)
    } else {
      message.success(`已删除应用「${app.name}」（容器等内容未动）`)
    }
    if (res.warnings?.length) {
      message.warning('部分内容清理失败：' + res.warnings.join('；'))
    }
    showDeleteModal.value = false
    await loadData()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  } finally {
    deleting.value = false
  }
}

onMounted(() => {
  loadData().then(refreshStatuses)
  statusTimer = setInterval(async () => {
    if (apps.value.length > 0) await refreshStatuses()
  }, 15000)
})

onActivated(() => {
  loadData().then(refreshStatuses)
})

onDeactivated(() => {
  if (statusTimer) {
    clearInterval(statusTimer)
    statusTimer = null
  }
})
</script>

<template>
  <div class="apps-view page-flex-column">
    <div class="page-header">
      <div class="header-left">
        <h2 class="page-title">应用中心</h2>
      </div>
    </div>

    <div class="table-toolbar">
      <NSpace align="center">
        <NInput
          v-model:value="searchKeyword"
          placeholder="搜索应用名 / 仓库 / Commit"
          clearable
          style="width: 260px"
        />
        <NButton :loading="loading" @click="refreshAll">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          刷新
        </NButton>
        <span class="muted">共 {{ filteredApps.length }} 个应用</span>
      </NSpace>
      <NButton v-if="canWrite" type="primary" @click="openCreate">
        <template #icon>
          <NIcon><AddCircleOutline /></NIcon>
        </template>
        创建应用
      </NButton>
    </div>

    <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、状态列逐台显示进度 -->
    <div v-if="canWrite && checkedRowKeys.length > 0" class="batch-bar">
      <span class="batch-info">
        已选 {{ checkedRowKeys.length }} 个
        <span class="text-muted">（可启动 {{ batchStartable.length }} · 可停止 {{ batchStoppable.length }} · 可重启 {{ batchRestartable.length }} · 可重新部署 {{ batchDeployable.length }}）</span>
      </span>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'start'"
        :disabled="batchRunning || batchStartable.length === 0"
        @click="runBatchPerApp('start', batchStartable, (a) => startApp(a.id), selectedApps.length - batchStartable.length)"
      >
        批量启动
      </NButton>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'stop'"
        :disabled="batchRunning || batchStoppable.length === 0"
        @click="runBatchPerApp('stop', batchStoppable, (a) => stopApp(a.id), selectedApps.length - batchStoppable.length)"
      >
        批量停止
      </NButton>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'restart'"
        :disabled="batchRunning || batchRestartable.length === 0"
        @click="runBatchPerApp('restart', batchRestartable, (a) => restartApp(a.id), selectedApps.length - batchRestartable.length)"
      >
        批量重启
      </NButton>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'deploy'"
        :disabled="batchRunning || batchDeployable.length === 0"
        @click="runBatchPerApp('deploy', batchDeployable, (a) => deployApp(a.id), selectedApps.length - batchDeployable.length)"
      >
        批量重新部署
      </NButton>
      <NButton
        size="small"
        type="error"
        secondary
        :disabled="batchRunning"
        @click="openBatchDeleteModal"
      >
        批量删除
      </NButton>
      <NButton
        v-if="batchSummary && batchSummary.failed > 0"
        size="small"
        type="warning"
        secondary
        :disabled="batchRunning"
        @click="retryBatchFailed"
      >
        重试失败（{{ batchSummary.failed }}）
      </NButton>
      <span v-if="batchSummary" class="batch-summary">
        成功 {{ batchSummary.done }} · 失败 {{ batchSummary.failed }}<span v-if="batchSummary.skipped > 0"> · 跳过 {{ batchSummary.skipped }}</span>
      </span>
      <NButton size="small" quaternary :disabled="batchRunning" @click="clearSelection">
        取消选择
      </NButton>
    </div>

    <div class="table-card table-flex-fill">
      <NDataTable
        v-if="apps.length > 0"
        flex-height
        :columns="columns"
        :data="filteredApps"
        :row-key="(row: AppEntity) => row.id"
        :checked-row-keys="checkedRowKeys"
        :row-class-name="rowClassName"
        :pagination="pagination"
        :scroll-x="1350"
        @update:checked-row-keys="onCheckedRowKeysChange"
      />
      <NEmpty
        v-else-if="!loading"
        style="flex: 1; display: flex; flex-direction: column; justify-content: center"
        description="还没有创建任何应用">
        <template #icon>
          <NIcon><RocketOutline /></NIcon>
        </template>
        <NButton v-if="canWrite" type="primary" @click="openCreate">
          创建第一个应用
        </NButton>
      </NEmpty>
      <NSpin v-else class="spin-center" />
    </div>

    <!-- 服务迁移：与详情页共用同一弹窗，成功后进入详情页部署历史看进度 -->
    <AppMigrateModal v-model:show="showMigrate" :app="migrateTarget" @migrated="onMigrated" />

    <NModal
      v-model:show="showDeleteModal"
      preset="dialog"
      title="删除应用"
      :loading="deleting"
      positive-text="删除"
      negative-text="取消"
      type="error"
      @positive-click="confirmDelete"
    >
      <NSpace vertical :size="12">
        <div>确定删除应用「{{ deleteTarget?.name }}」？</div>
        <NCheckbox v-model:checked="purgeContent">
          同时删除应用内容（容器、数据卷、镜像、代理配置等）
        </NCheckbox>
        <NAlert v-if="purgeContent" type="warning" :show-icon="false" style="font-size: 12px">
          将在主机上销毁该应用的容器/Compose 栈、命名数据卷、构建镜像及域名代理配置，数据不可恢复。
          若主机离线，删除会被拒绝（避免内容被静默遗留）。
        </NAlert>
        <div v-else style="font-size: 12px; color: var(--n-text-color-disabled, #999)">
          仅删除面板中的应用记录，主机上的容器、数据卷等内容不会被销毁。
        </div>
      </NSpace>
    </NModal>

    <!-- 批量删除：危险操作，弹窗内就地二次确认（DESIGN.md §9.17 / §9.22） -->
    <NModal
      v-model:show="showBatchDeleteModal"
      preset="card"
      :title="`批量删除应用（${selectedApps.length} 个）`"
      style="width: 520px"
    >
      <div class="batch-confirm-body">
        <div class="batch-target-list">
          <div v-for="a in selectedApps.slice(0, 5)" :key="a.id" class="batch-target-row">
            {{ a.name }}（{{ hostLabel(a) }}）
          </div>
          <div v-if="selectedApps.length > 5" class="batch-target-row muted">
            等 {{ selectedApps.length - 5 }} 个…
          </div>
        </div>
        <NCheckbox v-model:checked="batchPurgeContent">
          同时删除应用内容（容器、数据卷、镜像、代理配置等）
        </NCheckbox>
        <NAlert v-if="batchPurgeContent" type="warning" :show-icon="false" style="font-size: 12px">
          将在主机上销毁各应用的容器/Compose 栈、命名数据卷、构建镜像及域名代理配置，数据不可恢复。
          若主机离线，删除会被拒绝（避免内容被静默遗留），记为失败、可稍后重试。
        </NAlert>
        <div v-else class="field-hint">
          仅删除面板中的应用记录，主机上的容器、数据卷等内容不会被销毁。
        </div>
      </div>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBatchDeleteModal = false">取消</NButton>
          <NButton type="error" :loading="batchRunning" @click="confirmBatchDelete">确认删除</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.apps-view {
  gap: 12px;
}

.page-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;

  .header-left {
    display: flex;
    align-items: center;

    .page-title {
      margin: 0;
      font-size: 18px;
      font-weight: 600;
      color: var(--text-primary);
    }
  }
}

.table-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.muted {
  color: var(--text-secondary);
  font-size: 13px;
  white-space: nowrap;
}

// 批量操作栏：有勾选时出现，贴在工具栏下方、表格上方（与 NetworkList.vue 同款）
.batch-bar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 8px 12px;

  .batch-info {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);

    .text-muted {
      color: var(--n-text-color-3);
      font-size: 12px;
      font-weight: 400;
    }
  }

  .batch-summary {
    font-size: 12px;
    color: var(--text-secondary);
  }
}

.batch-confirm-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.batch-target-list {
  max-height: 180px;
  overflow-y: auto;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 6px 10px;

  .batch-target-row {
    padding: 3px 0;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;

    &.muted {
      color: var(--text-secondary);
    }
  }
}

.field-hint {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
  margin: 0;
}

// 批量执行中的行：操作列整体置灰禁用，防单机操作与批量并发冲突（DESIGN.md §9.22）
:deep(.batch-row-busy [data-col-key="actions"]) {
  pointer-events: none;
  opacity: 0.5;
}

.table-card {
  background: var(--bg-card);
  border-radius: 8px;
  padding: 4px;
}

// 首轮数据落定前以加载态占位，不渲染「还没有创建任何应用」假空状态
//（与 FileManager.vue 的 .spin-center 同款）。
.spin-center {
  display: flex;
  justify-content: center;
  align-items: center;
  flex: 1;
  min-height: 0;
}
</style>