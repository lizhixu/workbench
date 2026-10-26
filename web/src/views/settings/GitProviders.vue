<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NCard,
  NSpace,
  NButton,
  NInput,
  NFormItem,
  NAlert,
  NIcon,
  NAvatar,
  NTag,
  NModal,
  NSelect,
  NCode,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import { LogoGithub } from '@vicons/ionicons5'
import {
  getGitProviders, connectGitHubToken, disconnectGitHub,
  listGitHubRepos, type GitProvidersStatus,
  getGitHubAppStatus, createGitHubAppManifest, exchangeGitHubAppManifest,
  saveGitHubAppConfig, disconnectGitHubApp, syncGitHubAppInstallations,
  type GitHubAppStatus,
} from '../../api/git'

// 从旧 Settings.vue 抽取：代码源集成 (Git Providers) + 连接弹窗
defineOptions({ name: 'GitProviders' })

const message = useMessage()
const route = useRoute()
const router = useRouter()

const gitStatus = ref<GitProvidersStatus | null>(null)
const loadingGit = ref(false)
const showGitAuthModal = ref(false)
const gitTokenInput = ref('')
const connectingGit = ref(false)
const disconnectingGit = ref(false)
const refreshingGit = ref(false)

async function loadGitStatus() {
  loadingGit.value = true
  try {
    gitStatus.value = await getGitProviders()
  } catch (e: any) {
    // silently fail
  } finally {
    loadingGit.value = false
  }
}

async function submitGitToken() {
  if (!gitTokenInput.value.trim()) {
    message.warning('请输入 GitHub Personal Access Token')
    return
  }
  connectingGit.value = true
  try {
    const acc = await connectGitHubToken(gitTokenInput.value.trim())
    message.success(`成功连接 GitHub 账号: ${acc.login}`)
    showGitAuthModal.value = false
    gitTokenInput.value = ''
    await loadGitStatus()
  } catch (e: any) {
    message.error(e.message || '连接失败')
  } finally {
    connectingGit.value = false
  }
}

async function doDisconnectGit() {
  disconnectingGit.value = true
  try {
    await disconnectGitHub()
    message.success('已断开 GitHub 连接')
    await loadGitStatus()
  } catch (e: any) {
    message.error(e.message || '断开失败')
  } finally {
    disconnectingGit.value = false
  }
}

async function refreshGitRepos() {
  refreshingGit.value = true
  try {
    const repos = await listGitHubRepos()
    message.success(`GitHub 授权有效，已成功获取 ${repos.length} 个可用仓库`)
  } catch (e: any) {
    message.error(e.message || '同步失败')
  } finally {
    refreshingGit.value = false
  }
}

// ---- GitHub App（App 级推送事件，替代逐仓 Webhook）----

const appStatus = ref<GitHubAppStatus | null>(null)
const loadingApp = ref(false)
const appNameInput = ref('')
const appTarget = ref<string | null>('personal')
const appOrgInput = ref('')
const creatingApp = ref(false)
const syncingApp = ref(false)
const disconnectingApp = ref(false)
const showAppManualModal = ref(false)
const manualAppId = ref('')
const manualPrivateKey = ref('')
const manualWebhookSecret = ref('')
const savingManualApp = ref(false)

async function loadAppStatus() {
  loadingApp.value = true
  try {
    appStatus.value = await getGitHubAppStatus()
  } catch (e: any) {
    // silently fail
  } finally {
    loadingApp.value = false
  }
}

async function doCreateApp() {
  if (!appNameInput.value.trim()) {
    message.warning('请填写 GitHub App 名称（全局唯一）')
    return
  }
  if (appTarget.value === 'organization' && !appOrgInput.value.trim()) {
    message.warning('组织目标需要填写组织名')
    return
  }
  creatingApp.value = true
  try {
    const res = await createGitHubAppManifest(
      appNameInput.value.trim(),
      appTarget.value || 'personal',
      appOrgInput.value.trim() || undefined,
    )
    if (!res.public_url_configured) {
      message.warning('面板公网地址未配置，事件地址将使用本次请求地址，GitHub 可能无法访问，请先到系统设置配置 server.public_url')
    }
    // GitHub Manifest 流程要求浏览器以表单 POST 提交 manifest，换 code 回调。
    const form = document.createElement('form')
    form.method = 'POST'
    form.action = `${res.submit_url}?state=${encodeURIComponent(res.state)}`
    const input = document.createElement('input')
    input.type = 'hidden'
    input.name = 'manifest'
    input.value = JSON.stringify(res.manifest)
    form.appendChild(input)
    document.body.appendChild(form)
    form.submit()
  } catch (e: any) {
    message.error(e.message || '创建失败')
  } finally {
    creatingApp.value = false
  }
}

async function doSyncAppInstallations() {
  syncingApp.value = true
  try {
    const list = await syncGitHubAppInstallations()
    if (appStatus.value) appStatus.value.installations = list
    message.success(`已同步 ${list.length} 个安装`)
  } catch (e: any) {
    message.error(e.message || '同步失败')
  } finally {
    syncingApp.value = false
  }
}

async function doDisconnectApp() {
  disconnectingApp.value = true
  try {
    await disconnectGitHubApp()
    message.success('已断开 GitHub App')
    await loadAppStatus()
  } catch (e: any) {
    message.error(e.message || '断开失败')
  } finally {
    disconnectingApp.value = false
  }
}

async function submitManualApp() {
  const appId = Number(manualAppId.value)
  if (!appId || !manualPrivateKey.value.trim() || !manualWebhookSecret.value.trim()) {
    message.warning('请填写 App ID、私钥与 Webhook Secret')
    return
  }
  savingManualApp.value = true
  try {
    await saveGitHubAppConfig(appId, manualPrivateKey.value.trim(), manualWebhookSecret.value.trim())
    message.success('GitHub App 配置已保存')
    showAppManualModal.value = false
    manualAppId.value = ''
    manualPrivateKey.value = ''
    manualWebhookSecret.value = ''
    await loadAppStatus()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingManualApp.value = false
  }
}

function openAppInstallPage() {
  if (!appStatus.value?.slug) return
  window.open(`https://github.com/apps/${appStatus.value.slug}/installations/new`, '_blank')
}

function copyAppWebhook() {
  if (!appStatus.value?.webhook_url) return
  navigator.clipboard?.writeText(appStatus.value.webhook_url).then(
    () => message.success('事件地址已复制'),
    () => message.error('复制失败，请手动复制'),
  )
}

async function handleAppCallback() {
  // Manifest 回调：GitHub 把 code 追加到 redirect_url 后面；state 由服务端
  // 嵌在 redirect_url 的 github_app_state 参数里带回（submit URL 的 ?state=
  // GitHub 不保证透传，故不依赖它；兼容旧参数名 state）。
  const code = route.query.code as string | undefined
  const state = (route.query.github_app_state || route.query.state) as string | undefined
  if (!code && !state) return
  // 清理回调参数，保留 ?s=git 分区参数。
  const cleanQuery = { ...route.query }
  delete cleanQuery.code
  delete cleanQuery.state
  delete cleanQuery.github_app_state
  router.replace({ query: cleanQuery })
  if (!code || !state) return
  try {
    const res = await exchangeGitHubAppManifest(code, state)
    message.success(`GitHub App 已创建：${res.slug}，请继续安装到账号或组织`)
    await loadAppStatus()
  } catch (e: any) {
    message.error(e.message || '换取 App 配置失败，请重新创建')
  }
}

onMounted(() => {
  loadGitStatus()
  loadAppStatus()
  handleAppCallback()
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><LogoGithub /></NIcon>
        代码源集成 (Git Providers)
      </span>
    </template>
    <template #header-extra>
      <span class="muted" style="font-size: 13px">Connect your Git provider for authentication.</span>
    </template>

    <NSpace vertical :size="14">
      <p class="muted tip-hint">
        连接您的 Git 账号后，发布应用可直接读取私有与公开代码仓库。启用自动部署后，系统将自动通过 API 为仓库配置 Webhook，实现代码提交即自动构建与更新，无需手动去仓库配置 Webhook。
      </p>

      <div class="git-provider-row">
        <NSpace align="center" justify="space-between" style="width: 100%">
          <NSpace align="center" :size="12">
            <NAvatar
              v-if="gitStatus?.github?.connected && gitStatus.github.account?.avatar_url"
              :src="gitStatus.github.account.avatar_url"
              round
              :size="36"
            />
            <NIcon v-else size="32" :component="LogoGithub" />
            <div>
              <NSpace align="center" :size="8">
                <span style="font-size: 15px; font-weight: 600">GitHub</span>
                <NTag v-if="gitStatus?.github?.connected" type="success" size="small" round>已连接</NTag>
                <NTag v-else type="default" size="small" round>未连接</NTag>
              </NSpace>
              <div class="muted" style="font-size: 12px; margin-top: 2px">
                {{ gitStatus?.github?.connected
                    ? `已授权账号：${gitStatus.github.account?.name || gitStatus.github.account?.login} (@${gitStatus.github.account?.login})`
                    : '通过 Personal Access Token 连接您的 GitHub 账号' }}
              </div>
            </div>
          </NSpace>

          <NSpace align="center">
            <template v-if="gitStatus?.github?.connected">
              <NButton size="small" secondary :loading="refreshingGit" @click="refreshGitRepos">
                测试同步仓库
              </NButton>
              <NButton size="small" quaternary type="error" :loading="disconnectingGit" @click="doDisconnectGit">
                断开连接
              </NButton>
            </template>
            <template v-else>
              <NButton type="primary" size="small" @click="showGitAuthModal = true">
                连接 GitHub
              </NButton>
            </template>
          </NSpace>
        </NSpace>
      </div>
    </NSpace>
  </NCard>

  <!-- GitHub App：App 级推送事件，替代逐仓 Webhook -->
  <NCard :bordered="false" size="small" style="margin-top: 14px">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><LogoGithub /></NIcon>
        GitHub App（自动部署主推）
      </span>
    </template>
    <template #header-extra>
      <NTag v-if="appStatus?.configured" type="success" size="small" round>已配置</NTag>
      <NTag v-else type="default" size="small" round>未配置</NTag>
    </template>

    <NSpace vertical :size="14">
      <p class="muted tip-hint">
        在账号或组织里安装一次 App，之后该账号下所有仓库（或选定的仓库）推送到应用配置的分支时，都会自动触发面板部署，全程不用再给任何仓库单独配置 Webhook。注意：GitHub 仍需把事件推给面板，面板必须有公网地址，否则事件送不进来。
      </p>

      <template v-if="appStatus?.configured">
        <div class="git-provider-row">
          <NSpace vertical :size="8" style="width: 100%">
            <NSpace align="center" :size="8">
              <span style="font-weight: 600">{{ appStatus.slug || `App #${appStatus.app_id}` }}</span>
              <span class="muted" style="font-size: 12px">App ID：{{ appStatus.app_id }}</span>
            </NSpace>
            <NSpace align="center" :size="8" style="width: 100%">
              <span class="muted" style="font-size: 12px; flex-shrink: 0">事件地址：</span>
              <NCode :code="appStatus.webhook_url" style="flex: 1; min-width: 0" />
              <NButton size="tiny" secondary @click="copyAppWebhook">复制</NButton>
            </NSpace>
            <div v-if="appStatus.installations.length">
              <div class="muted" style="font-size: 12px; margin-bottom: 6px">已安装：</div>
              <NSpace vertical :size="6">
                <div v-for="inst in appStatus.installations" :key="inst.id">
                  <NSpace align="center" :size="8">
                    <span>@{{ inst.account_login }}</span>
                    <NTag size="small" :type="inst.account_type === 'Organization' ? 'info' : 'default'">
                      {{ inst.account_type === 'Organization' ? '组织' : '个人' }}
                    </NTag>
                    <NTag size="small" type="default">
                      {{ inst.target_type === 'all' ? '全部仓库' : `选定仓库 ${(inst.repos || []).length} 个` }}
                    </NTag>
                    <NTag v-if="inst.suspended" size="small" type="warning">已暂停</NTag>
                  </NSpace>
                </div>
              </NSpace>
            </div>
            <NAlert v-else type="info" :show-icon="true" style="margin-top: 4px">
              App 已创建但尚未安装。点「安装到账号或组织」选好仓库范围并授权，回来点「同步安装」即可。
            </NAlert>
          </NSpace>
        </div>
        <NSpace>
          <NButton type="primary" size="small" @click="openAppInstallPage">安装到账号或组织</NButton>
          <NButton size="small" secondary :loading="syncingApp" @click="doSyncAppInstallations">同步安装</NButton>
          <NPopconfirm @positive-click="doDisconnectApp">
            <template #trigger>
              <NButton size="small" quaternary type="error" :loading="disconnectingApp">断开 GitHub App</NButton>
            </template>
            断开后 App 推送不再触发部署，已配置的应用需切回逐仓 Webhook 或手动部署。
          </NPopconfirm>
        </NSpace>
      </template>

      <template v-else>
        <NSpace vertical :size="10">
          <NSpace align="center">
            <NInput
              v-model:value="appNameInput"
              placeholder="GitHub App 名称，如 Watchman-App（全局唯一）"
              style="width: 320px"
            />
            <NSelect
              v-model:value="appTarget"
              :options="[
                { label: '个人账号', value: 'personal' },
                { label: '组织', value: 'organization' },
              ]"
              style="width: 140px"
            />
            <NInput
              v-if="appTarget === 'organization'"
              v-model:value="appOrgInput"
              placeholder="组织名"
              style="width: 200px"
            />
          </NSpace>
          <NSpace>
            <NButton type="primary" :loading="creatingApp" @click="doCreateApp">创建 GitHub App</NButton>
            <NButton secondary @click="showAppManualModal = true">手动填写已有 App</NButton>
          </NSpace>
          <p class="muted tip-hint" style="font-size: 12px">
            点「创建」会跳到 GitHub 建 App（权限只要内容只读与元数据只读、只订阅 push 事件），建好自动跳回面板完成配置；第二步再点「安装到账号或组织」选仓库范围。老的手动逐仓 Webhook 方式仍可继续用。
          </p>
        </NSpace>
      </template>
    </NSpace>
  </NCard>

  <!-- 连接 GitHub 弹窗 -->
  <NModal
    v-model:show="showGitAuthModal"
    preset="card"
    title="连接 GitHub 账号 (Connect GitHub)"
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
              <li><b>Permissions -> Contents</b>：设置为 <b>Read-only</b>（用于读取代码与 Compose 编排）</li>
              <li><b>Permissions -> Webhooks</b>：设置为 <b>Read and write</b>（用于系统自动创建 push Webhook）</li>
            </ul>
          </div>
          <div>
            <b>2. 传统 Token（Tokens classic）</b>：
            <ul style="margin: 2px 0 0 18px; padding: 0">
              <li>私有仓库勾选 <b>repo</b>（公开仓库仅需 <b>public_repo</b>）</li>
              <li>勾选 <b>admin:repo_hook</b>（用于系统自动配置 Push Webhook）</li>
            </ul>
          </div>
        </div>
      </NAlert>

      <NFormItem label="GitHub Personal Access Token" required>
        <NInput
          v-model:value="gitTokenInput"
          type="password"
          show-password-on="click"
          placeholder="github_pat_xxx 或 ghp_xxx"
        />
      </NFormItem>

      <NSpace justify="end">
        <NButton @click="showGitAuthModal = false">取消</NButton>
        <NButton type="primary" :loading="connectingGit" @click="submitGitToken">
          验证并连接
        </NButton>
      </NSpace>
    </NSpace>
  </NModal>

  <!-- 手动填写已有 GitHub App -->
  <NModal
    v-model:show="showAppManualModal"
    preset="card"
    title="手动配置 GitHub App"
    style="width: 620px; max-width: 94vw"
  >
    <NSpace vertical size="medium">
      <NAlert type="info" :show-icon="true">
        在 GitHub 上已有 App 时填写：App ID 与私钥在 App 设置页查看，Webhook Secret 在 App 的 Webhook 设置里。保存后请到 GitHub 把 App 安装到账号或组织，再回面板点「同步安装」。
      </NAlert>
      <NFormItem label="App ID" required>
        <NInput v-model:value="manualAppId" placeholder="如 123456" />
      </NFormItem>
      <NFormItem label="私钥 (PEM)" required>
        <NInput
          v-model:value="manualPrivateKey"
          type="textarea"
          :autosize="{ minRows: 4, maxRows: 10 }"
          placeholder="-----BEGIN RSA PRIVATE KEY-----"
        />
      </NFormItem>
      <NFormItem label="Webhook Secret" required>
        <NInput
          v-model:value="manualWebhookSecret"
          type="password"
          show-password-on="click"
          placeholder="GitHub App 的 Webhook Secret"
        />
      </NFormItem>
      <NSpace justify="end">
        <NButton @click="showAppManualModal = false">取消</NButton>
        <NButton type="primary" :loading="savingManualApp" @click="submitManualApp">保存配置</NButton>
      </NSpace>
    </NSpace>
  </NModal>
</template>

<style scoped lang="scss">
.muted { color: var(--text-secondary); font-size: 13px; }

.git-provider-row {
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 14px 16px;
  background: var(--bg-card-subtle);
}
</style>
