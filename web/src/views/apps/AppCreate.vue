<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert, NAvatar, NButton, NCard, NForm, NFormItem, NIcon, NInput, NInputNumber,
  NModal, NRadio, NRadioGroup, NSelect, NSpace, NSwitch, NTag, useMessage,
} from 'naive-ui'
import {
  ArrowBackOutline, LogoGithub, RefreshOutline, RocketOutline,
} from '@vicons/ionicons5'
import { createApp, type AppEntity, type PortMapping } from '../../api/apps'
import { listHosts } from '../../api/hosts'
import { listCredentials } from '../../api/vault'
import {
  connectGitHubToken, disconnectGitHub, getGitProviders,
  listGitHubBranches, listGitHubRepos, type GitHubAccount,
  type GitHubBranch, type GitHubRepo,
} from '../../api/git'
import { useWorkspaceStore } from '../../stores/workspace'

defineOptions({ name: 'AppCreate' })

const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()

interface HostOption {
  id: string
  hostname: string
  status: string
  os: string
}

interface VaultOption {
  id: string
  name: string
  type: string
}

const hosts = ref<HostOption[]>([])
const vaults = ref<VaultOption[]>([])
const creating = ref(false)

// 来源模式：github（授权选择仓库） vs custom（手动输入 URL） vs compose（在线 YAML 编排）
const sourceMode = ref<'github' | 'custom' | 'compose'>('github')

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
  ports: [{ host: 8080, container: 8080 }] as PortMapping[],
  volumes_text: '',
  healthcheck_url: '',
  container_name: '',
})

const hostOptions = computed(() =>
  hosts.value.map((h) => ({
    label: `${h.hostname} (${h.os}${h.status !== 'online' ? ' · 离线' : ''})`,
    value: h.id,
    disabled: h.status !== 'online',
  })),
)

const vaultOptions = computed(() => [
  { label: '不使用凭据（公开仓库）', value: '' },
  ...vaults.value.map((v) => ({ label: `${v.name} (${v.type})`, value: v.id })),
])

const repoOptions = computed(() =>
  githubRepos.value.map((r) => ({
    label: `${r.full_name}${r.private ? ' 🔒 (私有)' : ''}${r.description ? ' - ' + r.description : ''}`,
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
      // If not connected, default to github so user can click connect
      sourceMode.value = 'github'
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
  if (!form.name || form.name === 'my-web-app') {
    form.name = repo.name
  }

  // Fetch branches
  loadingBranches.value = true
  try {
    const [owner, name] = repo.full_name.split('/')
    const bList = await listGitHubBranches(owner, name)
    branches.value = bList
    // Default to repo default_branch or first branch
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

async function submit() {
  if (!form.name.trim()) {
    message.warning('请填写应用名称')
    return
  }
  if (!form.host_id) {
    message.warning('请选择目标主机')
    return
  }

  if (sourceMode.value === 'compose') {
    if (!form.compose_content.trim()) {
      message.warning('请填写 Docker Compose 内容')
      return
    }
  } else {
    if (!form.repo_url.trim()) {
      message.warning('请选择或填写 Git 仓库地址')
      return
    }
    if (!/^https?:\/\//.test(form.repo_url.trim())) {
      message.warning('仓库地址必须是 http(s):// 开头的 URL')
      return
    }
  }

  creating.value = true
  try {
    const isCompose = sourceMode.value === 'compose'
    const app = (await createApp({
      name: form.name.trim(),
      host_id: form.host_id,
      source_type: isCompose ? 'raw_compose' : 'git',
      repo_url: isCompose ? 'compose://local' : form.repo_url.trim(),
      branch: isCompose ? '' : form.branch.trim() || 'main',
      auth_vault_id: form.auth_vault_id || undefined,
      auto_deploy: isCompose ? false : form.auto_deploy,
      build_type: isCompose ? 'compose' : form.build_type,
      compose_content: isCompose ? form.compose_content : undefined,
      dockerfile: form.dockerfile.trim() || (form.build_type === 'compose' ? 'compose.yaml' : 'Dockerfile'),
      build_context: form.build_context.trim() || '.',
      build_timeout_sec: Number(form.build_timeout_sec) || 900,
      env_vars: parsedEnvVars.value,
      ports: form.ports.filter((p) => p.host > 0 && p.container > 0),
      volumes: parsedVolumes.value,
      healthcheck_url: form.healthcheck_url.trim(),
      container_name: form.container_name.trim() || undefined,
    })) as AppEntity
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
  form.ports.push({ host: 0, container: 0 })
}

function removePort(idx: number) {
  form.ports.splice(idx, 1)
}

onMounted(loadData)
</script>

<template>
  <div class="app-create-view">
    <NSpace align="center" style="margin-bottom: 12px">
      <NButton quaternary @click="router.push('/apps')">
        <template #icon>
          <NIcon><ArrowBackOutline /></NIcon>
        </template>
        返回应用列表
      </NButton>
    </NSpace>

    <NCard title="链接 Git 应用" class="create-card">
      <template #header>
        <NSpace align="center" :size="6">
          <NIcon><RocketOutline /></NIcon>
          <span>链接 Git 应用</span>
        </NSpace>
      </template>

      <NSpace vertical size="large">
        <!-- 模式选择 -->
        <NFormItem label="应用部署模式" style="margin-bottom: 4px">
          <NRadioGroup v-model:value="sourceMode">
            <NSpace>
              <NRadio value="github">
                <NSpace align="center" :size="4">
                  <NIcon><LogoGithub /></NIcon>
                  <span>GitHub 授权仓库 (推荐)</span>
                </NSpace>
              </NRadio>
              <NRadio value="custom">通用 Git 仓库 (手动输入 URL)</NRadio>
              <NRadio value="compose">在线 Docker Compose 编排 (微服务栈)</NRadio>
            </NSpace>
          </NRadioGroup>
        </NFormItem>

        <!-- GitHub 授权模式卡片 -->
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
        </template>

        <NForm label-placement="top">
          <!-- GitHub 模式下的仓库与分支选择 -->
          <template v-if="sourceMode === 'github'">
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

          <!-- 在线 Docker Compose 编排模式 -->
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

          <!-- 通用手动输入模式 -->
          <template v-else>
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

          <template v-if="sourceMode !== 'compose'">
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
              <NFormItem label="容器名（可选）" style="flex: 1">
                <NInput v-model:value="form.container_name" placeholder="留空自动生成" />
              </NFormItem>
            </NSpace>

            <NFormItem label="健康检查 URL（可选，推荐）">
              <NInput
                v-model:value="form.healthcheck_url"
                placeholder="http://127.0.0.1:8080/healthz（探活失败将自动保留旧版本）"
              />
            </NFormItem>

            <NFormItem label="端口映射">
              <NSpace vertical style="width: 100%">
                <NSpace v-for="(p, idx) in form.ports" :key="idx" align="center">
                  <NInputNumber v-model:value="p.host" placeholder="宿主端口" style="width: 140px" />
                  <span>-></span>
                  <NInputNumber v-model:value="p.container" placeholder="容器端口" style="width: 140px" />
                  <NButton quaternary type="error" size="small" @click="removePort(idx)">移除</NButton>
                </NSpace>
                <NButton dashed size="small" @click="addPort">添加端口映射</NButton>
              </NSpace>
            </NFormItem>
          </template>

          <NFormItem label="环境变量（每行 KEY=VALUE，值支持 {{ vault:id:secret }} 引用）">
            <NInput
              v-model:value="form.env_vars_text"
              type="textarea"
              :rows="4"
              placeholder="DATABASE_URL=postgres://...&#10;API_KEY={{ vault:cred123:secret }}"
            />
          </NFormItem>

          <NFormItem label="挂载卷（每行 宿主路径:容器路径）">
            <NInput
              v-model:value="form.volumes_text"
              type="textarea"
              :rows="2"
              placeholder="/opt/apps/demo/data:/app/data"
            />
          </NFormItem>

          <NFormItem label="推送代码自动部署（Webhook）">
            <NSpace align="center">
              <NSwitch v-model:value="form.auto_deploy" />
              <span v-if="form.auto_deploy" class="hint">创建后可在应用详情页复制 Webhook 地址配置到仓库</span>
            </NSpace>
          </NFormItem>
        </NForm>

        <NSpace justify="end">
          <NButton @click="router.push('/apps')">取消</NButton>
          <NButton type="primary" :loading="creating" @click="submit">
            创建并链接应用
          </NButton>
        </NSpace>
      </NSpace>
    </NCard>

    <!-- 连接 GitHub 弹窗 -->
    <NModal
      v-model:show="showAuthModal"
      preset="card"
      title="连接 GitHub 账号"
      style="width: 540px; max-width: 94vw"
    >
      <NSpace vertical size="medium">
        <NAlert type="info" :show-icon="true">
          请输入您的 GitHub Personal Access Token (Classic 或 Fine-grained)。
          需勾选 <b>repo</b> 权限以便读取私有仓库和分支代码。
        </NAlert>

        <NFormItem label="GitHub Personal Access Token" required>
          <NInput
            v-model:value="githubTokenInput"
            type="password"
            show-password-on="click"
            placeholder="ghp_xxxxxxxxxxxxxxxxxxxx"
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

.create-card {
  max-width: 860px;
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
