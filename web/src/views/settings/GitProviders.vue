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
  NTag,
  NModal,
  NSelect,
  NCode,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import { LogoGithub } from '@vicons/ionicons5'
import {
  getGitHubAppStatus,
  createGitHubAppManifest,
  exchangeGitHubAppManifest,
  saveGitHubAppConfig,
  disconnectGitHubApp,
  syncGitHubAppInstallations,
  type GitHubAppStatus,
} from '../../api/git'

defineOptions({ name: 'GitProviders' })

const message = useMessage()
const route = useRoute()
const router = useRouter()

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
    message.error(e.message || '加载 GitHub App 状态失败')
  } finally {
    loadingApp.value = false
  }
}

async function doCreateApp() {
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
      message.warning('面板未绑定域名，事件地址将使用本次请求地址（IP），GitHub 可能无法访问；请先在系统设置中绑定面板域名')
    }
    // 名称留空时服务端生成唯一值：回填输入框，万一 GitHub 创建页没打开
    // （弹窗拦截/网络异常），用户能看到实际用了什么名字再重试。
    appNameInput.value = res.name
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

function openInstallSettings(instId: number, accountType: string, accountLogin: string) {
  const url = accountType === 'Organization' && accountLogin
    ? `https://github.com/organizations/${encodeURIComponent(accountLogin)}/settings/installations/${instId}`
    : `https://github.com/settings/installations/${instId}`
  window.open(url, '_blank')
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
  loadAppStatus()
  handleAppCallback()
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><LogoGithub /></NIcon>
        GitHub 代码源集成（GitHub App）
      </span>
    </template>
    <template #header-extra>
      <NTag v-if="appStatus?.configured" type="success" size="small" round>已配置</NTag>
      <NTag v-else type="default" size="small" round>未配置</NTag>
    </template>

    <NSpace vertical :size="14">
      <p class="muted tip-hint">
        在个人账号或组织安装一次 GitHub App，应用中心即可直接读取私有与公开仓库、实时拉取分支与文件，并在推送到目标分支时零配置自动触发部署（替代易过期的经典 PAT 与逐仓 Webhook）。私有镜像或其它 Git 托管平台（Gitee / GitLab / 自建 Gitea）请在「凭据保险库」存放 <b>Git 访问令牌</b> 或 <b>镜像仓库凭据</b>。
      </p>

      <template v-if="appStatus?.configured">
        <div class="git-provider-row">
          <NSpace vertical :size="10" style="width: 100%">
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
              <div class="muted" style="font-size: 12px; margin-bottom: 6px">已安装授权：</div>
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
                    <NButton size="tiny" text type="primary" @click="openInstallSettings(inst.id, inst.account_type, inst.account_login)">
                      管理授权仓库
                    </NButton>
                  </NSpace>
                </div>
              </NSpace>
            </div>
            <NAlert v-else type="info" :show-icon="true" style="margin-top: 4px">
              App 已创建但尚未安装。点击下方「安装到账号或组织」选好仓库范围并授权，回到面板点击「同步安装」即可。
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
            断开后 GitHub App 推送事件不再触发自动部署，应用可继续使用手工触发或逐仓 / CI Webhook。
          </NPopconfirm>
        </NSpace>
      </template>

      <template v-else>
        <NSpace vertical :size="10">
          <NSpace align="center">
            <NInput
              v-model:value="appNameInput"
              placeholder="GitHub App 名称（留空自动生成）"
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
            <NButton type="primary" :loading="creatingApp" @click="doCreateApp">一键创建 GitHub App</NButton>
            <NButton secondary @click="showAppManualModal = true">手动填写已有 App</NButton>
          </NSpace>
          <p class="muted tip-hint" style="font-size: 12px">
            名称留空会自动生成全局唯一名称（<code>watchman-随机后缀</code>）。点击「一键创建」会跳转到 GitHub 确认创建（仅申请 Contents 只读、Metadata 只读及 push 事件），完成后自动跳回面板；随后点「安装到账号或组织」选择授权仓库范围即可。注意：面板需可被 GitHub 访问以接收推送事件。
          </p>
        </NSpace>
      </template>
    </NSpace>
  </NCard>

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
        <NInput v-model:value="manualAppId" :input-props="{ autocomplete: 'off' }" placeholder="如 123456" />
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
          :input-props="{ autocomplete: 'new-password' }"
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
