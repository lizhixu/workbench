<script setup lang="ts">
import { computed, h, nextTick, onActivated, onDeactivated, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import {
  NAlert, NButton, NCode, NDataTable, NDescriptions, NDescriptionsItem,
  NEmpty, NForm, NFormItem, NIcon, NInput, NInputNumber, NModal, NPopconfirm, NRadio, NRadioGroup,
  NSelect, NSpace, NSpin, NSwitch, NTabPane, NTabs, NUpload,
  NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns, UploadCustomRequestOptions, UploadFileInfo } from 'naive-ui'
import {
  ArrowBackOutline, BuildOutline, CloudUploadOutline, CopyOutline,
  DownloadOutline,
  GitCommitOutline, MoveOutline, PlayOutline, RefreshOutline, SparklesOutline, StopOutline,
  SwapHorizontalOutline, TrashOutline,
} from '@vicons/ionicons5'
import { useTablePagination } from '../../composables/useTablePagination'
import {
  cancelDeployment, deployApp, diagnoseDeployment, getApp, getAppStatus, listDeployments, restartApp,
  rollbackApp, startApp, stopApp, updateApp, syncAppWebhook, type AppEntity, type AppStatus, type Deployment,
  type AIDiagnosis,
  backupAppVolumes, listVolumeBackups, restoreVolumeArchive, deleteVolumeArchive,
  uploadVolumeRestore,
  volumeArchiveDownloadUrl, type VolumeArchive,
} from '../../api/apps'
import {
  listBackupJobs, createBackupJob, updateBackupJob, deleteBackupJob, runBackupJob,
  listS3Targets, type BackupJob, type CreateJobReq, type S3Target,
} from '../../api/snapshots'
import { bindProxy, unbindProxy, listCerts, type Certificate } from '../../api/certs'
import { installDocker, listHosts } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import {
  getGitHubAppStatus, switchAppToGitHubApp, type GitHubAppStatus,
} from '../../api/git'
import { bothMeshed } from '../../utils/mesh'
import { copyToClipboard } from '../../utils/clipboard'
import { useAuthStore } from '../../stores/auth'
import OpsTunnelModal from '../../components/tunnels/OpsTunnelModal.vue'
import AppMigrateModal from '../../components/apps/AppMigrateModal.vue'

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

// Service migration (cross-host move): the modal drives precheck + submit,
// and a successful start switches to the deployment tab to follow progress.
const showMigrate = ref(false)

function onMigrated() {
  activeTab.value = 'deployments'
  page.value = 1
  refreshAll()
}

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
interface ProxyDomainRow {
  domain: string
  cert_id: string
}

const proxyForm = ref({
  domains: [] as ProxyDomainRow[],
  mode: 'local' as 'local' | 'gateway',
  // NSelect 绑定字段空值用 null：空串会被 naive-ui fallbackOption 合成空 label 选项，
  // 让 select 自认为已选中、占位符永不渲染。
  gateway_host_id: null as string | null,
  upstream: '',
  websocket: true,
})
const savingProxy = ref(false)

function syncProxyForm() {
  if (app.value) {
    if (app.value.proxy_domains && app.value.proxy_domains.length > 0) {
      proxyForm.value.domains = app.value.proxy_domains.map((d) => ({ domain: d.domain, cert_id: d.cert_id }))
    } else if (app.value.domain) {
      proxyForm.value.domains = [{ domain: app.value.domain, cert_id: '' }]
    } else {
      proxyForm.value.domains = []
    }
    proxyForm.value.mode = (app.value.proxy_mode as 'local' | 'gateway') ?? 'local'
    proxyForm.value.gateway_host_id = app.value.proxy_gateway_host_id ?? null
    proxyForm.value.upstream = app.value.proxy_upstream ?? ''
    // WebSocket 开关同样来自服务端存量：存量记录没有该字段时默认 true，
    // 与此前同步恒渲染 Upgrade 头的行为一致，避免老应用被静默关掉。
    proxyForm.value.websocket = app.value.proxy_websocket ?? true
  }
}

// syncForm=false 时只刷新证书/主机/组网等只读数据，不覆盖反代表单：
// onActivated 走这条路，否则切走再切回会用服务端值抹掉未保存的编辑。
async function loadProxyData(syncForm = true) {
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
  if (syncForm) syncProxyForm()
}

function certDaysRemaining(notAfter: string): number {
  const diff = new Date(notAfter).getTime() - Date.now()
  return Math.max(0, Math.ceil(diff / (1000 * 60 * 60 * 24)))
}

const certMap = computed(() => new Map(certs.value.map((c) => [c.id, c])))

const certOptions = computed(() => {
  return certs.value.map((c) => {
    const sans = c.domains.join(', ') || c.id
    const issuer = c.issuer || (c.source === 'manual_upload' ? '手动上传' : c.account_name || 'ACME')
    const days = certDaysRemaining(c.not_after)
    return {
      label: `${sans} · ${issuer} · 剩余 ${days} 天`,
      value: c.id,
    }
  })
})

const boundDomains = computed<string[]>(() => {
  if (app.value?.proxy_domains && app.value.proxy_domains.length > 0) {
    return app.value.proxy_domains.map((b) => b.domain)
  }
  return app.value?.domain ? [app.value.domain] : []
})

function addProxyDomain() {
  proxyForm.value.domains.push({ domain: '', cert_id: certs.value[0]?.id || '' })
}

function removeProxyDomain(index: number) {
  proxyForm.value.domains.splice(index, 1)
}

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
// 最近一次保存转发配置时后端返回的中继同步告警，展示在转发表单附近，
// 下一次保存时清空（成功即消失，不靠 toast 一闪而过）。
const forwardSyncWarning = ref('')

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
  forwardSyncWarning.value = ''
  try {
    const res = await updateApp(appId.value, {
      mesh_forward_enabled: forwardForm.value.enabled,
      mesh_forward_host_id: forwardForm.value.enabled ? forwardForm.value.host_id || undefined : undefined,
      mesh_forward_entry_port: forwardForm.value.enabled ? forwardForm.value.entry_port ?? undefined : undefined,
      mesh_forward_target_port: forwardForm.value.enabled ? forwardForm.value.target_port ?? undefined : undefined,
    })
    await loadApp()
    if (!forwardForm.value.enabled) {
      message.success('已关闭组网端口转发')
      return
    }
    // 配置已保存 ≠ 中继已生效：后端同步失败时顶层带 mesh_forward_warning，
    // 此时绝不能再报“已保存并同步”；重载后目标仍为空同样说明中继未就绪。
    if (res.meshForwardWarning) {
      forwardSyncWarning.value = res.meshForwardWarning
      message.warning(`组网端口转发已保存，但中继同步告警：${res.meshForwardWarning}`)
    } else if (!app.value?.mesh_forward_target) {
      forwardSyncWarning.value = '中继容器未同步成功（未解析到中继目标），请检查入口主机状态后重试'
      message.warning(`组网端口转发已保存，但${forwardSyncWarning.value}`)
    } else {
      message.success('组网端口转发已保存并同步中继容器')
    }
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingForward.value = false
  }
}

async function doBindProxy() {
  if (proxyForm.value.domains.length === 0) {
    message.warning('请至少添加一个域名绑定')
    return
  }
  for (let i = 0; i < proxyForm.value.domains.length; i++) {
    const r = proxyForm.value.domains[i]
    const d = r.domain.trim()
    if (!d) {
      message.warning(`第 ${i + 1} 行域名不能为空`)
      return
    }
    if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i.test(d)) {
      message.warning(`第 ${i + 1} 行域名格式不正确: ${d}`)
      return
    }
    if (!r.cert_id) {
      message.warning(`请为域名 ${d} 选择覆盖证书`)
      return
    }
  }
  const domainSet = new Set<string>()
  for (const r of proxyForm.value.domains) {
    const d = r.domain.trim().toLowerCase()
    if (domainSet.has(d)) {
      message.warning(`域名重复: ${d}`)
      return
    }
    domainSet.add(d)
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
    // 绑定响应可能带 warnings（旧目标主机离线、旧配置残留等）：绑定本身
    // 已生效，但残留必须让用户看见，不能只报“已下发”。
    const res = await bindProxy(appId.value, {
      domains: proxyForm.value.domains.map((r) => ({
        domain: r.domain.trim().toLowerCase(),
        cert_id: r.cert_id,
      })),
      mode: proxyForm.value.mode,
      gateway_host_id: proxyForm.value.gateway_host_id || undefined,
      upstream: proxyForm.value.upstream.trim() || undefined,
      websocket: proxyForm.value.websocket,
    })
    message.success('反代配置已下发')
    if (res.warnings && res.warnings.length > 0) {
      message.warning(`反代已生效，但有残留未清理：${res.warnings.join('；')}`)
    }
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
    const res = await unbindProxy(appId.value)
    message.success('已解除域名绑定')
    if (res.warnings && res.warnings.length > 0) {
      message.warning(`绑定记录已清除，但以下主机上的旧配置未能清理：${res.warnings.join('；')}`)
    }
    proxyForm.value.domains = []
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
    syncProxyForm()
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

// 值首尾成对的引号来自 .env 风格粘贴（KEY="value"），并不属于值本身；
// 服务端探测 .env.example / Dockerfile ENV 时同样会剥掉，这里保持同一口径。
// 无引号时原样返回，避免顺带改动值两侧的空格。
function stripEnvQuotes(value: string): string {
  const raw = value ?? ''
  const v = raw.trim()
  if (v.length < 2) return raw
  const q = v[0]
  return (q === '"' || q === "'") && v[v.length - 1] === q ? v.slice(1, -1) : raw
}

function syncEnvRows() {
  envRows.value = Object.entries(app.value?.env_vars ?? {}).map(([key, value]) => ({
    key,
    value: stripEnvQuotes(value),
  }))
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
  await Promise.all([loadApp(), loadStatus(), loadDeployments(), loadVolumeBackups(), loadS3Targets(), loadAppBackupJobs(), refreshNewestDeployment()])
}

const cancellingDeploy = ref(false)

async function doDeploy(force = false) {
  acting.value = true
  try {
    const dep = await deployApp(appId.value, force)
    // 记下本页会话内发起的任务：它随后若失败为“未安装 Docker”，
    // 下面 watch(newestDeployment) 会自动弹出安装提醒。
    if (dep?.id) seenActiveDepIds.add(dep.id)
    message.success(force ? '已终止当前任务并重新启动部署' : '已开始部署')
    await Promise.all([loadStatus(), loadDeployments(), refreshNewestDeployment()])
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

// ---- 未安装 Docker 时的安装提醒 ----
// 后端部署前置检查的失败文案是固定字符串（见 deploy.go
// classifyDockerPreflight），部署记录本身没有 error_code 字段，
// 前端只能以该文案识别“未安装 Docker”类失败；改动后端文案时
// 必须同步 DOCKER_MISSING_MARK。
const DOCKER_MISSING_MARK = '未安装 Docker'

// 最新一条部署记录：单独拉取第 1 页第 1 条，不依赖部署历史页签
// 的当前页码——否则用户翻到第 2 页时横幅与自动提醒都会失效。
const newestDeployment = ref<Deployment | null>(null)
async function refreshNewestDeployment() {
  try {
    const res = await listDeployments(appId.value, 0, 1)
    newestDeployment.value = res.data[0] ?? null
  } catch {
    // 提醒只是 best-effort：拉取失败时横幅/弹窗不出现，
    // 部署历史页签本身仍是事实来源。
  }
}
const dockerMissingFailed = computed(() =>
  newestDeployment.value?.status === 'failed' &&
  (newestDeployment.value.error ?? '').includes(DOCKER_MISSING_MARK),
)

// 本页会话内发起、随后失败为未安装 Docker 的任务才自动弹窗：
// 直接打开详情页时历史里已有的旧失败只由横幅提示，不自动打扰。
const seenActiveDepIds = new Set<string>()
const promptedDepIds = new Set<string>()
const showDockerInstallPrompt = ref(false)
const installingDocker = ref(false)
const dockerInstallOutput = ref('')
const dockerInstallDone = ref<'success' | 'failed' | null>(null)
const dockerLogRef = ref<HTMLPreElement | null>(null)
// 安装输出是一次性整段追加的，追加后跟到日志底部，否则用户看到的是
// 几分钟前的开头几行，还得自己往下滚。
watch(dockerInstallOutput, async () => {
  await nextTick()
  const el = dockerLogRef.value
  if (el) el.scrollTop = el.scrollHeight
})
watch(newestDeployment, (dep) => {
  if (
    !dep || dep.status !== 'failed' ||
    !(dep.error ?? '').includes(DOCKER_MISSING_MARK) ||
    !seenActiveDepIds.has(dep.id) || promptedDepIds.has(dep.id) ||
    !canWrite.value || showDockerInstallPrompt.value ||
    // 页签缓存实例在切走后仍会收到数据刷新：只给当前正在看的
    // 应用弹窗，别给后台页签的应用弹。
    !app.value || String(route.params.id ?? '') !== app.value.id
  ) return
  promptedDepIds.add(dep.id)
  openDockerInstallPrompt()
})
function openDockerInstallPrompt() {
  if (installingDocker.value) return
  dockerInstallDone.value = null
  dockerInstallOutput.value = ''
  showDockerInstallPrompt.value = true
}
function closeDockerInstallPrompt() {
  showDockerInstallPrompt.value = false
}
async function startInstallDockerFromPrompt() {
  if (!app.value) return
  installingDocker.value = true
  dockerInstallDone.value = null
  dockerInstallOutput.value = '>>> 正在下发服务端内置的 Docker 一键安装脚本 (curl -fsSL https://get.docker.com | sh)...\n'
  try {
    const res = await installDocker(app.value.host_id)
    if (res.ok) {
      dockerInstallDone.value = 'success'
      dockerInstallOutput.value += '\n>>> 安装执行完成！\n' + (res.stdout || '')
      message.success('Docker 安装完成')
    } else {
      dockerInstallDone.value = 'failed'
      dockerInstallOutput.value += '\n>>> 执行异常 (Exit code ' + res.exit_code + '):\n' + (res.stderr || res.stdout || '')
      message.error('Docker 安装未成功，请检查输出日志')
    }
  } catch (e: any) {
    dockerInstallDone.value = 'failed'
    dockerInstallOutput.value += '\n>>> 安装请求失败: ' + (e.message || e)
    message.error('安装请求失败: ' + e.message)
  } finally {
    installingDocker.value = false
  }
}
function redeployAfterDockerInstall() {
  showDockerInstallPrompt.value = false
  doDeploy(false)
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
// 备份把卷数据目录打成 tar.gz 后直传页内选定的 S3 存储目标（Dokploy 式：
// 目标先在「快照备份 → S3 存储」配好，备份时在本页选一个；目标列表走
// 快照模块的 s3-targets 接口，密钥已由服务端脱敏）。元数据在服务端；
// 下载由服务端 302 到短期 S3 预签名 URL。恢复语义：下载并校验
// SHA256 → 自动停机 → 安全副本 → 解压覆盖 → 重启容器（stop 失败则中止，绝不在线
// 覆盖运行中的数据）。
const volumeBackups = ref<VolumeArchive[]>([])
const volumeBackupTotal = ref(0)
// 备份位置选项（NSelect 空值约定 §8.3.1：未选 = null，不用 ''）。
const s3Targets = ref<S3Target[]>([])
const selectedBackupTargetId = ref<string | null>(null)
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

// 备份位置选项只在首屏与页签切回时拉一次，不跟归档 5 秒轮询走——
// 目标清单几乎不变，轮询里反复拉只是多余请求。
async function loadS3Targets() {
  try {
    const list = await listS3Targets()
    s3Targets.value = list
    // 已选目标被删掉时回到默认；从未选过时默认预选默认目标（有默认时），
    // 无默认则保持未选，强制用户显式挑一个（Dokploy 式显式选择）。
    if (selectedBackupTargetId.value && !list.some((t) => t.id === selectedBackupTargetId.value)) {
      selectedBackupTargetId.value = null
    }
    if (selectedBackupTargetId.value == null) {
      selectedBackupTargetId.value = list.find((t) => t.is_default)?.id ?? null
    }
  } catch {
    // 目标拉取失败不弹错：备份按钮由 selectedBackupTargetId 门控，
    // 用户仍可从归档列表与恢复入口正常工作。
  }
}

// ---- 本应用的备份任务（快照 app_volume 任务，详情页直接管理）----
// 任务定义与调度归快照模块，详情页只做本应用任务的 CRUD 与触发；
// 执行与归档仍走应用引擎，运行记录见下方「备份归档」与任务行的
// 最近执行/上次错误——两者合起来就是本页的备份日志（逐行 tar/上传
// 输出服务端不落盘，不在日志范围内）。
const appBackupJobs = ref<BackupJob[]>([])
const backupJobsLoading = ref(false)
const showJobModal = ref(false)
const editingJobId = ref<string | null>(null)
const savingJob = ref(false)
const runningJobId = ref('')
const togglingJobId = ref('')
const jobForm = reactive({
  name: '',
  target: null as string | null,
  storage_target: 'default',
  cron: '0 3 * * *',
  retention: 7,
  enabled: true,
})

const appBackupJobsSorted = computed(() =>
  [...appBackupJobs.value].sort((a, b) => (b.created_at || '').localeCompare(a.created_at || '')),
)

// 备份位置选项与详情页手动备份共用同一套 S3 目标（app_volume 仅支持 S3，
// 不提供「本地主机」；默认目标不存在时只列具体目标）。
const jobStorageOptions = computed(() => {
  const opts: { label: string; value: string }[] = []
  const def = s3Targets.value.find((t) => t.is_default)
  if (def) opts.push({ label: `默认 S3 目标 (${def.name} - ${def.bucket})`, value: 'default' })
  for (const t of s3Targets.value) opts.push({ label: `S3: ${t.name} (${t.bucket})`, value: t.id })
  return opts
})

function jobStorageLabel(job: BackupJob): string {
  const st = job.storage_target
  if (!st || st === 'default') {
    const def = s3Targets.value.find((t) => t.is_default)
    return def ? `S3: ${def.name}` : '未配置默认 S3'
  }
  const target = s3Targets.value.find((t) => t.id === st)
  return target ? `S3: ${target.name}` : 'S3 (未知)'
}

async function loadAppBackupJobs() {
  try {
    const all = await listBackupJobs()
    appBackupJobs.value = all.filter((j) => j.kind === 'app_volume' && j.app_id === appId.value)
  } catch {
    // 任务列表失败不弹错：归档与手动备份不受影响。
  }
}

async function refreshBackupJobs() {
  backupJobsLoading.value = true
  try {
    await loadAppBackupJobs()
  } finally {
    backupJobsLoading.value = false
  }
}

function openJobModal(job?: BackupJob) {
  if (job) {
    editingJobId.value = job.id
    jobForm.name = job.name
    jobForm.target = job.target
    jobForm.storage_target = job.storage_target || 'default'
    jobForm.cron = job.schedule?.cron ?? ''
    jobForm.retention = job.retention || 7
    jobForm.enabled = job.enabled
  } else {
    editingJobId.value = null
    jobForm.name = app.value ? `${app.value.name} 数据卷备份` : ''
    jobForm.target = parsedVolumes.value[0]?.spec ?? null
    const def = s3Targets.value.find((t) => t.is_default)
    jobForm.storage_target = def ? 'default' : (s3Targets.value[0]?.id ?? 'default')
    jobForm.cron = '0 3 * * *'
    jobForm.retention = 7
    jobForm.enabled = true
  }
  showJobModal.value = true
}

async function submitJob() {
  if (!jobForm.name.trim()) {
    message.warning('请填写任务名称')
    return
  }
  if (!jobForm.target) {
    message.warning('请选择要备份的挂载卷')
    return
  }
  if (!jobStorageOptions.value.some((o) => o.value === jobForm.storage_target)) {
    message.warning('请先在「快照备份 → S3 存储」中添加存储目标')
    return
  }
  savingJob.value = true
  try {
    if (editingJobId.value) {
      await updateBackupJob(editingJobId.value, {
        name: jobForm.name.trim(),
        target: jobForm.target,
        storage_target: jobForm.storage_target,
        retention: jobForm.retention,
        cron: jobForm.cron.trim(),
        enabled: jobForm.enabled,
      })
      message.success('备份任务已更新')
    } else {
      const req: CreateJobReq = {
        name: jobForm.name.trim(),
        host_id: '',
        kind: 'app_volume',
        app_id: appId.value,
        target: jobForm.target,
        storage_target: jobForm.storage_target,
        retention: jobForm.retention,
        cron: jobForm.cron.trim(),
        enabled: jobForm.enabled,
      }
      await createBackupJob(req)
      message.success('备份任务已创建')
    }
    showJobModal.value = false
    await loadAppBackupJobs()
  } catch (e: any) {
    message.error(e.message || '保存备份任务失败')
  } finally {
    savingJob.value = false
  }
}

async function doRunTask(job: BackupJob) {
  runningJobId.value = job.id
  try {
    await runBackupJob(job.id)
    message.success('备份任务已开始，完成后归档会出现在下方列表')
    // 复用归档轮询的有界窗口：任务运行同样产生 running 归档，窗口内
    // 轮询会同时刷新归档与任务的最近执行。
    volumePollUntil.value = Date.now() + 30 * 60_000
    setTimeout(loadAppBackupJobs, 3000)
  } catch (e: any) {
    message.error(e.message || '触发失败')
  } finally {
    runningJobId.value = ''
  }
}

async function doToggleTask(job: BackupJob) {
  togglingJobId.value = job.id
  try {
    await updateBackupJob(job.id, { enabled: !job.enabled })
    await loadAppBackupJobs()
  } catch (e: any) {
    message.error(e.message || '切换失败')
  } finally {
    togglingJobId.value = ''
  }
}

async function doDeleteTask(job: BackupJob) {
  try {
    await deleteBackupJob(job.id)
    message.success('备份任务已删除')
    await loadAppBackupJobs()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

const backupJobColumns = computed<DataTableColumns<BackupJob>>(() => [
  { title: '任务名称', key: 'name', width: 150, render: (r) => h('span', { style: 'font-weight: 600' }, r.name) },
  {
    title: '目标卷', key: 'target', ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'mono-font', style: 'font-size: 12px' }, r.target),
  },
  {
    title: '备份位置', key: 'storage_target', width: 160,
    render: (r) => {
      const label = jobStorageLabel(r)
      const warn = label === '未配置默认 S3' || label === 'S3 (未知)'
      return h(NTag, { size: 'small', type: warn ? 'warning' : 'success', bordered: false }, { default: () => label })
    },
  },
  {
    title: '调度', key: 'schedule', width: 120,
    render: (r) => r.schedule?.cron ? h('code', { style: 'font-size: 12px' }, r.schedule.cron) : '仅手动',
  },
  { title: '保留', key: 'retention', width: 70, render: (r) => `${r.retention} 份` },
  {
    title: '启用', key: 'enabled', width: 70,
    render: (r) => canWrite.value
      ? h(NSwitch, { size: 'small', value: r.enabled, loading: togglingJobId.value === r.id, onUpdateValue: () => doToggleTask(r) })
      : h(NTag, { size: 'small', bordered: false, type: r.enabled ? 'success' : 'default' }, { default: () => r.enabled ? '启用' : '停用' }),
  },
  {
    title: '最近执行', key: 'last_run', width: 130,
    render: (r) => {
      if (!r.last_run) return renderVolDash()
      return h(NSpace, { size: 4, align: 'center' }, {
        default: () => [
          renderVolTime(r.last_run),
          r.last_error ? h(NTag, { size: 'tiny', type: 'error', bordered: false }, { default: () => '失败' }) : null,
        ],
      })
    },
  },
  {
    title: '上次错误', key: 'last_error', ellipsis: { tooltip: true },
    render: (r) => r.last_error ? h('span', { style: 'font-size: 12px' }, r.last_error) : renderVolDash(),
  },
  {
    title: '操作', key: 'actions', width: 210, fixed: 'right',
    render: (r) => canWrite.value
      ? h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, type: 'primary', loading: runningJobId.value === r.id, onClick: () => doRunTask(r) }, { default: () => '立即备份' }),
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openJobModal(r) }, { default: () => '编辑' }),
          h(NPopconfirm, { onPositiveClick: () => doDeleteTask(r) }, {
            trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => '删除' }),
            default: () => `确定删除备份任务「${r.name}」？`,
          }),
        ],
      })
      : renderVolDash(),
  },
])

async function loadVolumeBackups() {
  // 不依赖 app.value：首屏 refreshAll 与 loadApp 并行，应用详情尚未返回时
  // 归档列表同样可以加载（只需路由里的 appId）。
  try {
    const res = await listVolumeBackups(appId.value, { page_size: 100 })
    volumeBackups.value = res.data
    volumeBackupTotal.value = res.total
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
  const targetId = selectedBackupTargetId.value
  if (!targetId) {
    message.warning('请先选择备份位置；若还没有存储目标，先在「快照备份 → S3 存储」中添加')
    return
  }
  backingVolumes.value = true
  try {
    const note = volBackupNote.value.trim()
    const payload = selectedVolumeSpecs.value.length > 0
      ? { volumes: selectedVolumeSpecs.value, note, target_id: targetId }
      : { note, target_id: targetId }
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

// ---- 上传备份并恢复 ----
// 浏览器到不了被管主机，文件先经面板暂存、再由 Agent 一次性回拉，
// 之后走与归档恢复同一条执行链（校验→停机→安全副本→解包→重启）。
// NUpload 只做文件选择，真正的上传在确认弹窗后由提交按钮发起。
const uploadTargetVolume = ref<string | null>(null)
const uploadFileList = ref<UploadFileInfo[]>([])
const uploadFile = ref<File | null>(null)
const uploadSha = ref('')
const uploadingRestore = ref(false)

function handleUploadSelect({ file, onFinish }: UploadCustomRequestOptions) {
  if (file.file) uploadFile.value = file.file
  // 只选文件不上传：标记完成，避免文件行一直停在上传中状态。
  onFinish()
}

function handleUploadRemove() {
  uploadFile.value = null
  return true
}

async function doUploadRestore() {
  if (!uploadTargetVolume.value) {
    message.warning('请选择要恢复到的挂载卷')
    return
  }
  if (!uploadFile.value) {
    message.warning('请选择备份文件')
    return
  }
  const sha = uploadSha.value.trim().toLowerCase()
  if (sha && !/^[0-9a-f]{64}$/.test(sha)) {
    message.warning('SHA256 格式无效（应为 64 位十六进制）')
    return
  }
  uploadingRestore.value = true
  try {
    const form = new FormData()
    form.append('file', uploadFile.value)
    form.append('volume_spec', uploadTargetVolume.value)
    if (sha) form.append('sha256', sha)
    const res = await uploadVolumeRestore(appId.value, form)
    // 复用恢复轮询：新记录的 last_restore_at 变化即判定恢复结束。
    restoringArchiveID.value = res.archive_id
    restoreBaseline.value = ''
    volumePollUntil.value = Date.now() + 30 * 60_000
    message.success(res.message || '恢复任务已开始')
    uploadFileList.value = []
    uploadFile.value = null
    uploadSha.value = ''
    uploadTargetVolume.value = null
    await loadVolumeBackups()
  } catch (e: any) {
    message.error(e.message || '上传恢复失败')
  } finally {
    uploadingRestore.value = false
  }
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
  {
    title: '状态', key: 'status', width: 84,
    // 上传记录无 S3 对象、状态恒为 uploaded，不走备份成功/失败的标签。
    render: (r) => r.source === 'upload'
      ? h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => '上传' })
      : volArchiveStatusTag(r.status),
  },
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
          default: () => r.source === 'upload'
            ? '删除该上传恢复记录？此操作不可恢复。'
            : '删除归档（含 S3 上的归档对象）？此操作不可恢复。',
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

// GitHub App 托管判定：App 已配置，且该应用仓库落在某个未暂停安装的
// 仓库清单里。仓库地址归一化与后端 NormalizeRepoURL 同款（去 .git 后缀、
// 小写、https/ssh 两种写法都认）。
const githubAppStatus = ref<GitHubAppStatus | null>(null)
function normalizeRepoUrl(raw: string): string {
  let s = (raw || '').trim().replace(/\/$/, '').replace(/\.git$/, '')
  const idx = s.indexOf('github.com')
  if (idx >= 0) return s.slice(idx + 'github.com'.length).replace(/^[/:]+/, '').toLowerCase()
  if ((s.match(/\//g) || []).length === 1 && !/[:@ ]/.test(s)) return s.toLowerCase()
  return ''
}
const appManagedByGitHubApp = computed(() => {
  const st = githubAppStatus.value
  if (!st?.configured || !app.value?.auto_deploy || !app.value?.repo_url) return false
  const norm = normalizeRepoUrl(app.value.repo_url)
  if (!norm) return false
  return (st.installations || []).some(
    (inst) => !inst.suspended && (inst.repos || []).includes(norm),
  )
})
async function loadGitHubAppStatus() {
  try {
    githubAppStatus.value = await getGitHubAppStatus()
  } catch (e: any) {
    // silently fail：GitHub App 未配置时详情页其余内容不受影响
  }
}
const switchingToApp = ref(false)
async function doSwitchToGitHubApp() {
  if (!app.value?.id) return
  switchingToApp.value = true
  try {
    const res = await switchAppToGitHubApp(app.value.id)
    message.success(res.message || '已切换到 GitHub App 触发')
    app.value = res.data as AppEntity
    await loadGitHubAppStatus()
  } catch (e: any) {
    message.error(e.message || '切换失败')
  } finally {
    switchingToApp.value = false
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
  t === 'create' ? '创建'
    : t === 'webhook' ? 'Git 推送'
      : t === 'rollback' ? '回滚'
        : t === 'migrate' ? '迁移'
          : '手动'

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

// 部署结束（部署中 → 非部署中）时刷新一次部署历史与最新记录：
// 快速失败的任务（如 Docker 前置检查，1 秒内落终态）会在两次
// 轮询之间结束，只靠“部署中才刷历史”的轮询永远刷不出来，
// 失败原因和安装提醒也就跟着出不来。
watch(deploying, (now, prev) => {
  if (prev && !now && route.name === 'app-detail') {
    loadDeployments()
    refreshNewestDeployment()
  }
})

function startPolling() {
  if (pollTimer) return
  pollTimer = setInterval(() => {
    if (route.name === 'app-detail') {
      loadStatus()
      if (deploying.value) loadDeployments()
      // 备份归档里的 running 记录由异步 agent tar 推进，轮询直到落终态；
      // 恢复只在终态回写归档，故用受理后开启的有界窗口兜底。
      if (volumeBackupRunning.value || Date.now() < volumePollUntil.value) {
        loadVolumeBackups()
        loadAppBackupJobs()
      }
    }
  }, 5000)
}

onMounted(() => {
  refreshAll()
  loadProxyData()
  loadGitHubAppStatus()
  startPolling()
})

onActivated(() => {
  // 切回页签时只刷新只读状态（状态/部署历史/归档/证书与主机列表），不重载
  // 应用详情：loadApp() 会经 sync* 用服务端值覆盖环境变量/反代/转发表单，
  // 用户切走前未保存的编辑会被无声抹掉（违反 AGENTS.md §8.4 页签保状态）。
  // 首屏仍由 onMounted 的 refreshAll() 全量加载。
  loadStatus()
  loadDeployments()
  refreshNewestDeployment()
  loadVolumeBackups()
  loadS3Targets()
  loadAppBackupJobs()
  loadProxyData(false)
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
        <NButton
          size="small"
          :loading="acting"
          :disabled="deploying || status?.state === 'undeployed'"
          @click="showMigrate = true"
        >
          <template #icon>
            <NIcon><MoveOutline /></NIcon>
          </template>
          迁移服务
        </NButton>
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

    <!-- 最新一次部署死于“未安装 Docker”：常驻提醒，直到一次非此类
      失败/成功的部署把它顶掉。安装入口与部署失败自动弹窗共用
      同一个安装流程。 -->
    <NAlert
      v-if="dockerMissingFailed"
      type="warning"
      style="flex-shrink: 0"
      title="目标主机未安装 Docker，本应用无法部署"
    >
      最近一次部署在前置检查失败：目标主机没有 Docker，任何部署都会直接失败。
      <div v-if="canWrite" style="margin-top: 8px">
        <NButton size="small" type="warning" @click="openDockerInstallPrompt">一键安装 Docker</NButton>
      </div>
    </NAlert>

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
              <NDescriptionsItem v-if="boundDomains.length" label="域名">
                <NSpace :size="6" :wrap="true">
                  <NTag v-for="d in boundDomains" :key="d" size="small" type="success" :bordered="false">{{ d }}</NTag>
                </NSpace>
              </NDescriptionsItem>
            </NDescriptions>

            <NAlert v-if="app.auto_deploy && appManagedByGitHubApp" type="success" style="margin-top: 14px" :show-icon="true">
              <template #header>自动部署已生效 (GitHub App 托管)</template>
              <div>
                该仓库在 GitHub App 的安装范围内。每当向 <code>{{ app.branch || 'main' }}</code> 分支推送代码（git push）时，App 级事件将自动触发构建并重新部署，无需为该仓库单独配置 Webhook。
              </div>
            </NAlert>
            <NAlert v-else-if="app.auto_deploy && app.webhook_auto_managed" type="success" style="margin-top: 14px" :show-icon="true">
              <template #header>自动部署已生效 (GitHub 自动托管)</template>
              <div>
                系统已自动通过已连接的 GitHub 账号在仓库中配置了 Webhook。每当向 <code>{{ app.branch || 'main' }}</code> 分支推送代码（git push）时，将自动触发构建并重新部署（无需手动配置 Webhook）。
              </div>
              <div style="margin-top: 8px">
                <NButton size="tiny" secondary :loading="syncingWebhook" @click="doSyncWebhook">
                  重新同步 Webhook
                </NButton>
                <NButton
                  v-if="githubAppStatus?.configured"
                  size="tiny"
                  secondary
                  :loading="switchingToApp"
                  style="margin-left: 8px"
                  @click="doSwitchToGitHubApp"
                >
                  切换到 GitHub App
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
                <NButton
                  v-if="app.repo_url?.includes('github.com') && githubAppStatus?.configured"
                  size="tiny"
                  secondary
                  :loading="switchingToApp"
                  @click="doSwitchToGitHubApp"
                >
                  切换到 GitHub App
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
              <NAlert v-if="s3Targets.length === 0" type="warning" style="margin-bottom: 12px">
                <template #header>还没有 S3 存储目标</template>
                数据卷归档统一存放在 S3（不在应用主机留存），请先在「快照备份 → S3 存储」中添加存储目标，添加后即可在本页发起备份时选择。
                <NButton size="tiny" secondary style="margin-left: 8px" @click="$router.push('/snapshots')">
                  前往 S3 存储配置
                </NButton>
              </NAlert>
              <NAlert v-else type="info" style="margin-bottom: 12px">
                备份将卷数据目录打包为 tar.gz，直传下方所选的 S3 存储目标，
                每卷保留最近 5 份成功归档。备份在容器运行中直接打包，属于在线文件快照——数据库等对跨文件一致性敏感的
                应用建议选业务低峰期执行。恢复时会先下载归档并校验 SHA256，<strong>自动停止应用容器</strong>、
                保留当前状态安全副本后解压覆盖，完成后自动重启容器。
                <div style="margin-top: 6px">
                  定时备份任务在下方「备份任务」中创建与管理（调度执行仍由快照模块负责，归档与本页共用同一份记录），全部任务也可在快照备份页按应用聚合查看。
                  <NButton secondary size="tiny" style="margin-left: 6px" @click="$router.push('/snapshots')">前往快照备份</NButton>
                </div>
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
                  <div class="vol-form-row">
                    <NSelect
                      v-model:value="selectedVolumeSpecs"
                      multiple
                      clearable
                      :options="parsedVolumes.map((v) => ({ label: v.spec, value: v.spec }))"
                      placeholder="全部挂载卷（不选则备份所有）"
                    />
                    <NSelect
                      v-model:value="selectedBackupTargetId"
                      :options="s3Targets.map((t) => ({ label: `${t.name} · ${t.bucket}`, value: t.id }))"
                      placeholder="请选择备份位置"
                      :disabled="s3Targets.length === 0"
                    />
                  </div>
                  <div class="vol-backup-row">
                    <NInput
                      v-model:value="volBackupNote"
                      placeholder="备注（可选）"
                      style="flex: 1; min-width: 200px"
                      :maxlength="200"
                      show-count
                    />
                    <NButton
                      type="primary"
                      size="small"
                      :loading="backingVolumes"
                      :disabled="!selectedBackupTargetId || volumeBusy || deploying"
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

              <!-- 备份任务（快照 app_volume 任务，详情页直接管理；任务数 = 本应用任务数，天然有限，属 §8.2 例外：不分页） -->
              <div class="vol-section">
                <div class="vol-section-title">
                  <span>
                    备份任务
                    <span v-if="appBackupJobs.length > 0" class="vol-count">（{{ appBackupJobs.length }} 个）</span>
                  </span>
                  <NSpace :size="6">
                    <NButton v-if="canWrite" size="tiny" secondary @click="openJobModal()">新建备份任务</NButton>
                    <NButton size="tiny" secondary :loading="backupJobsLoading" @click="refreshBackupJobs">刷新</NButton>
                  </NSpace>
                </div>
                <NDataTable
                  :columns="backupJobColumns"
                  :data="appBackupJobsSorted"
                  size="small"
                  :bordered="false"
                  :loading="backupJobsLoading"
                  :scroll-x="1080"
                  :row-key="(r: BackupJob) => r.id"
                >
                  <template #empty>
                    <NEmpty
                      :description="canWrite ? '暂无备份任务，点「新建备份任务」为本应用创建定时备份' : '暂无备份任务'"
                      size="small"
                    />
                  </template>
                </NDataTable>
                <p class="muted tip-hint" style="margin-top: 8px">
                  备份任务由快照备份模块调度执行，运行产生的归档与下方「备份归档」共用同一份记录；任务行的最近执行与上次错误即备份日志。
                </p>
              </div>

              <NModal
                v-model:show="showJobModal"
                preset="card"
                :title="editingJobId ? '编辑备份任务' : '新建备份任务'"
                style="width: 560px; max-width: 94vw"
              >
                <NForm label-placement="top">
                  <NFormItem label="任务名称" required>
                    <NInput v-model:value="jobForm.name" placeholder="如 每日凌晨备份数据卷" />
                  </NFormItem>
                  <NFormItem label="目标卷" required>
                    <NSelect
                      v-model:value="jobForm.target"
                      :options="parsedVolumes.map((v) => ({ label: v.spec, value: v.spec }))"
                      placeholder="选择要备份的挂载卷"
                    />
                  </NFormItem>
                  <NFormItem label="备份位置" required>
                    <NSelect
                      v-model:value="jobForm.storage_target"
                      :options="jobStorageOptions"
                      placeholder="请选择备份位置"
                      :disabled="jobStorageOptions.length === 0"
                    />
                  </NFormItem>
                  <NAlert v-if="jobStorageOptions.length === 0" type="warning" style="margin-bottom: 12px">
                    还没有 S3 存储目标，应用数据卷归档仅支持 S3。请先到「快照备份 → S3 存储」添加。
                  </NAlert>
                  <div class="vol-modal-row">
                    <NFormItem label="定时计划（Cron 5 段）" style="flex: 1; min-width: 0">
                      <NInput v-model:value="jobForm.cron" placeholder="0 3 * * *（留空仅手动）" class="mono-font" />
                    </NFormItem>
                    <NFormItem label="保留份数" style="flex-shrink: 0; width: 140px">
                      <NInputNumber v-model:value="jobForm.retention" :min="1" :max="100" style="width: 100%" />
                    </NFormItem>
                  </div>
                  <NFormItem label="启用定时调度">
                    <NSwitch v-model:value="jobForm.enabled" />
                  </NFormItem>
                </NForm>
                <template #footer>
                  <NSpace justify="end">
                    <NButton @click="showJobModal = false">取消</NButton>
                    <NButton type="primary" :loading="savingJob" :disabled="jobStorageOptions.length === 0" @click="submitJob">保存</NButton>
                  </NSpace>
                </template>
              </NModal>

              <!-- 上传备份并恢复：文件先经面板暂存、再由 Agent 回拉，恢复语义与归档恢复一致 -->
              <div v-if="canWrite" class="vol-section">
                <div class="vol-section-title">
                  <span>上传备份并恢复</span>
                </div>
                <NSpace vertical :size="10">
                  <div class="vol-form-row">
                    <NSelect
                      v-model:value="uploadTargetVolume"
                      :options="parsedVolumes.map((v) => ({ label: v.spec, value: v.spec }))"
                      placeholder="选择要恢复到的挂载卷"
                    />
                    <NInput
                      v-model:value="uploadSha"
                      placeholder="可选：文件 SHA256（填了才校验）"
                      class="mono-font"
                    />
                  </div>
                  <NUpload
                    v-model:file-list="uploadFileList"
                    :max="1"
                    :multiple="false"
                    accept=".tar.gz,.tgz,application/gzip"
                    :custom-request="handleUploadSelect"
                    @remove="handleUploadRemove"
                  >
                    <NButton secondary>选择备份文件（.tar.gz）</NButton>
                  </NUpload>
                  <NAlert type="warning">
                    恢复将自动停止应用容器，用上传文件覆盖所选卷数据（覆盖前保留当前状态安全副本），完成后自动重启容器。
                    文件先暂存面板、恢复结束即删（不超过 2GB）；Agent 须能通过面板地址回拉文件，拉取或校验失败在停机前即报错，不产生停机。
                  </NAlert>
                  <div>
                    <NPopconfirm @positive-click="doUploadRestore">
                      <template #trigger>
                        <NButton
                          type="warning"
                          :loading="uploadingRestore"
                          :disabled="!uploadTargetVolume || !uploadFile || volumeBusy || deploying"
                        >
                          上传并恢复
                        </NButton>
                      </template>
                      确认把上传文件恢复到所选挂载卷？恢复会覆盖该卷现有数据。
                    </NPopconfirm>
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
                      :description="s3Targets.length > 0 ? '暂无备份归档，选好备份位置与挂载卷后点「开始备份」' : '暂无备份归档'"
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
              v-if="boundDomains.length"
              type="success"
              :show-icon="true"
              style="margin-bottom: 14px"
            >
              <template #header>已绑定 {{ boundDomains.length }} 个域名</template>
              <div style="margin-bottom: 6px">
                <NSpace :size="6" :wrap="true">
                  <NTag
                    v-for="b in (app.proxy_domains || [{ domain: app.domain || '', cert_id: '' }])"
                    :key="b.domain"
                    size="small"
                    type="success"
                    :bordered="false"
                  >
                    {{ b.domain }}
                    <span v-if="b.cert_id && certMap.get(b.cert_id)" style="opacity: 0.8; margin-left: 4px">
                      ({{ certMap.get(b.cert_id)?.domains.join(', ') || b.cert_id }})
                    </span>
                  </NTag>
                </NSpace>
              </div>
              反代模式：{{ app.proxy_mode === 'gateway' ? '网关统一入口' : '节点本地' }}，上游：{{ app.proxy_upstream }}
            </NAlert>
            <NAlert
              v-else
              type="default"
              :show-icon="false"
              style="margin-bottom: 14px"
            >
              尚未绑定域名。绑定后 Watchman 会自动把证书与 Nginx 反代配置下发到目标主机并热加载。
            </NAlert>

            <!-- 流量拓扑：让用户直观看到公网流量如何到达内网应用 -->
            <div v-if="boundDomains.length" class="topology-box">
              <div class="topology-line">
                <div class="topology-node">
                  <div class="topology-label">公网用户</div>
                  <div class="topology-value">{{ boundDomains.join(', ') }}</div>
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

            <NForm label-placement="top" style="max-width: 680px">
              <NFormItem label="域名与证书绑定（多域名独立选择证书）" required>
                <div style="width: 100%">
                  <NSpace vertical style="width: 100%">
                    <div
                      v-for="(row, idx) in proxyForm.domains"
                      :key="idx"
                      style="display: flex; gap: 8px; align-items: center; margin-bottom: 8px"
                    >
                      <NInput
                        v-model:value="row.domain"
                        placeholder="api.example.com"
                        style="flex: 1"
                      />
                      <NSelect
                        v-model:value="row.cert_id"
                        :options="certOptions"
                        placeholder="选择覆盖证书"
                        filterable
                        style="flex: 1.5"
                      />
                      <NButton
                        type="error"
                        quaternary
                        @click="removeProxyDomain(idx)"
                      >
                        删除
                      </NButton>
                    </div>
                    <NButton dashed @click="addProxyDomain" style="width: 100%">
                      + 添加域名
                    </NButton>
                  </NSpace>
                </div>
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
                <NPopconfirm v-if="boundDomains.length" @positive-click="doUnbindProxy">
                  <template #trigger>
                    <NButton secondary type="error" :loading="savingProxy">解除绑定</NButton>
                  </template>
                  确定解除 {{ boundDomains.join(', ') }} 的反代绑定？
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

              <NAlert
                v-if="forwardSyncWarning"
                type="warning"
                :show-icon="true"
                style="margin-bottom: 12px"
              >
                <template #header>中继同步告警</template>
                {{ forwardSyncWarning }}
              </NAlert>

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

    <!-- 未安装 Docker：安装提醒。部署失败由 watch(newestDeployment)
      自动弹出；横幅上的一键安装也进同一个弹窗。安装中禁止关闭，
      否则请求在后台继续跑、结果没人接。 -->
    <NModal
      v-model:show="showDockerInstallPrompt"
      preset="card"
      title="目标主机未安装 Docker"
      style="width: 760px; max-width: 94vw"
      :mask-closable="!installingDocker"
      :closable="!installingDocker"
      :close-on-esc="!installingDocker"
    >
      <div class="docker-install-body">
        <template v-if="dockerInstallDone === null && !installingDocker">
          <p class="docker-install-lead">
            本次部署在前置检查就失败了：目标主机上没有 Docker，无法拉起任何容器。是否立即为该主机安装 Docker？
          </p>
          <div class="docker-install-cmd"><code>curl -fsSL https://get.docker.com | sh</code></div>
          <p class="docker-install-hint">
            将执行服务端内置的官方一键安装脚本，依赖网络下载，通常需要 1~3 分钟。
          </p>
        </template>
        <template v-else>
          <div class="docker-install-status">
            <template v-if="installingDocker">
              <NSpin :size="14" />
              <span>正在安装 Docker，依赖网络下载，通常需要 1~3 分钟，请耐心等待…</span>
            </template>
            <template v-else-if="dockerInstallDone === 'success'">
              <NTag type="success" size="small" :bordered="false">安装完成</NTag>
              <span>Docker 已就绪，可以立即重新部署该应用。</span>
            </template>
            <template v-else>
              <NTag type="error" size="small" :bordered="false">安装失败</NTag>
              <span>请检查下方输出，或到主机详情 Docker 页签手动安装。</span>
            </template>
          </div>
          <pre ref="dockerLogRef" class="docker-install-log">{{ dockerInstallOutput }}</pre>
        </template>
      </div>
      <template #footer>
        <NSpace justify="end">
          <NButton v-if="dockerInstallDone === null && !installingDocker" @click="closeDockerInstallPrompt">暂不安装</NButton>
          <NButton
            v-if="dockerInstallDone === null"
            type="primary"
            :loading="installingDocker"
            @click="startInstallDockerFromPrompt"
          >
            安装 Docker
          </NButton>
          <NButton v-if="dockerInstallDone === 'failed' && !installingDocker" @click="startInstallDockerFromPrompt">重新安装</NButton>
          <NButton v-if="dockerInstallDone === 'success'" type="primary" @click="redeployAfterDockerInstall">立即重新部署</NButton>
          <NButton v-if="dockerInstallDone !== null && !installingDocker" @click="closeDockerInstallPrompt">关闭</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 运维隧道一键入口（kubectl port-forward 式） -->
    <OpsTunnelModal v-model:show="showTunnel" :prefill="tunnelPrefill" />

    <!-- 服务迁移：跨主机搬迁（可选经 S3 携带挂载卷数据） -->
    <AppMigrateModal v-model:show="showMigrate" :app="app" @migrated="onMigrated" />

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
  gap: 12px;
}

.detail-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  background-color: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  box-shadow: var(--shadow-sm);
  padding: 10px 16px;
}

.app-title {
  font-size: 16px;
  font-weight: 600;
}

.detail-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background-color: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  box-shadow: var(--shadow-sm);
  padding: var(--card-padding);
  overflow: hidden;
}

/* 必须显式撑满页签高度：父级 .n-tab-pane 是固定高、.detail-body 又是
   overflow: hidden，子容器高度按内容自长时滚动条永不出现、底部内容被裁掉。 */
.overview-scroll {
  height: 100%;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 10px 2px 0 0;
}

.deployments-pane {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding-top: 10px;
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
  min-height: 28px;
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 10px;
  padding-bottom: 8px;
  border-bottom: 1px solid var(--border-subtle);
}

/* 表单并排一行：窄屏自动换行，固定宽输入不再挤出容器 */
.vol-form-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}

.vol-form-row > * {
  flex: 1;
  min-width: 240px;
}

.vol-modal-row {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
  align-items: flex-start;
}

.vol-modal-row > .n-form-item {
  margin-bottom: 18px;
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

/* 未安装 Docker 弹窗：介绍态与安装日志。日志面板限高滚动，
   弹窗高度可控、底部操作按钮始终可见；面板固定深色终端底，
   与浅色/暗色主题下的命令块保持同一视觉。 */
.docker-install-lead {
  margin: 0 0 12px;
  line-height: 1.7;
}

.docker-install-cmd {
  margin: 0 0 12px;
  padding: 10px 14px;
  border-radius: 8px;
  background: var(--cmd-box-bg);
  border: 1px solid rgba(255, 255, 255, 0.08);
  overflow-x: auto;

  code {
    font-family: 'JetBrains Mono', Consolas, monospace;
    font-size: 12px;
    color: #e2e8f0;
    white-space: nowrap;
  }
}

.docker-install-hint {
  margin: 0;
  font-size: 12px;
  color: var(--text-secondary);
}

.docker-install-status {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
  font-size: 13px;
  color: var(--text-secondary);
}

.docker-install-log {
  margin: 0;
  height: min(46vh, 420px);
  min-height: 200px;
  overflow: auto;
  overscroll-behavior: contain;
  background: var(--cmd-box-bg);
  border: 1px solid rgba(255, 255, 255, 0.08);
  border-radius: 8px;
  padding: 14px 16px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  line-height: 1.65;
  color: #dbe4f0;
  white-space: pre-wrap;
  word-break: break-all;

  &::-webkit-scrollbar {
    width: 10px;
    height: 10px;
  }

  &::-webkit-scrollbar-thumb {
    background: rgba(255, 255, 255, 0.16);
    border-radius: 6px;
    border: 2px solid transparent;
    background-clip: content-box;
  }

  &::-webkit-scrollbar-track {
    background: transparent;
  }
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