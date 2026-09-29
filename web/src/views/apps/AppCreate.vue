<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert, NAvatar, NButton, NCard, NForm, NFormItem, NIcon, NInput, NInputNumber,
  NModal, NRadio, NRadioGroup, NSelect, NSpace, NSwitch, NTag, useMessage,
} from 'naive-ui'
import {
  ArrowBackOutline, LogoGithub, RefreshOutline,
} from '@vicons/ionicons5'
import {
  createApp, listAppCatalog, type AppEntity, type AppTemplate, type PortMapping,
} from '../../api/apps'
import { bindProxy } from '../../api/certs'
import { listHosts } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import { listCredentials } from '../../api/vault'
import {
  connectGitHubToken, disconnectGitHub, getGitProviders,
  listGitHubBranches, listGitHubRepos, type GitHubAccount,
  type GitHubBranch, type GitHubRepo,
} from '../../api/git'
import { useWorkspaceStore } from '../../stores/workspace'

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
const showAuthModal = ref(false)
const githubTokenInput = ref('')
const connectingGitHub = ref(false)

const githubRepos = ref<GitHubRepo[]>([])
const loadingRepos = ref(false)
const selectedRepoFullName = ref<string | null>(null)

const branches = ref<GitHubBranch[]>([])
const loadingBranches = ref(false)

const form = reactive({
  name: '',
  host_id: '',
  template_id: '',
  image: '',
  repo_url: '',
  branch: 'main',
  auth_vault_id: '',
  auto_deploy: true,
  build_type: 'dockerfile' as 'dockerfile' | 'compose',
  dockerfile: 'Dockerfile',
  build_context: '.',
  build_timeout_sec: 900,
  compose_content: defaultComposeTemplate,
  env_vars_text: '',
  ports: [] as (PortMapping & { bind_scope: 'public' | 'mesh' })[],
  volumes_text: '',
  healthcheck_url: '',
  container_name: '',
  template_params: {} as Record<string, string>,
  // 域名与反代（可选，创建后立即绑定）
  domain: '',
  proxy_mode: 'local' as 'local' | 'gateway',
  gateway_host_id: '',
})

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

const bindScopeOptions = [
  { label: '公网开放 (0.0.0.0)', value: 'public' },
  { label: '仅异地组网 (Tailscale IP)', value: 'mesh' },
]

const hostOptions = computed(() =>
  hosts.value.map((h) => ({
    label: `${h.hostname}${h.public_ip ? ` · ${h.public_ip}` : ''}${h.status !== 'online' ? ' · 离线' : ''}`,
    value: h.id,
    disabled: h.status !== 'online',
  })),
)

// 有公网 IP 的在线主机作为推荐网关；绑定 mesh 端口提示节点 IP
const gatewayOptions = computed(() =>
  hosts.value
    .filter((h) => h.status === 'online')
    .map((h) => ({
      label: h.public_ip
        ? `${h.hostname}（公网 ${h.public_ip}，推荐）`
        : `${h.hostname}`,
      value: h.id,
    })),
)

const meshIPMap = computed(() => {
  const m = new Map<string, string>()
  for (const n of meshNodes.value) {
    if (n.ip) m.set(n.host_id, n.ip)
  }
  return m
})

const selectedHostMeshIP = computed(() => meshIPMap.value.get(form.host_id) || '')

const vaultOptions = computed(() => [
  { label: '不使用凭据（公开仓库）', value: '' },
  ...vaults.value.map((v) => ({ label: `${v.name} (${v.type})`, value: v.id })),
])

const repoOptions = computed(() =>
  githubRepos.value.map((r) => ({
    label: `${r.full_name}${r.private ? ' [私有]' : ''}${r.description ? ' - ' + r.description : ''}`,
    value: r.full_name,
  })),
)

const branchOptions = computed(() =>
  branches.value.map((b) => ({
    label: b.name + (b.protected ? ' (受保护)' : ''),
    value: b.name,
  })),
)

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
    }
  } catch {
    githubAccount.value = null
  } finally {
    checkingGitHub.value = false
  }
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

// 当用户在下拉框选中某个 GitHub 仓库时：自动填充应用名、克隆地址，并拉取所有分支
watch(selectedRepoFullName, async (fullName) => {
  if (!fullName) return
  const repo = githubRepos.value.find((r) => r.full_name === fullName)
  if (!repo) return

  form.repo_url = repo.clone_url
  if (!form.name) {
    form.name = repo.name
  }

  loadingBranches.value = true
  try {
    const [owner, name] = repo.full_name.split('/')
    const bList = await listGitHubBranches(owner, name)
    branches.value = bList
    if (bList.some((b) => b.name === repo.default_branch)) {
      form.branch = repo.default_branch
    } else if (bList.length > 0) {
      form.branch = bList[0].name
    }
  } catch (e: any) {
    message.error(e.message || '获取仓库分支失败')
  } finally {
    loadingBranches.value = false
  }
})

async function submitGitHubToken() {
  if (!githubTokenInput.value.trim()) {
    message.warning('请输入 GitHub Personal Access Token')
    return
  }
  connectingGitHub.value = true
  try {
    const acc = await connectGitHubToken(githubTokenInput.value.trim())
    githubAccount.value = acc
    message.success(`成功连接 GitHub 账号: ${acc.login}`)
    showAuthModal.value = false
    githubTokenInput.value = ''
    await fetchGitHubRepos()
  } catch (e: any) {
    message.error(e.message || '连接失败，请检查 Token 权限')
  } finally {
    connectingGitHub.value = false
  }
}

async function doDisconnectGitHub() {
  try {
    await disconnectGitHub()
    githubAccount.value = null
    githubRepos.value = []
    selectedRepoFullName.value = null
    message.success('已断开 GitHub 连接')
  } catch (e: any) {
    message.error(e.message || '断开失败')
  }
}

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
    case 'custom':
      if (!form.repo_url.trim()) return '请选择或填写 Git 仓库地址'
      if (!/^https?:\/\//.test(form.repo_url.trim())) return '仓库地址必须是 http(s):// 开头的 URL'
      break
  }
  for (const p of form.ports) {
    if (p.host <= 0 || p.host > 65535 || p.container <= 0 || p.container > 65535) {
      return `端口映射非法: ${p.host}:${p.container}`
    }
    if (p.bind_scope === 'mesh' && !meshIPMap.value.get(form.host_id)) {
      return '所选主机未加入异地组网，无法使用「仅异地组网」端口范围（可在主机详情-异地组网中加入）'
    }
  }
  if (form.domain.trim() && !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i.test(form.domain.trim())) {
    return '域名格式不正确'
  }
  if (form.domain.trim() && form.proxy_mode === 'gateway' && !form.gateway_host_id) {
    return '网关模式需选择网关主机'
  }
  return null
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
      host_id: form.host_id,
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
      env_vars: parsedEnvVars.value,
      ports: form.ports.filter((p) => p.host > 0 && p.container > 0),
      volumes: parsedVolumes.value,
      healthcheck_url: form.healthcheck_url.trim(),
      container_name: form.container_name.trim() || undefined,
    })) as AppEntity

    // 可选的创建时域名绑定：证书不存在时后端用默认 ACME 账户自动签发
    if (form.domain.trim()) {
      try {
        await bindProxy(app.id, {
          domain: form.domain.trim(),
          mode: form.proxy_mode,
          gateway_host_id: form.proxy_mode === 'gateway' ? form.gateway_host_id : undefined,
        })
        message.success(`域名 ${form.domain.trim()} 已绑定并下发反代`)
      } catch (e: any) {
        message.warning(`应用已创建，但域名绑定失败：${e.message || e}（可稍后在应用详情-域名与反代中重试）`)
      }
    }

    message.success(`应用「${app.name}」创建成功`)
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

    <NCard :bordered="false" class="create-card">
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
              <NInput v-model:value="form.image" placeholder="nginx:alpine / redis:7.2 / registry.example.com/app:1.0" />
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
            <div v-if="!githubAccount" class="github-connect-box">
              <NSpace align="center" justify="space-between" style="width: 100%">
                <NSpace align="center">
                  <NIcon size="28"><LogoGithub /></NIcon>
                  <div>
                    <div style="font-weight: 600">未连接 GitHub 账号</div>
                    <div class="hint">连接后可直接读取您公开与私有的所有仓库列表，点击即可创建，自动拉取分支。</div>
                  </div>
                </NSpace>
                <NButton type="primary" secondary @click="showAuthModal = true">
                  <template #icon>
                    <NIcon><LogoGithub /></NIcon>
                  </template>
                  连接 GitHub 账号
                </NButton>
              </NSpace>
            </div>

            <div v-else class="github-connected-bar">
              <NSpace align="center" justify="space-between" style="width: 100%">
                <NSpace align="center">
                  <NAvatar :src="githubAccount.avatar_url" round size="small" />
                  <span style="font-weight: 600">{{ githubAccount.name || githubAccount.login }}</span>
                  <NTag size="tiny" type="success" :bordered="false">已授权连接</NTag>
                </NSpace>
                <NSpace align="center" :size="4">
                  <NButton size="tiny" secondary @click="fetchGitHubRepos" :loading="loadingRepos">
                    <template #icon>
                      <NIcon><RefreshOutline /></NIcon>
                    </template>
                    刷新仓库
                  </NButton>
                  <NButton size="tiny" quaternary type="error" @click="doDisconnectGitHub">断开</NButton>
                </NSpace>
              </NSpace>
            </div>

            <NFormItem label="选择 GitHub 仓库" required>
              <NSelect
                v-model:value="selectedRepoFullName"
                :options="repoOptions"
                :loading="loadingRepos"
                placeholder="搜索并选择您的代码仓库 (支持私有库与组织仓库)"
                filterable
              />
            </NFormItem>

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

          <!-- 通用 Git -->
          <template v-else-if="sourceMode === 'custom'">
            <NFormItem label="应用名称" required>
              <NInput v-model:value="form.name" placeholder="如 my-web-api" />
            </NFormItem>

            <NFormItem label="Git 仓库地址" required>
              <NInput
                v-model:value="form.repo_url"
                placeholder="https://github.com/yourname/your-repo.git"
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
              <NRadioGroup v-model:value="form.build_type">
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

          <!-- 端口与网络（Compose 除外，其端口在 YAML 中声明） -->
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
                <span v-if="selectedHostMeshIP" class="hint">
                  目标主机组网 IP：{{ selectedHostMeshIP }}，「仅异地组网」端口将只绑定该 IP，公网无法访问
                </span>
              </NSpace>
            </NFormItem>

            <NFormItem label="健康检查 URL（可选，推荐）">
              <NInput
                v-model:value="form.healthcheck_url"
                placeholder="http://127.0.0.1:8080/healthz（探活失败将自动保留旧版本）"
              />
            </NFormItem>
          </template>

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
          <NFormItem label="域名（可选，创建后自动绑定反代与证书）">
            <NSpace vertical style="width: 100%">
              <NInput v-model:value="form.domain" placeholder="app.example.com（留空则不绑定域名）" />
              <template v-if="form.domain">
                <NRadioGroup v-model:value="form.proxy_mode">
                  <NSpace>
                    <NRadio value="local">节点本地（应用所在主机自建 Nginx）</NRadio>
                    <NRadio value="gateway">统一网关（公网网关主机反代穿透到应用）</NRadio>
                  </NSpace>
                </NRadioGroup>
                <NSelect
                  v-if="form.proxy_mode === 'gateway'"
                  v-model:value="form.gateway_host_id"
                  :options="gatewayOptions"
                  placeholder="选择网关主机（推荐选有公网 IP 的节点）"
                  filterable
                />
                <span v-if="form.proxy_mode === 'gateway'" class="hint">
                  公网用户 -&gt; 网关主机 (Nginx/SSL) -&gt; 异地组网 -&gt; 应用主机，应用主机无需公网 IP
                </span>
              </template>
            </NSpace>
          </NFormItem>

          <NFormItem v-if="sourceMode === 'github' || sourceMode === 'custom'" label="推送代码自动部署（Webhook）">
            <NSpace align="center">
              <NSwitch v-model:value="form.auto_deploy" />
              <span v-if="form.auto_deploy" class="hint">
                {{ sourceMode === 'github'
                    ? '系统将自动通过已连接的 GitHub 账号在仓库配置 Webhook，推送代码后自动部署（无需手动配置）'
                    : '创建后可在应用详情页复制 Webhook 地址配置到仓库' }}
              </span>
            </NSpace>
          </NFormItem>
        </NForm>

        <NSpace justify="end">
          <NButton @click="router.push('/apps')">取消</NButton>
          <NButton type="primary" :loading="creating" @click="submit">
            创建应用
          </NButton>
        </NSpace>
      </NSpace>
    </NCard>

    <!-- 连接 GitHub 弹窗 -->
    <NModal
      v-model:show="showAuthModal"
      preset="card"
      title="连接 GitHub 账号"
      style="width: 620px; max-width: 94vw"
    >
      <NSpace vertical size="medium">
        <NAlert type="info" :show-icon="true">
          <div style="font-weight: 600; margin-bottom: 6px">Token 权限配置说明（满足以下任一方式即可）：</div>
          <div style="font-size: 13px; line-height: 1.6">
            <div>
              <b>1. 细粒度 Token（Fine-grained，官方推荐）</b>：
              <ul style="margin: 2px 0 6px 18px; padding: 0">
                <li><b>Repository access</b>：选择目标仓库或 <i>All repositories</i></li>
                <li><b>Permissions -&gt; Contents</b>：设置为 <b>Read-only</b>（用于读取代码与 Compose 编排）</li>
                <li><i>(可选)</i> <b>Permissions -&gt; Webhooks</b>：设置为 <b>Read and write</b>（用于提交自动触发重部署）</li>
              </ul>
            </div>
            <div>
              <b>2. 传统 Token（Tokens classic）</b>：
              <ul style="margin: 2px 0 0 18px; padding: 0">
                <li>私有仓库勾选 <b>repo</b>（公开仓库仅需 <b>public_repo</b>）</li>
                <li><i>(可选)</i> 勾选 <b>admin:repo_hook</b>（用于自动注册 Push Webhook）</li>
              </ul>
            </div>
          </div>
        </NAlert>

        <NFormItem label="GitHub Personal Access Token" required>
          <NInput
            v-model:value="githubTokenInput"
            type="password"
            show-password-on="click"
            placeholder="github_pat_xxx 或 ghp_xxx"
          />
        </NFormItem>

        <NSpace justify="end">
          <NButton @click="showAuthModal = false">取消</NButton>
          <NButton type="primary" :loading="connectingGitHub" @click="submitGitHubToken">
            验证并连接
          </NButton>
        </NSpace>
      </NSpace>
      </NModal>
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

.create-card {
  max-width: 860px;
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
}

.github-connected-bar {
  background: rgba(24, 160, 88, 0.08);
  border: 1px solid rgba(24, 160, 88, 0.2);
  border-radius: 8px;
  padding: 10px 14px;
}

.hint {
  font-size: 12px;
  color: var(--n-text-color-disabled, #999);
}
</style>
