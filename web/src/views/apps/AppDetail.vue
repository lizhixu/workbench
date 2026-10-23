<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import {
  NAlert, NButton, NCode, NDataTable, NDescriptions, NDescriptionsItem,
  NEmpty, NForm, NFormItem, NIcon, NInput, NInputNumber, NModal, NPopconfirm, NRadio, NRadioGroup,
  NSelect, NSpace, NSwitch, NTabPane, NTabs,
  NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  ArrowBackOutline, BuildOutline, CloudUploadOutline, CopyOutline,
  DownloadOutline,
  GitCommitOutline, PlayOutline, RefreshOutline, SparklesOutline, StopOutline,
  SwapHorizontalOutline, TrashOutline,
} from '@vicons/ionicons5'
import { useTablePagination } from '../../composables/useTablePagination'
import {
  cancelDeployment, deployApp, diagnoseDeployment, getApp, getAppStatus, listDeployments, restartApp,
  rollbackApp, startApp, stopApp, updateApp, syncAppWebhook, type AppEntity, type AppStatus, type Deployment,
  type AIDiagnosis,
  backupAppVolumes, listVolumeBackups, restoreVolumeArchive, deleteVolumeArchive,
  volumeArchiveDownloadUrl, type VolumeArchive, type VolumeS3Status,
} from '../../api/apps'
import { bindProxy, unbindProxy, type Certificate } from '../../api/certs'
import { listCerts } from '../../api/certs'
import { listHosts } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import { bothMeshed } from '../../utils/mesh'
import { copyToClipboard } from '../../utils/clipboard'
import { useAuthStore } from '../../stores/auth'
import OpsTunnelModal from '../../components/tunnels/OpsTunnelModal.vue'

defineOptions({ name: 'AppDetail' })

const route = useRoute()
const message = useMessage()
const auth = useAuthStore()

const appId = computed(() => String(route.params.id ?? ''))
const app = ref<AppEntity | null>(null)
const status = ref<AppStatus | null>(null)
const deployments = ref<Deployment[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const acting = ref(false)
const activeTab = ref('overview')

// Deployment log viewer.
const showLog = ref(false)
const logDeployment = ref<Deployment | null>(null)

// Ops tunnel one-click entry (kubectl port-forward style): opened from a
// port mapping row, prefilled with that host port.
const showTunnel = ref(false)
const tunnelPort = ref(0)
const tunnelPrefill = computed(() => ({
  appId: app.value?.id ?? appId.value,
  appName: app.value?.name,
  port: tunnelPort.value,
}))
function openTunnel(port: number) {
  tunnelPort.value = port
  showTunnel.value = true
}

// AI Deployment diagnosis.
const showAIDiagnosis = ref(false)
const diagnosing = ref(false)
const aiResult = ref<AIDiagnosis | null>(null)
const diagnosingDepId = ref('')

async function doDiagnose(dep: Deployment) {
  diagnosingDepId.value = dep.id
  diagnosing.value = true
  aiResult.value = null
  showAIDiagnosis.value = true
  try {
    aiResult.value = await diagnoseDeployment(appId.value, dep.id)
  } catch (e: any) {
    message.error(e.message || 'AI 诊断失败')
  } finally {
    diagnosing.value = false
  }
}

// ---- Reverse proxy binding (domain + cert + nginx distribution) ----
const certs = ref<Certificate[]>([])
const hostsList = ref<{ id: string; hostname: string; status: string; public_ip?: string }[]>([])

// 网关候选按「有公网 IP」优先排序并标注组网/直连路径。应用信息与主机列表是
// 并发加载的（onMounted 同时 refreshAll/loadProxyData），故这里是 computed
// 而非一次性构建——否则 app 晚到时「禁用应用主机自身」和路径标注会基于空 app。
const hostOptions = computed(() => {
  const appMesh = app.value ? meshIPById.value.get(app.value.host_id) || '' : ''
  const list = hostsList.value
    .filter((h) => h.status === 'online')
    .map((h) => {
      const mesh = meshIPById.value.get(h.id)
      // 网关到应用主机：双方都在组网内则走 100.x，否则直连（公网/内网 IP）——组网非必需
      const path = mesh && appMesh ? '组网直达' : '直连'
      const recommended = !!h.public_ip
      const tags = [h.public_ip ? `公网 ${h.public_ip}` : '无公网 IP', mesh ? `组网 ${mesh}` : '未组网', path]
      return {
        label: `${h.hostname}（${tags.join(' · ')}${recommended ? '，推荐' : ''}）`,
        value: h.id,
        // 网关主机不能是应用主机自身（相同场景即 local 模式）
        disabled: h.id === app.value?.host_id,
        rank: recommended ? 0 : 1,
      }
    })
  list.sort((a, b) => a.rank - b.rank)
  return list
})
// 网关主机必须自身已加入组网（网关需经组网访问应用主机），据此标注/禁用选项
const meshIPById = ref<Map<string, string>>(new Map())
const proxyForm = ref({
  domain: '',
  mode: 'local' as 'local' | 'gateway',
  // NSelect 绑定字段空值用 null：空串会被 naive-ui fallbackOption 合成空 label 选项，
  // 让 select 自认为已选中、占位符永不渲染。
  gateway_host_id: null as string | null,
  upstream: '',
  cert_id: '',
  websocket: true,
})
const savingProxy = ref(false)

async function loadProxyData() {
  try {
    certs.value = (await listCerts()) as Certificate[]
  } catch {
    certs.value = []
  }
  try {
    const nodes = (await listNetworkNodes()) ?? []
    meshIPById.value = new Map(nodes.filter((n) => n.ip).map((n) => [n.host_id, n.ip]))
  } catch {
    meshIPById.value = new Map()
  }
  try {
    const res = await listHosts()
    const hosts = ((res as { data: { id: string; hostname: string; status: string; public_ip?: string }[] }).data ?? []) as { id: string; hostname: string; status: string; public_ip?: string }[]
    hostnameById.value = new Map(hosts.map((h) => [h.id, h.hostname]))
    hostsList.value = hosts
  } catch {
    hostsList.value = []
  }
  if (app.value) {
    proxyForm.value.domain = app.value.domain ?? ''
    proxyForm.value.mode = (app.value.proxy_mode as 'local' | 'gateway') ?? 'local'
    proxyForm.value.upstream = app.value.proxy_upstream ?? ''
  }
}

const certOptions = computed(() => [
  { label: '按域名自动匹配', value: '' },
  ...certs.value.map((c) => ({ label: c.domains.join(', '), value: c.id })),
])

// 拓扑展示用：应用所在主机与网关主机的名称
const hostnameById = ref<Map<string, string>>(new Map())

const appHostname = computed(() => {
  if (!app.value) return ''
  return hostnameById.value.get(app.value.host_id) || app.value.host_id
})

const gatewayHostname = computed(() => {
  if (!app.value?.proxy_gateway_host_id) return ''
  return hostnameById.value.get(app.value.proxy_gateway_host_id) || app.value.proxy_gateway_host_id
})

const forwardHostname = computed(() => {
  if (!app.value?.mesh_forward_host_id) return ''
  return hostnameById.value.get(app.value.mesh_forward_host_id) || app.value.mesh_forward_host_id
})

const appHostMeshIP = computed(() => {
  if (!app.value?.host_id) return ''
  return meshIPById.value.get(app.value.host_id) || ''
})

const forwardMeshLink = computed(() => {
  const appHostId = app.value?.host_id
  const fwdHostId = app.value?.mesh_forward_host_id || forwardForm.value.host_id
  if (!appHostId || !fwdHostId) return false
  return bothMeshed(meshIPById.value.get(appHostId), meshIPById.value.get(fwdHostId))
})

// 网关上游落在 Tailscale CGNAT 段（100.64.0.0/10）即视为走了异地组网，否则是直连
const gatewayUpstreamIsMesh = computed(() => {
  const ip = (app.value?.proxy_upstream ?? '').split(':')[0]
  const parts = ip.split('.')
  if (parts.length !== 4 || parts[0] !== '100') return false
  const second = Number(parts[1])
  return Number.isFinite(second) && second >= 64 && second <= 127
})

// ---- 组网端口转发表单与操作 ----
const forwardForm = ref({
  enabled: false,
  host_id: null as string | null, // 同 proxyForm.gateway_host_id：NSelect 空值用 null
  entry_port: null as number | null,
  target_port: null as number | null,
})
const savingForward = ref(false)

function syncForwardForm() {
  if (app.value) {
    forwardForm.value.enabled = !!app.value.mesh_forward_enabled
    forwardForm.value.host_id = app.value.mesh_forward_host_id ?? null
    forwardForm.value.entry_port = app.value.mesh_forward_entry_port || null
    forwardForm.value.target_port = app.value.mesh_forward_target_port || null
    if (!forwardForm.value.target_port && app.value.ports && app.value.ports.length > 0) {
      // 中继拨的是宿主机发布端口：预填 host 端口，未发布时才回退容器端口
      forwardForm.value.target_port = app.value.ports[0].host || app.value.ports[0].container || null
    }
  }
}

watch(
  () => forwardForm.value.enabled,
  (val) => {
    if (val) {
      if (!forwardForm.value.target_port && app.value?.ports && app.value.ports.length > 0) {
        forwardForm.value.target_port = app.value.ports[0].host || app.value.ports[0].container || null
      }
      if (!forwardForm.value.entry_port && forwardForm.value.target_port) {
        forwardForm.value.entry_port = forwardForm.value.target_port
      }
    }
  }
)

async function doSaveForward() {
  if (forwardForm.value.enabled) {
    if (!forwardForm.value.host_id) {
      message.warning('启用组网端口转发时必须选择入口主机')
      return
    }
    if (forwardForm.value.host_id === app.value?.host_id) {
      message.warning('组网转发入口主机不能与应用部署主机相同')
      return
    }
    if (!forwardForm.value.entry_port || forwardForm.value.entry_port < 1 || forwardForm.value.entry_port > 65535) {
      message.warning('请填写合法的入口监听端口（1-65535）')
      return
    }
    if (!forwardForm.value.target_port || forwardForm.value.target_port < 1 || forwardForm.value.target_port > 65535) {
      message.warning('请填写合法的应用目标端口（1-65535）')
      return
    }
  }
  savingForward.value = true
  try {
    await updateApp(appId.value, {
      mesh_forward_enabled: forwardForm.value.enabled,
      mesh_forward_host_id: forwardForm.value.enabled ? forwardForm.value.host_id || undefined : undefined,
      mesh_forward_entry_port: forwardForm.value.enabled ? forwardForm.value.entry_port ?? undefined : undefined,
      mesh_forward_target_port: forwardForm.value.enabled ? forwardForm.value.target_port ?? undefined : undefined,
    })
    message.success(forwardForm.value.enabled ? '组网端口转发已保存并同步中继容器' : '已关闭组网端口转发')
    await loadApp()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingForward.value = false
  }
}

async function doBindProxy() {
  if (!proxyForm.value.domain.trim()) {
    message.warning('请填写域名')
    return
  }
  if (proxyForm.value.mode === 'gateway') {
    if (!proxyForm.value.gateway_host_id) {
      message.warning('网关模式需选择网关主机')
      return
    }
    if (proxyForm.value.gateway_host_id === app.value?.host_id) {
      message.warning('网关主机不能与应用主机相同（相同场景请改用 local 模式）')
      return
    }
  }
  savingProxy.value = true
  try {
    await bindProxy(appId.value, {
      domain: proxyForm.value.domain.trim(),
      mode: proxyForm.value.mode,
      gateway_host_id: proxyForm.value.gateway_host_id || undefined,
      upstream: proxyForm.value.upstream.trim() || undefined,
      cert_id: proxyForm.value.cert_id || undefined,
      websocket: proxyForm.value.websocket,
    })
    message.success('反代配置已下发')
    await loadApp()
  } catch (e: any) {
    message.error(e.message || '下发失败')
  } finally {
    savingProxy.value = false
  }
}

async function doUnbindProxy() {
  savingProxy.value = true
  try {
    await unbindProxy(appId.value)
    message.success('已解除域名绑定')
    proxyForm.value.domain = ''
    await loadApp()
  } catch (e: any) {
    message.error(e.message || '解绑失败')
  } finally {
    savingProxy.value = false
  }
}

let pollTimer: ReturnType<typeof setInterval> | null = null

const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')
const deploying = computed(() => status.value?.deploying ?? false)

const webhookURL = computed(() => {
  if (!app.value?.webhook_token) return ''
  const base = window.location.origin
  return `${base}/api/v1/apps/webhook/${app.value.webhook_token}`
})

const sourceTypeLabel = computed(() => {
  switch (app.value?.source_type) {
    case 'git': return 'Git 仓库'
    case 'image': return '单镜像'
    case 'template': return '应用模板'
    case 'raw_compose': return 'Compose 编排'
    default: return app.value?.source_type ?? '-'
  }
})

const composeEditContent = ref('')
const savingCompose = ref(false)

async function loadApp() {
  try {
    app.value = await getApp(appId.value)
    if (app.value.compose_content) {
      composeEditContent.value = app.value.compose_content
    }
    syncForwardForm()
    syncEnvRows()
  } catch (e: any) {
    message.error(e.message || '获取应用信息失败')
  }
}

async function saveAndRedeployCompose() {
  if (!composeEditContent.value.trim()) {
    message.warning('Compose 内容不能为空')
    return
  }
  savingCompose.value = true
  try {
    await updateApp(appId.value, {
      compose_content: composeEditContent.value,
    })
    message.success('配置已保存，正在重新部署...')
    await doDeploy()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingCompose.value = false
  }
}

// ---- 应用配置编辑（概览页签的「编辑配置」弹窗）----
// PUT /apps/:id 是部分更新：这里只提交用户真正改过的字段，避免覆盖未展示
// 的配置（Git 凭据、自动部署、组网转发等）。已部署应用改完需重新部署生效，
// 弹窗内提供「保存并重新部署」。
const showEdit = ref(false)
const savingEdit = ref(false)
const editForm = ref({
  name: '',
  image: '',
  healthcheck_url: '',
  ports: [] as { host: number | null; container: number | null; bind_scope: 'public' | 'mesh' }[],
  volumes_text: '',
})

function openEdit() {
  if (!app.value) return
  editForm.value = {
    name: app.value.name,
    image: app.value.image ?? '',
    healthcheck_url: app.value.healthcheck_url ?? '',
    ports: (app.value.ports ?? []).map((p) => ({
      host: p.host,
      container: p.container,
      bind_scope: (p.bind_scope === 'mesh' ? 'mesh' : 'public') as 'public' | 'mesh',
    })),
    volumes_text: (app.value.volumes ?? []).join('\n'),
  }
  showEdit.value = true
}

const editIsDeployed = computed(() => {
  if (!app.value) return false
  return !!app.value.current_commit || app.value.source_type === 'raw_compose' || !!status.value?.state && status.value.state !== 'undeployed'
})

// 只发送真正改动过的字段（部分更新语义）。
function buildEditPatch() {
  const cur = app.value
  if (!cur) return {}
  const patch: Record<string, unknown> = {}
  const name = editForm.value.name.trim()
  if (name && name !== cur.name) patch.name = name
  if (cur.source_type === 'image') {
    const image = editForm.value.image.trim()
    if (image && image !== (cur.image ?? '')) patch.image = image
  }
  const health = editForm.value.healthcheck_url.trim()
  if (health !== (cur.healthcheck_url ?? '')) patch.healthcheck_url = health
  const ports = editForm.value.ports
    .filter((p) => (p.host ?? 0) > 0 && (p.container ?? 0) > 0)
    .map((p) => ({ host: Number(p.host), container: Number(p.container), bind_scope: p.bind_scope }))
  const curPorts = (cur.ports ?? []).map((p) => ({
    host: p.host,
    container: p.container,
    bind_scope: p.bind_scope === 'mesh' ? 'mesh' : 'public',
  }))
  if (JSON.stringify(ports) !== JSON.stringify(curPorts)) patch.ports = ports
  const volumes = editForm.value.volumes_text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0)
  if (JSON.stringify(volumes) !== JSON.stringify(cur.volumes ?? [])) patch.volumes = volumes
  return patch
}

async function saveEdit(redeploy: boolean) {
  const patch = buildEditPatch()
  if (Object.keys(patch).length === 0) {
    if (redeploy) await doDeploy()
    else message.info('没有改动需要保存')
    showEdit.value = false
    return
  }
  // 端口行填写不完整（只填了 host 或 container）时提示，避免静默丢弃。
  const incomplete = editForm.value.ports.some((p) => ((p.host ?? 0) > 0) !== ((p.container ?? 0) > 0))
  if (incomplete) {
    message.warning('端口映射需同时填写宿主机端口与容器端口')
    return
  }
  savingEdit.value = true
  try {
    await updateApp(appId.value, patch)
    message.success(redeploy ? '配置已保存，正在重新部署…' : '配置已保存')
    showEdit.value = false
    await loadApp()
    if (redeploy) await doDeploy()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingEdit.value = false
  }
}

// ---- 环境变量编辑 ----
// 行式编辑器，保留插入顺序；key 为空的行忽略。Vault 引用
// （{{ vault:<id>:<key> }}）按原文编辑，部署时由服务端解析。
const envRows = ref<{ key: string; value: string }[]>([])
const savingEnv = ref(false)

function syncEnvRows() {
  envRows.value = Object.entries(app.value?.env_vars ?? {}).map(([key, value]) => ({ key, value }))
}

function addEnvRow() {
  envRows.value.push({ key: '', value: '' })
}

function removeEnvRow(idx: number) {
  envRows.value.splice(idx, 1)
}

function collectEnvVars(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const row of envRows.value) {
    const k = row.key.trim()
    if (!k) continue
    out[k] = row.value
  }
  return out
}

async function saveEnv(redeploy: boolean) {
  const vars = collectEnvVars()
  for (const [k, v] of Object.entries(vars)) {
    if (!v.trim()) {
      message.warning(`环境变量「${k}」的值不能为空`)
      return
    }
  }
  savingEnv.value = true
  try {
    await updateApp(appId.value, { env_vars: vars })
    message.success(redeploy ? '环境变量已保存，正在重新部署…' : '环境变量已保存')
    await loadApp()
    if (redeploy) await doDeploy()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingEnv.value = false
  }
}

async function loadStatus() {
  try {
    status.value = await getAppStatus(appId.value)
  } catch {
    status.value = null
  }
}

async function loadDeployments() {
  loading.value = true
  try {
    const res = await listDeployments(appId.value, (page.value - 1) * pageSize.value, pageSize.value)
    deployments.value = res.data
    total.value = res.total
  } catch (e: any) {
    message.error(e.message || '获取部署历史失败')
  } finally {
    loading.value = false
  }
}

async function refreshAll() {
  await Promise.all([loadApp(), loadStatus(), loadDeployments(), loadVolumeBackups()])
}

const cancellingDeploy = ref(false)

async function doDeploy(force = false) {
  acting.value = true
  try {
    await deployApp(appId.value, force)
    message.success(force ? '已终止当前任务并重新启动部署' : '已开始部署')
    await Promise.all([loadStatus(), loadDeployments()])
  } catch (e: any) {
    message.error(e.message || '触发部署失败')
  } finally {
    acting.value = false
  }
}

async function doCancelDeploy() {
  cancellingDeploy.value = true
  try {
    const res = await cancelDeployment(appId.value)
    message.success(res.message || '部署任务已终止')
    await Promise.all([loadStatus(), loadDeployments()])
  } catch (e: any) {
    message.error(e.message || '终止部署失败')
  } finally {
    cancellingDeploy.value = false
  }
}

async function doRollback(dep?: Deployment) {
  acting.value = true
  try {
    await rollbackApp(appId.value, dep?.id)
    message.success(dep ? `正在回滚到 ${dep.commit_hash}` : '正在回滚到上一版本')
    await Promise.all([loadStatus(), loadDeployments()])
  } catch (e: any) {
    message.error(e.message || '回滚失败')
  } finally {
    acting.value = false
  }
}

// ---- 数据卷备份与恢复 ----
// 备份把卷数据目录打成 tar.gz 后直传 S3 默认存储目标（走快照备份的 S3 配置），
// 元数据在服务端；下载由服务端 302 到短期 S3 预签名 URL。恢复语义：下载并校验
// SHA256 → 自动停机 → 安全副本 → 解压覆盖 → 重启容器（stop 失败则中止，绝不在线
// 覆盖运行中的数据）。
const volumeBackups = ref<VolumeArchive[]>([])
const volumeBackupTotal = ref(0)
const volumeS3 = ref<VolumeS3Status>({ ready: false })
const volBackupNote = ref('')
const selectedVolumeSpecs = ref<string[]>([])
const backingVolumes = ref(false)
const restoringArchive = ref('')
const volumeListLoading = ref(false)
// 归档体量有界（每卷保留 5 份成功归档），仅在行数超过一屏时启用分页，
// 避免少量归档也常驻一条分页栏。
const volumeArchiveRowCount = computed(() => volumeBackups.value.length)
const { pagination: volumeArchivePagination } = useTablePagination({
  pageSize: 10,
  pageSizes: [10, 20, 50],
  rowCount: volumeArchiveRowCount,
})
const volumeArchiveTablePagination = computed(
  () => (volumeArchiveRowCount.value > 10 ? volumeArchivePagination : false),
)
// 备份/恢复都是 202 受理的异步 agent 任务，没有推送通道，只能轮询归档列表。
// 备份的 running 记录可能晚于 202 几百毫秒才落库、恢复只在终态回写归档，因此
// 用一个有界轮询窗口覆盖「已受理但记录尚未反映」的空窗，而不是只看 running。
const volumePollUntil = ref(0)
const restoringArchiveID = ref('')
const restoreBaseline = ref('')

interface ParsedVolume {
  spec: string
  src: string
  dst: string
  mode: string
  bind: boolean
}

// 与后端 parseVolumeSpec 同规则解析展示：Windows 盘符（C:\data:/data）不切开。
const parsedVolumes = computed<ParsedVolume[]>(() => {
  const out: ParsedVolume[] = []
  for (const spec of app.value?.volumes ?? []) {
    const parts = spec.split(':')
    const hasMode = parts.length >= 3 && (parts[parts.length - 1] === 'ro' || parts[parts.length - 1] === 'rw')
    const mode = hasMode ? parts[parts.length - 1] : ''
    // 先摘掉 ro/rw 再取容器路径，否则 "/opt/data:/data:ro" 的 dst 会渲染成 "ro"。
    const body = hasMode ? parts.slice(0, -1) : parts
    const dst = body.length >= 2 ? body[body.length - 1] : ''
    const src = body.slice(0, Math.max(body.length - 1, 1)).join(':')
    if (!src || !dst.startsWith('/')) continue
    out.push({ spec, src, dst, mode, bind: src.startsWith('/') || /^[A-Za-z]:[\\/]/.test(src) })
  }
  return out
})

// Compose 应用的挂载卷在编排文件中声明，应用级备份不覆盖（后端同款 400）。
const volumeBackupSupported = computed(() => {
  const a = app.value
  if (!a) return false
  if (a.source_type === 'raw_compose') return false
  if (a.source_type === 'git' && (a.build_type ?? '').toLowerCase() === 'compose') return false
  return true
})

async function loadVolumeBackups() {
  // 不依赖 app.value：首屏 refreshAll 与 loadApp 并行，应用详情尚未返回时
  // 归档列表同样可以加载（只需路由里的 appId）。
  try {
    const res = await listVolumeBackups(appId.value, { page_size: 100 })
    volumeBackups.value = res.data
    volumeBackupTotal.value = res.total
    volumeS3.value = res.s3 ?? { ready: false }
    if (volumeBackupRunning.value) {
      // 备份仍在进行：保持轮询。
      volumePollUntil.value = Math.max(volumePollUntil.value, Date.now() + 60_000)
    } else if (restoringArchiveID.value) {
      // 恢复结束会回写 last_restore_at；一旦变化即停止窗口与按钮 loading。
      const target = volumeBackups.value.find((a) => a.id === restoringArchiveID.value)
      if (target && (target.last_restore_at || '') !== restoreBaseline.value) {
        restoringArchiveID.value = ''
        restoreBaseline.value = ''
        volumePollUntil.value = 0
      }
    }
  } catch (e: any) {
    message.error(e.message || '获取备份归档失败')
  }
}

const volumeBackupRunning = computed(() => volumeBackups.value.some((a) => a.status === 'running'))
const volumeBusy = computed(() => volumeBackupRunning.value || !!restoringArchiveID.value)

async function refreshVolumeBackups() {
  volumeListLoading.value = true
  try {
    await loadVolumeBackups()
  } finally {
    volumeListLoading.value = false
  }
}

// 归档时间按「日期 / 时刻」两行排版：日期弱化，时刻保持等宽可扫读。
function formatShortTime(v?: string) {
  if (!v) return { date: '—', clock: '' }
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return { date: '—', clock: '' }
  const pad = (n: number) => String(n).padStart(2, '0')
  return {
    date: `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`,
    clock: `${pad(d.getHours())}:${pad(d.getMinutes())}`,
  }
}

// render 生成的节点由 NDataTable 渲染，拿不到本组件的 scoped 作用域，
// 因此单元格样式只走内联 style 与全局 .mono-font（global.scss）。
function renderVolTime(v?: string) {
  const t = formatShortTime(v)
  return h('div', { style: 'display:flex;flex-direction:column;line-height:1.4' }, [
    h('span', { style: 'font-size:12px;color:var(--n-text-color-3)' }, t.date),
    h('span', { class: 'mono-font' }, t.clock || '—'),
  ])
}

const renderVolDash = () => h('span', { style: 'color: var(--n-text-color-3)' }, '—')

async function copyArchiveSHA(sha: string) {
  const ok = await copyToClipboard(sha)
  ok ? message.success('SHA256 已复制') : message.warning('复制失败，请手动复制')
}

async function doBackupVolumes() {
  if (!volumeS3.value.ready) {
    message.warning('未配置默认 S3 存储目标，请先在「快照备份 → S3 存储」中添加并设为默认')
    return
  }
  backingVolumes.value = true
  try {
    const note = volBackupNote.value.trim()
    const payload = selectedVolumeSpecs.value.length > 0
      ? { volumes: selectedVolumeSpecs.value, note }
      : { note }
    await backupAppVolumes(appId.value, payload)
    volumePollUntil.value = Date.now() + 30 * 60_000
    message.success('备份任务已开始，完成后归档会出现在下方列表')
    await loadVolumeBackups()
  } catch (e: any) {
    message.error(e.message || '启动备份失败')
  } finally {
    backingVolumes.value = false
  }
}

async function doRestoreArchive(a: VolumeArchive) {
  restoringArchive.value = a.id
  restoringArchiveID.value = a.id
  restoreBaseline.value = a.last_restore_at || ''
  try {
    const res = await restoreVolumeArchive(appId.value, a.id)
    volumePollUntil.value = Date.now() + 30 * 60_000
    message.success(res.message || '恢复任务已开始')
    await loadVolumeBackups()
  } catch (e: any) {
    restoringArchiveID.value = ''
    restoreBaseline.value = ''
    message.error(e.message || '恢复失败')
  } finally {
    restoringArchive.value = ''
  }
}

async function doDeleteArchive(a: VolumeArchive) {
  try {
    await deleteVolumeArchive(appId.value, a.id)
    message.success('归档已删除')
    await loadVolumeBackups()
  } catch (e: any) {
    message.error(e.message || '删除归档失败')
  }
}

function downloadArchive(a: VolumeArchive) {
  // 服务端校验后 302 到短期 S3 预签名 URL，字节从 S3 直达浏览器
  // （JWT 走 ?token=，由 volumeArchiveDownloadUrl 拼好）。
  window.open(volumeArchiveDownloadUrl(appId.value, a.id), '_blank')
}

const volArchiveStatusTag = (s: string) => {
  if (s === 'running') return h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => '备份中' })
  if (s === 'failed') return h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => '失败' })
  return h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '成功' })
}

function formatBytes(b: number) {
  if (b <= 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(b) / Math.log(k)), sizes.length - 1)
  return `${(b / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`
}

const volumeColumns = computed<DataTableColumns<ParsedVolume>>(() => [
  {
    title: '挂载规格', key: 'spec', ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'mono-font' }, r.spec),
  },
  {
    title: '类型', key: 'bind', width: 120,
    render: (r) => h(
      NTag,
      { size: 'small', bordered: false, type: r.bind ? 'info' : 'success' },
      { default: () => (r.bind ? '宿主机目录' : 'Docker 具名卷') },
    ),
  },
  {
    title: '宿主机路径 / 卷名', key: 'src', ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'mono-font' }, r.src),
  },
  {
    title: '容器内路径', key: 'dst', ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'mono-font' }, r.dst),
  },
  {
    title: '模式', key: 'mode', width: 80,
    render: (r) => h(
      'span',
      { style: r.mode === 'ro' ? 'color: var(--n-text-color-3)' : '' },
      r.mode === 'ro' ? '只读' : '读写',
    ),
  },
])

const volArchiveColumns = computed<DataTableColumns<VolumeArchive>>(() => [
  { title: '时间', key: 'created_at', width: 80, render: (r) => renderVolTime(r.created_at) },
  {
    title: '挂载卷', key: 'volume_spec', minWidth: 170, ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'mono-font' }, r.volume_spec),
  },
  {
    // S3 去向：目标名弱化前置，对象键等宽承接；完整「目标 · 键」由单元格 tooltip 给出。
    title: '存储位置（S3）', key: 'storage_path', minWidth: 200, ellipsis: { tooltip: true },
    render: (r) => (r.storage_path
      ? h('span', { class: 'mono-font' }, [
        h('span', { style: 'color: var(--n-text-color-3)' }, `${r.storage_target_name || r.storage_target} · `),
        r.storage_path,
      ])
      : renderVolDash()),
  },
  {
    title: '大小', key: 'size', width: 88,
    render: (r) => h('span', { class: 'mono-font', style: 'font-size: 12px' }, r.size > 0 ? formatBytes(r.size) : '—'),
  },
  { title: '状态', key: 'status', width: 84, render: (r) => volArchiveStatusTag(r.status) },
  {
    title: '最近恢复', key: 'last_restore', width: 148,
    render: (r) => {
      if (!r.last_restore_at) return renderVolDash()
      const failed = r.last_restore_status === 'failed'
      const cell = [
        h('div', { style: 'display:flex;align-items:center;gap:6px' }, [
          h(
            NTag,
            { size: 'small', bordered: false, type: failed ? 'error' : 'success' },
            { default: () => (failed ? '失败' : '成功') },
          ),
          h('span', { class: 'mono-font', style: 'font-size: 12px' }, formatTime(r.last_restore_at)),
        ]),
      ]
      // 失败原因只在失败时出现，成功行保持单行高度、列表可扫读。
      if (failed && r.last_restore_error) {
        cell.push(h(
          'div',
          {
            style: 'margin-top:2px;font-size:11px;line-height:1.35;color:var(--n-error-color);'
              + 'overflow:hidden;text-overflow:ellipsis;white-space:nowrap',
            title: r.last_restore_error,
          },
          r.last_restore_error,
        ))
      }
      return h('div', { style: 'min-width:0' }, cell)
    },
  },
  {
    title: '备注 / 错误', key: 'note', minWidth: 130, ellipsis: { tooltip: true },
    render: (r) => {
      if (r.status === 'failed' && r.error) {
        return h('span', { style: 'color: var(--n-error-color)' }, r.error)
      }
      return r.note || renderVolDash()
    },
  },
  {
    title: 'SHA256', key: 'sha256', width: 132,
    render: (r) => {
      const sha = r.sha256
      if (!sha) return renderVolDash()
      return h('div', { style: 'display:flex;align-items:center;gap:4px' }, [
        h('span', { class: 'mono-font', style: 'font-size: 12px; color: var(--n-text-color-3)' }, sha.slice(0, 12)),
        h(
          NButton,
          { text: true, size: 'tiny', title: '复制完整 SHA256', onClick: () => copyArchiveSHA(sha) },
          { icon: () => h(NIcon, null, { default: () => h(CopyOutline) }) },
        ),
      ])
    },
  },
  {
    title: '操作', key: 'actions', width: 208, fixed: 'right',
    render: (r) => h(NSpace, { size: 6, wrap: false }, () => [
      r.status === 'success'
        ? h(
          NPopconfirm,
          { onPositiveClick: () => doRestoreArchive(r) },
          {
            trigger: () => h(
              NButton,
              {
                size: 'tiny', secondary: true, type: 'warning',
                loading: restoringArchive.value === r.id || restoringArchiveID.value === r.id,
                disabled: deploying.value || (volumeBusy.value && restoringArchiveID.value !== r.id),
              },
              {
                default: () => '恢复',
                icon: () => h(NIcon, null, { default: () => h(SwapHorizontalOutline) }),
              },
            ),
            default: () => '恢复将自动停止应用容器，用该归档覆盖卷数据（覆盖前保留当前状态安全副本），完成后自动重启容器。确认继续？',
          },
        )
        : null,
      r.status === 'success'
        ? h(
          NButton,
          { size: 'tiny', secondary: true, onClick: () => downloadArchive(r) },
          {
            default: () => '下载',
            icon: () => h(NIcon, null, { default: () => h(DownloadOutline) }),
          },
        )
        : null,
      h(
        NPopconfirm,
        { onPositiveClick: () => doDeleteArchive(r) },
        {
          trigger: () => h(
            NButton,
            {
              size: 'tiny', secondary: true, type: 'error',
              disabled: r.status === 'running' || volumeBusy.value,
            },
            {
              default: () => '删除',
              icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
            },
          ),
          default: () => '删除归档（含 S3 上的归档对象）？此操作不可恢复。',
        },
      ),
    ].filter(Boolean) as any),
  },
])

async function doAction(op: 'stop' | 'start' | 'restart') {
  acting.value = true
  try {
    const fn = op === 'stop' ? stopApp : op === 'start' ? startApp : restartApp
    await fn(appId.value)
    message.success(op === 'stop' ? '已停止' : op === 'start' ? '已启动' : '已重启')
    await loadStatus()
  } catch (e: any) {
    message.error(e.message || '操作失败')
  } finally {
    acting.value = false
  }
}

async function copyWebhook() {
  if (!webhookURL.value) return
  const ok = await copyToClipboard(webhookURL.value)
  ok ? message.success('已复制 Webhook 地址') : message.warning('复制失败，请手动复制')
}

const syncingWebhook = ref(false)
async function doSyncWebhook() {
  if (!app.value?.id) return
  syncingWebhook.value = true
  try {
    const res = await syncAppWebhook(app.value.id)
    message.success(res.message || '已成功通过 GitHub API 配置 Webhook')
    app.value = res.data
  } catch (e: any) {
    message.error(e.message || '同步失败')
  } finally {
    syncingWebhook.value = false
  }
}

function openLog(dep: Deployment) {
  logDeployment.value = dep
  showLog.value = true
}

const depStatusTag = (s: string) => {
  const type = s === 'success' ? 'success' : s === 'failed' ? 'error' : s === 'cancelled' ? 'warning' : 'info'
  const label = s === 'success' ? '成功' : s === 'failed' ? '失败' : s === 'cancelled' ? '已取消' : s === 'queued' ? '排队' : s === 'building' ? '构建中' : s === 'deploying' ? '发布中' : s
  return h(NTag, { size: 'small', type, bordered: false }, { default: () => label })
}

const triggerLabel = (t: string) =>
  t === 'create' ? '创建' : t === 'webhook' ? 'Git 推送' : t === 'rollback' ? '回滚' : '手动'

const columns = computed<DataTableColumns<Deployment>>(() => [
  { title: '状态', key: 'status', width: 90, render: (r) => depStatusTag(r.status) },
  { title: '触发', key: 'trigger', width: 90, render: (r) => triggerLabel(r.trigger) },
  {
    title: 'Commit',
    key: 'commit_hash',
    width: 120,
    render: (r) => r.commit_hash
      ? h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => r.commit_hash })
      : '-',
  },
  {
    title: '提交信息',
    key: 'commit_message',
    ellipsis: { tooltip: true },
    render: (r) => {
      if (r.status === 'cancelled') {
        const who = r.cancelled_by ? ` (由 ${r.cancelled_by} 取消)` : ''
        return (r.commit_message ? `${r.commit_message} · ` : '') + (r.error || '部署已取消') + who
      }
      return r.commit_message || (r.error ?? '-')
    },
  },
  {
    title: '耗时',
    key: 'duration_ms',
    width: 100,
    render: (r) => (r.duration_ms ? `${(r.duration_ms / 1000).toFixed(1)}s` : '-'),
  },
  {
    title: '开始时间',
    key: 'started_at',
    width: 150,
    render: (r) => formatTime(r.started_at),
  },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => {
          const btns = [
            h(NButton, { size: 'tiny', secondary: true, onClick: () => openLog(r) }, { default: () => '日志' }),
          ]
          if (canWrite.value && (r.status === 'queued' || r.status === 'building' || r.status === 'deploying')) {
            btns.push(
              h(
                NPopconfirm,
                { onPositiveClick: () => doCancelDeploy() },
                {
                  trigger: () =>
                    h(
                      NButton,
                      {
                        size: 'tiny',
                        secondary: true,
                        type: 'error',
                        loading: cancellingDeploy.value,
                      },
                      { default: () => '停止' },
                    ),
                  default: () => '确定终止当前正在进行的部署任务？已拉起的临时资源将被安全清理。',
                },
              ),
            )
          }
          if (r.status === 'failed') {
            btns.push(
              h(
                NButton,
                {
                  size: 'tiny',
                  secondary: true,
                  type: 'info',
                  loading: diagnosing.value && diagnosingDepId.value === r.id,
                  onClick: () => doDiagnose(r),
                },
                {
                  icon: () => h(NIcon, null, { default: () => h(SparklesOutline) }),
                  default: () => 'AI 排障',
                },
              ),
            )
          }
          if (canWrite.value && r.status === 'success' && r.commit_hash) {
            btns.push(
              h(
                NPopconfirm,
                { onPositiveClick: () => doRollback(r) },
                {
                  trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'warning' }, { default: () => '回滚' }),
                  default: () => `将应用回滚到 Commit ${r.commit_hash}？`,
                },
              ),
            )
          }
          return btns
        },
      }),
  },
])

function formatTime(v?: string) {
  if (!v) return '-'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// 仅当仍是当前路由时才响应 tab 变化（组件被 KeepAlive 缓存，route 可能
// 已指向别的页面，见 AGENTS.md 8.4-6）。
watch(activeTab, () => {
  if (route.name !== 'app-detail') return
})
watch(page, () => {
  if (route.name !== 'app-detail') return
  loadDeployments()
})

function startPolling() {
  if (pollTimer) return
  pollTimer = setInterval(() => {
    if (route.name === 'app-detail') {
      loadStatus()
      if (deploying.value) loadDeployments()
      // 备份归档里的 running 记录由异步 agent tar 推进，轮询直到落终态；
      // 恢复只在终态回写归档，故用受理后开启的有界窗口兜底。
      if (volumeBackupRunning.value || Date.now() < volumePollUntil.value) loadVolumeBackups()
    }
  }, 5000)
}

onMounted(() => {
  refreshAll()
  loadProxyData()
  startPolling()
})

onActivated(() => {
  refreshAll()
  loadProxyData()
  // 页签切走期间轮询窗口可能已过期，仍在执行的任务需要重新开启观察。
  if (volumeBusy.value) volumePollUntil.value = Date.now() + 60_000
  startPolling()
})

onDeactivated(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
})
</script>

<template>
  <div v-if="app" class="app-detail-view page-flex-column">
    <div class="detail-header">
      <NButton quaternary @click="$router.push('/apps')">
        <template #icon>
          <NIcon><ArrowBackOutline /></NIcon>
        </template>
        应用列表
      </NButton>
      <NSpace align="center" style="flex: 1; justify-content: center">
        <NIcon size="20"><BuildOutline /></NIcon>
        <span class="app-title">{{ app.name }}</span>
        <NTag v-if="deploying" type="info" size="small">部署中</NTag>
        <NTag v-else-if="status?.state === 'running'" type="success" size="small">运行中</NTag>
        <NTag v-else-if="status?.state === 'undeployed'" size="small">未部署</NTag>
        <NTag v-else-if="status" :type="status.agent_online ? 'warning' : 'error'" size="small">
          {{ status.agent_online ? status.state : '主机离线' }}
        </NTag>
      </NSpace>
      <NSpace v-if="canWrite">
        <NPopconfirm v-if="deploying" @positive-click="doDeploy(true)">
          <template #trigger>
            <NButton size="small" type="primary" :loading="acting">
              <template #icon>
                <NIcon><CloudUploadOutline /></NIcon>
              </template>
              重新部署
            </NButton>
          </template>
          当前已有部署任务正在进行中，继续将终止当前任务并启动全新部署。是否继续？
        </NPopconfirm>
        <NButton v-else size="small" type="primary" :loading="acting" @click="doDeploy(false)">
          <template #icon>
            <NIcon><CloudUploadOutline /></NIcon>
          </template>
          {{ status?.state === 'undeployed' ? '部署' : '重新部署' }}
        </NButton>
        <NPopconfirm v-if="deploying" @positive-click="doCancelDeploy">
          <template #trigger>
            <NButton size="small" type="error" :loading="cancellingDeploy">
              <template #icon>
                <NIcon><StopOutline /></NIcon>
              </template>
              终止部署
            </NButton>
          </template>
          确定终止当前部署任务？半拉起的临时容器将被清理，不影响在线服务。
        </NPopconfirm>
        <NPopconfirm @positive-click="doRollback()">
          <template #trigger>
            <NButton size="small" :loading="acting || deploying" :disabled="deploying">
              <template #icon>
                <NIcon><SwapHorizontalOutline /></NIcon>
              </template>
              回滚上一版
            </NButton>
          </template>
          回滚到上一个成功版本？
        </NPopconfirm>
        <NButton size="small" :loading="acting" @click="doAction('restart')">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          重启
        </NButton>
        <NButton v-if="status?.state === 'running'" size="small" :loading="acting" @click="doAction('stop')">
          <template #icon>
            <NIcon><StopOutline /></NIcon>
          </template>
          停止
        </NButton>
        <NButton v-else size="small" :loading="acting" :disabled="status?.state === 'undeployed'" @click="doAction('start')">
          <template #icon>
            <NIcon><PlayOutline /></NIcon>
          </template>
          启动
        </NButton>
      </NSpace>
    </div>

    <div class="detail-body">
      <NTabs v-model:value="activeTab" type="line" style="height: 100%; display: flex; flex-direction: column">
        <NTabPane name="overview" tab="概览">
          <div class="overview-scroll">
            <NSpace justify="end" style="margin-bottom: 10px">
              <NButton v-if="canWrite" size="small" secondary @click="openEdit">
                编辑配置
              </NButton>
            </NSpace>
            <NDescriptions :column="2" bordered size="small" label-placement="left">
              <NDescriptionsItem label="部署来源">
                <NTag size="small" :bordered="false">
                  {{ sourceTypeLabel }}
                </NTag>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.source_type === 'template'" label="应用模板">
                <span class="mono">{{ app.template_id }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.source_type === 'git'" label="Git 仓库">
                <span class="mono">{{ app.repo_url }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.source_type === 'git'" label="监听分支">
                <NTag size="small" :bordered="false">{{ app.branch }}</NTag>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.source_type === 'git'" label="当前 Commit">
                <NTag v-if="app.current_commit" size="small" type="info">
                  <template #icon>
                    <NIcon><GitCommitOutline /></NIcon>
                  </template>
                  {{ app.current_commit }}
                </NTag>
                <span v-else>-</span>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.source_type === 'image' || app.source_type === 'template'" label="镜像">
                <span class="mono">{{ app.image || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="组网端口转发">
                <template v-if="app.mesh_forward_enabled">
                  <NTag size="small" type="success" :bordered="false">已启用</NTag>
                  <span class="mono" style="margin-left: 8px">
                    {{ forwardHostname || app.mesh_forward_host_id }}:{{ app.mesh_forward_entry_port }} → {{ app.mesh_forward_target || (app.mesh_forward_target_port ? `${appHostMeshIP || appHostname}:${app.mesh_forward_target_port}` : '-') }}
                  </span>
                </template>
                <span v-else style="color: var(--n-text-color-3)">未启用</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="容器名">
                <span class="mono">{{ app.container_name || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="健康检查">
                <span class="mono">{{ app.healthcheck_url || '未配置' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="端口映射">
                <div v-if="(app.ports ?? []).length" class="port-rows">
                  <div v-for="p in app.ports" :key="`${p.host}:${p.container}`" class="port-row">
                    <span class="mono">
                      {{ p.host }}:{{ p.container }}<template v-if="p.bind_scope === 'mesh'">（仅组网）</template>
                    </span>
                    <NButton size="tiny" secondary @click="openTunnel(p.host)">
                      <template #icon><NIcon :component="SwapHorizontalOutline" /></template>
                      隧道
                    </NButton>
                  </div>
                </div>
                <span v-else>-</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="挂载卷">
                <span class="mono">{{ (app.volumes ?? []).join(', ') || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem v-if="app.domain" label="域名">
                <NTag size="small" type="success" :bordered="false">{{ app.domain }}</NTag>
              </NDescriptionsItem>
            </NDescriptions>

            <NAlert v-if="app.auto_deploy && app.webhook_auto_managed" type="success" style="margin-top: 14px" :show-icon="true">
              <template #header>自动部署已生效 (GitHub 自动托管)</template>
              <div>
                系统已自动通过已连接的 GitHub 账号在仓库中配置了 Webhook。每当向 <code>{{ app.branch || 'main' }}</code> 分支推送代码（git push）时，将自动触发构建并重新部署（无需手动配置 Webhook）。
              </div>
              <div style="margin-top: 8px">
                <NButton size="tiny" secondary :loading="syncingWebhook" @click="doSyncWebhook">
                  重新同步 Webhook
                </NButton>
              </div>
            </NAlert>
            <NAlert v-else-if="app.auto_deploy && webhookURL" type="info" style="margin-top: 14px" :show-icon="true">
              <template #header>自动部署 Webhook（手动模式）</template>
              <span v-if="app.webhook_error" style="color: var(--n-error-color); margin-bottom: 6px; display: block">
                自动注册 Webhook 失败（{{ app.webhook_error }}），您可以手动将此地址配置到代码仓库的 Webhooks（push 事件）：
              </span>
              <span v-else>
                将此地址配置到代码仓库的 Webhooks（push 事件）即可实现推送自动构建：
              </span>
              <div class="webhook-row">
                <NCode :code="webhookURL" />
                <NButton size="tiny" secondary @click="copyWebhook">
                  <template #icon>
                    <NIcon><CopyOutline /></NIcon>
                  </template>
                  复制
                </NButton>
                <NButton v-if="app.repo_url?.includes('github.com')" size="tiny" secondary :loading="syncingWebhook" @click="doSyncWebhook">
                  重试自动同步
                </NButton>
              </div>
            </NAlert>
          </div>
        </NTabPane>

        <NTabPane name="volumes" tab="数据卷备份">
          <div class="overview-scroll">
            <NAlert v-if="!volumeBackupSupported" type="warning" style="margin-bottom: 12px">
              <template #header>Compose 应用的挂载卷在编排文件中声明</template>
              应用级备份只覆盖应用声明的挂载卷；Compose 服务的卷请在「快照备份」中按目录或 Docker 卷备份。
              <NButton size="tiny" secondary style="margin-left: 8px" @click="$router.push('/snapshots')">
                前往快照备份
              </NButton>
            </NAlert>
            <NAlert v-else-if="parsedVolumes.length === 0" type="default" :show-icon="false" style="margin-bottom: 12px">
              该应用未配置挂载卷。挂载卷可在「编辑配置」中添加（宿主路径:容器路径），保存并重新部署后生效。
            </NAlert>
            <template v-else>
              <NAlert v-if="!volumeS3.ready" type="warning" style="margin-bottom: 12px">
                <template #header>未配置 S3 存储目标</template>
                数据卷归档统一存放在 S3（不在应用主机留存），请先在「快照备份 → S3 存储」中添加存储目标并设为默认。
                <NButton size="tiny" secondary style="margin-left: 8px" @click="$router.push('/snapshots')">
                  前往 S3 存储配置
                </NButton>
              </NAlert>
              <NAlert v-else type="info" style="margin-bottom: 12px">
                备份将卷数据目录打包为 tar.gz，直传 S3 存储目标
                <strong>{{ volumeS3.target_name || volumeS3.target_id }}</strong>
                <template v-if="volumeS3.bucket">（存储桶 <code>{{ volumeS3.bucket }}</code>）</template>，
                每卷保留最近 5 份成功归档。备份在容器运行中直接打包，属于在线文件快照——数据库等对跨文件一致性敏感的
                应用建议选业务低峰期执行。恢复时会先下载归档并校验 SHA256，<strong>自动停止应用容器</strong>、
                保留当前状态安全副本后解压覆盖，完成后自动重启容器。
                <br />
                需要定时自动备份？到
                <NButton text type="primary" size="tiny" @click="$router.push('/snapshots')">快照备份</NButton>
                新建「应用数据卷」任务：调度与保留份数由快照模块管理，归档与本页共用同一份记录。
              </NAlert>

              <!-- 应用挂载卷清单（数据量 = 应用声明的卷数，天然有限，属 §8.2 例外：不分页） -->
              <div class="vol-section">
                <div class="vol-section-title">应用挂载卷</div>
                <NDataTable
                  :columns="volumeColumns"
                  :data="parsedVolumes"
                  size="small"
                  :bordered="false"
                />
              </div>

              <!-- 发起备份 -->
              <div v-if="canWrite" class="vol-section">
                <div class="vol-section-title">发起备份</div>
                <NSpace vertical :size="10">
                  <NSelect
                    v-model:value="selectedVolumeSpecs"
                    multiple
                    clearable
                    :options="parsedVolumes.map((v) => ({ label: v.spec, value: v.spec }))"
                    placeholder="全部挂载卷（不选则备份所有）"
                  />
                  <div class="vol-backup-row">
                    <NInput
                      v-model:value="volBackupNote"
                      placeholder="备注（可选）"
                      style="width: 320px"
                      :maxlength="200"
                      show-count
                    />
                    <NButton
                      type="primary"
                      size="small"
                      :loading="backingVolumes"
                      :disabled="!volumeS3.ready || volumeBusy || deploying"
                      @click="doBackupVolumes"
                    >
                      <template #icon>
                        <NIcon><CloudUploadOutline /></NIcon>
                      </template>
                      {{ volumeBusy ? '任务执行中' : '开始备份' }}
                    </NButton>
                    <span v-if="volumeBusy" class="vol-busy-hint">
                      <NIcon><RefreshOutline /></NIcon>
                      归档状态每 5 秒自动刷新
                    </span>
                  </div>
                </NSpace>
              </div>

              <!-- 归档历史 -->
              <div class="vol-section">
                <div class="vol-section-title">
                  <span>
                    备份归档
                    <span v-if="volumeBackupTotal > 0" class="vol-count">（共 {{ volumeBackupTotal }} 份）</span>
                  </span>
                  <NButton size="tiny" secondary :loading="volumeListLoading" @click="refreshVolumeBackups">
                    <template #icon>
                      <NIcon><RefreshOutline /></NIcon>
                    </template>
                    刷新
                  </NButton>
                </div>
                <NDataTable
                  :columns="volArchiveColumns"
                  :data="volumeBackups"
                  size="small"
                  :bordered="false"
                  :loading="volumeListLoading"
                  :pagination="volumeArchiveTablePagination"
                  :scroll-x="1240"
                  :row-key="(r: VolumeArchive) => r.id"
                >
                  <template #empty>
                    <NEmpty
                      :description="volumeS3.ready ? '暂无备份归档，选好挂载卷后点「开始备份」' : '暂无备份归档'"
                      size="small"
                    />
                  </template>
                </NDataTable>
              </div>
            </template>
          </div>
        </NTabPane>

        <NTabPane name="deployments" tab="部署历史">
          <div class="deployments-pane">
            <NDataTable
              flex-height
              :columns="columns"
              :data="deployments"
              :loading="loading"
              :row-key="(r: Deployment) => r.id"
              :pagination="{
                page: page,
                pageSize: pageSize,
                itemCount: total,
                showSizePicker: true,
                pageSizes: [10, 20, 50],
                'onUpdate:page': (p: number) => (page = p),
                'onUpdate:pageSize': (s: number) => { pageSize = s; page = 1 },
              }"
              remote
            />
          </div>
        </NTabPane>

        <NTabPane name="proxy" tab="域名与反代">
          <div class="overview-scroll">
            <NAlert
              v-if="app.domain"
              type="success"
              :show-icon="true"
              style="margin-bottom: 14px"
            >
              <template #header>已绑定 {{ app.domain }}</template>
              反代模式：{{ app.proxy_mode === 'gateway' ? '网关统一入口' : '节点本地' }}，上游：{{ app.proxy_upstream }}
            </NAlert>
            <NAlert
              v-else
              type="default"
              :show-icon="false"
              style="margin-bottom: 14px"
            >
              尚未绑定域名。绑定后 Watchman 会自动把证书与 Nginx 反代配置下发到网关主机并热加载。
            </NAlert>

            <!-- 流量拓扑：让用户直观看到公网流量如何到达内网应用 -->
            <div v-if="app.domain" class="topology-box">
              <div class="topology-line">
                <div class="topology-node">
                  <div class="topology-label">公网用户</div>
                  <div class="topology-value">{{ app.domain }}</div>
                </div>
                <div class="topology-arrow">-&gt;</div>
                <div class="topology-node">
                  <div class="topology-label">
                    {{ app.proxy_mode === 'gateway' ? '网关节点（Nginx / SSL 终止）' : '应用主机（Nginx / SSL 终止）' }}
                  </div>
                  <div class="topology-value">
                    {{ gatewayHostname || appHostname }}
                  </div>
                </div>
                <div v-if="app.proxy_mode === 'gateway'" class="topology-arrow">
                  {{ gatewayUpstreamIsMesh ? 'Tailscale 组网' : '直连' }}
                </div>
                <div class="topology-node">
                  <div class="topology-label">应用容器</div>
                  <div class="topology-value">{{ app.container_name || app.name }}（{{ app.proxy_upstream }}）</div>
                </div>
              </div>
              <div v-if="app.proxy_mode === 'gateway'" class="topology-hint">
                <template v-if="gatewayUpstreamIsMesh">
                  应用主机无需公网 IP：公网流量在网关主机卸载 TLS 后，经 Tailscale 组网转发到应用容器的上游端口。
                </template>
                <template v-else>
                  公网流量在网关主机卸载 TLS 后，经公网/内网 IP 直连转发到应用容器（未走异地组网）。
                </template>
              </div>
            </div>

            <NForm label-placement="top" style="max-width: 640px">
              <NFormItem label="域名" required>
                <NInput v-model:value="proxyForm.domain" placeholder="api.example.com" />
              </NFormItem>
              <NFormItem label="反代模式">
                <NSpace vertical style="width: 100%">
                  <NRadioGroup v-model:value="proxyForm.mode">
                    <NRadio value="local">节点本地（应用所在主机运行反代）</NRadio>
                    <NRadio value="gateway">统一网关（公网网关主机反代穿透到应用）</NRadio>
                  </NRadioGroup>
                  <NSelect
                    v-if="proxyForm.mode === 'gateway'"
                    v-model:value="proxyForm.gateway_host_id"
                    :options="hostOptions"
                    placeholder="选择作为反代网关的主机（可走组网或直连，将自动初始化反代容器）"
                    filterable
                  />
                  <span v-if="proxyForm.mode === 'gateway'" class="hint" style="display: block; margin-top: 4px">
                    网关主机不能是应用主机自身；到应用主机可走异地组网（双方均在组网内）或直连（公网/内网 IP），组网非必需
                  </span>
                </NSpace>
              </NFormItem>
              <NFormItem label="证书">
                <NSelect v-model:value="proxyForm.cert_id" :options="certOptions" />
              </NFormItem>
              <NFormItem label="上游地址（可选，默认自动计算）">
                <NInput v-model:value="proxyForm.upstream" placeholder="127.0.0.1:8080" />
              </NFormItem>
              <NFormItem label="WebSocket 支持">
                <NSwitch v-model:value="proxyForm.websocket" />
              </NFormItem>
              <NSpace>
                <NButton type="primary" :loading="savingProxy" @click="doBindProxy">
                  下发并绑定
                </NButton>
                <NPopconfirm v-if="app.domain" @positive-click="doUnbindProxy">
                  <template #trigger>
                    <NButton secondary type="error" :loading="savingProxy">解除绑定</NButton>
                  </template>
                  确定解除 {{ app.domain }} 的反代绑定？
                </NPopconfirm>
              </NSpace>
            </NForm>
          </div>
        </NTabPane>

        <NTabPane name="forward" tab="组网端口转发">
          <div class="overview-scroll">
            <NAlert
              v-if="app.mesh_forward_enabled"
              type="success"
              :show-icon="true"
              style="margin-bottom: 14px"
            >
              <template #header>组网端口转发已启用</template>
              入口：{{ forwardHostname || app.mesh_forward_host_id }}:{{ app.mesh_forward_entry_port }}，
              转发目标：{{ app.mesh_forward_target || (app.mesh_forward_target_port ? `${appHostMeshIP || appHostname}:${app.mesh_forward_target_port}` : '-') }}
            </NAlert>
            <NAlert
              v-else
              type="default"
              :show-icon="false"
              style="margin-bottom: 14px"
            >
              未启用组网端口转发。启用后，系统将在选定的入口主机（如具有公网 IP 的中继节点）上运行 socat 容器，通过异地组网或直连将 4 层 TCP 端口流量转发至本应用。
            </NAlert>

            <!-- 流量拓扑：4层 TCP 端口转发链路 -->
            <div v-if="app.mesh_forward_enabled" class="topology-box" style="margin-bottom: 16px;">
              <div class="topology-line">
                <div class="topology-node">
                  <div class="topology-label">外部访问 / 客户端</div>
                  <div class="topology-value">
                    {{ forwardHostname || '入口主机' }}:{{ app.mesh_forward_entry_port }}
                  </div>
                </div>
                <div class="topology-arrow">-&gt;</div>
                <div class="topology-node">
                  <div class="topology-label">入口主机（socat 中继容器）</div>
                  <div class="topology-value">{{ forwardHostname }}</div>
                </div>
                <div class="topology-arrow">
                  {{ forwardMeshLink ? 'Tailscale 组网' : '直连' }}
                </div>
                <div class="topology-node">
                  <div class="topology-label">应用主机</div>
                  <div class="topology-value">{{ appHostMeshIP || appHostname }}:{{ app.mesh_forward_target_port }}</div>
                </div>
                <div class="topology-arrow">-&gt;</div>
                <div class="topology-node">
                  <div class="topology-label">应用容器</div>
                  <div class="topology-value">{{ app.container_name || app.name }}</div>
                </div>
              </div>
              <div class="topology-hint">
                <template v-if="forwardMeshLink">
                  应用主机无需公网 IP：外部客户端连接入口主机的 {{ app.mesh_forward_entry_port }} 端口，流量经 Tailscale 组网直接送达应用主机的 {{ app.mesh_forward_target_port }} 端口。
                </template>
                <template v-else>
                  外部客户端连接入口主机的 {{ app.mesh_forward_entry_port }} 端口，流量经公网/内网 IP 直连转发至应用主机（未走异地组网）。
                </template>
              </div>
            </div>

            <NForm label-placement="top" style="max-width: 640px">
              <NFormItem label="启用组网端口转发 (TCP 4层中继)">
                <NSwitch v-model:value="forwardForm.enabled" />
              </NFormItem>

              <template v-if="forwardForm.enabled">
                <NFormItem label="入口主机（流量进入的主机）" required>
                  <NSelect
                    v-model:value="forwardForm.host_id"
                    :options="hostOptions"
                    placeholder="选择运行 socat 中继容器的主机"
                    filterable
                  />
                  <span class="hint" style="display: block; margin-top: 4px">
                    入口主机不能是应用主机自身；优先选择有公网 IP 的主机以对外暴露服务
                  </span>
                </NFormItem>

                <NFormItem label="入口监听端口" required>
                  <NInputNumber
                    v-model:value="forwardForm.entry_port"
                    placeholder="如 16379"
                    :min="1"
                    :max="65535"
                    style="width: 100%"
                  />
                  <span class="hint" style="display: block; margin-top: 4px">
                    入口主机上对外监听并接收客户端连接的 TCP 端口
                  </span>
                </NFormItem>

                <NFormItem label="应用目标端口" required>
                  <NInputNumber
                    v-model:value="forwardForm.target_port"
                    placeholder="如 6379"
                    :min="1"
                    :max="65535"
                    style="width: 100%"
                  />
                  <span class="hint" style="display: block; margin-top: 4px">
                    本应用主机上实际接收流量的服务端口
                  </span>
                </NFormItem>
              </template>

              <NSpace>
                <NButton type="primary" :loading="savingForward" @click="doSaveForward">
                  保存并同步中继
                </NButton>
              </NSpace>
            </NForm>
          </div>
        </NTabPane>

        <NTabPane v-if="app.source_type === 'raw_compose' || app.compose_content" name="compose" tab="Compose 编排">
          <div class="overview-scroll" style="max-width: 800px">
            <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
              当前应用为 Docker Compose 复合微服务栈。您可直接在此修改 YAML 编排内容，保存后系统会自动在目标主机更新配置并重新发布。
            </NAlert>
            <NInput
              v-model:value="composeEditContent"
              type="textarea"
              :rows="16"
              style="font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px"
            />
            <NSpace style="margin-top: 12px" justify="end">
              <NButton type="primary" :loading="savingCompose || deploying" @click="saveAndRedeployCompose">
                保存并重新部署
              </NButton>
            </NSpace>
          </div>
        </NTabPane>

        <NTabPane name="env" tab="环境变量">
          <div class="overview-scroll">
            <NAlert v-if="app.source_type === 'template'" type="info" :show-icon="false" style="margin-bottom: 12px">
              该应用来自内置模板：部署时模板参数会重新渲染对应变量，此处的手动修改可能被覆盖。
            </NAlert>
            <template v-if="envRows.length > 0">
              <div v-for="(row, idx) in envRows" :key="idx" class="env-edit-row">
                <NInput v-model:value="row.key" placeholder="变量名" style="width: 220px" :disabled="!canWrite" />
                <NInput v-model:value="row.value" placeholder="变量值（支持 {{ vault:<id>:<key> }} 引用）" :disabled="!canWrite" />
                <NButton v-if="canWrite" size="small" quaternary type="error" @click="removeEnvRow(idx)">
                  删除
                </NButton>
              </div>
            </template>
            <NAlert v-else type="default" :show-icon="false">未配置环境变量</NAlert>
            <NSpace v-if="canWrite" justify="end" style="margin-top: 12px">
              <NButton size="small" secondary @click="addEnvRow">添加变量</NButton>
              <NButton size="small" :loading="savingEnv" @click="saveEnv(false)">保存</NButton>
              <NButton size="small" type="primary" :loading="savingEnv || deploying" @click="saveEnv(true)">
                保存并重新部署
              </NButton>
            </NSpace>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <NModal
      v-model:show="showLog"
      preset="card"
      :title="`构建日志 · ${logDeployment?.commit_hash ?? ''}`"
      style="width: 860px; max-width: 94vw"
    >
      <div class="log-body">
        <pre class="log-pre">{{ logDeployment?.build_log || '（此部署记录没有日志）' }}</pre>
        <NAlert v-if="logDeployment?.error" type="error" style="margin-top: 10px">
          {{ logDeployment.error }}
        </NAlert>
      </div>
    </NModal>

    <!-- 运维隧道一键入口（kubectl port-forward 式） -->
    <OpsTunnelModal v-model:show="showTunnel" :prefill="tunnelPrefill" />

    <!-- 应用配置编辑（部分更新：只提交改动字段） -->
    <NModal
      v-model:show="showEdit"
      preset="card"
      title="编辑应用配置"
      style="width: 640px; max-width: 94vw"
    >
      <NForm label-placement="top" size="small">
        <NFormItem label="应用名称">
          <NInput v-model:value="editForm.name" placeholder="应用名称" />
        </NFormItem>
        <NFormItem v-if="app?.source_type === 'image'" label="镜像地址">
          <NInput v-model:value="editForm.image" placeholder="nginx:alpine" />
        </NFormItem>
        <NFormItem label="健康检查 URL">
          <NInput v-model:value="editForm.healthcheck_url" placeholder="http://127.0.0.1:8080/healthz（留空则不检查）" />
        </NFormItem>
        <NFormItem label="端口映射">
          <div style="width: 100%">
            <div v-for="(p, idx) in editForm.ports" :key="idx" class="port-edit-row">
              <NInputNumber v-model:value="p.host" placeholder="宿主端口" :min="1" :max="65535" style="width: 130px" />
              <span class="mono">:</span>
              <NInputNumber v-model:value="p.container" placeholder="容器端口" :min="1" :max="65535" style="width: 130px" />
              <NSelect
                v-model:value="p.bind_scope"
                :options="[{ label: '公网开放', value: 'public' }, { label: '仅异地组网', value: 'mesh' }]"
                style="width: 140px"
              />
              <NButton size="tiny" quaternary type="error" @click="editForm.ports.splice(idx, 1)">删除</NButton>
            </div>
            <NButton size="tiny" secondary @click="editForm.ports.push({ host: null, container: null, bind_scope: 'public' })">
              添加端口
            </NButton>
          </div>
        </NFormItem>
        <NFormItem label="挂载卷（每行 host:container）">
          <NInput
            v-model:value="editForm.volumes_text"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 5 }"
            placeholder="/opt/data:/data"
          />
        </NFormItem>
      </NForm>
      <NAlert v-if="editIsDeployed" type="warning" :show-icon="false" style="margin-bottom: 12px">
        配置改动需重新部署后生效；可点「保存并重新部署」一次完成。
      </NAlert>
      <NSpace justify="end">
        <NButton @click="showEdit = false">取消</NButton>
        <NButton :loading="savingEdit" @click="saveEdit(false)">保存</NButton>
        <NButton type="primary" :loading="savingEdit || deploying" @click="saveEdit(true)">
          保存并重新部署
        </NButton>
      </NSpace>
    </NModal>

    <!-- AI 部署排障诊断弹窗 -->
    <NModal
      v-model:show="showAIDiagnosis"
      preset="card"
      title="AI 智能部署排障诊断"
      style="width: 720px; max-width: 94vw"
    >
      <div v-if="diagnosing" style="text-align: center; padding: 30px">
        <NSpace vertical align="center">
          <NIcon size="36" color="#18a058"><SparklesOutline /></NIcon>
          <span>AI 正在深入分析构建报错、退出码与系统上下文...</span>
        </NSpace>
      </div>
      <div v-else-if="aiResult" class="ai-diag-body">
        <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
          <template #header>根因总结</template>
          {{ aiResult.summary }}
        </NAlert>

        <div v-if="aiResult.suggestions && aiResult.suggestions.length > 0" style="margin-bottom: 14px">
          <div style="font-weight: 600; margin-bottom: 6px">修复与排查建议：</div>
          <ul style="padding-left: 20px; margin: 0">
            <li v-for="(s, idx) in aiResult.suggestions" :key="idx" style="margin-bottom: 4px">{{ s }}</li>
          </ul>
        </div>

        <div v-if="aiResult.commands && aiResult.commands.length > 0">
          <div style="font-weight: 600; margin-bottom: 6px">推荐执行命令：</div>
          <div v-for="(cmd, idx) in aiResult.commands" :key="idx" class="cmd-box">
            <NCode :code="cmd" />
            <NButton size="tiny" secondary @click="copyToClipboard(cmd).then(() => message.success('已复制命令'))">复制</NButton>
          </div>
        </div>
      </div>
      <div v-else style="padding: 20px; text-align: center; color: #999">
        暂无诊断分析结果
      </div>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.port-rows {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.port-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.app-detail-view {
  gap: 10px;
}

.detail-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 12px;
}

.app-title {
  font-size: 16px;
  font-weight: 600;
}

.detail-body {
  flex: 1;
  min-height: 0;
  overflow: hidden;
}

/* 必须显式撑满页签高度：父级 .n-tab-pane 是固定高、.detail-body 又是
   overflow: hidden，子容器高度按内容自长时滚动条永不出现、底部内容被裁掉。 */
.overview-scroll {
  height: 100%;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 8px 4px;
}

.deployments-pane {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.mono {
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
}

/* 数据卷备份页签：三个子区块共用同一标题层级，归档表宽度由 scroll-x 兜住 */
.vol-section {
  margin-bottom: 18px;
}

.vol-section:last-child {
  margin-bottom: 0;
}

.vol-section-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 8px;
}

.vol-count {
  margin-left: 4px;
  font-weight: 400;
  color: var(--n-text-color-3, var(--text-secondary));
}

.vol-backup-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.vol-busy-hint {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--n-text-color-3, var(--text-secondary));
}

.topology-box {
  background: rgba(99, 102, 241, 0.06);
  border: 1px solid rgba(99, 102, 241, 0.2);
  border-radius: 8px;
  padding: 14px;
  margin-bottom: 14px;
}

.topology-line {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.topology-node {
  flex: 1;
  min-width: 140px;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 8px 12px;

  .topology-label {
    font-size: 11px;
    color: var(--text-secondary);
    margin-bottom: 2px;
  }

  .topology-value {
    font-size: 13px;
    font-weight: 600;
    word-break: break-all;
  }
}

.topology-arrow {
  color: #6366f1;
  font-weight: 600;
  font-size: 13px;
  flex-shrink: 0;
}

.topology-hint {
  margin-top: 10px;
  font-size: 12px;
  color: var(--text-secondary);
}

.cmd-box {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  background: rgba(128, 128, 128, 0.08);
  padding: 6px 10px;
  border-radius: 6px;
  margin-bottom: 6px;
}

.webhook-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 6px;
}

.env-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 0;

  .env-value {
    color: var(--n-text-color);
  }
}

/* 环境变量行式编辑器：变量名固定宽，值占满剩余，删除按钮贴右 */
.env-edit-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 0;
}

/* 端口映射行编辑：宿主:容器 + 绑定范围 + 删除 */
.port-edit-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}

.log-body {
  max-height: 60vh;
  display: flex;
  flex-direction: column;
}

.log-pre {
  flex: 1;
  overflow: auto;
  overscroll-behavior: contain;
  background: rgba(128, 128, 128, 0.08);
  border-radius: 6px;
  padding: 12px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}

:deep(.n-tabs .n-tab-pane) {
  height: 100%;
  min-height: 0;
}

:deep(.n-tabs-pane-wrapper) {
  flex: 1;
  min-height: 0;
}
</style>