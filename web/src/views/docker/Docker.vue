<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NCard,
  NSpace,
  NButton,
  NCheckbox,
  NDataTable,
  NTabs,
  NTabPane,
  NTag,
  NEllipsis,
  NPopconfirm,
  NInput,
  NIcon,
  NModal,
  NForm,
  NFormItem,
  NDynamicInput,
  NAlert,
  NSpin,
  useMessage,
} from 'naive-ui'
import {
  RefreshOutline,
  TrashOutline,
  DownloadOutline,
  TerminalOutline,
  ConstructOutline,
  AddOutline,
  SpeedometerOutline,
} from '@vicons/ionicons5'
import {
  dockerAll,
  dockerOp,
  dockerPsAll,
  dockerSystemDf,
  dockerGetMirrors,
  dockerSetMirrors,
  getHost,
  installDocker,
} from '../../api/hosts'
import { listApps, type AppEntity } from '../../api/apps'
import TerminalPane from '../../components/host/TerminalPane.vue'
import { useAuthStore } from '../../stores/auth'
import { useWorkspaceStore } from '../../stores/workspace'
import { useTablePagination } from '../../composables/useTablePagination'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'Docker' })

const route = useRoute()
const router = useRouter()
const message = useMessage()
const hostId = route.params.id as string
const auth = useAuthStore()
const workspace = useWorkspaceStore()

// Installing Docker changes host state, matching the server-side role gate on
// POST /hosts/:id/docker/install-script.
const canInstall = computed(() => auth.role === 'admin' || auth.role === 'operator')

const loading = ref(false)
const tab = ref('containers')
const containers = ref<any[]>([])
const images = ref<any[]>([])

// usedImageKeys 是全部容器（docker ps -a，含已停止）引用的镜像键：容器
// Image（仓库:标签）与 ImageID。镜像表「使用状态」列据此判断使用/未使用；
// usageLoaded 为 false 时取数失败，该列显示 — 而不是把全部镜像错标成未使用。
const usedImageKeys = ref<Set<string>>(new Set())
const usageLoaded = ref(false)
const logsContainer = ref('')
const logsText = ref('')
const pullImageName = ref('')
// 大镜像拉取可耗时数分钟（服务端预算 5 分钟），按钮转圈让执行过程可见，
// 也防止重复点击并发拉取。
const pulling = ref(false)
const dockerNotInstalled = ref(false)

// 应用中心部署的容器带 watchman.app=<appID> 标签（Labels 在 docker ps
// --format {{json .}} 输出里），用来区分纳管容器与外部/临时容器。
const managedApps = ref<Map<string, AppEntity>>(new Map())

// 容器与镜像列表都可能很长（AGENTS.md 8.2），各自独立分页。
const containerCount = computed(() => containers.value.length)
const imageCount = computed(() => images.value.length)
const { pagination: containerPagination } = useTablePagination({ pageSize: 20, rowCount: containerCount })
const { pagination: imagePagination } = useTablePagination({ pageSize: 20, rowCount: imageCount })

// Container Terminal Modal state
const showContainerTerm = ref(false)
const targetContainer = ref('')

// Docker Installer state
const showInstallModal = ref(false)
const installing = ref(false)
const installOutput = ref('')

// 应用中心是唯一的部署入口；Docker 面板只保留观测与运维。
function openDeployApp() {
  workspace.openTab({
    key: `/apps/create`,
    title: '创建应用',
    path: `/apps/create?host_id=${hostId}`,
    closable: true,
    viewName: 'AppCreate',
  })
  router.push(`/apps/create?host_id=${hostId}`)
}

// loadAll fetches containers + images in a single round trip via the
// /docker/all endpoint, so the dashboard doesn't pay two sequential calls.
async function loadAll() {
  loading.value = true
  try {
    const res = await dockerAll(hostId)
    containers.value = res.containers || []
    images.value = res.images || []
    dockerNotInstalled.value = false
  } catch (e: any) {
    if (
      e.message?.includes('not found') ||
      e.message?.includes('executable file not found') ||
      e.message?.includes('Docker 未安装') ||
      e.message?.includes('not in PATH')
    ) {
      dockerNotInstalled.value = true
      containers.value = []
      images.value = []
    } else {
      message.error(e.message || '加载 Docker 数据失败')
    }
  } finally {
    loading.value = false
  }
}

// watchmanAppLabel 从 docker ps 的 Labels 字段里提取 watchman.app=<appID>。
// `docker ps --format {{json .}}` 把 Labels 渲染成逗号分隔的 key=value 字符串
// （`watchman.app=app_xxx,...`），而非 JSON 对象，两种形态都兼容。
function watchmanAppLabel(labels: unknown): string {
  if (!labels) return ''
  if (typeof labels === 'object') {
    const v = (labels as Record<string, string>)['watchman.app']
    return v || ''
  }
  if (typeof labels !== 'string') return ''
  try {
    const parsed = JSON.parse(labels)
    if (parsed && typeof parsed === 'object') {
      return parsed['watchman.app'] || ''
    }
  } catch {
    // Not JSON — fall through to the comma-separated key=value form.
  }
  const entry = labels
    .split(',')
    .map((kv) => kv.trim())
    .find((kv) => kv.startsWith('watchman.app='))
  return entry ? entry.slice('watchman.app='.length) : ''
}

// loadManagedApps 拉一次全局应用清单，建立 watchman.app 标签 -> 应用实体的
// 映射，供容器列表的「纳管状态」列跳转应用详情。
async function loadManagedApps() {
  try {
    const apps = (await listApps()) as AppEntity[]
    const m = new Map<string, AppEntity>()
    for (const a of apps) {
      if (a.host_id === hostId) m.set(a.id, a)
    }
    managedApps.value = m
  } catch {
    managedApps.value = new Map()
  }
}

function openAppDetail(appID: string) {
  const app = managedApps.value.get(appID)
  workspace.openTab({
    key: `/apps/${appID}`,
    title: `应用 ${app?.name ?? appID}`,
    path: `/apps/${appID}`,
    closable: true,
    viewName: 'AppDetail',
  })
  router.push(`/apps/${appID}`)
}

async function loadContainers() {
  await loadAll()
}

async function loadImages() {
  // dockerAll 一次往返同时带回容器与镜像，没有单独的镜像列表接口；镜像操作
  // （拉取/删除/清理）后与手动刷新都必须无条件重拉，此前仅当列表为空才刷新，
  // 导致操作成功后页面停留在旧数据。
  // 使用状态另取一次 ps -a：dockerAll 的容器列表只含运行中容器，按它算会
  // 把已停止容器引用的镜像错标成未使用。
  await Promise.all([loadAll(), loadImageUsage()])
}

// loadImageUsage 取全部容器（含已停止）建立「被引用镜像」集合，供镜像表
// 「使用状态」列判断。取数失败时 usageLoaded 保持 false，该列显示 —。
async function loadImageUsage() {
  try {
    const all = await dockerPsAll(hostId)
    const keys = new Set<string>()
    for (const c of all || []) {
      if (c.Image) keys.add(c.Image)
      if (c.ImageID) keys.add(String(c.ImageID))
    }
    usedImageKeys.value = keys
    usageLoaded.value = true
  } catch {
    usageLoaded.value = false
  }
}

// normImageId 去掉 sha256: 前缀：docker ps 与 docker images 给出的 ID
// 可能是完整摘要或 12 位短 ID，比较时按前缀归一。
function normImageId(id: string): string {
  return (id || '').replace(/^sha256:/, '')
}

// imageUsed 按 Docker 口径判断镜像是否被任意容器引用：仓库:标签命中，
// 或 ID 前缀互含（ps 的 ImageID 与 images 的 ID 长度可能不同，
// 仅当双方都是十六进制 ID 时才做前缀比较，防仓库名误撞）。
function imageUsed(row: any): boolean {
  if (usedImageKeys.value.has(`${row.Repository}:${row.Tag}`)) return true
  const id = normImageId(row.ID || row.Id || '')
  if (!id || !/^[0-9a-f]+$/.test(id)) return false
  for (const k of usedImageKeys.value) {
    const nk = normImageId(k)
    if (nk && /^[0-9a-f]+$/.test(nk) && (nk === id || nk.startsWith(id) || id.startsWith(nk))) {
      return true
    }
  }
  return false
}

async function doOp(op: string, container: string) {
  // Optimistic update: tweak the local container state immediately so the UI
  // reacts without waiting for a full re-fetch. For start/stop/restart we
  // rewrite the Status field; for rm we drop the row. A background re-fetch
  // reconciles any drift.
  const name = container.replace(/^\//, '')
  const prev = containers.value.map((c) => ({ ...c }))
  try {
    if (op === 'rm') {
      containers.value = containers.value.filter(
        (c) => (c.Names || c.ID || '').replace(/^\//, '') !== name,
      )
    } else if (op === 'start' || op === 'stop' || op === 'restart') {
      containers.value = containers.value.map((c) => {
        const cn = (c.Names || c.ID || '').replace(/^\//, '')
        if (cn === name) {
          return {
            ...c,
            // 乐观值只写裸 Up（本地化后显示「运行中」），后台重拉后对账。
            Status: op === 'stop' ? 'Exited' : 'Up',
            State: op === 'stop' ? 'exited' : 'running',
          }
        }
        return c
      })
    }
    await dockerOp(hostId, op, container)
    message.success(`${op} 成功`)
    // Reconcile with a background refresh (no await, no loading spinner).
    loadAll()
  } catch (e: any) {
    // Roll back on failure.
    containers.value = prev
    message.error(e.message)
  }
}

function openContainerTerminal(container: string) {
  targetContainer.value = container.replace(/^\//, '')
  showContainerTerm.value = true
}

async function viewLogs(container: string) {
  logsContainer.value = container
  tab.value = 'logs'
  try {
    const res = await dockerOp(hostId, 'logs', container)
    logsText.value = res?.raw || res?.data || JSON.stringify(res, null, 2) || ''
  } catch (e: any) {
    logsText.value = e.message
  }
}

async function removeImage(imageId: string) {
  try {
    await dockerOp(hostId, 'remove_image', '', imageId)
    message.success('镜像已删除')
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function pruneImages() {
  try {
    await dockerOp(hostId, 'prune_images')
    message.success('已清除悬空镜像')
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  }
}

// Docker 深度清理弹窗状态
const showDeepClean = ref(false)
const deepCleanLoading = ref(false)
const deepCleanRunning = ref(false)
const deepCleanVolumes = ref(false)
const deepCleanDf = ref<any[]>([])
const deepCleanResult = ref<string[]>([])
const deepCleanError = ref('')

// docker system df 的 Type 字段是英文原值，预览表展示中文。
function dfTypeLabel(t: unknown): string {
  switch (t) {
    case 'Images':
      return '镜像'
    case 'Containers':
      return '容器'
    case 'Local Volumes':
      return '本地卷'
    case 'Build Cache':
      return '构建缓存'
    default:
      return typeof t === 'string' ? t : '未知'
  }
}

const deepCleanColumns = [
  { title: '类型', key: 'typeLabel', width: 90 },
  { title: '总数', key: 'TotalCount', width: 70 },
  { title: '活跃', key: 'Active', width: 70 },
  { title: '占用', key: 'Size', width: 110 },
  { title: '可回收', key: 'Reclaimable' },
]

// system_prune 各步骤的英文名 → 结果行中文。
const deepCleanStepLabels: Record<string, string> = {
  containers: '已停止容器',
  images: '未使用镜像',
  networks: '未使用网络',
  builder: '构建缓存',
}

async function openDeepClean() {
  showDeepClean.value = true
  deepCleanResult.value = []
  deepCleanError.value = ''
  deepCleanVolumes.value = false
  await loadDeepCleanDf()
}

async function loadDeepCleanDf() {
  deepCleanLoading.value = true
  try {
    const rows = await dockerSystemDf(hostId)
    deepCleanDf.value = (rows || []).map((r: any) => ({ ...r, typeLabel: dfTypeLabel(r.Type) }))
    deepCleanError.value = ''
  } catch (e: any) {
    deepCleanDf.value = []
    deepCleanError.value = e.message
  } finally {
    deepCleanLoading.value = false
  }
}

// volume_prune 走通用 dockerOp 通道，返回的是纯文本 raw；回收量从
// Total reclaimed space 行解析。
function reclaimedFromRaw(raw: unknown): string {
  const text = typeof raw === 'string' ? raw : ''
  const m = text.match(/Total reclaimed space:\s*(.+)/)
  return m ? m[1].trim() : ''
}

async function runDeepClean() {
  deepCleanRunning.value = true
  deepCleanResult.value = []
  deepCleanError.value = ''
  try {
    const res = await dockerOp(hostId, 'system_prune')
    const steps = res?.data?.steps
    if (Array.isArray(steps)) {
      for (const s of steps) {
        const label = deepCleanStepLabels[s.name] || s.name
        deepCleanResult.value.push(`${label}：已回收 ${s.reclaimed || '未知'}`)
      }
    } else {
      deepCleanResult.value.push('清理已完成')
    }
    if (deepCleanVolumes.value) {
      const vres = await dockerOp(hostId, 'volume_prune')
      const line = reclaimedFromRaw(vres?.raw ?? vres?.data)
      deepCleanResult.value.push(`未使用卷：已回收 ${line || '未知'}`)
    }
    // 清理后刷新容器/镜像列表与预览（可回收量应归零）。
    await loadAll()
    await loadDeepCleanDf()
  } catch (e: any) {
    deepCleanError.value = e.message
  } finally {
    deepCleanRunning.value = false
  }
}

async function pullImage() {
  if (!pullImageName.value) {
    message.warning('请输入镜像名称')
    return
  }
  if (pulling.value) return
  pulling.value = true
  try {
    await dockerOp(hostId, 'pull', '', pullImageName.value)
    message.success(`镜像 ${pullImageName.value} 拉取成功`)
    pullImageName.value = ''
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  } finally {
    pulling.value = false
  }
}

async function startInstallDocker() {
  installing.value = true
  installOutput.value = '>>> 正在下发服务端内置的 Docker 一键安装脚本（优先使用国内 docker-ce 镜像源，全部不可用时回退官方脚本）...\n'
  try {
    const res = await installDocker(hostId)
    if (res.ok) {
      installOutput.value += '\n>>> 安装执行完成！\n' + (res.stdout || '')
      message.success('Docker 安装完成，正在重新加载...')
      dockerNotInstalled.value = false
      setTimeout(() => {
        loadAll()
      }, 2000)
    } else {
      installOutput.value += '\n>>> 执行异常 (Exit code ' + res.exit_code + '):\n' + (res.stderr || res.stdout || '')
      message.error('Docker 安装未成功，请检查输出日志')
    }
  } catch (e: any) {
    installOutput.value += '\n>>> 安装请求失败: ' + (e.message || e)
    message.error('安装请求失败: ' + e.message)
  } finally {
    installing.value = false
  }
}

// 镜像加速配置状态
const showMirrorsModal = ref(false)
const mirrorsLoading = ref(false)
const mirrorsSaving = ref(false)
const mirrorList = ref<string[]>([])

const PRESET_MIRRORS = [
  { label: 'DaoCloud 镜像站', value: 'https://docker.m.daocloud.io' },
  { label: '1ms 加速源', value: 'https://docker.1ms.run' },
  { label: 'Docker 代理加速', value: 'https://dockerproxy.net' },
  { label: 'AtomHub 镜像', value: 'https://atomhub.openatom.cn' },
]

function addPresetMirror(val: string) {
  if (mirrorList.value.includes(val)) {
    message.info('该镜像加速源已在列表中')
    return
  }
  if (mirrorList.value.length === 1 && !mirrorList.value[0].trim()) {
    mirrorList.value[0] = val
  } else {
    mirrorList.value.push(val)
  }
}

async function openMirrorsModal() {
  showMirrorsModal.value = true
  mirrorsLoading.value = true
  try {
    const res = await dockerGetMirrors(hostId)
    mirrorList.value = res.mirrors && res.mirrors.length > 0 ? [...res.mirrors] : ['']
  } catch (e: any) {
    message.error(e.message || '获取镜像加速配置失败')
    mirrorList.value = ['']
  } finally {
    mirrorsLoading.value = false
  }
}

async function handleSaveMirrors() {
  const clean = mirrorList.value
    .map((m) => m.trim())
    .filter(Boolean)

  for (const m of clean) {
    if (!/^https?:\/\//i.test(m)) {
      message.warning(`加速源地址格式不正确（需以 http:// 或 https:// 开头）：${m}`)
      return
    }
  }

  mirrorsSaving.value = true
  try {
    await dockerSetMirrors(hostId, clean)
    message.success('Docker 镜像加速配置已更新并成功重载守护进程')
    showMirrorsModal.value = false
  } catch (e: any) {
    message.error(e.message || '更新 Docker 镜像加速失败')
  } finally {
    mirrorsSaving.value = false
  }
}

// docker ps 的状态串是英文原始值（如 Up 7 weeks、Exited (0) 3 days ago），
// 直接展示用户读不懂，按中文习惯本地化；未知形态原样返回，不编造。
const DURATION_UNIT_ZH: Record<string, string> = {
  second: '秒',
  seconds: '秒',
  minute: '分钟',
  minutes: '分钟',
  hour: '小时',
  hours: '小时',
  day: '天',
  days: '天',
  week: '周',
  weeks: '周',
  month: '月',
  months: '月',
  year: '年',
  years: '年',
}

function localizeDuration(text: string): string {
  return text.replace(
    /(\d+)\s*(seconds?|minutes?|hours?|days?|weeks?|months?|years?)/g,
    (_m, n: string, unit: string) => `${n} ${DURATION_UNIT_ZH[unit] ?? unit}`,
  )
}

function localizeContainerStatus(status: string): string {
  if (!status) return '-'
  const s = status.trim()
  const healthMatch = s.match(/\((healthy|unhealthy|health: starting)\)\s*$/)
  const healthZh: Record<string, string> = {
    healthy: '健康',
    unhealthy: '不健康',
    'health: starting': '健康检查启动中',
  }
  const base = healthMatch ? s.slice(0, healthMatch.index).trim() : s
  const healthSuffix = healthMatch ? `（${healthZh[healthMatch[1]]}）` : ''
  if (/^Up\b/i.test(base)) {
    const rest = base.replace(/^Up\b/i, '').trim()
    if (!rest) return `运行中${healthSuffix}`
    if (/^About an hour$/i.test(rest)) return `已运行约 1 小时${healthSuffix}`
    if (/^About a minute$/i.test(rest)) return `已运行约 1 分钟${healthSuffix}`
    if (/^Less than a second$/i.test(rest)) return `已运行不到 1 秒${healthSuffix}`
    return `已运行 ${localizeDuration(rest)}${healthSuffix}`
  }
  const exited = base.match(/^Exited\s*\((\d+)\)\s*(.*)$/i)
  if (exited) {
    const ago = exited[2] ? `${localizeDuration(exited[2].replace(/\s*ago\s*$/i, ''))}前` : ''
    return ago ? `已退出 ${ago}（退出码 ${exited[1]}）` : `已退出（退出码 ${exited[1]}）`
  }
  if (/^Exited$/i.test(base)) return `已退出${healthSuffix}`
  if (/^Created$/i.test(base)) return `已创建${healthSuffix}`
  if (/^Paused$/i.test(base)) return `已暂停${healthSuffix}`
  if (/^Restarting$/i.test(base)) return `重启中${healthSuffix}`
  if (/^Removing$/i.test(base)) return `删除中${healthSuffix}`
  if (/^Dead$/i.test(base)) return `已失效${healthSuffix}`
  if (/^running$/i.test(base)) return '运行中'
  if (/^exited$/i.test(base)) return '已退出'
  return status
}

const containerColumns = [
  {
    title: '名称',
    key: 'Names',
    width: 140,
    // 容器名超长截断时悬停显示全名：内置 ellipsis tooltip 默认单行铺开、无
    // 宽度上限，弹层会被视口裁掉（DESIGN.md §9.4），故限宽 + 任意断行。
    // render 去掉 docker ps 自带的前导 /（如 /watchman-heads），与启停等
    // 操作里用的去斜杠名字保持一致。
    ellipsis: {
      tooltip: {
        style: { maxWidth: '480px' },
        contentStyle: { wordBreak: 'break-all' as const },
      },
    },
    render: (row: any) => (row.Names || row.ID || '').replace(/^\//, ''),
  },
  { title: '镜像', key: 'Image', ellipsis: { tooltip: true } },
  {
    title: '状态',
    key: 'Status',
    width: 180,
    render: (row: any) => {
      const running = row.Status?.includes('Up') || row.State === 'running'
      return h(
        NTag,
        { type: running ? 'success' : 'default', size: 'small', round: true, class: 'container-status-tag' },
        {
          default: () =>
            h(NEllipsis, { tooltip: true }, { default: () => localizeContainerStatus(row.Status || row.State || '') }),
        },
      )
    },
  },
  { title: '端口', key: 'Ports', ellipsis: { tooltip: true } },
  {
    title: '纳管状态',
    key: 'managed',
    width: 110,
    render: (row: any) => {
      const appID = watchmanAppLabel(row.Labels)
      if (appID && managedApps.value.get(appID)) {
        return h(
          NButton,
          { size: 'tiny', tertiary: true, type: 'info', onClick: () => openAppDetail(appID) },
          { default: () => '应用中心纳管' },
        )
      }
      if (appID) {
        return h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => '纳管记录已删除' })
      }
      return h(NTag, { size: 'small', bordered: false }, { default: () => '外部/临时' })
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 280,
    render: (row: any) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(
            NButton,
            {
              size: 'tiny',
              type: 'primary',
              quaternary: true,
              onClick: () => openContainerTerminal(row.Names || row.ID),
            },
            {
              icon: () => h(NIcon, { component: TerminalOutline }),
              default: () => '终端',
            },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('start', row.Names) },
            { default: () => '启动' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('stop', row.Names) },
            { default: () => '停止' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('restart', row.Names) },
            { default: () => '重启' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => viewLogs(row.Names) },
            { default: () => '日志' },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => doOp('rm', row.Names) },
            {
              trigger: () =>
                h(NButton, { size: 'tiny', quaternary: true, type: 'error' }, { default: () => '删除' }),
              default: () => `确认删除容器 ${row.Names}?`,
            },
          ),
        ],
      }),
  },
]

const imageColumns = [
  { title: '仓库', key: 'Repository', ellipsis: { tooltip: true } },
  { title: '标签', key: 'Tag', width: 100 },
  { title: 'ID', key: 'ID', width: 80, ellipsis: { tooltip: true } },
  { title: '大小', key: 'Size', width: 80 },
  {
    title: '使用状态',
    key: 'usage',
    width: 90,
    render: (row: any) => {
      // 使用状态取数失败时显示 —：宁可未知，不把镜像错标成未使用。
      if (!usageLoaded.value) {
        return h('span', { style: 'color: var(--text-color-3)' }, '—')
      }
      return imageUsed(row)
        ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '使用中' })
        : h(NTag, { size: 'small', bordered: false }, { default: () => '未使用' })
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 100,
    render: (row: any) =>
      h(
        NPopconfirm,
        { onPositiveClick: () => removeImage(row.ID || row.Id) },
        {
          trigger: () =>
            h(
              NButton,
              { size: 'tiny', quaternary: true, type: 'error' },
              {
                icon: () => h(NIcon, { component: TrashOutline }),
                default: () => '删除',
              },
            ),
          default: () => `确认删除镜像 ${row.Repository}:${row.Tag}?`,
        },
      ),
  },
]

// 挂载时自注册 Workspace 页签（与 HostDetail.load() 同款）：直接访问
// /docker/:id 时 AppShell 不会自动建签，标题取主机名、拉不到时回落主机 ID。
// viewName 取路由 meta（Docker），与组件 defineOptions 的 name 一致，缺了
// 它该页签不会进 KeepAlive 的 include。
// 守卫：本组件也被主机详情以 DockerView 内嵌在 docker 子页签里，那时当前
// 路由是 host-detail，页签属于主机详情——注册会抢走激活态并多出一个
// /docker/:id 页签，故只在独立路由（route.name === 'docker'）时注册。
async function registerWorkspaceTab() {
  if (route.name !== 'docker') return
  let title = hostId
  try {
    const host = await getHost(hostId)
    if (host?.hostname) title = host.hostname
  } catch {
    // 主机信息拉不到也不阻塞页签注册，标题回落主机 ID。
  }
  workspace.openTab({
    key: `/docker/${hostId}`,
    title,
    path: `/docker/${hostId}`,
    closable: true,
    viewName: (route.meta.viewName as string) || 'Docker',
  })
}

onMounted(() => {
  loadContainers()
  loadManagedApps()
  registerWorkspaceTab()
})
</script>

<template>
  <div class="docker-view page-flex-column">
    <div class="docker-toolbar">
      <h2 class="page-title">Docker 管理</h2>
    </div>

    <!-- 未安装 Docker 时的引导卡片 -->
    <NAlert
      v-if="dockerNotInstalled"
      type="warning"
      title="未检测到 Docker 环境"
      closable
      class="docker-alert"
    >
      <div style="display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 4px">
        <span v-if="canInstall">当前主机可能尚未安装 Docker 引擎或守护进程未启动。点击右侧按钮可通过自动化脚本一键安装配置 Docker 环境（国内镜像源优先，官方源兜底）。</span>
        <span v-else>当前主机可能尚未安装 Docker 引擎或守护进程未启动。安装需要管理员或运维角色，请联系管理员处理。</span>
        <NButton
          v-if="canInstall"
          type="warning"
          size="small"
          @click="showInstallModal = true; startInstallDocker()"
        >
          <template #icon><NIcon :component="ConstructOutline" /></template>
          一键安装 Docker
        </NButton>
      </div>
    </NAlert>

    <!-- Tab 在卡片外（DESIGN.md §9.5）：tabs-container 包 NTabs，各页签内
         tab-pane-content 包 NCard.table-flex-fill，再放工具栏与表格。 -->
    <div class="tabs-container">
      <NTabs
        v-model:value="tab"
        type="line"
        @update:value="(v: string) => { if (v === 'images' && images.length === 0) loadImages() }"
      >
        <NTabPane name="containers" tab="容器" display-directive="show:lazy">
          <div class="tab-pane-content">
            <NCard :bordered="false" class="table-flex-fill">
              <div class="tab-body">
                <NSpace align="center" justify="space-between" class="tab-toolbar">
                  <NSpace align="center" :size="8">
                    <NButton size="small" :loading="loading" @click="loadContainers">
                      <template #icon><NIcon :component="RefreshOutline" /></template>
                      刷新
                    </NButton>
                    <NButton size="small" type="primary" @click="openDeployApp">
                      <template #icon><NIcon :component="AddOutline" /></template>
                      部署新应用
                    </NButton>
                  </NSpace>
                  <NButton
                    v-if="dockerNotInstalled && canInstall"
                    size="small"
                    type="warning"
                    @click="showInstallModal = true; startInstallDocker()"
                  >
                    一键安装 Docker
                  </NButton>
                </NSpace>
                <NDataTable
                  flex-height
                  :columns="containerColumns"
                  :data="containers"
                  :pagination="containerPagination"
                  :bordered="false"
                  size="small"
                  :loading="loading"
                  :scroll-x="900"
                >
                  <template #empty>无容器或 Docker 未运行</template>
                </NDataTable>
              </div>
            </NCard>
          </div>
        </NTabPane>

        <NTabPane name="images" tab="镜像" display-directive="show:lazy">
          <div class="tab-pane-content">
            <NCard :bordered="false" class="table-flex-fill">
              <div class="tab-body">
                <NSpace align="center" justify="space-between" class="tab-toolbar">
                  <NSpace align="center">
                    <NButton size="small" :loading="loading" @click="loadImages">
                      <template #icon><NIcon :component="RefreshOutline" /></template>
                      刷新
                    </NButton>
                    <NInput
                      v-model:value="pullImageName"
                      placeholder="nginx:latest"
                      size="small"
                      style="width: 200px"
                    />
                    <NButton
                      size="small"
                      type="primary"
                      :loading="pulling"
                      @click="pullImage"
                    >
                      <template #icon><NIcon :component="DownloadOutline" /></template>
                      拉取
                    </NButton>
                    <NPopconfirm @positive-click="pruneImages">
                      <template #trigger>
                        <NButton size="small" type="warning">清除悬空镜像</NButton>
                      </template>
                      确认清除所有悬空镜像？
                    </NPopconfirm>
                    <NButton size="small" type="error" @click="openDeepClean">
                      深度清理
                    </NButton>
                  </NSpace>
                  <NButton size="small" @click="openMirrorsModal">
                    <template #icon><NIcon :component="SpeedometerOutline" /></template>
                    镜像加速
                  </NButton>
                </NSpace>
                <NDataTable
                  flex-height
                  :columns="imageColumns"
                  :data="images"
                  :pagination="imagePagination"
                  :bordered="false"
                  size="small"
                  :loading="loading"
                >
                  <template #empty>无镜像或 Docker 未运行</template>
                </NDataTable>
              </div>
            </NCard>
          </div>
        </NTabPane>

        <NTabPane name="logs" tab="容器日志" display-directive="show:lazy">
          <div class="tab-pane-content">
            <NCard :bordered="false" class="table-flex-fill">
              <div class="tab-body">
                <span class="muted">容器: {{ logsContainer || '请从容器列表点击"日志"' }}</span>
                <pre class="log-output log-output-fill">{{ logsText || '暂无日志' }}</pre>
              </div>
            </NCard>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <!-- 容器终端直连弹窗 -->
    <NModal
      v-model:show="showContainerTerm"
      preset="card"
      :title="`容器终端直连: ${targetContainer}`"
      style="width: 80vw; max-width: 1000px; height: 600px"
    >
      <div style="height: 500px">
        <TerminalPane
          v-if="showContainerTerm"
          :host-id="hostId"
          :shell="`docker exec -it ${targetContainer} /bin/sh`"
        />
      </div>
    </NModal>

    <!-- Docker 一键安装实时日志弹窗 -->
    <NModal
      v-model:show="showInstallModal"
      preset="card"
      title="一键安装 Docker"
      style="width: 650px"
    >
      <NSpace vertical size="medium">
        <NAlert v-if="installing" type="info">
          正在远程执行安装脚本，由于依赖网络下载，可能需要 1~3 分钟，请耐心等待...
        </NAlert>
        <pre class="log-output install-log">{{ installOutput }}</pre>
        <NSpace justify="end">
          <NButton :disabled="installing" @click="showInstallModal = false">
            关闭
          </NButton>
        </NSpace>
      </NSpace>
    </NModal>

    <!-- Docker 镜像加速配置弹窗 -->
    <NModal
      v-model:show="showMirrorsModal"
      preset="card"
      title="Docker 镜像加速配置"
      style="width: 580px"
    >
      <NSpin :show="mirrorsLoading">
        <NForm label-placement="top" size="small">
          <NAlert type="info" :bordered="false" style="margin-bottom: 14px">
            配置后将写入主机的 Docker 守护进程配置文件（Linux 为 <code>/etc/docker/daemon.json</code>），并通过优雅重载（<code>reload</code>）生效，无需重启运行中的容器。
          </NAlert>
          <NFormItem label="常用加速源快捷添加">
            <NSpace :size="6" style="margin-bottom: 4px">
              <NButton
                v-for="preset in PRESET_MIRRORS"
                :key="preset.value"
                size="tiny"
                secondary
                @click="addPresetMirror(preset.value)"
              >
                + {{ preset.label }}
              </NButton>
            </NSpace>
          </NFormItem>
          <NFormItem label="镜像加速源地址 (Registry Mirrors)">
            <NDynamicInput
              v-model:value="mirrorList"
              placeholder="https://..."
              :min="1"
              :on-create="() => ''"
            />
          </NFormItem>
        </NForm>
      </NSpin>
      <template #footer>
        <NSpace justify="end">
          <NButton :disabled="mirrorsSaving" @click="showMirrorsModal = false">取消</NButton>
          <NButton
            type="primary"
            :loading="mirrorsSaving"
            :disabled="mirrorsLoading"
            @click="handleSaveMirrors"
          >
            保存并重载 Docker
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- Docker 深度清理弹窗 -->
    <NModal
      v-model:show="showDeepClean"
      preset="card"
      title="Docker 深度清理"
      style="width: 620px"
    >
      <NSpace vertical size="medium">
        <NAlert type="warning" :bordered="false">
          将清理已停止的容器、未使用的镜像、未使用的网络与构建缓存。运行中的容器与正在使用的镜像不受影响；应用中心纳管的已停止容器（含 -prev 回滚容器）同样保留，不会被删除。
        </NAlert>
        <NSpin :show="deepCleanLoading">
          <NDataTable
            :columns="deepCleanColumns"
            :data="deepCleanDf"
            :bordered="false"
            size="small"
          >
            <template #empty>暂无磁盘占用数据</template>
          </NDataTable>
        </NSpin>
        <NCheckbox v-model:checked="deepCleanVolumes" :disabled="deepCleanRunning">
          同时清理未使用的卷（docker volume prune）
        </NCheckbox>
        <NAlert v-if="deepCleanError" type="error" :bordered="false">
          {{ deepCleanError }}
        </NAlert>
        <NAlert v-if="deepCleanResult.length" type="success" :bordered="false">
          <div v-for="line in deepCleanResult" :key="line">{{ line }}</div>
        </NAlert>
      </NSpace>
      <template #footer>
        <NSpace justify="end">
          <NButton :disabled="deepCleanRunning" @click="showDeepClean = false">关闭</NButton>
          <NButton
            type="error"
            :loading="deepCleanRunning"
            :disabled="deepCleanLoading || deepCleanDf.length === 0"
            @click="runDeepClean"
          >
            开始清理
          </NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.docker-view {
  gap: 12px;
  overflow: hidden;
}

.docker-toolbar {
  flex-shrink: 0;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: var(--text-primary);
}

.docker-alert {
  flex-shrink: 0;
}

/* Tab 在卡片外（DESIGN.md §9.5）：tabs-container 与 pane 都要具备 flex
   能力，否则表格无法按视口高度收缩、分页条会被推到首屏之外。 */
.tabs-container {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  overflow: hidden;

  :deep(.n-tabs) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  :deep(.n-tabs-pane-wrapper),
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

.tab-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
  /* 全局 .table-flex-fill 会把卡片内容区 padding 清零，这里按 §9.2 在
     内容层补回 --card-padding，保持工具栏与表格离卡片边一个档位。 */
  padding: var(--card-padding, 16px);

  .tab-toolbar {
    flex-shrink: 0;
  }

  :deep(.n-data-table) {
    flex: 1;
    min-height: 0;
  }
}

/* 状态标签默认按内容撑开，长状态串（如 Up 13 hours (healthy)）会溢出
   定宽列压住端口列，这里让它收缩到列宽内并由 NEllipsis 出省略号。 */
:deep(.container-status-tag) {
  max-width: 100%;

  .n-tag__content {
    min-width: 0;
    overflow: hidden;
  }
}

/* 日志页签：填满剩余高度而不是固定 400px */
.log-output-fill {
  flex: 1;
  min-height: 0;
  max-height: none;
  margin: 0;
}

.muted {
  color: var(--text-secondary);
  font-size: 12px;
}

.log-output {
  background: var(--code-box-bg);
  color: var(--text-primary);
  border: 1px solid var(--border-color);
  padding: 12px;
  border-radius: 4px;
  font-size: 13px;
  white-space: pre-wrap;
  max-height: 400px;
  overflow: auto;

  &.install-log {
    max-height: 320px;
    background: var(--cmd-box-bg);
    color: var(--cmd-text-color);
    border: 1px solid var(--border-color);
  }
}
</style>
