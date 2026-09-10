<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton, NDataTable, NEmpty, NIcon, NInput, NPopconfirm,
  NSpace, NTag, useMessage,
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
                NPopconfirm,
                { onPositiveClick: () => doDelete(row) },
                {
                  trigger: () =>
                    h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) }),
                  default: () => `确定删除应用「${row.name}」？容器不会被销毁。`,
                },
              ),
            )
          }
          return buttons
        },
      }),
  },
])

function formatTime(v: string) {
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
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

async function doDelete(app: AppEntity) {
  try {
    await deleteApp(app.id)
    message.success(`已删除应用「${app.name}」`)
    await loadData()
  } catch (e: any) {
    message.error(e.message || '删除失败')
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
  </div>
</template>

<style scoped lang="scss">
.apps-view {
  gap: 12px;
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