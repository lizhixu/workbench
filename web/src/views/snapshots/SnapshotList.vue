<script setup lang="ts">
import { computed, h, onActivated, onMounted, reactive, ref, watch } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NEmpty, NForm, NFormItem, NIcon, NInput,
  NInputNumber, NModal, NPopconfirm, NRadio, NRadioButton, NRadioGroup, NSelect,
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
import { listApps, type AppEntity } from '../../api/apps'
import { fmtDateTimeMinute } from '../../utils/time'

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

// ---- 应用数据卷（app_volume）任务 ----
const apps = ref<AppEntity[]>([])
// NSelect 空值必须用 null（AGENTS.md §8.3.1），'' 会渲染幽灵选中态。
const selectedAppID = ref<string | null>(null)
const selectedAppVolume = ref<string | null>(null)

const selectedApp = computed(() => apps.value.find((a) => a.id === selectedAppID.value) || null)
const defaultS3 = computed(() => s3Targets.value.find((t) => t.is_default) || null)

// Compose 应用的卷在编排 YAML 内声明，引擎拒绝卷备份；无声明卷的应用也无可备份对象。
const appOptions = computed(() =>
  apps.value
    .filter((a) => {
      if (a.source_type === 'raw_compose') return false
      if (a.source_type === 'git' && a.build_type === 'compose') return false
      return (a.volumes?.length ?? 0) > 0
    })
    .map((a) => {
      const host = hostOptions.value.find((o) => o.value === a.host_id)
      return { label: host ? `${a.name} · ${host.label}` : a.name, value: a.id }
    }),
)

const appVolumeOptions = computed(() =>
  (selectedApp.value?.volumes ?? []).map((v) => ({ label: v, value: v })),
)

// 应用数据卷归档仅支持 S3：不提供「本地主机」选项，也给不出默认目标时引导去配置。
const appVolumeStorageOptions = computed(() => {
  const opts: { label: string; value: string }[] = []
  if (defaultS3.value) {
    opts.push({ label: `默认 S3 目标 (${defaultS3.value.name} - ${defaultS3.value.bucket})`, value: 'default' })
  }
  for (const t of s3Targets.value) {
    opts.push({ label: `S3: ${t.name} (${t.bucket})`, value: t.id })
  }
  return opts
})

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

watch(() => form.kind, (kind) => {
  if (kind !== 'app_volume') return
  // 「本地主机」对 app_volume 不可用，切过来时校正到默认或首个 S3 目标。
  const valid = form.storage_target === 'default'
    ? Boolean(defaultS3.value)
    : s3Targets.value.some((t) => t.id === form.storage_target)
  if (!valid) form.storage_target = defaultS3.value ? 'default' : (s3Targets.value[0]?.id ?? 'default')
})

const kindOptions = [
  { label: '应用数据卷（应用中心应用）', value: 'app_volume' },
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
      const label = r.kind === 'app_volume'
        ? '应用数据卷'
        : r.kind === 'database' ? `DB (${r.db_type?.toUpperCase()})` : r.kind === 'volume' ? 'Docker 卷' : '主机目录'
      return h(NTag, {
        size: 'small',
        bordered: false,
        type: r.kind === 'app_volume' ? 'success' : 'info',
      }, { default: () => label })
    },
  },
  {
    title: '目标对象',
    key: 'target',
    ellipsis: { tooltip: true },
    render: (r) => {
      if (r.kind === 'app_volume') {
        const app = apps.value.find((a) => a.id === r.app_id)
        return h(NSpace, { size: 6, align: 'center' }, {
          default: () => [
            h('span', { style: 'font-weight: 500' }, app?.name || r.app_id || '应用已删除'),
            h('span', { class: 'mono-font', style: { fontSize: '11px' } }, r.target),
          ],
        })
      }
      return r.target + (r.db_name ? ` / ${r.db_name}` : '')
    },
  },
  {
    title: '存储位置',
    key: 'storage_target',
    width: 170,
    render: (r) => {
      const st = r.storage_target
      if (!st || st === 'default') {
        if (defaultS3.value) {
          return h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => `S3: ${defaultS3.value!.name}` })
        }
        return r.kind === 'app_volume'
          ? h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => '未配置默认 S3' })
          : h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
      }
      if (st === 'local') {
        return r.kind === 'app_volume'
          ? h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => '仅支持 S3' })
          : h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
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

function archiveStatusTag(status?: string, source?: string) {
  // 上传恢复记录无 S3 对象、状态恒为 uploaded，不按成功/失败渲染。
  if (source === 'upload') {
    return h(NTag, { size: 'tiny', type: 'info', bordered: false }, { default: () => '上传' })
  }
  // 旧类型归档只在成功终态落记录，status 字段缺省视为成功。
  if (!status || status === 'success') {
    return h(NTag, { size: 'tiny', type: 'success', bordered: false }, { default: () => '成功' })
  }
  if (status === 'running') {
    return h(NTag, { size: 'tiny', type: 'info', bordered: false }, { default: () => '执行中' })
  }
  return h(NTag, { size: 'tiny', type: 'error', bordered: false }, { default: () => '失败' })
}

const archiveColumns = computed<DataTableColumns<Archive>>(() => {
  const appVolume = selectedJob.value?.kind === 'app_volume'
  const cols: DataTableColumns<Archive> = [
    {
      title: '归档编号',
      key: 'id',
      width: 190,
      ellipsis: { tooltip: true },
      render: (r) => h('span', { class: 'mono-font', style: { fontSize: '12px' } }, r.id),
    },
  ]
  if (appVolume) {
    cols.push({
      title: '备份卷',
      key: 'target',
      width: 170,
      ellipsis: { tooltip: true },
      render: (r) => h('span', { class: 'mono-font', style: { fontSize: '11px' } }, r.target),
    })
    cols.push({
      title: '状态',
      key: 'status',
      width: 82,
      render: (r) => archiveStatusTag(r.status, r.source),
    })
  }
  cols.push({
    title: '存储位置',
    key: 'storage_target',
    width: 160,
    render: (r) => {
      // 上传恢复记录无 S3 对象：文件只在面板暂存、恢复结束即删，不是本地主机。
      if (r.source === 'upload') {
        return h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => '上传文件' })
      }
      // app_volume 归档冻结了备份时的 S3 目标名，切换默认目标后仍按记录展示。
      if (r.storage_target_name) {
        return h(NTag, { size: 'small', type: 'success', bordered: false }, {
          default: () => `S3: ${r.storage_target_name}`,
        })
      }
      const st = r.storage_target
      if (!st || st === 'local') {
        return h(NTag, { size: 'small', bordered: false }, { default: () => '本地主机' })
      }
      const target = s3Targets.value.find((t) => t.id === st)
      return h(NTag, { size: 'small', type: 'success', bordered: false }, {
        default: () => target ? `S3: ${target.name}` : 'S3 远端',
      })
    },
  })
  cols.push({
    title: '大小',
    key: 'size',
    width: 96,
    render: (r) => (r.size > 0 ? formatBytes(r.size) : '—'),
  })
  cols.push({
    title: 'SHA256 校验和',
    key: 'sha256',
    width: 130,
    render: (r) => r.sha256
      ? h('span', { class: 'mono-font', style: { fontSize: '11px' }, title: r.sha256 }, r.sha256.slice(0, 12) + '…')
      : '—',
  })
  cols.push({
    title: '创建时间',
    key: 'created_at',
    width: 150,
    render: (r) => formatTime(r.created_at),
  })
  if (appVolume) {
    cols.push({
      title: '最近恢复',
      key: 'last_restore_at',
      width: 170,
      render: (r) => {
        if (!r.last_restore_at) return '—'
        const ok = r.last_restore_status === 'success'
        return h('div', null, [
          h(NSpace, { size: 4, align: 'center' }, {
            default: () => [
              h(NTag, { size: 'tiny', type: ok ? 'success' : 'error', bordered: false }, {
                default: () => (ok ? '成功' : '失败'),
              }),
              h('span', { class: 'mono-font', style: { fontSize: '11px' } }, formatTime(r.last_restore_at)),
            ],
          }),
          !ok && r.last_restore_error
            ? h('div', { style: { fontSize: '11px', color: 'var(--error-color, #d03050)', marginTop: '2px' } }, r.last_restore_error)
            : null,
        ])
      },
    })
    cols.push({
      title: '错误信息',
      key: 'error',
      ellipsis: { tooltip: true },
      render: (r) => (r.error
        ? h('span', { style: { fontSize: '12px', color: 'var(--error-color, #d03050)' } }, r.error)
        : '—'),
    })
  }
  cols.push({
    title: '操作',
    key: 'actions',
    width: 150,
    fixed: 'right' as const,
    render: (r) => {
      const running = r.status === 'running'
      const canRestore = !appVolume || r.status === 'success'
      return h(NSpace, { size: 6 }, {
        default: () => [
          h(
            NPopconfirm,
            { onPositiveClick: () => doRestore(r), disabled: !canRestore },
            {
              trigger: () => h(NButton, {
                size: 'tiny',
                secondary: true,
                type: 'warning',
                disabled: !canRestore,
              }, { default: () => '恢复' }),
              default: () => appVolume
                ? '确定恢复该数据卷归档？系统会先停机应用容器，校验 SHA256 后覆盖数据并自动重启。'
                : `确定从该归档恢复？恢复前会自动在主机生成现场快照（SafetyCopy）。`,
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDeleteArchive(r), disabled: running },
            {
              trigger: () => h(NButton, {
                size: 'tiny',
                secondary: true,
                type: 'error',
                disabled: running,
              }, {
                icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
              }),
              default: () => (running ? '备份任务执行中，不能删除该归档。' : '确定删除此归档记录？'),
            },
          ),
        ],
      })
    },
  })
  return cols
})

function formatTime(v?: string) {
  if (!v) return '-'
  return fmtDateTimeMinute(v)
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

async function loadApps() {
  try {
    apps.value = await listApps()
  } catch {
    apps.value = []
  }
}

function openCreateJob() {
  selectedAppID.value = null
  selectedAppVolume.value = null
  showCreate.value = true
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
  let payload: CreateJobReq
  if (form.kind === 'app_volume') {
    if (!selectedAppID.value) {
      message.warning('请选择要备份的应用')
      return
    }
    if (!selectedAppVolume.value) {
      message.warning('请选择要备份的挂载卷')
      return
    }
    if (!s3Targets.value.length) {
      message.warning('应用数据卷归档仅支持 S3 存储，请先在「S3 异地存储配置」添加存储桶')
      return
    }
    payload = {
      ...form,
      host_id: '',
      app_id: selectedAppID.value,
      target: selectedAppVolume.value,
      db_type: '',
      db_name: '',
    }
  } else {
    if (!form.host_id) {
      message.warning('请选择目标主机')
      return
    }
    if (!form.target.trim()) {
      message.warning('请填写备份目标对象')
      return
    }
    payload = { ...form, app_id: '' }
  }
  creating.value = true
  try {
    await createBackupJob(payload)
    message.success(form.kind === 'app_volume' ? '应用数据卷备份任务已创建' : '备份任务已创建')
    showCreate.value = false
    await loadData()
  } catch (e: any) {
    message.error(e.message || '创建失败')
  } finally {
    creating.value = false
  }
}

// ---- 按应用聚合视图 ----
// 应用详情页可以直接建本应用的备份任务，快照页则按应用聚合展示：
// app_volume 任务按 app_id 分组，其他种类任务仍平铺在「其他备份任务」。
const jobView = ref<'flat' | 'byApp'>('byApp')

const appVolumeGroups = computed(() => {
  const groups: { appId: string; appName: string; jobs: BackupJob[] }[] = []
  const byId = new Map<string, { appId: string; appName: string; jobs: BackupJob[] }>()
  for (const j of jobs.value) {
    if (j.kind !== 'app_volume') continue
    let g = byId.get(j.app_id || '')
    if (!g) {
      const app = apps.value.find((a) => a.id === j.app_id)
      g = { appId: j.app_id || '', appName: app?.name || j.app_id || '应用已删除', jobs: [] }
      byId.set(j.app_id || '', g)
      groups.push(g)
    }
    g.jobs.push(j)
  }
  return groups
})

const otherJobs = computed(() => jobs.value.filter((j) => j.kind !== 'app_volume'))

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
    // app_volume 恢复为异步任务（停机→校验→解压→重启），稍后刷新结果列。
    if (selectedJob.value.kind === 'app_volume') {
      setTimeout(fetchArchives, 4000)
    }
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
  loadApps()
})

onActivated(() => {
  loadData()
  loadS3()
  loadApps()
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
          <NButton v-else type="primary" @click="openCreateJob">
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
            <div class="job-view-toolbar">
              <span class="tip-hint" style="font-size: 12px; color: var(--text-secondary)">按应用聚合时，应用数据卷任务按应用分组，其他类型任务归入「其他备份任务」。</span>
              <NRadioGroup v-model:value="jobView" size="small">
                <NRadioButton value="byApp">按应用聚合</NRadioButton>
                <NRadioButton value="flat">平铺</NRadioButton>
              </NRadioGroup>
            </div>
            <!-- 聚合视图：每组任务数天然有限，属 §8.2 例外：不分页；组列表自身滚动（外层 overflow hidden，见下方样式） -->
            <div v-if="jobView === 'byApp' && jobs.length > 0" class="job-group-scroll">
              <div v-for="g in appVolumeGroups" :key="g.appId" class="job-group-card">
                <NCard :bordered="false" size="small" :title="`${g.appName}（${g.jobs.length} 个任务）`" :content-style="{ padding: 0 }">
                  <NDataTable
                    :columns="columns"
                    :data="g.jobs"
                    :row-key="(r: BackupJob) => r.id"
                    :bordered="false"
                    size="small"
                  />
                </NCard>
              </div>
              <NCard v-if="otherJobs.length > 0" :bordered="false" size="small" title="其他备份任务" :content-style="{ padding: 0 }">
                <NDataTable
                  :columns="columns"
                  :data="otherJobs"
                  :row-key="(r: BackupJob) => r.id"
                  :bordered="false"
                  size="small"
                />
              </NCard>
            </div>
            <NCard v-else :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="columns"
                :data="jobs"
                :row-key="(r: BackupJob) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                size="small"
              >
                <template #empty>
                  <NEmpty description="暂无备份任务，点右上角「新建备份任务」创建一个" size="small" />
                </template>
              </NDataTable>
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

        <NFormItem label="备份类型" required>
          <NRadioGroup v-model:value="form.kind">
            <NSpace vertical>
              <NRadio v-for="opt in kindOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</NRadio>
            </NSpace>
          </NRadioGroup>
        </NFormItem>

        <template v-if="form.kind === 'app_volume'">
          <NSpace>
            <NFormItem label="应用" required style="flex: 1">
              <NSelect
                v-model:value="selectedAppID"
                :options="appOptions"
                placeholder="选择已声明挂载卷的应用"
                filterable
                @update:value="selectedAppVolume = null"
              />
            </NFormItem>
            <NFormItem label="挂载卷" required style="flex: 1">
              <NSelect
                v-model:value="selectedAppVolume"
                :options="appVolumeOptions"
                :disabled="!selectedAppID"
                placeholder="选择要备份的卷"
              />
            </NFormItem>
          </NSpace>
          <NAlert v-if="!appOptions.length" type="info" :show-icon="true" style="margin-bottom: 12px">
            暂无可备份的应用：仅支持已声明挂载卷的非 Compose 应用（Compose 卷在编排 YAML 内声明，请使用快照备份的其他类型）。
          </NAlert>
          <NAlert v-if="!s3Targets.length" type="warning" :show-icon="true" style="margin-bottom: 12px">
            应用数据卷归档仅支持 S3 存储。请先到「S3 异地存储配置」添加存储桶。
          </NAlert>
        </template>

        <NFormItem v-if="form.kind !== 'app_volume'" label="目标主机" required>
          <NSelect v-model:value="form.host_id" :options="hostOptions" placeholder="选择受管主机" filterable />
        </NFormItem>

        <NFormItem label="备份存储位置 (可独立配置)" required>
          <NSelect
            v-model:value="form.storage_target"
            :options="form.kind === 'app_volume' ? appVolumeStorageOptions : storageTargetOptions"
            placeholder="选择备份归档存放目标"
          />
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

        <template v-else-if="form.kind === 'dir'">
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
          <template v-if="selectedJob?.kind === 'app_volume'">
            数据卷归档保存在 S3。恢复会先停机应用容器、校验 SHA256 后覆盖数据并自动重启；备份为在线文件级快照，数据库类卷建议停写后备份。
          </template>
          <template v-else>
            归档保存在本地主机或 S3 存储桶中。执行恢复前系统会自动为当前状态创建临时安全副本（SafetyCopy）。
          </template>
        </NAlert>
        <div style="flex: 1; min-height: 0">
          <NDataTable
            flex-height
            :scroll-x="1180"
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
      <template #footer>
        <NSpace justify="end">
          <NButton :loading="loadingArchives" @click="fetchArchives">刷新</NButton>
          <NButton type="primary" @click="showArchives = false">关闭</NButton>
        </NSpace>
      </template>
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

  /* 备份任务视图切换栏：左侧说明 + 右侧分段控件，窄屏自动换行 */
  .job-view-toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
    margin-bottom: 10px;
    flex-shrink: 0;
  }

  .job-group-scroll {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    overscroll-behavior: contain;
  }

  .job-group-card {
    margin-bottom: 12px;
  }

  .job-group-card:last-child {
    margin-bottom: 0;
  }
}
</style>
