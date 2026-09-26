<script setup lang="ts">
import { ref, onMounted } from 'vue'
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
  useMessage,
} from 'naive-ui'
import { LogoGithub } from '@vicons/ionicons5'
import {
  getGitProviders, connectGitHubToken, disconnectGitHub,
  listGitHubRepos, type GitProvidersStatus,
} from '../../api/git'

// 从旧 Settings.vue 抽取：代码源集成 (Git Providers) + 连接弹窗
defineOptions({ name: 'GitProviders' })

const message = useMessage()

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

onMounted(() => {
  loadGitStatus()
})
</script>

<template>
  <NCard title="代码源集成 (Git Providers)" :bordered="false" size="small">
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
