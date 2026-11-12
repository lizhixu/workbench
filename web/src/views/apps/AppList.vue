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
  deleteApp, getAppStatus, listApps, type AppEntity, type AppStatus,
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
    // 各列现全部定宽，合计 1310，配 :scroll-x="1310"。
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
        <NButton :loading="loading" @click="loadData().then(refreshStatuses)">
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

    <div class="table-card table-flex-fill">
      <NDataTable
        v-if="apps.length > 0"
        flex-height
        :columns="columns"
        :data="filteredApps"
        :row-key="(row: AppEntity) => row.id"
        :pagination="pagination"
        :scroll-x="1310"
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