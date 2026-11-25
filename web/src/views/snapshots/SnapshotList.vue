<script setup lang="ts">
import { computed, h, onActivated, onMounted, reactive, ref, watch } from 'vue'
import {
  NAlert, NButton, NCard, NCheckbox, NDataTable, NEmpty, NForm, NFormItem, NIcon, NInput,
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
import { listApps, type AppEntity } from '../../api/apps'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTimeMinute } from '../../utils/time'

defineOptions({ name: 'SnapshotList' })

const message = useMessage()
const auth = useAuthStore()
const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')

const activeTab = ref<'jobs' | 's3'>('jobs')

// ---- 备份任务列表状态 ----
const jobs = ref<BackupJob[]>([])
const loading = ref(false)
const showCreate = ref(false)
const creating = ref(false)

// 备份任务前端分页（§9.4：全量数组已在内存，走 useTablePagination）。
const jobCount = computed(() => jobs.value.length)
const { pagination: jobPagination, resetPage: resetJobPage } = useTablePagination({ pageSize: 20, rowCount: jobCount })

// ---- 批量删除备份任务（范式见 NetworkList.vue） ----
// 勾选按任务 id 存、跨页保留；执行期 3 路并发、逐行显示进度、失败可重试。
// 注意：本文件已有的 selectedJob / selectedAppID 是归档抽屉与新建表单的
// 选择态，与批量勾选的 selectedJobIds 无关，不得混用。
const selectedJobIds = ref<Set<string>>(new Set())
const BATCH_CONCURRENCY = 3
type BatchPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
const showBatchDeleteModal = ref(false)

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
// 挂载卷多选（'ALL' 哨兵 = 整应用全部卷，与后端 snapshots.AppVolumeTargetAll 对应）。
const selectedAppVolumes = ref<string[]>([])
const jobAllVolumes = ref(false)

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

// 任务目标卷的提交值：勾选「全部」或选中了全部选项时归一为 ALL（整应用任务
// 会跟随后续新增的挂载卷），否则提交逗号拼接的多选清单（与后端契约一致）。
function appVolumeJobTarget(): string {
  if (jobAllVolumes.value ||
      (selectedAppVolumes.value.length > 0 && selectedAppVolumes.value.length === appVolumeOptions.value.length)) {
    return 'ALL'
  }
  return selectedAppVolumes.value.join(',')
}

// 任务「目标对象」的挂载卷文案：ALL → 全部；多选 → N 个卷。
function appVolumeTargetLabel(target: string): string {
  if (target === 'ALL') return '全部挂载卷'
  if (target.includes(',')) return `${target.split(',').length} 个挂载卷`
  return target
}

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
  ...(canWrite.value
    ? [{
        // 批量勾选列：NDataTable 复选框列取 40px（BatchExec.vue 同款口径；
        // DESIGN.md §9.20 的 28px 是 flex 行列表口径，复选框 + 单元格内边距放不下）。
        type: 'selection' as const,
        width: 40,
        disabled: () => batchRunning.value,
      }]
    : []),
  {
    title: '任务名称',
    key: 'name',
    width: 170,
    ellipsis: { tooltip: true },
    render: (r) => h('div', null, [
      h('span', { style: 'font-weight: 600' }, r.name),
      batchProgressNode(r),
    ]),
  },
  {
    title: '类型',
    key: 'kind',
    width: 110,
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
    minWidth: 220,
    ellipsis: { tooltip: true },
    render: (r) => {
      if (r.kind === 'app_volume') {
        const app = apps.value.find((a) => a.id === r.app_id)
        const appName = app?.name || r.app_id || '应用已删除'
        // 单行 flex、不换行：应用名超长时省略（title 给全名），卷文案不换行不压缩。
        return h('div', {
          style: {
            display: 'flex', alignItems: 'center', gap: '6px', minWidth: 0,
          },
        }, [
          h('span', {
            style: {
              fontWeight: 500, overflow: 'hidden', textOverflow: 'ellipsis',
              whiteSpace: 'nowrap', minWidth: 0, flex: '1 1 auto',
            },
            title: appName,
          }, appName),
          h('span', {
            class: 'mono-font',
            style: { fontSize: '11px', whiteSpace: 'nowrap', flexShrink: 0 },
            title: r.target,
          }, appVolumeTargetLabel(r.target)),
        ])
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
  { title: '调度计划', key: 'schedule', width: 100, render: (r) => r.schedule?.cron ? h('code', { style: 'font-size: 12px' }, r.schedule.cron) : '仅手动' },
  { title: '保留', key: 'retention', width: 64, render: (r) => `${r.retention} 份` },
  {
    title: '最近执行',
    key: 'last_run',
    width: 150,
    render: (r) => {
      if (!r.last_run) return '-'
      const hasErr = Boolean(r.last_error)
      return h(NSpace, { size: 4, align: 'center', wrap: false }, {
        default: () => [
          h('span', { style: 'font-size: 12px', whiteSpace: 'nowrap' }, formatTime(r.last_run)),
          hasErr ? h(NTag, { size: 'tiny', type: 'error', bordered: false }, { default: () => '失败' }) : null,
        ],
      })
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 224,
    render: (r) =>
      h(NSpace, { size: 6, wrap: false }, {
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

async function loadData(resetPageOnLoad = false) {
  loading.value = true
  try {
    jobs.value = await listBackupJobs()
    // 刷新清掉上一批的旧进度；批量执行中不清除（runBatchDelete 在执行完才刷新，
    // 此时 batchRunning 仍为 true，进度得以保留到汇总展示）。
    if (!batchRunning.value) {
      opStatus.value = {}
      batchSummary.value = null
    }
    // 手动刷新回第 1 页（§9.4）；批量后或单次操作后刷新保留当前页并由 rowCount 自动夹回有效页。
    if (resetPageOnLoad) {
      resetJobPage()
    }
  } catch (e: any) {
    message.error(e.message || '获取备份任务失败')
  } finally {
    loading.value = false
  }
}

function isOpBusy(id: string) {
  const st = opStatus.value[id]
  return !!st && (st.phase === 'queued' || st.phase === 'running')
}

function batchRowClassName(row: BackupJob) {
  return isOpBusy(row.id) ? 'batch-row-busy' : ''
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
  selectedAppVolumes.value = []
  jobAllVolumes.value = false
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
    if (!jobAllVolumes.value && selectedAppVolumes.value.length === 0) {
      message.warning('请选择要备份的挂载卷，或勾选全部挂载卷')
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
      target: appVolumeJobTarget(),
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
// 单列表按应用排序：应用数据卷任务按所属应用名升序（应用 ID 本身是
// app_xxx 随机串，按它排看不出规律，故按名字排）；无所属应用的任务
// （数据库/卷/目录）排在最后；同应用内按任务名称排序。
const sortedJobs = computed(() => {
  const appName = (j: BackupJob) => {
    if (j.kind !== 'app_volume') return ''
    return apps.value.find((a) => a.id === j.app_id)?.name || j.app_id || '应用已删除'
  }
  return [...jobs.value].sort((a, b) => {
    const an = appName(a)
    const bn = appName(b)
    if (!an && bn) return 1
    if (an && !bn) return -1
    if (an !== bn) return an.localeCompare(bn, 'zh-CN')
    return a.name.localeCompare(b.name, 'zh-CN')
  })
})

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
    selectedJobIds.value.delete(job.id)
    await loadData()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

// ---- 批量删除备份任务（NetworkList.vue 范式：前端并发编排、复用单机接口） ----

// 批量执行行内进度（NetworkList .op-progress 同款文案与色板）：render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透（见文末样式）。
function batchProgressNode(row: { id: string }) {
  const st = opStatus.value[row.id]
  if (!st) return null
  const text =
    st.phase === 'queued' ? '排队中…'
    : st.phase === 'running' ? '执行中…'
    : st.phase === 'done' ? '已完成'
    : `失败：${st.message || '执行出错'}`
  return h('div', { class: ['batch-op-progress', `is-${st.phase}`], title: st.message || '' }, text)
}

const selectedJobs = computed(() => jobs.value.filter((j) => selectedJobIds.value.has(j.id)))

// 当前页任务（前端分页切片，排序后切片）：勾选同步以当前页为基准合并，
// 跨页已选保持不变。
const pagedJobs = computed(() => {
  const start = (jobPagination.page - 1) * jobPagination.pageSize
  return sortedJobs.value.slice(start, start + jobPagination.pageSize)
})

function onJobChecked(keys: (string | number)[]) {
  const pageIds = new Set(pagedJobs.value.map((j) => j.id))
  const next = new Set([...selectedJobIds.value].filter((id) => !pageIds.has(id)))
  keys.forEach((k) => {
    if (pageIds.has(String(k))) next.add(String(k))
  })
  selectedJobIds.value = next
}

// 任务在列表里消失（被删除）后清理对应的勾选与进度。
watch(jobs, (list) => {
  const alive = new Set(list.map((j) => j.id))
  selectedJobIds.value = new Set([...selectedJobIds.value].filter((id) => alive.has(id)))
  Object.keys(opStatus.value).forEach((id) => {
    if (!alive.has(id)) delete opStatus.value[id]
  })
})

async function runBatchDelete(targets: BackupJob[]) {
  if (targets.length === 0) {
    message.warning('所选任务中没有可删除的')
    return
  }
  batchRunning.value = true
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((j) => {
    opStatus.value[j.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...targets]
  async function worker() {
    while (queue.length > 0) {
      const job = queue.shift()!
      opStatus.value[job.id] = { phase: 'running' }
      try {
        // 复用单机删除接口（只删任务定义，历史归档随任务删除口径与单机一致），
        // 审计走既有单机接口的审计，无需新埋点。
        await deleteBackupJob(job.id)
        opStatus.value[job.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[job.id] = { phase: 'failed', message: e.message || '执行出错' }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, targets.length) }, () => worker()),
  )
  // 批量执行完再刷新列表（batchRunning 仍为 true，loadData 的清进度守卫
  // 不触发）：成功删除的行随之消失，失败行保留进度供重试。
  await loadData()
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped: 0 }
  if (failed === 0) {
    message.success(`批量删除完成：成功 ${done} 个备份任务`)
  } else {
    message.warning(`批量删除完成：成功 ${done} 个，失败 ${failed} 个，可点「重试失败」只重跑失败的任务`)
  }
}

function retryBatchFailed() {
  if (!batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  runBatchDelete(jobs.value.filter((j) => failedIds.includes(j.id)))
}

function confirmBatchDelete() {
  showBatchDeleteModal.value = false
  runBatchDelete(selectedJobs.value)
}

function clearBatchSelection() {
  selectedJobIds.value = new Set()
  opStatus.value = {}
  batchSummary.value = null
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
          <NButton :loading="loading || loadingS3" @click="() => { loadData(true); loadS3() }">
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
        <!-- 备份任务列表：单表，按应用排序（应用数据卷任务按所属应用名升序，
          无所属应用的任务排最后），不再按应用分多张卡片。 -->
        <NTabPane name="jobs" tab="备份任务列表">
          <div class="tab-pane-content">
            <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐行显示进度 -->
            <div v-if="canWrite && selectedJobIds.size > 0" class="batch-bar">
              <span class="batch-info">已选 {{ selectedJobIds.size }} 个备份任务</span>
              <NButton
                size="small"
                type="error"
                secondary
                :loading="batchRunning"
                :disabled="batchRunning"
                @click="showBatchDeleteModal = true"
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
                成功 {{ batchSummary.done }} · 失败 {{ batchSummary.failed }}
              </span>
              <NButton size="small" quaternary :disabled="batchRunning" @click="clearBatchSelection">
                取消选择
              </NButton>
            </div>
            <NCard :bordered="false" size="small" class="table-flex-fill job-list-card" :header-style="{ paddingLeft: '8px' }">
              <template #header>
                <span class="job-list-title">备份任务（共 {{ jobs.length }} 个任务）</span>
              </template>
              <NDataTable
                flex-height
                :columns="columns"
                :data="sortedJobs"
                :row-key="(r: BackupJob) => r.id"
                :checked-row-keys="[...selectedJobIds]"
                :row-class-name="batchRowClassName"
                :scroll-x="canWrite ? 1200 : 1160"
                :pagination="jobPagination"
                :bordered="false"
                size="small"
                @update:checked-row-keys="onJobChecked"
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

    <!-- 批量删除二次确认（DESIGN.md §9.17：破坏性确认在弹窗内就地展开） -->
    <NModal
      v-model:show="showBatchDeleteModal"
      preset="card"
      :title="`批量删除备份任务（${selectedJobs.length} 个）`"
      style="width: 520px"
    >
      <div class="batch-confirm-body">
        <div class="batch-target-list">
          <div
            v-for="j in selectedJobs.slice(0, 5)"
            :key="j.id"
            class="batch-target-row"
          >
            {{ j.name }}
          </div>
          <div v-if="selectedJobs.length > 5" class="batch-target-row muted">
            等 {{ selectedJobs.length - 5 }} 个…
          </div>
        </div>
        <p class="field-hint">删除备份任务后将不再按计划执行新的备份，请确认。</p>
      </div>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBatchDeleteModal = false">取消</NButton>
          <NButton type="error" :loading="batchRunning" @click="confirmBatchDelete">
            确认删除
          </NButton>
        </NSpace>
      </template>
    </NModal>

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
                @update:value="selectedAppVolumes = []"
              />
            </NFormItem>
            <NFormItem label="挂载卷" required style="flex: 1">
              <div class="appvol-select-col">
                <NSelect
                  v-model:value="selectedAppVolumes"
                  multiple
                  clearable
                  :options="appVolumeOptions"
                  :disabled="!selectedAppID || jobAllVolumes"
                  :placeholder="jobAllVolumes ? '全部挂载卷' : '选择要备份的卷（可多选）'"
                />
                <NCheckbox
                  :checked="jobAllVolumes"
                  :disabled="!selectedAppID"
                  @update:checked="jobAllVolumes = $event"
                >
                  全部挂载卷（新增卷自动纳入备份）
                </NCheckbox>
              </div>
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
        <!-- NDataTable flex-height 必须自身有 flex: 1 才能拿到可用高度：
          早前表格直接塞进 flex 列，表体高度算出来是 0，只剩表头和分页条，
          5 条归档全渲染在 0 高滚动区里（AppDetail 部署历史同款教训）。 -->
        <div style="flex: 1; min-height: 0; display: flex; flex-direction: column">
          <NDataTable
            flex-height
            style="flex: 1; min-height: 0"
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
// 应用数据卷多选 + 「全部」勾选的纵向排布（挂载卷表单项内部）。
.appvol-select-col {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
}

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

  /* 列表标题与设置分区卡片标题同款：16px/700（NCard small 默认标题字重偏轻，
     与 600 的表头比层级不突出）。标题左缘对齐首列文字：small 表格单元格
     横向内边距 8px，卡片 header 左内边距覆写为 8px（模板 :header-style）。 */
  .job-list-title {
    font-size: 16px;
    font-weight: 700;
  }

  /* 标题行底部分隔线：标题与表头色带之间划一道线，标题行不再悬浮。
     用 var(--border-color)，禁用 --n-border-color（卡片内继承为透明，见 §9.6）。 */
  .job-list-card :deep(.n-card-header) {
    flex-shrink: 0;
    border-bottom: 1px solid var(--border-color);
  }
}

// 批量操作栏（NetworkList.vue 同款）
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 8px 12px;
  flex-shrink: 0;

  .batch-info {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .batch-summary {
    font-size: 12px;
    color: var(--text-secondary);
  }
}

/* 批量执行中的行：操作列整体置灰禁用，防单机操作与批量并发冲突（DESIGN.md §9.22） */
:deep(.batch-row-busy [data-col-key="actions"]) {
  pointer-events: none;
  opacity: 0.5;
}

// 批量删除确认弹窗：目标清单内部滚动，行数多也不撑出视口
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

// 批量执行行内进度（NetworkList .op-progress 同款色板；render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透）。
:deep(.batch-op-progress) {
  font-size: 12px;
  max-width: 100%;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;

  &.is-queued,
  &.is-running {
    color: #3b82f6;
  }

  &.is-done {
    color: #059669;
  }

  &.is-failed {
    color: #dc2626;
  }
}
</style>
