<script setup lang="ts">
import { computed, h, onActivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NDataTable, NForm, NFormItem, NIcon, NInput,
  NInputNumber, NModal, NPopconfirm, NRadio, NRadioGroup, NSelect,
  NSpace, NSwitch, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, ArchiveOutline, PlayOutline, RefreshOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import {
  createBackupJob, deleteArchive, deleteBackupJob, listArchives,
  listBackupJobs, restoreArchive, runBackupJob, type Archive,
  type BackupJob, type CreateJobReq,
} from '../../api/snapshots'
import { listHosts } from '../../api/hosts'

defineOptions({ name: 'SnapshotList' })

const message = useMessage()

const jobs = ref<BackupJob[]>([])
const loading = ref(false)
const showCreate = ref(false)
const creating = ref(false)

// 归档弹窗
const showArchives = ref(false)
const selectedJob = ref<BackupJob | null>(null)
const archives = ref<Archive[]>([])
const archiveTotal = ref(0)
const archivePage = ref(1)
const archivePageSize = ref(20)
const loadingArchives = ref(false)

interface HostOption {
  id: string
  hostname: string
  status: string
}
const hostOptions = ref<{ label: string; value: string }[]>([])

const form = reactive<CreateJobReq>({
  name: '',
  host_id: '',
  kind: 'database',
  target: '',
  db_type: 'mysql',
  db_name: '',
  retention: 7,
  cron: '0 3 * * *',
  enabled: true,
})

const kindOptions = [
  { label: '数据库热备 (MySQL / Postgres / Redis / Mongo)', value: 'database' },
  { label: 'Docker 命名卷 (Volume)', value: 'volume' },
  { label: '主机目录 (Directory)', value: 'dir' },
]

const dbTypeOptions = [
  { label: 'MySQL', value: 'mysql' },
  { label: 'PostgreSQL', value: 'postgres' },
  { label: 'Redis', value: 'redis' },
  { label: 'MongoDB', value: 'mongo' },
]

const columns = computed<DataTableColumns<BackupJob>>(() => [
  { title: '任务名称', key: 'name', width: 180, render: (r) => h('span', { style: 'font-weight: 600' }, r.name) },
  {
    title: '类型',
    key: 'kind',
    width: 140,
    render: (r) => {
      const label = r.kind === 'database' ? `DB (${r.db_type?.toUpperCase()})` : r.kind === 'volume' ? 'Docker 卷' : '主机目录'
      return h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => label })
    },
  },
  { title: '目标对象', key: 'target', ellipsis: { tooltip: true }, render: (r) => r.target + (r.db_name ? ` / ${r.db_name}` : '') },
  { title: '调度计划', key: 'schedule', width: 130, render: (r) => r.schedule?.cron ? h('code', { style: 'font-size: 12px' }, r.schedule.cron) : '仅手动' },
  { title: '保留份数', key: 'retention', width: 90, render: (r) => `${r.retention} 份` },
  {
    title: '最近执行',
    key: 'last_run',
    width: 170,
    render: (r) => {
      if (!r.last_run) return '-'
      const hasErr = Boolean(r.last_error)
      return h(NSpace, { size: 4, align: 'center' }, {
        default: () => [
          h('span', { style: 'font-size: 12px' }, formatTime(r.last_run)),
          hasErr ? h(NTag, { size: 'tiny', type: 'error', bordered: false }, { default: () => '失败' }) : null,
        ],
      })
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 220,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, type: 'primary', onClick: () => doRun(r) }, {
            icon: () => h(NIcon, null, { default: () => h(PlayOutline) }),
            default: () => '立即备份',
          }),
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openArchives(r) }, {
            icon: () => h(NIcon, null, { default: () => h(ArchiveOutline) }),
            default: () => '历史归档',
          }),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDelete(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, {
                icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
              }),
              default: () => `确定删除备份任务「${r.name}」？已生成的归档仍会保存在主机上。`,
            },
          ),
        ],
      }),
  },
])

const archiveColumns = computed<DataTableColumns<Archive>>(() => [
  { title: '归档编号', key: 'id', ellipsis: { tooltip: true } },
  {
    title: '大小',
    key: 'size',
    width: 110,
    render: (r) => formatBytes(r.size),
  },
  {
    title: 'SHA256 校验和',
    key: 'sha256',
    width: 140,
    render: (r) => r.sha256 ? h('code', { style: 'font-size: 11px' }, r.sha256.slice(0, 12) + '...') : '-',
  },
  {
    title: '创建时间',
    key: 'created_at',
    width: 170,
    render: (r) => formatTime(r.created_at),
  },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(
            NPopconfirm,
            { onPositiveClick: () => doRestore(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'warning' }, { default: () => '恢复' }),
              default: () => `确定从该归档恢复？恢复前会自动在主机生成一份现场快照（SafetyCopy）。`,
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDeleteArchive(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, {
                icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
              }),
              default: () => '确定删除此归档记录？',
            },
          ),
        ],
      }),
  },
])

function formatTime(v?: string) {
  if (!v) return '-'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function formatBytes(b: number) {
  if (b <= 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(b) / Math.log(k))
  return `${(b / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`
}

async function loadData() {
  loading.value = true
  try {
    jobs.value = await listBackupJobs()
  } catch (e: any) {
    message.error(e.message || '获取备份任务失败')
  } finally {
    loading.value = false
  }
}

async function loadHosts() {
  try {
    const res = await listHosts()
    const list = ((res as { data: HostOption[] }).data ?? res) as HostOption[]
    hostOptions.value = list.map((h) => ({
      label: `${h.hostname} (${h.status === 'online' ? '在线' : '离线'})`,
      value: h.id,
    }))
  } catch {
    hostOptions.value = []
  }
}

async function submitCreate() {
  if (!form.name.trim()) {
    message.warning('请填写任务名称')
    return
  }
  if (!form.host_id) {
    message.warning('请选择目标主机')
    return
  }
  if (!form.target.trim()) {
    message.warning('请填写备份目标对象')
    return
  }
  creating.value = true
  try {
    await createBackupJob({ ...form })
    message.success('备份任务已创建')
    showCreate.value = false
    await loadData()
  } catch (e: any) {
    message.error(e.message || '创建失败')
  } finally {
    creating.value = false
  }
}

async function doRun(job: BackupJob) {
  try {
    const res = await runBackupJob(job.id)
    message.success(res.message || '已触发备份任务')
    setTimeout(loadData, 3000)
  } catch (e: any) {
    message.error(e.message || '触发失败')
  }
}

async function doDelete(job: BackupJob) {
  try {
    await deleteBackupJob(job.id)
    message.success('任务已删除')
    await loadData()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

async function openArchives(job: BackupJob) {
  selectedJob.value = job
  archivePage.value = 1
  showArchives.value = true
  await fetchArchives()
}

async function fetchArchives() {
  if (!selectedJob.value) return
  loadingArchives.value = true
  try {
    const res = await listArchives(selectedJob.value.id, (archivePage.value - 1) * archivePageSize.value, archivePageSize.value)
    archives.value = res.data
    archiveTotal.value = res.total
  } catch (e: any) {
    message.error(e.message || '获取归档列表失败')
  } finally {
    loadingArchives.value = false
  }
}

async function doRestore(arch: Archive) {
  if (!selectedJob.value) return
  try {
    const res = await restoreArchive(selectedJob.value.id, arch.id)
    message.success(res.message || '已开始恢复任务')
  } catch (e: any) {
    message.error(e.message || '恢复失败')
  }
}

async function doDeleteArchive(arch: Archive) {
  if (!selectedJob.value) return
  try {
    await deleteArchive(selectedJob.value.id, arch.id)
    message.success('归档记录已删除')
    await fetchArchives()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

onMounted(() => {
  loadData()
  loadHosts()
})

onActivated(() => {
  loadData()
})
</script>

<template>
  <div class="snapshots-view page-flex-column">
    <div class="table-toolbar">
      <NSpace align="center">
        <NButton :loading="loading" @click="loadData">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          刷新
        </NButton>
      </NSpace>
      <NButton type="primary" @click="showCreate = true">
        <template #icon>
          <NIcon><AddCircleOutline /></NIcon>
        </template>
        新建备份任务
      </NButton>
    </div>

    <div class="table-card table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="jobs"
        :row-key="(r: BackupJob) => r.id"
        :pagination="{ pageSize: 20 }"
      />
    </div>

    <NModal
      v-model:show="showCreate"
      preset="card"
      title="新建备份任务"
      style="width: 600px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="任务名称" required>
          <NInput v-model:value="form.name" placeholder="如 mysql-prod-daily" />
        </NFormItem>
        <NFormItem label="目标主机" required>
          <NSelect v-model:value="form.host_id" :options="hostOptions" placeholder="选择受管主机" filterable />
        </NFormItem>
        <NFormItem label="备份类型" required>
          <NRadioGroup v-model:value="form.kind">
            <NSpace vertical>
              <NRadio v-for="opt in kindOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</NRadio>
            </NSpace>
          </NRadioGroup>
        </NFormItem>

        <template v-if="form.kind === 'database'">
          <NSpace>
            <NFormItem label="数据库引擎" style="flex: 1">
              <NSelect v-model:value="form.db_type" :options="dbTypeOptions" />
            </NFormItem>
            <NFormItem label="容器名称" required style="flex: 1">
              <NInput v-model:value="form.target" placeholder="如 watchman-app-mysql" />
            </NFormItem>
          </NSpace>
          <NFormItem label="特定数据库名称（可选，留空备份全库）">
            <NInput v-model:value="form.db_name" placeholder="如 app_db" />
          </NFormItem>
        </template>

        <template v-else-if="form.kind === 'volume'">
          <NFormItem label="Docker 命名卷名称" required>
            <NInput v-model:value="form.target" placeholder="如 myapp_data" />
          </NFormItem>
        </template>

        <template v-else>
          <NFormItem label="绝对目录路径" required>
            <NInput v-model:value="form.target" placeholder="/opt/apps/myapp/data" />
          </NFormItem>
        </template>

        <NSpace>
          <NFormItem label="定时计划（Cron 5 段）" style="flex: 1">
            <NInput v-model:value="form.cron" placeholder="0 3 * * *（留空仅手动）" />
          </NFormItem>
          <NFormItem label="保留份数" style="width: 140px">
            <NInputNumber v-model:value="form.retention" :min="1" :max="100" />
          </NFormItem>
        </NSpace>

        <NFormItem label="启用定时调度">
          <NSwitch v-model:value="form.enabled" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showCreate = false">取消</NButton>
          <NButton type="primary" :loading="creating" @click="submitCreate">创建任务</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 历史归档列表弹窗 -->
    <NModal
      v-model:show="showArchives"
      preset="card"
      :title="`历史归档 · ${selectedJob?.name ?? ''}`"
      style="width: 860px; max-width: 94vw"
    >
      <div style="height: 480px; display: flex; flex-direction: column">
        <NAlert type="info" :show-icon="true" style="margin-bottom: 10px; flex-shrink: 0">
          归档保存在主机的 /var/backups/watchman 目录下。执行恢复前系统会自动为当前状态创建临时安全副本（SafetyCopy）。
        </NAlert>
        <div style="flex: 1; min-height: 0">
          <NDataTable
            flex-height
            :columns="archiveColumns"
            :data="archives"
            :loading="loadingArchives"
            :row-key="(r: Archive) => r.id"
            :pagination="{
              page: archivePage,
              pageSize: archivePageSize,
              itemCount: archiveTotal,
              'onUpdate:page': (p: number) => { archivePage = p; fetchArchives() },
            }"
            remote
          />
        </div>
      </div>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.snapshots-view {
  gap: 12px;
}

.table-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.table-card {
  background: var(--n-color, rgba(128, 128, 128, 0.06));
  border-radius: 8px;
  padding: 4px;
}
</style>