<script setup lang="ts">
import { computed, h, onActivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem, NIcon, NInput,
  NInputNumber, NModal, NPopconfirm, NRadio, NRadioGroup, NSelect,
  NSpace, NSwitch, NTabPane, NTabs, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, ArchiveOutline, CloudUploadOutline,
  PlayOutline, RefreshOutline,
  TrashOutline, PulseOutline,
} from '@vicons/ionicons5'
import {
  createBackupJob, createS3Target, deleteArchive, deleteBackupJob,
  deleteS3Target, listArchives, listBackupJobs, listS3Targets,
  restoreArchive, runBackupJob, testS3Target, updateS3Target,
  type Archive, type BackupJob, type CreateJobReq, type S3Target,
} from '../../api/snapshots'
import { listHosts } from '../../api/hosts'

defineOptions({ name: 'SnapshotList' })

const message = useMessage()

const activeTab = ref<'jobs' | 's3'>('jobs')

// ---- 备份任务列表状态 ----
const jobs = ref<BackupJob[]>([])
const loading = ref(false)
const showCreate = ref(false)
const creating = ref(false)

// ---- S3 存储目标状态 ----
const s3Targets = ref<S3Target[]>([])
const loadingS3 = ref(false)
const showS3Modal = ref(false)
const editingS3ID = ref<string | null>(null)
const savingS3 = ref(false)
const testingS3ID = ref<string | null>(null)

const s3Form = reactive<Partial<S3Target>>({
  name: '',
  provider: 'r2',
  endpoint: '',
  region: 'auto',
  bucket: '',
  prefix: 'snapshots',
  access_key: '',
  secret_key: '',
  force_path_style: false,
  is_default: false,
})

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
  storage_target: 'default',
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

const s3ProviderOptions = [
  { label: 'Cloudflare R2', value: 'r2' },
  { label: 'MinIO', value: 'minio' },
  { label: 'AWS S3', value: 'aws' },
  { label: '阿里云 OSS (S3 兼容)', value: 'aliyun' },
  { label: '腾讯云 COS (S3 兼容)', value: 'tencent' },
  { label: '自建 S3 / 其他 S3 兼容服务', value: 'custom' },
]

const storageTargetOptions = computed(() => {
  const def = s3Targets.value.find((t) => t.is_default)
  const defLabel = def ? `默认存储目标 (${def.name} - ${def.bucket})` : '默认存储目标 (本地主机)'
  return [
    { label: defLabel, value: 'default' },
    { label: '本地主机 (/var/backups/watchman)', value: 'local' },
    ...s3Targets.value.map((t) => ({
      label: `S3: ${t.name} (${t.bucket})`,
      value: t.id,
    })),
  ]
})

function onS3ProviderChange(val: string) {
  s3Form.provider = val as any
  if (val === 'r2') {
    s3Form.region = 'auto'
    s3Form.force_path_style = false
    if (!s3Form.endpoint) s3Form.endpoint = 'https://<account_id>.r2.cloudflarestorage.com'
  } else if (val === 'minio') {
    s3Form.region = 'us-east-1'
    s3Form.force_path_style = true
    if (!s3Form.endpoint) s3Form.endpoint = 'http://127.0.0.1:9000'
  } else if (val === 'aws') {
    s3Form.region = 'us-east-1'
    s3Form.force_path_style = false
    if (!s3Form.endpoint) s3Form.endpoint = 'https://s3.us-east-1.amazonaws.com'
  } else if (val === 'aliyun') {
    s3Form.region = 'cn-hangzhou'
    s3Form.force_path_style = true
    if (!s3Form.endpoint) s3Form.endpoint = 'https://oss-cn-hangzhou.aliyuncs.com'
  } else if (val === 'tencent') {
    s3Form.region = 'ap-guangzhou'
    s3Form.force_path_style = true
    if (!s3Form.endpoint) s3Form.endpoint = 'https://cos.ap-guangzhou.myqcloud.com'
  }
}

const columns = computed<DataTableColumns<BackupJob>>(() => [
  { title: '任务名称', key: 'name', width: 170, render: (r) => h('span', { style: 'font-weight: 600' }, r.name) },
  {
    title: '类型',
    key: 'kind',
    width: 130,
    render: (r) => {
      const label = r.kind === 'database' ? `DB (${r.db_type?.toUpperCase()})` : r.kind === 'volume' ? 'Docker 卷' : '主机目录'
      return h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => label })
    },
  },
  { title: '目标对象', key: 'target', ellipsis: { tooltip: true }, render: (r) => r.target + (r.db_name ? ` / ${r.db_name}` : '') },
  {
    title: '存储位置',
    key: 'storage_target',
    width: 170,
    render: (r) => {
      const st = r.storage_target
      if (!st || st === 'default') {
        const def = s3Targets.value.find((t) => t.is_default)
        return def
          ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => `S3: ${def.name}` })
          : h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
      }
      if (st === 'local') {
        return h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
      }
      const target = s3Targets.value.find((t) => t.id === st)
      return target
        ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => `S3: ${target.name}` })
        : h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => 'S3 (未知)' })
    },
  },
  { title: '调度计划', key: 'schedule', width: 120, render: (r) => r.schedule?.cron ? h('code', { style: 'font-size: 12px' }, r.schedule.cron) : '仅手动' },
  { title: '保留', key: 'retention', width: 80, render: (r) => `${r.retention} 份` },
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
              default: () => `确定删除备份任务「${r.name}」？`,
            },
          ),
        ],
      }),
  },
])

// S3 存储目标列表列
const s3Columns = computed<DataTableColumns<S3Target>>(() => [
  {
    title: '存储目标名称',
    key: 'name',
    width: 200,
    render: (r) =>
      h(NSpace, { size: 6, align: 'center' }, {
        default: () => [
          h('span', { style: 'font-weight: 600' }, r.name),
          r.is_default ? h(NTag, { size: 'tiny', type: 'success', bordered: false }, { default: () => '默认目标' }) : null,
        ],
      }),
  },
  {
    title: '类型',
    key: 'provider',
    width: 140,
    render: (r) => h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => r.provider.toUpperCase() }),
  },
  {
    title: 'Endpoint 服务地址',
    key: 'endpoint',
    ellipsis: { tooltip: true },
    render: (r) => h('code', { style: 'font-size: 11px' }, r.endpoint),
  },
  { title: 'Bucket 存储桶', key: 'bucket', width: 160 },
  { title: '前缀目录', key: 'prefix', width: 120, render: (r) => r.prefix ? `/${r.prefix}/` : '-' },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, {
            size: 'tiny',
            secondary: true,
            loading: testingS3ID.value === r.id,
            onClick: () => doTestS3(r),
          }, {
            icon: () => h(NIcon, null, { default: () => h(PulseOutline) }),
            default: () => '测试连通',
          }),
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openEditS3(r) }, { default: () => '编辑' }),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDeleteS3(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, {
                icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
              }),
              default: () => `确定删除 S3 目标「${r.name}」？已上传的对象不会被清理。`,
            },
          ),
        ],
      }),
  },
])

const archiveColumns = computed<DataTableColumns<Archive>>(() => [
  { title: '归档编号', key: 'id', ellipsis: { tooltip: true } },
  {
    title: '存储位置',
    key: 'storage_target',
    width: 140,
    render: (r) => {
      const st = r.storage_target
      if (!st || st === 'local') {
        return h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
      }
      const target = s3Targets.value.find((t) => t.id === st)
      return h(NTag, { size: 'small', type: 'success', bordered: false }, {
        default: () => target ? `S3: ${target.name}` : 'S3 远端',
      })
    },
  },
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
    width: 160,
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
              default: () => `确定从该归档恢复？恢复前会自动在主机生成现场快照（SafetyCopy）。`,
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

async function loadS3() {
  loadingS3.value = true
  try {
    s3Targets.value = await listS3Targets()
  } catch (e: any) {
    message.error(e.message || '获取 S3 存储列表失败')
  } finally {
    loadingS3.value = false
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

function openCreateS3() {
  editingS3ID.value = null
  s3Form.name = 'Cloudflare R2 备份'
  s3Form.provider = 'r2'
  s3Form.endpoint = 'https://<account_id>.r2.cloudflarestorage.com'
  s3Form.region = 'auto'
  s3Form.bucket = 'watchman-backups'
  s3Form.prefix = 'snapshots'
  s3Form.access_key = ''
  s3Form.secret_key = ''
  s3Form.force_path_style = false
  s3Form.is_default = s3Targets.value.length === 0
  showS3Modal.value = true
}

function openEditS3(target: S3Target) {
  editingS3ID.value = target.id
  s3Form.name = target.name
  s3Form.provider = target.provider
  s3Form.endpoint = target.endpoint
  s3Form.region = target.region
  s3Form.bucket = target.bucket
  s3Form.prefix = target.prefix || ''
  s3Form.access_key = target.access_key
  s3Form.secret_key = ''
  s3Form.force_path_style = target.force_path_style
  s3Form.is_default = target.is_default
  showS3Modal.value = true
}

async function submitS3() {
  if (!s3Form.name?.trim()) {
    message.warning('请填写存储目标名称')
    return
  }
  if (!s3Form.endpoint?.trim() || !s3Form.bucket?.trim()) {
    message.warning('请填写 Endpoint 与 Bucket 名称')
    return
  }
  if (!s3Form.access_key?.trim()) {
    message.warning('请填写 AccessKey')
    return
  }
  if (!editingS3ID.value && !s3Form.secret_key?.trim()) {
    message.warning('请填写 SecretKey')
    return
  }
  savingS3.value = true
  try {
    if (editingS3ID.value) {
      await updateS3Target(editingS3ID.value, { ...s3Form })
      message.success('S3 存储配置已更新')
    } else {
      await createS3Target({ ...s3Form })
      message.success('S3 存储目标已添加')
    }
    showS3Modal.value = false
    await loadS3()
  } catch (e: any) {
    message.error(e.message || '保存 S3 配置失败')
  } finally {
    savingS3.value = false
  }
}

async function doTestS3(target: S3Target) {
  testingS3ID.value = target.id
  try {
    const res = await testS3Target(target.id)
    message.success(res.message || 'S3 连通性测试成功')
  } catch (e: any) {
    message.error(e.message || '连通性测试失败')
  } finally {
    testingS3ID.value = null
  }
}

async function doDeleteS3(target: S3Target) {
  try {
    await deleteS3Target(target.id)
    message.success('S3 存储目标已删除')
    await loadS3()
  } catch (e: any) {
    message.error(e.message || '删除失败')
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
  loadS3()
  loadHosts()
})

onActivated(() => {
  loadData()
  loadS3()
})
</script>

<template>
  <div class="snapshots-view page-flex-column">
    <!-- Header -->
    <div class="page-header">
      <div class="header-left">
        <h2 class="page-title">快照备份</h2>
      </div>
      <div class="header-right">
        <NSpace align="center" :size="10">
          <NButton :loading="loading || loadingS3" @click="() => { loadData(); loadS3() }">
            <template #icon>
              <NIcon><RefreshOutline /></NIcon>
            </template>
            刷新
          </NButton>
          <NButton v-if="activeTab === 's3'" type="primary" @click="openCreateS3">
            <template #icon>
              <NIcon><CloudUploadOutline /></NIcon>
            </template>
            添加 S3 存储桶
          </NButton>
          <NButton v-else type="primary" @click="showCreate = true">
            <template #icon>
              <NIcon><AddCircleOutline /></NIcon>
            </template>
            新建备份任务
          </NButton>
        </NSpace>
      </div>
    </div>

    <!-- Tabs Container：Tab 与表格分层，完全对齐 AlertList 规范 -->
    <div class="tabs-container">
      <NTabs v-model:value="activeTab" type="line">
        <!-- 备份任务列表 -->
        <NTabPane name="jobs" tab="备份任务列表">
          <div class="tab-pane-content">
            <NCard :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="columns"
                :data="jobs"
                :row-key="(r: BackupJob) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                size="small"
              />
            </NCard>
          </div>
        </NTabPane>

        <!-- S3 存储配置 -->
        <NTabPane name="s3" tab="S3 异地存储配置">
          <div class="tab-pane-content">
            <NAlert type="info" :show-icon="true" style="margin-bottom: 10px; flex-shrink: 0">
              配置多个 S3 存储桶后，可设置默认存储位置。每个备份任务可选择使用默认、本地主机或指定某个具体的 S3 目标进行异地备份上云。
            </NAlert>
            <NCard :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="s3Columns"
                :data="s3Targets"
                :row-key="(r: S3Target) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                size="small"
              />
            </NCard>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <!-- 新建备份任务弹窗 -->
    <NModal
      v-model:show="showCreate"
      preset="card"
      title="新建备份任务"
      style="width: 620px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="任务名称" required>
          <NInput v-model:value="form.name" placeholder="如 mysql-prod-daily" />
        </NFormItem>
        <NFormItem label="目标主机" required>
          <NSelect v-model:value="form.host_id" :options="hostOptions" placeholder="选择受管主机" filterable />
        </NFormItem>

        <NFormItem label="备份存储位置 (可独立配置)" required>
          <NSelect
            v-model:value="form.storage_target"
            :options="storageTargetOptions"
            placeholder="选择备份归档存放目标"
          />
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

    <!-- 添加/编辑 S3 存储目标弹窗 -->
    <NModal
      v-model:show="showS3Modal"
      preset="card"
      :title="editingS3ID ? '编辑 S3 存储桶配置' : '添加 S3 存储桶'"
      style="width: 620px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="快速预设服务商">
          <NSelect :options="s3ProviderOptions" :value="s3Form.provider" @update:value="onS3ProviderChange" />
        </NFormItem>

        <NFormItem label="目标名称" required>
          <NInput v-model:value="s3Form.name" placeholder="如 Cloudflare R2 生产备份" />
        </NFormItem>

        <NFormItem label="S3 Endpoint 服务地址" required>
          <NInput v-model:value="s3Form.endpoint" placeholder="https://<account>.r2.cloudflarestorage.com" />
        </NFormItem>

        <NSpace>
          <NFormItem label="Bucket 存储桶名称" required style="flex: 1">
            <NInput v-model:value="s3Form.bucket" placeholder="watchman-backups" />
          </NFormItem>
          <NFormItem label="Region 区域" style="width: 160px">
            <NInput v-model:value="s3Form.region" placeholder="auto 或 us-east-1" />
          </NFormItem>
        </NSpace>

        <NFormItem label="存储前缀目录（可选）">
          <NInput v-model:value="s3Form.prefix" placeholder="snapshots（上传至 bucket/snapshots/ 目录下）" />
        </NFormItem>

        <NSpace>
          <NFormItem label="Access Key" required style="flex: 1">
            <NInput v-model:value="s3Form.access_key" placeholder="AKIDxxxxxxxxxxxx" />
          </NFormItem>
          <NFormItem label="Secret Key" :required="!editingS3ID" style="flex: 1">
            <NInput
              v-model:value="s3Form.secret_key"
              type="password"
              show-password-on="click"
              :placeholder="editingS3ID ? '留空保持原秘钥' : 'Secret Key'"
            />
          </NFormItem>
        </NSpace>

        <NSpace>
          <NFormItem label="路径样式 (Path-Style)（MinIO 必须开启）">
            <NSwitch v-model:value="s3Form.force_path_style" />
          </NFormItem>
          <NFormItem label="设为全局默认备份目标">
            <NSwitch v-model:value="s3Form.is_default" />
          </NFormItem>
        </NSpace>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showS3Modal = false">取消</NButton>
          <NButton type="primary" :loading="savingS3" @click="submitS3">保存 S3 目标</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 历史归档列表弹窗 -->
    <NModal
      v-model:show="showArchives"
      preset="card"
      :title="`历史归档 · ${selectedJob?.name ?? ''}`"
      style="width: 880px; max-width: 94vw"
    >
      <div style="height: 480px; display: flex; flex-direction: column">
        <NAlert type="info" :show-icon="true" style="margin-bottom: 10px; flex-shrink: 0">
          归档保存在本地主机或 S3 存储桶中。执行恢复前系统会自动为当前状态创建临时安全副本（SafetyCopy）。
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
  gap: 14px;
  overflow: hidden;

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

  .tabs-container {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;

    :deep(.n-tabs) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
    }

    :deep(.n-tabs-pane-wrapper) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    :deep(.n-tab-pane) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }
  }

  .tab-pane-content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
}
</style>
