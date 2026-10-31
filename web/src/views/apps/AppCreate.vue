<script setup lang="ts">
import { computed, onActivated, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert, NAvatar, NButton, NCard, NCollapse, NCollapseItem, NForm, NFormItem, NGi, NGrid, NIcon, NInput, NInputNumber,
  NRadio, NRadioButton, NRadioGroup, NSelect, NSpace, NSwitch, NTag, useMessage,
} from 'naive-ui'
import {
  ArrowBackOutline, LogoGithub, RefreshOutline,
} from '@vicons/ionicons5'
import {
  createApp, listAppCatalog, inspectImage, inspectRepo,
  type AppEntity, type AppTemplate, type ImageMetadata, type PortMapping, type RepoMetadata,
} from '../../api/apps'
import {
  bindProxy, getCertCover, listCerts,
  type CertCover, type Certificate, type ProxyBindDomain,
} from '../../api/certs'
import { listHosts } from '../../api/hosts'
import { getNetworkConfig, listNetworkNodes, probeMeshIP, type NetworkConfig } from '../../api/network'
import { bothMeshed } from '../../utils/mesh'
import { listCredentials } from '../../api/vault'
import {
  getGitProviders,
  listGitHubBranches, listGitHubRepos, type GitHubAccount,
  type GitHubBranch, type GitHubRepo,
  getGitHubAppStatus, listGitHubAppRepos, listGitHubAppBranches,
  type GitHubAppInstalledRepo,
} from '../../api/git'
import { useWorkspaceStore } from '../../stores/workspace'
import AppAiAssistant from '../../components/apps/AppAiAssistant.vue'
import AppTopologyPreview from '../../components/apps/AppTopologyPreview.vue'
import CertLogModal from '../../components/cert/CertLogModal.vue'
import { useCertIssuance } from '../../composables/useCertIssuance'
import type { AppFormDraft, AppFormSnapshot } from '../../api/ai'

defineOptions({ name: 'AppCreate' })

const route = useRoute()
const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()

interface HostOption {
  id: string
  hostname: string
  status: string
  os: string
  public_ip?: string
}

interface VaultOption {
  id: string
  name: string
  type: string
}

interface MeshNode {
  host_id: string
  ip: string
  online: boolean
}

const hosts = ref<HostOption[]>([])
const vaults = ref<VaultOption[]>([])
const templates = ref<AppTemplate[]>([])
const meshNodes = ref<MeshNode[]>([])
const creating = ref(false)

// ---- 异地组网：全局配置、选中主机实时组网 IP ----
const networkConfig = ref<NetworkConfig | null>(null)
const networkConfigLoaded = ref(false)
const meshConfigDismissed = ref(false)
// 配置拉取失败（null）时不误报；确实未配置 Auth Key 时才提示
const meshConfigured = computed(() =>
  networkConfig.value === null ? true : !!networkConfig.value.auth_key_set,
)
const meshProbeLoading = ref(false)
// 实时探测结果只对「当前选中主机」有效，换主机即失效
const meshProbeResult = ref<{ hostId: string; ip: string; source: '' | 'live' | 'cache' } | null>(null)

// 部署来源。模板/单镜像/Compose 不依赖 Git，Git 保持原有授权与手填两种子模式。
type SourceMode = 'template' | 'image' | 'compose' | 'github' | 'custom'
const sourceMode = ref<SourceMode>('template')

const defaultComposeTemplate = `services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
    restart: unless-stopped
    environment:
      - NODE_ENV=production
  redis:
    image: redis:alpine
    restart: unless-stopped
`

const wordpressTemplate = `services:
  wordpress:
    image: wordpress:latest
    ports:
      - "8080:80"
    restart: unless-stopped
    environment:
      - WORDPRESS_DB_HOST=db
      - WORDPRESS_DB_USER=wordpress
      - WORDPRESS_DB_PASSWORD=wordpress
      - WORDPRESS_DB_NAME=wordpress
  db:
    image: mysql:5.7
    restart: unless-stopped
    environment:
      - MYSQL_DATABASE=wordpress
      - MYSQL_USER=wordpress
      - MYSQL_PASSWORD=wordpress
      - MYSQL_RANDOM_ROOT_PASSWORD='1'
`

// GitHub 授权与仓库列表状态
const githubAccount = ref<GitHubAccount | null>(null)
const checkingGitHub = ref(false)

const githubRepos = ref<GitHubRepo[]>([])
const loadingRepos = ref(false)
const selectedRepoFullName = ref<string | null>(null)

// GitHub App 已安装仓库（无 PAT 时也能选仓；取数走 App 安装令牌）。
const appInstalledRepos = ref<GitHubAppInstalledRepo[]>([])
const appConfigured = ref(false)
const githubSourceReady = computed(() => githubAccount.value !== null || appConfigured.value)

// 先选接入方式、再选仓库：两种方式各自独立的仓库清单，不做跨来源合并去重。
type GitHubMethod = 'pat' | 'app'
const githubMethod = ref<GitHubMethod | null>(null)
// 只列出实际已配置的接入方式；都未配置时由上方连接引导接管。
const githubMethodOptions = computed(() => {
  const opts: Array<{ label: string; value: GitHubMethod }> = []
  if (githubAccount.value) opts.push({ label: 'GitHub 账号（PAT）', value: 'pat' })
  if (appConfigured.value) opts.push({ label: 'GitHub App', value: 'app' })
  return opts
})

async function fetchAppInstalledRepos() {
  try {
    const st = await getGitHubAppStatus()
    appConfigured.value = st.configured
    // 断开 App 后必须清空旧清单，否则已移除的安装仓库仍会出现在下拉里。
    appInstalledRepos.value = st.configured ? await listGitHubAppRepos() : []
  } catch {
    // GitHub App 未配置时静默，PAT 流程不受影响
  }
}

// 只有一种方式可用时自动选中；两种都可用时留空，强制先做出选择。
function syncGithubMethod() {
  const available = githubMethodOptions.value.map((o) => o.value)
  if (githubMethod.value && available.includes(githubMethod.value)) return
  githubMethod.value = available.length === 1 ? available[0] : null
}

const branches = ref<GitHubBranch[]>([])
const loadingBranches = ref(false)

const form = reactive({
  name: '',
  // NSelect 绑定的字段必须用 null 做空值：空串会被 naive-ui 的 fallbackOption
  // 合成为一个 label 为空的真实选项，select 自认为"已选中"，占位符永不渲染。
  host_id: null as string | null,
  template_id: '',
  image: '',
  repo_url: '',
  branch: 'main',
  auth_vault_id: null as string | null,
  auto_deploy: true,
  build_type: 'dockerfile' as 'dockerfile' | 'compose',
  dockerfile: 'Dockerfile',
  build_context: '.',
  build_timeout_sec: 900,
  // 默认留空，由用户自行编写或点上方按钮载入「Web + Redis / WordPress + MySQL」模板
  compose_content: '',
  // 组网端口转发（TCP 流量中继）：由入口主机运行 socat 中继容器
  mesh_forward_enabled: false,
  mesh_forward_host_id: null as string | null,
  mesh_forward_entry_port: null as number | null,
  mesh_forward_target_port: null as number | null,
  env_vars_text: '',
  ports: [] as (PortMapping & { bind_scope: 'public' | 'mesh' })[],
  volumes_text: '',
  healthcheck_url: '',
  container_name: '',
  template_params: {} as Record<string, string>,
  // 域名与反代（可选，创建后立即绑定）
  domain: '',
  proxy_mode: 'local' as 'local' | 'gateway',
  gateway_host_id: null as string | null,
})

// ---- 域名与证书覆盖探测/签发配置 ----
export interface CreateDomainItem {
  id: number
  domain: string
  cert_id: string
  certCover: CertCover | null
  loading: boolean
  error: string
  seq: number
  timer: ReturnType<typeof setTimeout> | null
}

let nextRowId = 1
const domainRows = ref<CreateDomainItem[]>([
  {
    id: nextRowId++,
    domain: '',
    cert_id: '',
    certCover: null,
    loading: false,
    error: '',
    seq: 0,
    timer: null,
  },
])

const availableCerts = ref<Certificate[]>([])
const certOptions = ref<{ label: string; value: string }[]>([])
// 提交按钮文案阶段化：签发证书中… / 绑定反代中…
const creatingLabel = ref('')

// 证书自动签发编排（与详情页共用）：ACME 账户、验证方式、任务日志弹窗
const {
  canIssue,
  accounts: issueAccounts,
  accountId: issueAccountId,
  challenge: issueChallenge,
  dnsReady: dnsConfigReady,
  accountOptions,
  loadSupport: loadIssueSupport,
  logShow: certLogShow,
  logTaskId: certLogTaskId,
  logDomains: certLogDomains,
  issueDomains,
} = useCertIssuance()

function certDaysRemaining(notAfter: string): number {
  const diff = new Date(notAfter).getTime() - Date.now()
  return Math.max(0, Math.ceil(diff / (1000 * 60 * 60 * 24)))
}

function updateCertOptions() {
  certOptions.value = availableCerts.value.map((c) => {
    const sans = c.domains.join(', ') || c.id
    const issuer = c.issuer || (c.source === 'manual_upload' ? '手动上传' : c.account_name || 'ACME')
    const days = certDaysRemaining(c.not_after)
    return {
      label: `${sans} · ${issuer} · 剩余 ${days} 天`,
      value: c.id,
    }
  })
}

// 证书支撑数据：可选证书清单（高级手动指定用）+ 自动签发配置（ACME 账户 / dns-mng）
async function loadCertSupport() {
  const certs = await listCerts().catch(() => [] as Certificate[])
  availableCerts.value = certs
  updateCertOptions()
  void loadIssueSupport()
}

function addDomainRow() {
  domainRows.value.push({
    id: nextRowId++,
    domain: '',
    cert_id: '',
    certCover: null,
    loading: false,
    error: '',
    seq: 0,
    timer: null,
  })
}

function removeDomainRow(idx: number) {
  const row = domainRows.value[idx]
  if (row.timer) clearTimeout(row.timer)
  domainRows.value.splice(idx, 1)
  if (domainRows.value.length === 0) {
    addDomainRow()
  }
}

function onDomainRowChange(row: CreateDomainItem) {
  if (row.timer) clearTimeout(row.timer)
  const d = row.domain.trim().toLowerCase()
  if (domainRows.value[0]?.id === row.id) {
    form.domain = row.domain
  }
  if (!d) {
    row.cert_id = ''
    row.certCover = null
    row.loading = false
    row.error = ''
    return
  }
  row.loading = true
  row.timer = setTimeout(async () => {
    const seq = ++row.seq
    try {
      const cover = await getCertCover(d)
      if (seq !== row.seq) return
      row.certCover = cover
      row.error = ''
    } catch {
      if (seq !== row.seq) return
      row.certCover = null
      row.error = '覆盖探测失败（可手动选择证书）'
    } finally {
      if (seq === row.seq) row.loading = false
    }
  }, 500)
}

watch(() => form.domain, (newVal) => {
  if (domainRows.value[0] && domainRows.value[0].domain !== newVal) {
    domainRows.value[0].domain = newVal
    onDomainRowChange(domainRows.value[0])
  }
})

onUnmounted(() => {
  for (const r of domainRows.value) {
    if (r.timer) clearTimeout(r.timer)
  }
})

const activeDomains = computed(() => {
  return domainRows.value.filter((r) => r.domain.trim().length > 0)
})

function domainHasUsableCert(row: CreateDomainItem): boolean {
  return row.certCover?.covered === true && row.certCover.usable !== false
}

// 未显式指定证书、且当前也没有可用证书覆盖的域名：保存时由服务端自动匹配，
// 匹配不到再对这些域名批量签发一张多 SAN 证书后重试绑定。
const autoIssueDomains = computed(() => {
  return activeDomains.value.filter((r) => !r.cert_id && !domainHasUsableCert(r))
})

const hasUncoveredDomains = computed(() => autoIssueDomains.value.length > 0)

const overallCertStatus = computed<'' | 'covered' | 'issue' | 'selected'>(() => {
  if (activeDomains.value.length === 0) return ''
  if (activeDomains.value.some((r) => r.loading)) return ''
  if (activeDomains.value.every((r) => !!r.cert_id)) return 'selected'
  if (autoIssueDomains.value.length === 0) return 'covered'
  return 'issue'
})

// 高级手动指定证书：'' 是真实选项（自动匹配），不走 §8.3.1 的 null 空值约定
const certOverrideOptions = computed(() => [
  { label: '自动匹配（推荐）', value: '' },
  ...certOptions.value,
])

function domainCertHint(row: CreateDomainItem): { tone: 'ok' | 'warn'; text: string } {
  if (row.cert_id) {
    const c = availableCerts.value.find((x) => x.id === row.cert_id)
    return { tone: 'ok', text: `已手动指定证书：${c ? c.domains.join(', ') : row.cert_id}（保存时直接绑定）` }
  }
  if (row.certCover?.covered) {
    if (row.certCover.usable !== false && row.certCover.cert) {
      return {
        tone: 'ok',
        text: `已匹配可用证书：${row.certCover.cert.domains.join(', ')}（保存时自动绑定）`,
      }
    }
    return {
      tone: 'warn',
      text: `已有覆盖证书但当前不可用（${row.certCover.reason || '不可用'}），保存时将尝试自动签发`,
    }
  }
  return { tone: 'warn', text: '暂无覆盖证书，保存时将尝试自动签发' }
}

// 选模板后预填端口与卷，参数控件读模板 EnvFields
const selectedTemplate = computed(() =>
  templates.value.find((t) => t.id === form.template_id),
)

function selectTemplate(id: string) {
  form.template_id = id
  const tpl = templates.value.find((t) => t.id === id)
  if (!tpl) return
  if (!form.name) form.name = tpl.id
  form.ports = [{
    host: tpl.default_port,
    container: tpl.container_port,
    bind_scope: tpl.id === 'redis' || tpl.id === 'mysql' || tpl.id === 'postgres' ? 'mesh' : 'public',
  }]
  form.volumes_text = tpl.default_volume || ''
  form.template_params = {}
}

watch(sourceMode, (mode) => {
  // 切来源时清空模板残留，避免把模板端口误提交成镜像应用的配置
  if (mode !== 'template') {
    form.template_id = ''
    form.template_params = {}
  }
  if (mode === 'image' && form.ports.length === 0) {
    form.ports = [{ host: 8080, container: 8080, bind_scope: 'public' }]
  }
})

// ---- 镜像 / 仓库真实元数据探测（只读辅助，失败静默不打扰手动填写）----
// 探测结果用于提示与一键预填端口/环境变量/构建方式，绝不自动提交表单。

let probeSeq = 0
const imageProbe = ref<{ loading: boolean; data: ImageMetadata | null; error: string }>({
  loading: false, data: null, error: '',
})
let lastProbedImage = ''

function plausibleImageRef(v: string): boolean {
  const s = v.trim()
  if (!s || /\s/.test(s) || s.includes('://') || s.includes('@')) return false
  return /^[a-zA-Z0-9][\w./:-]*$/.test(s)
}

async function probeImage() {
  const v = form.image.trim()
  if (imageProbe.value.data || imageProbe.value.error) {
    imageProbe.value = { loading: false, data: null, error: '' }
  }
  if (!plausibleImageRef(v)) return
  lastProbedImage = v
  const seq = ++probeSeq
  imageProbe.value.loading = true
  try {
    const meta = await inspectImage(v)
    if (seq !== probeSeq || lastProbedImage !== form.image.trim()) return
    imageProbe.value = { loading: false, data: meta, error: '' }
  } catch (e: any) {
    if (seq !== probeSeq) return
    imageProbe.value = { loading: false, data: null, error: e?.message || '镜像元数据读取失败' }
  }
}

watch(() => form.image, () => {
  if (form.image.trim() !== lastProbedImage) {
    lastProbedImage = ''
    imageProbe.value = { loading: false, data: null, error: '' }
  }
})

const imageProbeSummary = computed(() => {
  const m = imageProbe.value.data
  if (!m) return ''
  const parts: string[] = []
  parts.push(m.exposed_ports?.length ? `端口 ${m.exposed_ports.join(' / ')}` : '未声明暴露端口')
  const envCount = Object.keys(m.env_vars || {}).length
  if (envCount > 0) parts.push(`预置变量 ${envCount} 项`)
  if (m.volumes?.length) parts.push(`数据卷 ${m.volumes.length} 项`)
  return parts.join(' · ')
})

function applyImageProbe() {
  const m = imageProbe.value.data
  if (!m) return
  const real = (m.exposed_ports || []).filter((p) => p > 0 && p <= 65535)
  if (real.length > 0) {
    const isPlaceholder = form.ports.length === 1
      && form.ports[0].host === 8080 && form.ports[0].container === 8080
    if (form.ports.length === 0 || isPlaceholder) {
      form.ports = real.map((p) => ({ host: p, container: p, bind_scope: 'public' as const }))
    } else {
      const realSet = new Set(real)
      const kept = form.ports.filter((p) => realSet.has(p.container))
      form.ports = kept.length > 0
        ? kept
        : real.map((p) => ({ host: p, container: p, bind_scope: 'public' as const }))
    }
  }
  if (!form.env_vars_text.trim() && Object.keys(m.env_vars || {}).length > 0) {
    form.env_vars_text = Object.entries(m.env_vars)
      .map(([k, v]) => `${k}=${v}`)
      .join('\n')
  }
  if (!form.healthcheck_url.trim() && real.length > 0) {
    form.healthcheck_url = `http://127.0.0.1:${real[0]}/`
  }
  if (!form.name.trim()) {
    const seg = (m.repository || '').split('/').pop() || ''
    if (seg) form.name = seg
  }
  message.success('已按镜像真实配置带入端口与环境变量，请核对后提交')
}

const repoProbe = ref<{ loading: boolean; data: RepoMetadata | null; error: string }>({
  loading: false, data: null, error: '',
})
let lastProbedRepo = ''
// 构建方式的单选框一旦被用户手点过就不再自动替用户切换
let buildTypeTouched = false
function markBuildTypeTouched() {
  buildTypeTouched = true
}

function normalizeRepoURL(v: string): string {
  return v.trim().toLowerCase().replace(/\.git$/, '').replace(/\/+$/, '')
}

function isGitHubRepoURL(v: string): boolean {
  return /^https?:\/\/(www\.)?github\.com\/[\w.-]+\/[\w.-]+/i.test(v.trim())
}

async function probeRepo(url: string) {
  const v = url.trim()
  if (repoProbe.value.data || repoProbe.value.error) {
    repoProbe.value = { loading: false, data: null, error: '' }
  }
  if (!isGitHubRepoURL(v)) return
  lastProbedRepo = normalizeRepoURL(v)
  const seq = ++probeSeq
  repoProbe.value.loading = true
  try {
    const meta = await inspectRepo(v)
    if (seq !== probeSeq || lastProbedRepo !== normalizeRepoURL(form.repo_url)) return
    repoProbe.value = { loading: false, data: meta, error: '' }
    applyRepoProbeAuto(meta)
  } catch (e: any) {
    if (seq !== probeSeq) return
    repoProbe.value = { loading: false, data: null, error: e?.message || '仓库配置读取失败' }
  }
}

watch(() => form.repo_url, () => {
  if (normalizeRepoURL(form.repo_url) !== lastProbedRepo) {
    lastProbedRepo = ''
    repoProbe.value = { loading: false, data: null, error: '' }
  }
})

// 探测到事实后自动对齐：构建方式（仅用户未手改时）、编排/Dockerfile 路径
//（仅仍是默认值时）、Dockerfile 模式下端口映射（仅端口列表为空时）。
function applyRepoProbeAuto(meta: RepoMetadata) {
  if (!buildTypeTouched) {
    if (meta.has_compose && !meta.has_dockerfile) {
      form.build_type = 'compose'
    } else if (meta.has_dockerfile && !meta.has_compose) {
      form.build_type = 'dockerfile'
    }
  }
  const pathField = form.dockerfile.trim()
  const pathIsDefault = pathField === '' || pathField === 'Dockerfile'
  if (form.build_type === 'compose') {
    if (meta.has_compose && pathIsDefault) {
      form.dockerfile = meta.compose_file || 'compose.yaml'
    }
  } else if (meta.has_dockerfile && pathIsDefault && meta.dockerfile_path !== 'Dockerfile') {
    form.dockerfile = meta.dockerfile_path
  }
  if (form.build_type === 'dockerfile' && meta.exposed_ports?.length && form.ports.length === 0) {
    form.ports = meta.exposed_ports
      .filter((p) => p > 0 && p <= 65535)
      .map((p) => ({ host: p, container: p, bind_scope: 'public' as const }))
  }
  if (meta.branch && (!form.branch.trim() || form.branch.trim() === 'main') && meta.branch !== 'main') {
    form.branch = meta.branch
  }
  if (!form.name.trim() && meta.name) {
    form.name = meta.name
  }
}

const repoProbeTags = computed(() => {
  const m = repoProbe.value.data
  if (!m) return []
  const tags: string[] = []
  if (m.has_compose) tags.push(`编排文件 ${m.compose_file}`)
  if (m.has_dockerfile) {
    tags.push(m.exposed_ports?.length
      ? `${m.dockerfile_path}（EXPOSE ${m.exposed_ports.join(' / ')}）`
      : m.dockerfile_path)
  }
  if (!m.has_compose && !m.has_dockerfile) {
    tags.push(m.language ? `未检出 Dockerfile/Compose（${m.language} 项目）` : '未检出 Dockerfile/Compose')
  }
  const envKeys = Object.keys(m.env_example || {} || {})
  if (envKeys.length > 0) tags.push(`.env 示例键：${envKeys.slice(0, 6).join('、')}${envKeys.length > 6 ? '…' : ''}`)
  return tags
})

const bindScopeOptions = [
  { label: '公网开放 (0.0.0.0)', value: 'public' },
  { label: '仅异地组网 (Tailscale IP)', value: 'mesh' },
]

const hostOptions = computed(() =>
  hosts.value.map((h) => {
    const mesh = meshIPMap.value.get(h.id)
    const parts = [h.hostname]
    if (h.public_ip) parts.push(h.public_ip)
    parts.push(mesh ? `组网 ${mesh}` : '未组网')
    if (h.status !== 'online') parts.push('离线')
    return {
      label: parts.join(' · '),
      value: h.id,
      disabled: h.status !== 'online',
    }
  }),
)

// 网关主机不能是应用主机自身（相同场景即 local）；到应用主机的链路有两种：
// 双方都在组网内走 100.x（组网直达），否则直连应用主机的公网/内网 IP——
// 异地组网不是网关模式的必需条件。
const gatewayOptions = computed(() => {
  const appMesh = form.host_id ? meshIPMap.value.get(form.host_id) || '' : ''
  const list = hosts.value
    .filter((h) => h.status === 'online')
    .map((h) => {
      const mesh = meshIPMap.value.get(h.id) || ''
      const path = mesh && appMesh ? '组网直达' : '直连'
      const recommended = !!h.public_ip
      const tags = [h.public_ip ? `公网 ${h.public_ip}` : '无公网 IP', mesh ? `组网 ${mesh}` : '未组网', path]
      return {
        label: `${h.hostname}（${tags.join(' · ')}${recommended ? '，推荐' : ''}）`,
        value: h.id,
        disabled: h.id === form.host_id,
        rank: recommended ? 0 : 1,
      }
    })
  list.sort((a, b) => a.rank - b.rank)
  return list
})

const meshIPMap = computed(() => {
  const m = new Map<string, string>()
  for (const n of meshNodes.value) {
    if (n.ip) m.set(n.host_id, n.ip)
  }
  return m
})

// 选中主机的组网 IP：实时探测值优先、缓存兜底（与部署侧 resolveMeshIP 同序）
const selectedHostMeshIP = computed(() => {
  if (meshProbeResult.value?.hostId === form.host_id && meshProbeResult.value.ip) {
    return meshProbeResult.value.ip
  }
  return form.host_id ? meshIPMap.value.get(form.host_id) || '' : ''
})

const selectedHostMeshSource = computed<'' | 'live' | 'cache'>(() => {
  if (meshProbeResult.value?.hostId === form.host_id && meshProbeResult.value.ip) {
    return meshProbeResult.value.source
  }
  return form.host_id && meshIPMap.value.get(form.host_id) ? 'cache' : ''
})

const selectedHost = computed(() => hosts.value.find((h) => h.id === form.host_id) || null)
const selectedGateway = computed(() => hosts.value.find((h) => h.id === form.gateway_host_id) || null)
const hasMeshPort = computed(() => form.ports.some((p) => p.bind_scope === 'mesh'))
// 网关与应用主机都已在组网内才走 100.x；否则网关直连应用主机（公网/内网 IP）
const gatewayMeshLink = computed(() =>
  bothMeshed(selectedHostMeshIP.value, meshIPMap.value.get(form.gateway_host_id || '')),
)

// ---- 组网端口转发：入口主机下拉与直连/组网状态判定 ----
const forwardHostOptions = computed(() => {
  return hosts.value
    .filter((h) => h.status === 'online')
    .map((h) => {
      const isSelf = h.id === form.host_id
      const meshIP = meshIPMap.value.get(h.id) || ''
      const tags: string[] = []
      if (h.public_ip) tags.push(`公网 ${h.public_ip}`)
      if (meshIP) tags.push(`组网 ${meshIP}`)
      else tags.push('未组网')
      if (isSelf) tags.push('应用所在主机，不可选')
      const tagStr = tags.length ? ` (${tags.join(' · ')})` : ''
      return {
        label: `${h.hostname}${tagStr}`,
        value: h.id,
        disabled: isSelf,
        hasPublicIp: !!h.public_ip,
        hasMeshIp: !!meshIP,
      }
    })
    .sort((a, b) => {
      if (a.disabled !== b.disabled) return a.disabled ? 1 : -1
      if (a.hasPublicIp !== b.hasPublicIp) return a.hasPublicIp ? -1 : 1
      return 0
    })
})

const selectedForwardHost = computed(() =>
  hosts.value.find((h) => h.id === form.mesh_forward_host_id) || null,
)

const forwardMeshLink = computed(() =>
  bothMeshed(selectedHostMeshIP.value, meshIPMap.value.get(form.mesh_forward_host_id || '')),
)

// 开启组网转发时，自动从已有端口映射中推荐预填目标端口和入口端口
watch(() => form.mesh_forward_enabled, (enabled) => {
  if (enabled) {
    if (!form.mesh_forward_target_port && form.ports.length > 0 && form.ports[0].host > 0) {
      form.mesh_forward_target_port = form.ports[0].host
    }
    if (!form.mesh_forward_entry_port && form.mesh_forward_target_port) {
      form.mesh_forward_entry_port = form.mesh_forward_target_port
    }
  }
})

// 选中主机后实时探测其组网 IP（只读端点，不记审计）：--reset 重加入会更换
// 100.x 地址，纯缓存值可能误导用户。
let meshProbeSeq = 0
watch(() => form.host_id, async (id) => {
  meshProbeResult.value = null
  // 应用主机变更后，原网关与原转发入口若恰好等于新主机则失去意义，清掉避免提交时报错
  if (form.gateway_host_id && form.gateway_host_id === id) form.gateway_host_id = null
  if (form.mesh_forward_host_id && form.mesh_forward_host_id === id) form.mesh_forward_host_id = null
  if (!id) return
  const seq = ++meshProbeSeq
  meshProbeLoading.value = true
  try {
    const r = await probeMeshIP(id)
    if (seq !== meshProbeSeq) return
    meshProbeResult.value = { hostId: id, ip: r.ip, source: (r.source || '') as '' | 'live' | 'cache' }
  } catch {
    if (seq !== meshProbeSeq) return
    meshProbeResult.value = null
  } finally {
    if (seq === meshProbeSeq) meshProbeLoading.value = false
  }
})

const vaultOptions = computed(() => [
  { label: '不使用凭据（公开仓库）', value: '' },
  ...vaults.value.map((v) => ({ label: `${v.name} (${v.type})`, value: v.id })),
])

const patRepoOptions = computed(() =>
  githubRepos.value.map((r) => ({
    // 账号清单里同时被 App 覆盖的仓库仍如实列出，但要点明实际触发与拉码
    // 由 App 优先，不能让人误以为是 PAT 在生效。
    label:
      `${r.full_name}${r.private ? ' [私有]' : ''}${r.description ? ' - ' + r.description : ''}` +
      (appInstalledRepos.value.some((a) => a.full_name === r.full_name) ? ' · App 已覆盖' : ''),
    value: r.full_name,
  })),
)
const appRepoOptions = computed(() =>
  appInstalledRepos.value.map((r) => ({ label: r.full_name, value: r.full_name })),
)
// 当前接入方式下的仓库；未选择方式时不展示任何来源的清单。
const repoOptions = computed(() => {
  if (githubMethod.value === 'pat') return patRepoOptions.value
  if (githubMethod.value === 'app') return appRepoOptions.value
  return []
})

const branchOptions = computed(() =>
  branches.value.map((b) => ({
    label: b.name + (b.protected ? ' (受保护)' : ''),
    value: b.name,
  })),
)

// ---- 代码推送自动部署：独立于代码来源的触发方式 ----
// 所选仓库是否在 GitHub App（未暂停安装）的仓库清单内。命中时推送由
// App 级事件触发，后端不会再给该仓库注册逐仓 Webhook。
const selectedRepoAppCovered = computed(
  () =>
    !!selectedRepoFullName.value &&
    appInstalledRepos.value.some((r) => r.full_name === selectedRepoFullName.value),
)

// 仓库地址是否已确定：GitHub 来源要先选仓，通用 Git 来源看手填地址。
const autoDeployRepoReady = computed(() =>
  sourceMode.value === 'github' ? !!selectedRepoFullName.value : form.repo_url.trim() !== '',
)

// 实际触发机制（与后端注册逻辑一致）：App 级事件 / 创建时自动注册逐仓
// Webhook（仅 github.com 且已连接 GitHub 账号）/ 手动配置 Webhook 地址。
const autoDeployMechanism = computed<'app' | 'webhook' | 'manual'>(() => {
  if (sourceMode.value === 'github' && selectedRepoAppCovered.value) return 'app'
  if (githubAccount.value && form.repo_url.toLowerCase().includes('github.com')) return 'webhook'
  return 'manual'
})

const autoDeployMechanismLabel = computed(
  () =>
    ({ app: 'GitHub App 托管', webhook: '自动注册 Webhook', manual: '手动 Webhook' })[
      autoDeployMechanism.value
    ],
)

const autoDeployHint = computed(() => {
  if (!form.auto_deploy) {
    return '已关闭：推送代码不会触发部署；创建后仍可在应用详情页开启。'
  }
  if (!autoDeployRepoReady.value) {
    return '选定仓库后按实际接入情况自动识别触发方式。'
  }
  const branch = form.branch.trim() || 'main'
  switch (autoDeployMechanism.value) {
    case 'app':
      return `该仓库在 GitHub App 安装范围内：推送 ${branch} 分支时由 App 级事件触发部署，无需逐仓配置 Webhook。`
    case 'webhook':
      return `创建时将自动通过已连接的 GitHub 账号为该仓库注册 Push Webhook，推送 ${branch} 分支后自动部署。`
    default:
      return sourceMode.value === 'github'
        ? '该仓库不在 GitHub App 安装范围内且未连接 GitHub 账号：创建后到应用详情页复制 Webhook 地址配置到仓库，或先在系统设置接入 GitHub App。'
        : '创建后到应用详情页复制 Webhook 地址，配置到代码仓库的 push 事件。'
  }
})

const parsedEnvVars = computed<Record<string, string> | undefined>(() => {
  // 模板来源的 env 由 TemplateParams 渲染，不读文本框
  if (sourceMode.value === 'template') return undefined
  const text = form.env_vars_text.trim()
  if (!text) return undefined
  const vars: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const eq = trimmed.indexOf('=')
    if (eq <= 0) continue
    vars[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim()
  }
  return Object.keys(vars).length > 0 ? vars : undefined
})

const parsedVolumes = computed<string[] | undefined>(() => {
  const text = form.volumes_text.trim()
  if (!text) return undefined
  const list = text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0)
  return list.length > 0 ? list : undefined
})

// 当前表单快照，随每次 AI 请求带给后端作上下文（密钥值由后端脱敏）。
const formSnapshot = computed<AppFormSnapshot>(() => ({
  source_mode: sourceMode.value,
  name: form.name,
  container_name: form.container_name,
  host_id: form.host_id || '',
  template_id: form.template_id,
  template_params: { ...form.template_params },
  image: form.image,
  compose_content: form.compose_content,
  mesh_forward_enabled: form.mesh_forward_enabled,
  mesh_forward_host_id: form.mesh_forward_host_id || '',
  mesh_forward_entry_port: form.mesh_forward_entry_port ?? 0,
  mesh_forward_target_port: form.mesh_forward_target_port ?? 0,
  repo_url: form.repo_url,
  branch: form.branch,
  auth_vault_id: form.auth_vault_id || '',
  auto_deploy: form.auto_deploy,
  build_type: form.build_type,
  dockerfile: form.dockerfile,
  build_context: form.build_context,
  build_timeout_sec: form.build_timeout_sec,
  ports: form.ports.map((p) => ({ host: p.host, container: p.container, bind_scope: p.bind_scope })),
  env_vars: parsedEnvVars.value ?? {},
  volumes: parsedVolumes.value ?? [],
  healthcheck_url: form.healthcheck_url,
  domain: form.domain,
  proxy_mode: form.proxy_mode,
  gateway_host_id: form.gateway_host_id || '',
}))

// AI 助手填充：把后端消毒过的草稿合并进表单。只覆盖草稿里出现的字段，
// 绝不触发提交——最终仍由用户检查后点「创建应用」。
function applyDraft(draft: AppFormDraft) {
  const modeMap: Record<string, SourceMode> = {
    template: 'template', image: 'image', raw_compose: 'compose', git: 'custom',
  }
  if (draft.source_type && modeMap[draft.source_type]) {
    sourceMode.value = modeMap[draft.source_type]
  }
  if (sourceMode.value === 'template' && draft.template_id) {
    if (templates.value.some((t) => t.id === draft.template_id)) {
      // 先走选模板的常规预填（端口/卷/参数基线），再叠加草稿覆盖值。
      selectTemplate(draft.template_id)
    } else {
      form.template_id = draft.template_id
    }
  }
  if (draft.host_id && hosts.value.some((h) => h.id === draft.host_id)) {
    form.host_id = draft.host_id
  }
  if (draft.name !== undefined) form.name = draft.name
  if (draft.container_name !== undefined) form.container_name = draft.container_name
  if (draft.template_params && sourceMode.value === 'template') {
    form.template_params = { ...form.template_params, ...draft.template_params }
  }
  if (draft.image !== undefined) form.image = draft.image
  if (draft.compose_content !== undefined) form.compose_content = draft.compose_content
  if (draft.mesh_forward_enabled !== undefined) form.mesh_forward_enabled = draft.mesh_forward_enabled
  if (draft.mesh_forward_host_id && hosts.value.some((h) => h.id === draft.mesh_forward_host_id)) {
    form.mesh_forward_host_id = draft.mesh_forward_host_id
  }
  if (draft.mesh_forward_entry_port !== undefined) form.mesh_forward_entry_port = draft.mesh_forward_entry_port
  if (draft.mesh_forward_target_port !== undefined) form.mesh_forward_target_port = draft.mesh_forward_target_port
  if (draft.repo_url !== undefined) form.repo_url = draft.repo_url
  if (draft.branch !== undefined) form.branch = draft.branch
  if (draft.auto_deploy !== undefined) form.auto_deploy = draft.auto_deploy
  if (draft.build_type !== undefined) form.build_type = draft.build_type
  if (draft.dockerfile !== undefined) form.dockerfile = draft.dockerfile
  if (draft.build_context !== undefined) form.build_context = draft.build_context
  if (draft.build_timeout_sec !== undefined) form.build_timeout_sec = draft.build_timeout_sec
  if (draft.ports && draft.ports.length > 0) {
    form.ports = draft.ports.map((p) => ({
      host: p.host,
      container: p.container,
      bind_scope: p.bind_scope === 'mesh' ? 'mesh' as const : 'public' as const,
    }))
  }
  if (draft.env_vars && sourceMode.value !== 'template') {
    form.env_vars_text = Object.entries(draft.env_vars)
      .map(([k, v]) => `${k}=${v}`)
      .join('\n')
  }
  if (draft.volumes && draft.volumes.length > 0) {
    form.volumes_text = draft.volumes.join('\n')
  }
  if (draft.healthcheck_url !== undefined) form.healthcheck_url = draft.healthcheck_url
  if (draft.domain !== undefined) form.domain = draft.domain
  if (draft.proxy_mode !== undefined) form.proxy_mode = draft.proxy_mode
  if (draft.gateway_host_id !== undefined) form.gateway_host_id = draft.gateway_host_id
}

async function loadData() {
  try {
    const res = await listHosts()
    hosts.value = ((res as { data: HostOption[] }).data ?? res) as HostOption[]
  } catch {
    hosts.value = []
  }
  try {
    vaults.value = (await listCredentials()) as VaultOption[]
  } catch {
    vaults.value = []
  }
  try {
    templates.value = (await listAppCatalog()) ?? []
  } catch {
    templates.value = []
  }
  try {
    meshNodes.value = (await listNetworkNodes()) ?? []
  } catch {
    meshNodes.value = []
  }
  try {
    networkConfig.value = await getNetworkConfig()
  } catch {
    networkConfig.value = null
  } finally {
    networkConfigLoaded.value = true
  }
  // 证书支撑数据（账户/dns-mng 就绪/可选证书）独立加载，失败不影响主流程
  void loadCertSupport()
  // Docker 面板「部署新应用」跳转时带 host_id，直接预选目标主机
  const presetHost = typeof route.query.host_id === 'string' ? route.query.host_id : ''
  if (presetHost && hosts.value.some((h) => h.id === presetHost)) {
    form.host_id = presetHost
  }
  await checkGitHubStatus()
}

async function checkGitHubStatus() {
  checkingGitHub.value = true
  try {
    const st = await getGitProviders()
    if (st.github.connected && st.github.account) {
      githubAccount.value = st.github.account
      await fetchGitHubRepos()
    } else {
      githubAccount.value = null
      // 在系统设置里断开账号后返回：PAT 仓库清单已失效，清掉避免下拉框
      // 继续展示当前凭证拉取不到的旧仓库。
      githubRepos.value = []
    }
  } catch {
    githubAccount.value = null
  } finally {
    checkingGitHub.value = false
  }
  await fetchAppInstalledRepos()
  syncGithubMethod()
}

async function fetchGitHubRepos() {
  loadingRepos.value = true
  try {
    githubRepos.value = await listGitHubRepos()
  } catch (e: any) {
    message.error(e.message || '获取 GitHub 仓库列表失败')
  } finally {
    loadingRepos.value = false
  }
}

async function refreshRepoLists() {
  if (githubAccount.value) await fetchGitHubRepos()
  await fetchAppInstalledRepos()
  syncGithubMethod()
}

// 当用户在下拉框选中某个 GitHub 仓库时：自动填充应用名、克隆地址，并拉取所有分支。
// 取数通道由接入方式决定：App 方式即使该仓库也在 PAT 清单里，分支仍走安装令牌。
let repoPickSeq = 0
watch(selectedRepoFullName, async (fullName) => {
  if (!fullName) return
  const seq = ++repoPickSeq
  const repo =
    githubMethod.value === 'pat'
      ? githubRepos.value.find((r) => r.full_name === fullName)
      : undefined
  if (!repo) {
    // GitHub App 安装仓库：只填克隆地址与应用名，分支走 App 令牌拉取。
    const [owner, name] = fullName.split('/')
    if (!owner || !name) return
    form.repo_url = `https://github.com/${fullName}.git`
    if (!form.name) form.name = name
    void probeRepo(form.repo_url)
    // 先清空分支：上一个仓库的分支残留到新仓库是静默错配，必须先清再拉。
    form.branch = ''
    loadingBranches.value = true
    try {
      const names = await listGitHubAppBranches(owner, name)
      if (seq !== repoPickSeq) return
      branches.value = names.map((n) => ({ name: n, protected: false, sha: '' }))
      if (names.includes('main')) form.branch = 'main'
      else if (names.length > 0) form.branch = names[0]
    } catch (e: any) {
      if (seq === repoPickSeq) message.error(e.message || '获取仓库分支失败')
    } finally {
      if (seq === repoPickSeq) loadingBranches.value = false
    }
    return
  }

  form.repo_url = repo.clone_url
  if (!form.name) {
    form.name = repo.name
  }
  // 顺带客观读取仓库的部署配置（Dockerfile/Compose/.env），失败静默
  void probeRepo(repo.clone_url)

  loadingBranches.value = true
  try {
    const [owner, name] = repo.full_name.split('/')
    const bList = await listGitHubBranches(owner, name)
    if (seq !== repoPickSeq) return
    branches.value = bList
    if (bList.some((b) => b.name === repo.default_branch)) {
      form.branch = repo.default_branch
    } else if (bList.length > 0) {
      form.branch = bList[0].name
    }
  } catch (e: any) {
    if (seq === repoPickSeq) message.error(e.message || '获取仓库分支失败')
  } finally {
    if (seq === repoPickSeq) loadingBranches.value = false
  }
})

// 切换接入方式后旧仓库与分支不再属于当前来源，必须清空，避免带着另一来源
// 的元数据提交；在途的分支请求一并作废。
watch(githubMethod, (method, prev) => {
  if (!prev || prev === method) return
  repoPickSeq++
  selectedRepoFullName.value = null
  branches.value = []
  loadingBranches.value = false
  form.branch = ''
  form.repo_url = ''
})

// 账号接入属于系统级凭据管理：创建页不再内嵌 Token 表单，统一跳到
// 「系统设置 → 代码源集成」，避免同一份授权在两处各维护一套 UI。
function openGitHubSettings() {
  router.push('/settings?s=git')
}

// 从系统设置连好/断开账号后切回本页时，授权与仓库清单可能已经变化。
// 首次激活由 onMounted 的 loadData 负责，这里跳过以免重复请求。
let githubActivatedOnce = false
onActivated(() => {
  if (!githubActivatedOnce) {
    githubActivatedOnce = true
    return
  }
  void checkGitHubStatus()
})

function validate(): string | null {
  if (!form.name.trim()) return '请填写应用名称'
  if (!form.host_id) return '请选择目标主机'
  switch (sourceMode.value) {
    case 'template':
      if (!form.template_id) return '请选择应用模板'
      for (const f of selectedTemplate.value?.env_fields ?? []) {
        const v = (form.template_params[f.key] ?? '').trim()
        if (f.required && !v) return `模板参数「${f.label}」为必填项`
      }
      break
    case 'image':
      if (!form.image.trim()) return '请填写镜像地址'
      break
    case 'compose':
      if (!form.compose_content.trim()) return '请填写 Docker Compose 内容'
      break
    case 'github':
      if (!githubMethod.value) return '请先选择代码接入方式'
      if (!selectedRepoFullName.value) return '请选择 GitHub 仓库'
      if (!form.repo_url.trim()) return '请选择或填写 Git 仓库地址'
      if (!/^https?:\/\//.test(form.repo_url.trim())) return '仓库地址必须是 http(s):// 开头的 URL'
      break
    case 'custom':
      if (!form.repo_url.trim()) return '请选择或填写 Git 仓库地址'
      if (!/^https?:\/\//.test(form.repo_url.trim())) return '仓库地址必须是 http(s):// 开头的 URL'
      break
  }
  for (const p of form.ports) {
    if (p.host <= 0 || p.host > 65535 || p.container <= 0 || p.container > 65535) {
      return `端口映射非法: ${p.host}:${p.container}`
    }
    if (p.bind_scope === 'mesh' && !selectedHostMeshIP.value) {
      return '所选主机未加入异地组网，无法使用「仅异地组网」端口范围（可在主机详情-异地组网中加入）'
    }
  }
  if (form.mesh_forward_enabled) {
    if (!form.mesh_forward_host_id) return '开启组网端口转发时必须选择入口主机'
    if (form.mesh_forward_host_id === form.host_id) return '组网转发入口主机不能与应用部署主机相同'
    if (!form.mesh_forward_entry_port || form.mesh_forward_entry_port < 1 || form.mesh_forward_entry_port > 65535) {
      return '请填写合法的组网转发入口端口（1-65535）'
    }
    if (!form.mesh_forward_target_port || form.mesh_forward_target_port < 1 || form.mesh_forward_target_port > 65535) {
      return '请填写合法的组网转发目标端口（1-65535）'
    }
  }
  if (activeDomains.value.length > 0) {
    const domainSet = new Set<string>()
    for (const r of activeDomains.value) {
      const d = r.domain.trim()
      if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i.test(d)) {
        return `域名格式不正确: ${d}`
      }
      if (domainSet.has(d.toLowerCase())) {
        return `域名重复: ${d}`
      }
      domainSet.add(d.toLowerCase())
    }
    if (form.proxy_mode === 'gateway') {
      if (!form.gateway_host_id) return '网关模式需选择网关主机'
      if (form.gateway_host_id === form.host_id) return '网关主机不能与应用主机相同（相同场景请改用 local 模式）'
    }
  }
  return null
}

// 创建后的域名绑定编排：先让服务端自动匹配可用证书直接绑定；返回 missing_domains
// 时对这组域名批量签发一张多 SAN 证书再重试整批绑定。签发/重试失败则退化为绑定
// 已备妥的域名子集并告警。任何失败都不回滚应用创建、不阻断跳转。
async function bindDomainsWithCert(appId: string) {
  const rows = activeDomains.value
  if (rows.length === 0) return
  const domains = rows.map((r) => ({
    domain: r.domain.trim().toLowerCase(),
    cert_id: r.cert_id || undefined,
  }))
  const doBind = (list: ProxyBindDomain[]) =>
    bindProxy(appId, {
      domains: list,
      mode: form.proxy_mode,
      gateway_host_id: form.proxy_mode === 'gateway' ? form.gateway_host_id || undefined : undefined,
    })

  creatingLabel.value = '绑定反代中…'
  let missing: string[] = []
  try {
    const res = await doBind(domains)
    if (res?.warnings?.length) message.warning(res.warnings.join('；'))
    message.success(`已绑定 ${domains.length} 个域名并下发反代`)
    return
  } catch (e: any) {
    missing = Array.isArray(e?.missingDomains) ? e.missingDomains : []
    if (missing.length === 0) {
      message.warning(`应用已创建，但域名反代绑定失败：${e?.message || e}；可稍后在应用详情-域名与反代中重试`)
      return
    }
  } finally {
    creatingLabel.value = ''
  }

  const failureNotes: string[] = []
  if (!canIssue.value) {
    failureNotes.push('当前账号无证书签发权限，请联系管理员签发覆盖证书')
  } else {
    creatingLabel.value = '签发证书中…'
    const issued = await issueDomains(missing)
    if (issued.ok) {
      creatingLabel.value = '绑定反代中…'
      try {
        const retry = await doBind(domains)
        if (retry?.warnings?.length) message.warning(retry.warnings.join('；'))
        message.success(`已为 ${missing.join('、')} 签发证书，并完成 ${domains.length} 个域名的反代绑定`)
        return
      } catch (e: any) {
        failureNotes.push(`证书已签发但重新绑定失败：${e?.message || e}`)
      }
    } else {
      failureNotes.push(`证书签发失败：${issued.error || '未知原因'}`)
    }
  }
  creatingLabel.value = ''

  const ready = domains.filter((d) => !missing.includes(d.domain))
  if (ready.length > 0) {
    creatingLabel.value = '绑定反代中…'
    try {
      await doBind(ready)
      message.warning(`应用已创建，已绑定 ${ready.length} 个域名；${missing.join('、')} 未绑定：${failureNotes.join('；')}。可稍后在应用详情-域名与反代中重试`)
    } catch (e: any) {
      message.warning(`应用已创建，但域名反代绑定失败：${e?.message || e}；可稍后在应用详情-域名与反代中重试`)
    } finally {
      creatingLabel.value = ''
    }
    return
  }
  message.warning(`应用已创建，但域名未完成反代绑定：${failureNotes.join('；')}。可稍后在应用详情-域名与反代中重试`)
}

async function submit() {
  const err = validate()
  if (err) {
    message.warning(err)
    return
  }
  creating.value = true
  try {
    const mode = sourceMode.value
    const isCompose = mode === 'compose'
    const isGit = mode === 'github' || mode === 'custom'
    const app = (await createApp({
      name: form.name.trim(),
      host_id: form.host_id as string, // validate() 已拦截空值
      source_type: mode === 'template' ? 'template' : isCompose ? 'raw_compose' : isGit ? 'git' : 'image',
      template_id: mode === 'template' ? form.template_id : undefined,
      template_params: mode === 'template' && Object.keys(form.template_params).length > 0
        ? form.template_params
        : undefined,
      repo_url: isGit ? form.repo_url.trim() : isCompose ? 'compose://local' : undefined,
      branch: isGit ? form.branch.trim() || 'main' : undefined,
      auth_vault_id: isGit ? form.auth_vault_id || undefined : undefined,
      auto_deploy: isGit ? form.auto_deploy : false,
      build_type: isCompose ? 'compose' : isGit ? form.build_type : undefined,
      compose_content: isCompose ? form.compose_content : undefined,
      dockerfile: isGit ? form.dockerfile.trim() || 'Dockerfile' : undefined,
      build_context: isGit ? form.build_context.trim() || '.' : undefined,
      build_timeout_sec: isGit ? Number(form.build_timeout_sec) || 900 : undefined,
      image: mode === 'image' ? form.image.trim() : undefined,
      mesh_forward_enabled: form.mesh_forward_enabled,
      mesh_forward_host_id: form.mesh_forward_enabled ? form.mesh_forward_host_id || undefined : undefined,
      mesh_forward_entry_port: form.mesh_forward_enabled ? form.mesh_forward_entry_port ?? undefined : undefined,
      mesh_forward_target_port: form.mesh_forward_enabled ? form.mesh_forward_target_port ?? undefined : undefined,
      env_vars: parsedEnvVars.value,
      ports: form.ports.filter((p) => p.host > 0 && p.container > 0),
      volumes: parsedVolumes.value,
      healthcheck_url: form.healthcheck_url.trim(),
      container_name: form.container_name.trim() || undefined,
    })) as AppEntity

    // 可选的创建时域名绑定：多域名显式证书编排见 bindDomainsWithCert
    if (activeDomains.value.length > 0) {
      await bindDomainsWithCert(app.id)
    }

    message.success(`应用「${app.name}」已创建，正在部署…`)
    workspace.openTab({
      key: `/apps/${app.id}`,
      title: `应用 ${app.name}`,
      path: `/apps/${app.id}`,
      viewName: 'AppDetail',
    })
    router.replace(`/apps/${app.id}`)
  } catch (e: any) {
    message.error(e.message || '创建失败')
  } finally {
    creating.value = false
  }
}

function addPort() {
  form.ports.push({ host: 0, container: 0, bind_scope: 'public' })
}

function removePort(idx: number) {
  form.ports.splice(idx, 1)
}

onMounted(loadData)
</script>

<template>
  <div class="app-create-view">
    <div class="page-header create-header">
      <NButton quaternary @click="router.push('/apps')">
        <template #icon>
          <NIcon><ArrowBackOutline /></NIcon>
        </template>
        返回应用列表
      </NButton>
      <h2 class="page-title">创建应用</h2>
    </div>

    <div class="create-layout">
    <NCard :bordered="false" class="create-card">
      <NAlert
        v-if="networkConfigLoaded && !meshConfigured && !meshConfigDismissed"
        type="warning"
        :show-icon="true"
        closable
        style="margin-bottom: 12px"
        @close="meshConfigDismissed = true"
      >
        <template #header>异地组网尚未配置</template>
        尚未配置组网控制面与 Auth Key，主机无法加入组网，「仅异地组网」端口与网关反代暂不可用。
        <NButton
          size="tiny"
          secondary
          type="primary"
          style="margin-left: 8px"
          @click="router.push('/network')"
        >
          前往配置
        </NButton>
      </NAlert>
      <NSpace vertical size="large">
        <!-- 部署来源 -->
        <NFormItem label="部署来源" required>
          <NRadioGroup v-model:value="sourceMode">
            <NSpace>
              <NRadio value="template">应用模板</NRadio>
              <NRadio value="image">单镜像</NRadio>
              <NRadio value="compose">Compose 编排</NRadio>
              <NRadio value="github">GitHub 仓库</NRadio>
              <NRadio value="custom">通用 Git</NRadio>
            </NSpace>
          </NRadioGroup>
        </NFormItem>

        <NForm label-placement="top">
          <!-- 模板来源 -->
          <template v-if="sourceMode === 'template'">
            <NFormItem label="选择模板" required>
              <div class="template-grid">
                <div
                  v-for="tpl in templates"
                  :key="tpl.id"
                  class="template-card"
                  :class="{ active: form.template_id === tpl.id }"
                  @click="selectTemplate(tpl.id)"
                >
                  <div class="tpl-head">
                    <span class="tpl-name">{{ tpl.name }}</span>
                    <NTag size="tiny" :bordered="false">{{ tpl.category }}</NTag>
                  </div>
                  <div class="tpl-desc">{{ tpl.description }}</div>
                  <div class="tpl-meta">镜像 {{ tpl.image }} · 端口 {{ tpl.default_port }}</div>
                </div>
              </div>
            </NFormItem>

            <NFormItem v-if="selectedTemplate" :label="`模板参数（${selectedTemplate.name}）`">
              <div class="param-list">
                <div v-for="f in selectedTemplate.env_fields" :key="f.key" class="param-row">
                  <div class="param-label">
                    {{ f.label }}
                    <NTag v-if="f.required" size="tiny" type="error" :bordered="false">必填</NTag>
                  </div>
                  <NInput
                    v-model:value="form.template_params[f.key]"
                    :type="f.is_secret ? 'password' : 'text'"
                    :show-password-on="f.is_secret ? 'click' : undefined"
                    :placeholder="f.default ? `默认: ${f.default}` : f.description"
                  />
                </div>
                <NAlert v-if="!selectedTemplate.env_fields.length" type="default" :show-icon="false">
                  该模板无必填参数，直接部署即可
                </NAlert>
              </div>
            </NFormItem>

            <NSpace>
              <NFormItem label="应用名称" required style="flex: 1">
                <NInput v-model:value="form.name" placeholder="留空默认使用模板名" />
              </NFormItem>
              <NFormItem label="容器名（可选）" style="flex: 1">
                <NInput v-model:value="form.container_name" placeholder="留空自动生成" />
              </NFormItem>
            </NSpace>
          </template>

          <!-- 单镜像来源 -->
          <template v-else-if="sourceMode === 'image'">
            <NSpace>
              <NFormItem label="应用名称" required style="flex: 1">
                <NInput v-model:value="form.name" placeholder="如 my-nginx" />
              </NFormItem>
              <NFormItem label="容器名（可选）" style="flex: 1">
                <NInput v-model:value="form.container_name" placeholder="留空自动生成" />
              </NFormItem>
            </NSpace>

            <NFormItem label="镜像地址" required>
              <NSpace vertical style="width: 100%" :size="6">
                <NInput
                  v-model:value="form.image"
                  placeholder="nginx:alpine / redis:7.2 / registry.example.com/app:1.0"
                  @blur="probeImage"
                  @keyup.enter="probeImage"
                />
                <div v-if="imageProbe.loading" class="probe-bar loading">正在读取镜像仓库的真实配置…</div>
                <div v-else-if="imageProbe.data" class="probe-bar ok">
                  <span class="probe-text">已客观读取：{{ imageProbeSummary }}</span>
                  <NButton size="tiny" secondary type="primary" @click="applyImageProbe">按真实配置填入</NButton>
                </div>
                <div v-else-if="imageProbe.error" class="probe-bar warn">
                  {{ imageProbe.error }}（不影响手动填写）
                </div>
              </NSpace>
            </NFormItem>
          </template>

          <!-- Compose 来源 -->
          <template v-else-if="sourceMode === 'compose'">
            <NFormItem label="应用/微服务栈名称" required>
              <NInput v-model:value="form.name" placeholder="如 my-compose-stack" />
            </NFormItem>

            <NFormItem label="Docker Compose YAML 编排内容" required>
              <NSpace vertical style="width: 100%">
                <NSpace align="center" :size="8">
                  <span style="font-size: 12px; color: #999">快速载入模板：</span>
                  <NButton size="tiny" secondary @click="form.compose_content = defaultComposeTemplate">Web + Redis</NButton>
                  <NButton size="tiny" secondary @click="form.compose_content = wordpressTemplate">WordPress + MySQL</NButton>
                </NSpace>
                <NInput
                  v-model:value="form.compose_content"
                  type="textarea"
                  :rows="12"
                  placeholder="services:&#10;  app:&#10;    image: ...&#10;    ports:&#10;      - '8080:80'"
                  style="font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px"
                />
              </NSpace>
            </NFormItem>
          </template>

          <!-- GitHub 授权仓库 -->
          <template v-if="sourceMode === 'github'">
            <div v-if="!githubSourceReady" class="github-connect-box">
              <NSpace align="center" justify="space-between" style="width: 100%">
                <NSpace align="center">
                  <NIcon size="28"><LogoGithub /></NIcon>
                  <div>
                    <div style="font-weight: 600">未配置 GitHub 接入</div>
                    <div class="hint">可接入 GitHub 账号（PAT）或 GitHub App，配置后即可直接选仓并自动拉取分支。两种接入统一在「系统设置 → 代码源集成」中管理。</div>
                  </div>
                </NSpace>
                <NButton type="primary" secondary @click="openGitHubSettings">
                  <template #icon>
                    <NIcon><LogoGithub /></NIcon>
                  </template>
                  前往代码源设置
                </NButton>
              </NSpace>
            </div>

            <div v-else class="github-connected-bar">
              <NSpace align="center" justify="space-between" style="width: 100%">
                <NSpace align="center">
                  <template v-if="githubAccount">
                    <NAvatar :src="githubAccount.avatar_url" round size="small" />
                    <span style="font-weight: 600">{{ githubAccount.name || githubAccount.login }}</span>
                    <NTag size="tiny" type="success" :bordered="false">GitHub 账号已授权</NTag>
                  </template>
                  <NTag v-if="appConfigured" size="tiny" type="info" :bordered="false">
                    GitHub App 已配置
                  </NTag>
                </NSpace>
                <NSpace align="center" :size="4">
                  <NButton size="tiny" secondary @click="refreshRepoLists" :loading="loadingRepos">
                    <template #icon>
                      <NIcon><RefreshOutline /></NIcon>
                    </template>
                    刷新仓库
                  </NButton>
                  <NButton size="tiny" quaternary @click="openGitHubSettings">代码源设置</NButton>
                </NSpace>
              </NSpace>
            </div>

            <template v-if="githubSourceReady">
              <NFormItem label="接入方式" required>
                <NRadioGroup v-model:value="githubMethod">
                  <NSpace>
                    <NRadioButton v-for="m in githubMethodOptions" :key="m.value" :value="m.value">
                      {{ m.label }}
                    </NRadioButton>
                  </NSpace>
                </NRadioGroup>
              </NFormItem>
              <div v-if="githubMethod" class="hint tip-hint github-method-hint">
                {{
                  githubMethod === 'pat'
                    ? '读取已授权 GitHub 账号的仓库；未被 App 安装覆盖的仓库推送时走逐仓 Webhook。'
                    : '读取 GitHub App 安装范围内的仓库；拉码使用短期安装令牌，推送由 App 级事件触发。'
                }}
              </div>
              <div v-else class="hint github-method-hint">先选择本次使用的接入方式，再从中挑选仓库。</div>

              <NFormItem label="选择 GitHub 仓库" required>
                <NSelect
                  v-model:value="selectedRepoFullName"
                  :options="repoOptions"
                  :loading="loadingRepos"
                  :disabled="!githubMethod"
                  :placeholder="githubMethod ? '搜索并选择您的代码仓库 (支持私有库与组织仓库)' : '请先选择接入方式'"
                  filterable
                />
              </NFormItem>
              <div v-if="githubMethod && !loadingRepos && repoOptions.length === 0" class="hint github-method-hint">
                {{
                  githubMethod === 'pat'
                    ? '该账号下没有读取到仓库：请到「代码源设置」检查授权后刷新。'
                    : 'App 尚未同步到任何已安装仓库：请到「代码源设置」安装到账号/组织并同步。'
                }}
              </div>

              <NSpace v-if="selectedRepoFullName">
                <NFormItem label="应用名称" required style="flex: 1">
                  <NInput v-model:value="form.name" placeholder="如 my-web-api" />
                </NFormItem>
                <NFormItem label="部署分支" required style="flex: 1">
                  <NSelect
                    v-model:value="form.branch"
                    :options="branchOptions"
                    :loading="loadingBranches"
                    placeholder="选择分支"
                    filterable
                  />
                </NFormItem>
              </NSpace>
            </template>
          </template>

          <!-- 通用 Git -->
          <template v-else-if="sourceMode === 'custom'">
            <NFormItem label="应用名称" required>
              <NInput v-model:value="form.name" placeholder="如 my-web-api" />
            </NFormItem>

            <NFormItem label="Git 仓库地址" required>
              <NInput
                v-model:value="form.repo_url"
                placeholder="https://github.com/yourname/your-repo.git"
                @blur="probeRepo(form.repo_url)"
                @keyup.enter="probeRepo(form.repo_url)"
              />
            </NFormItem>

            <NFormItem label="监听分支" required>
              <NInput v-model:value="form.branch" placeholder="main" />
            </NFormItem>

            <NFormItem label="Git 凭据（私有仓库需要）">
              <NSelect
                v-model:value="form.auth_vault_id"
                :options="vaultOptions"
                placeholder="从凭据金库选择 Access Token"
              />
            </NFormItem>
          </template>

          <!-- 仓库客观探测结果徽标（GitHub / 通用 Git 共用，渲染在两段的下方） -->
          <template v-if="sourceMode === 'github' || sourceMode === 'custom'">
            <div v-if="repoProbe.loading" class="probe-bar loading">正在读取仓库的部署配置（Dockerfile / Compose / .env）…</div>
            <div v-else-if="repoProbe.data" class="probe-bar ok">
              <NTag size="tiny" type="success" :bordered="false">已客观读取</NTag>
              <span class="probe-text">默认分支 {{ repoProbe.data.branch }}</span>
              <NTag v-for="t in repoProbeTags" :key="t" size="tiny" type="info" :bordered="false">{{ t }}</NTag>
              <span v-if="!buildTypeTouched && (repoProbe.data.has_compose !== repoProbe.data.has_dockerfile)" class="probe-text">
                构建方式已按仓库事实自动对齐
              </span>
            </div>
            <div v-else-if="repoProbe.error" class="probe-bar warn">
              {{ repoProbe.error }}（不影响手动填写）
            </div>
          </template>

          <!-- 代码推送自动部署：与「代码来源」概念分离——来源决定构建什么，
               这里只决定何时构建；触发机制按实际接入情况显式标注。 -->
          <div v-if="sourceMode === 'github' || sourceMode === 'custom'" class="auto-deploy-box">
            <div class="auto-deploy-head">
              <div class="auto-deploy-title">代码推送自动部署</div>
              <NSpace align="center" :size="8">
                <NTag v-if="form.auto_deploy && autoDeployRepoReady" size="tiny" type="info" :bordered="false">
                  {{ autoDeployMechanismLabel }}
                </NTag>
                <NSwitch v-model:value="form.auto_deploy" size="small" />
              </NSpace>
            </div>
            <div class="hint">{{ autoDeployHint }}</div>
          </div>

          <NFormItem label="目标主机" required>
            <NSelect
              v-model:value="form.host_id"
              :options="hostOptions"
              placeholder="选择部署到的受管主机（需在线）"
              filterable
            />
          </NFormItem>

          <!-- Git 构建配置 -->
          <template v-if="sourceMode === 'github' || sourceMode === 'custom'">
            <NFormItem label="构建部署模式">
              <NRadioGroup v-model:value="form.build_type" @update:value="markBuildTypeTouched">
                <NSpace>
                  <NRadio value="dockerfile">Dockerfile 单容器镜像构建</NRadio>
                  <NRadio value="compose">Docker Compose 复合微服务栈</NRadio>
                </NSpace>
              </NRadioGroup>
            </NFormItem>

            <NSpace>
              <NFormItem :label="form.build_type === 'compose' ? 'Compose 文件路径' : 'Dockerfile 路径'" style="flex: 1">
                <NInput
                  v-model:value="form.dockerfile"
                  :placeholder="form.build_type === 'compose' ? 'compose.yaml' : 'Dockerfile'"
                />
              </NFormItem>
              <NFormItem label="构建超时（秒）" style="flex: 1">
                <NInputNumber v-model:value="form.build_timeout_sec" :min="60" style="width: 100%" />
              </NFormItem>
            </NSpace>
          </template>

          <!-- 端口与网络（Compose 在 YAML 中声明） -->
          <template v-if="sourceMode !== 'compose'">
            <NFormItem label="端口映射与网络范围">
              <NSpace vertical style="width: 100%">
                <NSpace v-for="(p, idx) in form.ports" :key="idx" align="center">
                  <NInputNumber v-model:value="p.host" placeholder="宿主端口" style="width: 120px" />
                  <span>-&gt;</span>
                  <NInputNumber v-model:value="p.container" placeholder="容器端口" style="width: 120px" />
                  <NSelect
                    v-model:value="p.bind_scope"
                    :options="bindScopeOptions"
                    style="width: 200px"
                  />
                  <NButton quaternary type="error" size="small" @click="removePort(idx)">移除</NButton>
                </NSpace>
                <NButton dashed size="small" @click="addPort">添加端口映射</NButton>
                <span v-if="meshProbeLoading" class="hint">正在探测目标主机的组网 IP…</span>
                <span v-else-if="selectedHostMeshIP" class="hint">
                  目标主机组网 IP：{{ selectedHostMeshIP }}（{{ selectedHostMeshSource === 'live' ? '实时探测' : '缓存' }}），「仅异地组网」端口将只绑定该 IP，公网无法访问
                </span>
                <NAlert
                  v-if="form.host_id && hasMeshPort && !selectedHostMeshIP"
                  type="warning"
                  size="small"
                  :show-icon="true"
                  style="margin-top: 8px"
                >
                  <template #header>所选主机尚未加入异地组网</template>
                  「仅异地组网」端口需要主机先在组网内取得 100.x 地址。
                  <NButton
                    size="tiny"
                    secondary
                    type="primary"
                    style="margin-left: 8px"
                    @click="router.push(`/hosts/${form.host_id}?tab=network`)"
                  >
                    前往加入组网
                  </NButton>
                </NAlert>
              </NSpace>
            </NFormItem>

            <NFormItem label="健康检查 URL（可选，推荐）">
              <NInput
                v-model:value="form.healthcheck_url"
                placeholder="http://127.0.0.1:8080/healthz（探活失败将自动保留旧版本）"
              />
            </NFormItem>
          </template>

          <!-- 组网端口转发 (TCP 流量中继) -->
          <div class="network-forward-box" style="margin-bottom: 16px; padding: 14px; background: rgba(99, 102, 241, 0.04); border: 1px solid rgba(99, 102, 241, 0.15); border-radius: 8px;">
            <div style="display: flex; align-items: center; justify-content: space-between;">
              <div>
                <span style="font-weight: 600; font-size: 14px;">组网端口转发 (TCP 流量中继)</span>
                <span class="hint" style="margin-left: 8px;">由选定的入口主机运行 socat 容器，通过异地组网或直连将 4 层 TCP 端口转发给本应用</span>
              </div>
              <NSwitch v-model:value="form.mesh_forward_enabled" />
            </div>

            <template v-if="form.mesh_forward_enabled">
              <NGrid :cols="24" :x-gap="12" style="margin-top: 12px;">
                <NGi :span="12">
                  <NFormItem label="入口主机（流量进入的主机）" required>
                    <NSelect
                      v-model:value="form.mesh_forward_host_id"
                      :options="forwardHostOptions"
                      placeholder="选择入口主机（优先推荐带公网 IP 的节点）"
                      filterable
                    />
                  </NFormItem>
                </NGi>
                <NGi :span="6">
                  <NFormItem label="入口监听端口" required>
                    <NInputNumber
                      v-model:value="form.mesh_forward_entry_port"
                      placeholder="如 16379"
                      :min="1"
                      :max="65535"
                      style="width: 100%"
                    />
                  </NFormItem>
                </NGi>
                <NGi :span="6">
                  <NFormItem label="应用目标端口" required>
                    <NInputNumber
                      v-model:value="form.mesh_forward_target_port"
                      placeholder="如 6379"
                      :min="1"
                      :max="65535"
                      style="width: 100%"
                    />
                  </NFormItem>
                </NGi>
              </NGrid>

              <div class="hint" style="margin-top: 4px;">
                <template v-if="form.mesh_forward_host_id && selectedForwardHost">
                  <span v-if="forwardMeshLink">
                    链路：外部客户端 → {{ selectedForwardHost.hostname }} (:{{ form.mesh_forward_entry_port || '入口端口' }}) → 异地组网 ({{ selectedHostMeshIP }}:{{ form.mesh_forward_target_port || '目标端口' }}) → 应用容器，应用主机无需公网 IP
                  </span>
                  <span v-else>
                    链路：外部客户端 → {{ selectedForwardHost.hostname }} (:{{ form.mesh_forward_entry_port || '入口端口' }}) → 直连 ({{ selectedHost?.public_ip || '内网 IP' }}:{{ form.mesh_forward_target_port || '目标端口' }}) → 应用容器；请确认入口主机能访问该目标
                  </span>
                </template>
                <template v-else>
                  请选择入口主机并指定入口与目标端口。
                </template>
              </div>
            </template>
          </div>

          <!-- 流量拓扑预览：随表单实时反映主机/端口范围/反代模式/组网转发的最终链路 -->
          <AppTopologyPreview
            v-if="form.host_id"
            :domains="activeDomains.map((r) => r.domain.trim())"
            :proxy-mode="form.proxy_mode"
            :cert-status="overallCertStatus"
            :app-name="form.name.trim()"
            :host-name="selectedHost?.hostname || ''"
            :host-online="selectedHost?.status === 'online'"
            :host-public-ip="selectedHost?.public_ip || ''"
            :host-mesh-ip="selectedHostMeshIP"
            :host-mesh-source="selectedHostMeshSource"
            :gateway-name="selectedGateway?.hostname || ''"
            :gateway-online="selectedGateway?.status === 'online'"
            :gateway-public-ip="selectedGateway?.public_ip || ''"
            :gateway-mesh-ip="meshIPMap.get(selectedGateway?.id || '') || ''"
            :ports="form.ports"
            :compose-mode="sourceMode === 'compose'"
            :mesh-forward-enabled="form.mesh_forward_enabled"
            :mesh-forward-host-name="selectedForwardHost?.hostname || ''"
            :mesh-forward-host-online="selectedForwardHost?.status === 'online'"
            :mesh-forward-host-public-ip="selectedForwardHost?.public_ip || ''"
            :mesh-forward-host-mesh-ip="meshIPMap.get(selectedForwardHost?.id || '') || ''"
            :mesh-forward-entry-port="form.mesh_forward_entry_port"
            :mesh-forward-target-port="form.mesh_forward_target_port"
          />

          <NFormItem v-if="sourceMode === 'image' || sourceMode === 'template'" label="挂载卷（每行 宿主路径:容器路径）">
            <NInput
              v-model:value="form.volumes_text"
              type="textarea"
              :rows="2"
              placeholder="/opt/apps/demo/data:/app/data"
            />
          </NFormItem>

          <NFormItem v-if="sourceMode !== 'template'" label="环境变量（每行 KEY=VALUE，值支持 {{ vault:id:secret }} 引用）">
            <NInput
              v-model:value="form.env_vars_text"
              type="textarea"
              :rows="4"
              placeholder="DATABASE_URL=postgres://...&#10;API_KEY={{ vault:cred123:secret }}"
            />
          </NFormItem>

          <!-- 域名与网关（创建时可选，创建后立即绑定反代） -->
          <NFormItem label="域名与反代（可选，多域名支持，创建后自动绑定）">
            <NSpace vertical style="width: 100%">
              <div
                v-for="(item, idx) in domainRows"
                :key="item.id"
                class="create-domain-row"
                style="display: flex; gap: 8px; align-items: flex-start; margin-bottom: 6px;"
              >
                <div style="flex: 1; min-width: 0;">
                  <NInput
                    v-model:value="item.domain"
                    placeholder="如 app.example.com"
                    @input="onDomainRowChange(item)"
                  />
                  <div v-if="item.loading" class="create-domain-status">探测证书覆盖中…</div>
                  <div v-else-if="item.error" class="create-domain-status is-error">{{ item.error }}</div>
                  <div
                    v-else-if="item.domain.trim()"
                    class="create-domain-status"
                    :class="`is-${domainCertHint(item).tone}`"
                  >
                    {{ domainCertHint(item).text }}
                  </div>
                </div>
                <NButton quaternary type="error" @click="removeDomainRow(idx)" :disabled="domainRows.length <= 1">
                  删除
                </NButton>
              </div>
              <div style="display: flex; align-items: center; gap: 8px;">
                <NButton dashed size="small" @click="addDomainRow">添加域名</NButton>
                <span class="hint">证书默认自动匹配覆盖该域名的可用证书，无需逐个选择</span>
              </div>

              <template v-if="activeDomains.length > 0">
                <span class="hint">绑定后将在目标主机自动初始化系统托管的反代 Nginx 容器（占用 80/443，仅 Linux 主机）</span>
                <NRadioGroup v-model:value="form.proxy_mode">
                  <NSpace>
                    <NRadio value="local">节点本地（应用所在主机运行反代）</NRadio>
                    <NRadio value="gateway">统一网关（公网网关主机反代穿透到应用）</NRadio>
                  </NSpace>
                </NRadioGroup>
                <NSelect
                  v-if="form.proxy_mode === 'gateway'"
                  v-model:value="form.gateway_host_id"
                  :options="gatewayOptions"
                  placeholder="选择网关主机（可走组网或直连）"
                  filterable
                />
                <span v-if="form.proxy_mode === 'gateway'" class="hint">
                  <template v-if="gatewayMeshLink">
                    公网用户 -&gt; 网关主机 (Nginx/SSL) -&gt; 异地组网 ({{ selectedHostMeshIP }}) -&gt; 应用主机，应用主机无需公网 IP
                  </template>
                  <template v-else>
                    公网用户 -&gt; 网关主机 (Nginx/SSL) -&gt; 直连 应用主机 {{ selectedHost?.public_ip || '内网 IP' }}，无需异地组网；请确认网关能访问该地址
                  </template>
                </span>
                <NAlert
                  v-if="form.proxy_mode === 'local' && selectedHost && !selectedHost.public_ip"
                  type="warning"
                  size="small"
                  :show-icon="true"
                  style="margin-top: 4px"
                >
                  <template #header>应用主机没有公网 IP</template>
                  local 模式下域名无法从公网访问该应用；如主机位于 NAT 内网，建议改用「统一网关」模式。
                  <NButton
                    size="tiny"
                    secondary
                    type="primary"
                    style="margin-left: 8px"
                    @click="form.proxy_mode = 'gateway'"
                  >
                    切换到网关模式
                  </NButton>
                </NAlert>

                <!-- 高级：手动指定证书（默认自动匹配，仅特殊场景需要逐个覆盖） -->
                <NCollapse>
                  <NCollapseItem title="高级：手动指定证书" name="cert-override">
                    <NSpace vertical style="width: 100%">
                      <span class="hint">默认按域名自动匹配当前可用的覆盖证书；这里可逐域名改用其他证书（如专用多 SAN 证书）。</span>
                      <div
                        v-for="item in activeDomains"
                        :key="item.id"
                        style="display: flex; gap: 8px; align-items: center;"
                      >
                        <span style="min-width: 180px; font-size: 13px;">{{ item.domain.trim() }}</span>
                        <NSelect
                          v-model:value="item.cert_id"
                          :options="certOverrideOptions"
                          filterable
                          style="flex: 1"
                        />
                      </div>
                    </NSpace>
                  </NCollapseItem>
                </NCollapse>

                <!-- 自动签发配置区：提交时服务端报告哪些域名无可用证书，再对这些域名批量签发 -->
                <template v-if="hasUncoveredDomains">
                  <NAlert type="info" size="small" :show-icon="true">
                    <template #header>部分域名暂无可用证书</template>
                    提交时会先尝试自动绑定；服务端报告缺失证书后，将为这些域名（{{ autoIssueDomains.map((r) => r.domain.trim()).join('、') }}）自动签发一张多 SAN 证书，成功后再重试绑定。
                  </NAlert>
                  <NAlert v-if="!canIssue" type="warning" size="small" :show-icon="true">
                    <template #header>当前账号无证书签发权限</template>
                    证书签发仅管理员可用。可先提交创建应用，之后由管理员在应用详情-域名与反代中签发并绑定；或提前在证书中心准备好覆盖证书。
                  </NAlert>
                  <NAlert v-else-if="issueAccounts.length === 0" type="warning" size="small" :show-icon="true">
                    <template #header>尚未配置 ACME 账户</template>
                    请先到证书中心创建 ACME 账户，再回到本页提交（或在高级中为域名手动指定已有证书）。
                    <NButton
                      size="tiny"
                      secondary
                      type="primary"
                      style="margin-left: 8px"
                      @click="router.push('/certs')"
                    >
                      前往证书中心
                    </NButton>
                  </NAlert>
                  <template v-else>
                    <NSelect
                      v-model:value="issueAccountId"
                      :options="accountOptions"
                      size="small"
                      placeholder="选择 ACME 账户"
                    />
                    <NRadioGroup v-model:value="issueChallenge" size="small">
                      <NSpace>
                        <NRadio value="">自动推荐</NRadio>
                        <NRadio value="http-01">HTTP-01</NRadio>
                        <NRadio value="dns-01">DNS-01</NRadio>
                      </NSpace>
                    </NRadioGroup>
                    <NAlert
                      v-if="issueChallenge === 'dns-01' && !dnsConfigReady"
                      type="warning"
                      size="small"
                      :show-icon="true"
                    >
                      <template #header>DNS-01 需要 dns-mng 集成</template>
                      自动 DNS 验证依赖证书中心的 dns-mng 集成（自动添加/清理 TXT 解析），尚未配置。
                      <NButton
                        size="tiny"
                        secondary
                        type="primary"
                        style="margin-left: 8px"
                        @click="router.push('/certs')"
                      >
                        前往配置
                      </NButton>
                    </NAlert>
                    <span class="hint">
                      HTTP-01 验证在控制端主机临时监听 80 端口，仅当域名解析到控制端时可用；应用域名通常应选 DNS-01（需 dns-mng 自动解析）。
                    </span>
                  </template>
                  <span class="hint">
                    通配符域名（如 *.example.com）不支持在线自动签发：请先到
                    <a @click.prevent="router.push('/certs')">证书中心</a>
                    手动 DNS-01 签发后，回到本页提交（或在上方高级中直接指定）。
                  </span>
                </template>
              </template>
            </NSpace>
          </NFormItem>
        </NForm>

        <NSpace justify="end">
          <NButton @click="router.push('/apps')">取消</NButton>
          <NButton type="primary" :loading="creating" @click="submit">
            {{ creatingLabel || '创建应用' }}
          </NButton>
        </NSpace>
      </NSpace>
    </NCard>

    <AppAiAssistant
      class="ai-side"
      :snapshot="formSnapshot"
      @apply-draft="applyDraft"
    />
    </div>

    <CertLogModal
      v-model:show="certLogShow"
      :task-id="certLogTaskId"
      :domains="certLogDomains"
      title="证书签发日志"
    />
  </div>
</template>

<style scoped lang="scss">
.app-create-view {
  height: 100%;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.create-header {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;

  .page-title {
    margin: 0;
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
  }
  flex-shrink: 0;
}

.create-layout {
  display: flex;
  gap: 16px;
  align-items: flex-start;
}

/* 左侧表单保持原有 860px 上限（空间不足时按比例收缩），右侧 AI 区域
   以 0% 基准纯靠 grow 吃掉全部剩余宽度（其根卡片 width:100% 不可作
   为 flex 基准），大屏不再留白。 */
.create-card {
  width: 100%;
  max-width: 860px;
  flex-shrink: 1;
  flex-grow: 0;
  min-width: 0;
}

.create-domain-status {
  font-size: 11px;
  margin-top: 2px;
  color: var(--text-tertiary, #999);

  &.is-ok {
    color: #10b981;
  }

  &.is-warn {
    color: #f59e0b;
  }

  &.is-error {
    color: #e06c75;
  }
}

.ai-side {
  flex: 1 1 0%;
  min-width: 420px;
  position: sticky;
  top: 0;
  /* 右侧面板高度铺满可视区域：100vh 减去顶栏(50px)、页面上下内边距与
     页头（返回按钮+标题一行 ≈46px），卡片内部再自行切分滚动区。 */
  height: calc(100vh - 50px - var(--page-padding) * 2 - 46px);
}

@media (max-width: 1240px) {
  .create-layout {
    flex-direction: column;
  }

  .create-card {
    width: 100%;
    max-width: none;
  }

  .ai-side {
    width: 100%;
    position: static;
    height: auto;
  }
}

.template-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(230px, 1fr));
  gap: 10px;
  width: 100%;
}

.template-card {
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 12px;
  cursor: pointer;
  transition: all 0.15s ease;
  background: var(--bg-card);

  &:hover {
    border-color: #6366f1;
  }

  &.active {
    border-color: #6366f1;
    box-shadow: 0 0 0 1px #6366f1 inset;
  }

  .tpl-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 6px;

    .tpl-name {
      font-weight: 600;
      font-size: 13px;
    }
  }

  .tpl-desc {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
    margin-bottom: 6px;
    min-height: 36px;
  }

  .tpl-meta {
    font-size: 11px;
    color: var(--text-tertiary, #999);
    font-family: 'JetBrains Mono', Consolas, monospace;
  }
}

.param-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
  width: 100%;
}

.param-row {
  .param-label {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
    margin-bottom: 4px;
  }
}

.github-connect-box {
  background: rgba(128, 128, 128, 0.08);
  border: 1px dashed rgba(128, 128, 128, 0.25);
  border-radius: 8px;
  padding: 16px;
  margin-bottom: 20px;
}

.github-connected-bar {
  background: rgba(24, 160, 88, 0.08);
  border: 1px solid rgba(24, 160, 88, 0.2);
  border-radius: 8px;
  padding: 10px 14px;
  margin-bottom: 20px;
}

.hint {
  font-size: 12px;
  color: var(--n-text-color-disabled, #999);
}

.github-method-hint {
  margin: -6px 0 14px;
}

/* 代码推送自动部署：独立小节边界，避免与「代码来源」字段混读 */
.auto-deploy-box {
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 12px 14px;
  margin-bottom: 20px;
  background: rgba(128, 128, 128, 0.04);

  .auto-deploy-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 6px;
  }

  .auto-deploy-title {
    font-size: 13px;
    font-weight: 600;
  }

  .hint {
    display: block;
    line-height: 1.6;
  }
}

/* 镜像/仓库真实配置的探测提示条 */
.probe-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  width: 100%;
  font-size: 12px;
  line-height: 1.5;
  padding: 5px 10px;
  border-radius: 6px;

  &.ok {
    background: rgba(24, 160, 88, 0.08);
    border: 1px solid rgba(24, 160, 88, 0.22);
  }

  &.loading {
    color: var(--text-secondary);
    background: rgba(128, 128, 128, 0.08);
  }

  &.warn {
    color: #d48806;
    background: rgba(250, 173, 20, 0.08);
    border: 1px solid rgba(250, 173, 20, 0.22);
  }

  .probe-text {
    color: var(--text-secondary);
  }
}
</style>
