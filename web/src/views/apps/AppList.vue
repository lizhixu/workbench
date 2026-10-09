<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton, NDataTable, NEmpty, NIcon, NInput, NModal,
  NSpace, NTag, NCheckbox, NAlert, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, RocketOutline, GitCommitOutline, RefreshOutline,
  TrashOutline, CloudUploadOutline,
} from '@vicons/ionicons5'
import {
  deleteApp, getAppStatus, listApps, type AppEntity, type AppStatus,
} from '../../api/apps'
import { useWorkspaceStore } from '../../stores/workspace'
import { useAuthStore } from '../../stores/auth'
import { fmtDateTimeMinute } from '../../utils/time'

defineOptions({ name: 'AppList' })

const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()
const auth = useAuthStore()

const apps = ref<AppEntity[]>([])
const loading = ref(false)
const searchKeyword = ref('')
const statuses = ref<Record<string, AppStatus>>({})
let statusTimer: ReturnType<typeof setInterval> | null = null

const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')

const filteredApps = computed(() => {
  const q = searchKeyword.value.trim().toLowerCase()
  if (!q) return apps.value
  return apps.value.filter((a) =>
    a.name.toLowerCase().includes(q) ||
    (a.repo_url ?? '').toLowerCase().includes(q) ||
    (a.branch ?? '').toLowerCase().includes(q) ||
    (a.current_commit ?? '').toLowerCase().includes(q),
  )
})

const stateTag = (a: AppEntity) => {
  const st = statuses.value[a.id]
  if (!st) return h(NTag, { size: 'small', bordered: false }, { default: () => '未知' })
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
    title: '应用',
    key: 'name',
    width: 180,
    render: (row) =>
      h('span', { style: 'font-weight: 600; cursor: pointer;', onClick: () => openDetail(row) }, row.name),
  },
  {
    title: 'Git 仓库',
    key: 'repo_url',
    ellipsis: { tooltip: true },
    render: (row) =>
      h('span', { style: 'font-size: 12px; color: var(--n-text-color-disabled, #999)' },
        `${row.repo_url ?? ''} @ ${row.branch ?? 'main'}`),
  },
  {
    title: '当前版本',
    key: 'current_commit',
    width: 140,
    render: (row) => row.current_commit
      ? h(NTag, { size: 'small', bordered: false, type: 'info' }, { icon: () => h(NIcon, null, { default: () => h(GitCommitOutline) }), default: () => row.current_commit })
      : h('span', { style: 'color: #999' }, '-'),
  },
  {
    title: '自动部署',
    key: 'auto_deploy',
    width: 90,
    render: (row) => row.auto_deploy
      ? h(NIcon, { color: '#18a058' }, { default: () => h(CloudUploadOutline) })
      : h('span', { style: 'color: #999' }, '-'),
  },
  { title: '状态', key: 'state', width: 100, render: (row) => stateTag(row) },
  {
    title: '最近部署',
    key: 'last_deploy_at',
    width: 160,
    render: (row) => row.last_deploy_at ? formatTime(row.last_deploy_at) : '-',
  },
  {
    title: '操作',
    key: 'actions',
    width: 120,
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
  try {
    apps.value = (await listApps()) as AppEntity[]
  } catch (e: any) {
    message.error(e.message || '获取应用列表失败')
  } finally {
    loading.value = false
  }
}

async function refreshStatuses() {
  for (const a of apps.value) {
    try {
      statuses.value[a.id] = await getAppStatus(a.id)
    } catch {
      statuses.value[a.id] = { app_id: a.id, deploying: false, agent_online: false, state: 'unknown' }
    }
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
      </NSpace>
      <NButton v-if="canWrite" type="primary" @click="openCreate">
        <template #icon>
          <NIcon><AddCircleOutline /></NIcon>
        </template>
        链接 Git 应用
      </NButton>
    </div>

    <div class="table-card table-flex-fill">
      <NDataTable
        v-if="apps.length > 0"
        flex-height
        :columns="columns"
        :data="filteredApps"
        :row-key="(row: AppEntity) => row.id"
        :pagination="{
          pageSize: 20,
          showSizePicker: true,
          pageSizes: [10, 20, 50],
        }"
      />
      <NEmpty
        v-else
        style="flex: 1; display: flex; flex-direction: column; justify-content: center"
        description="还没有链接任何 Git 应用">
        <template #icon>
          <NIcon><RocketOutline /></NIcon>
        </template>
        <NButton v-if="canWrite" type="primary" @click="openCreate">
          创建第一个应用
        </NButton>
      </NEmpty>
    </div>

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

.table-card {
  background: var(--bg-card);
  border-radius: 8px;
  padding: 4px;
}
</style>