<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NDescriptions,
  NDescriptionsItem,
  NTag,
  useMessage,
  NInput,
  NSwitch,
  NFormItem,
  NSelect,
  NAlert,
  NIcon,
  NAvatar,
  NModal,
} from 'naive-ui'
import { CheckmarkCircleOutline, SparklesOutline, LogoGithub } from '@vicons/ionicons5'
import { health, enroll } from '../../api/hosts'
import { getAIConfig, setAIConfig, testAIConfig, type AIConfig } from '../../api/ai'
import {
  getGitProviders, connectGitHubToken, disconnectGitHub,
  listGitHubRepos, type GitProvidersStatus,
} from '../../api/git'
import { copyToClipboard } from '../../utils/clipboard'
import { useWorkspaceStore } from '../../stores/workspace'
import CommandPolicy from './CommandPolicy.vue'
import CommandLibrary from './CommandLibrary.vue'
import TerminalPrefs from './TerminalPrefs.vue'
import BackupRestore from './BackupRestore.vue'
import SystemUpgrade from './SystemUpgrade.vue'
import OsLogo from '../../components/common/OsLogo.vue'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'Settings' })

const message = useMessage()
const workspace = useWorkspaceStore()
const healthData = ref<any>(null)
const enrollToken = ref('')
const osType = ref('linux')

const aiConfig = ref<AIConfig>({
  base_url: 'https://api.deepseek.com',
  model: 'deepseek-chat',
  api_key: '',
  enabled: false,
  provider: 'deepseek',
})
const selectedProvider = ref('deepseek')
const savingAI = ref(false)
const testingAI = ref(false)
const testResult = ref<{ ok: boolean; message: string } | null>(null)

const providerPresets: Record<string, { label: string; base_url: string; model: string; key_tip: string }> = {
  deepseek: {
    label: 'DeepSeek (推荐)',
    base_url: 'https://api.deepseek.com',
    model: 'deepseek-chat',
    key_tip: '填入 DeepSeek API Key (sk-...)',
  },
  openai: {
    label: 'OpenAI',
    base_url: 'https://api.openai.com/v1',
    model: 'gpt-4o-mini',
    key_tip: '填入 OpenAI API Key (sk-...)',
  },
  ollama: {
    label: 'Ollama (本地私有部署)',
    base_url: 'http://localhost:11434',
    model: 'qwen2.5:14b',
    key_tip: '本地 Ollama 通常无需填 API Key',
  },
  siliconflow: {
    label: '硅基流动 (SiliconFlow)',
    base_url: 'https://api.siliconflow.cn/v1',
    model: 'deepseek-ai/DeepSeek-V3',
    key_tip: '填入 SiliconFlow API Key',
  },
  dashscope: {
    label: '阿里云百炼 (通义千问)',
    base_url: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    model: 'qwen-plus',
    key_tip: '填入 DashScope API Key',
  },
  custom: {
    label: '自定义 OpenAI 兼容接口 / vLLM / OneAPI',
    base_url: 'http://localhost:8000/v1',
    model: 'qwen2.5:14b',
    key_tip: '根据服务提供方要求填入 Key',
  },
}

const providerOptions = Object.entries(providerPresets).map(([k, v]) => ({
  label: v.label,
  value: k,
}))

function handleProviderChange(val: string) {
  selectedProvider.value = val
  const preset = providerPresets[val]
  if (preset) {
    aiConfig.value.base_url = preset.base_url
    aiConfig.value.model = preset.model
    aiConfig.value.provider = val
  }
}

const serverHost = computed(() => {
  if (typeof window !== 'undefined') {
    const port = window.location.port ? `:${window.location.port}` : ''
    return `${window.location.hostname}${port}`
  }
  return 'localhost'
})

const installCmd = computed(() => {
  const token = enrollToken.value || '<TOKEN>'
  const proto = typeof window !== 'undefined' ? window.location.protocol || 'http:' : 'http:'
  if (osType.value === 'windows') {
    return `irm '${proto}//${serverHost.value}/install?os_type=windows^&token=${token}' | iex`
  }
  return `curl -kfsSL '${proto}//${serverHost.value}/install?token=${token}' | sudo bash`
})

const installScriptUrl = computed(() => {
  const token = enrollToken.value || '<TOKEN>'
  const proto = typeof window !== 'undefined' ? window.location.protocol || 'http:' : 'http:'
  return `${proto}//${serverHost.value}/install?os_type=${osType.value}&token=${token}`
})

async function loadHealth() {
  try {
    healthData.value = await health()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function loadAIConfig() {
  try {
    const data = await getAIConfig()
    if (data) {
      aiConfig.value = data
      if (data.provider) {
        selectedProvider.value = data.provider
      } else if (data.base_url?.includes('deepseek')) {
        selectedProvider.value = 'deepseek'
      } else if (data.base_url?.includes(':11434')) {
        selectedProvider.value = 'ollama'
      } else if (data.base_url?.includes('openai.com')) {
        selectedProvider.value = 'openai'
      } else {
        selectedProvider.value = 'custom'
      }
    }
  } catch {
    // AI not configured — ignore
  }
}

async function saveAIConfig() {
  savingAI.value = true
  try {
    aiConfig.value.provider = selectedProvider.value
    await setAIConfig(aiConfig.value)
    message.success('AI 大模型配置已成功保存并持久化！')
    await loadAIConfig()
  } catch (e: any) {
    message.error(e.message || '保存 AI 配置失败')
  } finally {
    savingAI.value = false
  }
}

async function handleTestAI() {
  if (!aiConfig.value.base_url) {
    message.warning('请先填写 LLM 端点 URL')
    return
  }
  testingAI.value = true
  testResult.value = null
  try {
    const res = await testAIConfig(aiConfig.value)
    testResult.value = { ok: true, message: res.message }
    message.success('AI 接口连通性测试通过！')
  } catch (e: any) {
    testResult.value = { ok: false, message: e.message || '连通性测试失败' }
    message.error(e.message || 'AI 接口连通性测试失败')
  } finally {
    testingAI.value = false
  }
}

async function genToken() {
  try {
    const res = await enroll()
    enrollToken.value = res.enroll_token
  } catch (e: any) {
    message.error(e.message)
  }
}

async function copyCmd() {
  const ok = await copyToClipboard(installCmd.value)
  if (ok) {
    message.success('已复制安装命令')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

// ---------------- Git Providers (对齐 Dokploy: Connect your Git provider) ----------------
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
  workspace.openTab({
    key: '/settings',
    title: '系统设置',
    path: '/settings',
    closable: true,
  })
  loadHealth()
  genToken()
  loadAIConfig()
  loadGitStatus()
})
</script>

<template>
  <div class="settings-view">
    <div class="settings-toolbar">
      <h2 class="page-title">系统设置</h2>
    </div>
    <NSpace vertical :size="16">
    <!-- System Status -->
    <NCard title="系统状态" :bordered="false">
      <NDescriptions :column="3" label-placement="left" bordered v-if="healthData">
        <NDescriptionsItem label="状态">
          <NTag type="success" size="small">运行中</NTag>
        </NDescriptionsItem>
        <NDescriptionsItem label="在线主机">{{ healthData.agents }}</NDescriptionsItem>
        <NDescriptionsItem label="版本">{{ healthData.version }}</NDescriptionsItem>
      </NDescriptions>
    </NCard>

    <!-- AI Config (后台配置大模型) -->
    <NCard title="AI 大模型配置与助手" :bordered="false" size="small">
      <template #header-extra>
        <NTag :type="aiConfig.enabled ? 'success' : 'default'" size="small" round>
          {{ aiConfig.enabled ? '已启用 AI 功能' : '未启用' }}
        </NTag>
      </template>

      <NSpace vertical :size="14">
        <p class="muted">
          Watchman 支持集成各类大语言模型（DeepSeek、OpenAI、Ollama、vLLM 等）。启用后将在在线终端、命令执行排错、主机资源诊断中提供智能辅助。
        </p>

        <NFormItem label="服务提供商 / 预设方案">
          <NSelect
            v-model:value="selectedProvider"
            :options="providerOptions"
            @update:value="handleProviderChange"
          />
        </NFormItem>

        <NFormItem label="LLM 接口端点 (Base URL)" required>
          <NInput
            v-model:value="aiConfig.base_url"
            placeholder="例如: https://api.deepseek.com 或 http://localhost:11434"
          />
        </NFormItem>

        <NFormItem label="模型名称 (Model)" required>
          <NInput
            v-model:value="aiConfig.model"
            placeholder="例如: deepseek-chat 或 qwen2.5:14b"
          />
        </NFormItem>

        <NFormItem label="API Key (密钥)">
          <NInput
            v-model:value="aiConfig.api_key"
            type="password"
            show-password-on="click"
            :placeholder="providerPresets[selectedProvider]?.key_tip || '输入 API Key'"
          />
        </NFormItem>

        <NFormItem label="启用 AI 助手与诊断功能">
          <NSwitch v-model:value="aiConfig.enabled" />
        </NFormItem>

        <NAlert
          v-if="testResult"
          :type="testResult.ok ? 'success' : 'error'"
          :title="testResult.ok ? '连通性测试通过' : '测试失败'"
          closable
          @close="testResult = null"
        >
          {{ testResult.message }}
        </NAlert>

        <NSpace justify="space-between" align="center" style="margin-top: 8px">
          <NButton
            secondary
            type="info"
            :loading="testingAI"
            @click="handleTestAI"
          >
            <template #icon><NIcon :component="SparklesOutline" /></template>
            测试连接
          </NButton>

          <NButton
            type="primary"
            :loading="savingAI"
            @click="saveAIConfig"
          >
            <template #icon><NIcon :component="CheckmarkCircleOutline" /></template>
            保存配置
          </NButton>
        </NSpace>
      </NSpace>
    </NCard>

    <!-- Git Providers 代码源集成 (对齐 Dokploy: Connect your Git provider for authentication) -->
    <NCard title="代码源集成 (Git Providers)" :bordered="false" size="small">
      <template #header-extra>
        <span class="muted" style="font-size: 13px">Connect your Git provider for authentication.</span>
      </template>

      <NSpace vertical :size="14">
        <p class="muted">
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

    <!-- Saved-command library -->
    <CommandLibrary />

    <!-- Per-user terminal preferences (theme / shell / font) -->
    <TerminalPrefs />

    <!-- High-risk command control (P3) -->
    <CommandPolicy />

    <!-- Control-plane backup / restore (admin only) -->
    <BackupRestore />

    <!-- Version & System / Agent Upgrade Center -->
    <SystemUpgrade />

    <!-- One-line Install -->
    <NCard :bordered="false">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">一键安装 Agent</span>
      </template>
      <template #header-extra>
        <div class="os-selector-wrap">
          <span class="selector-label">目标系统:</span>
          <div class="os-segment-group">
            <button
              type="button"
              class="os-segment-btn"
              :class="{ active: osType === 'linux' }"
              @click="osType = 'linux'"
            >
              <OsLogo os="linux" :show-badge="false" :size="16" class="btn-logo" />
              <span class="btn-text">Linux</span>
              <span class="btn-sub">x86 / arm</span>
            </button>
            <button
              type="button"
              class="os-segment-btn"
              :class="{ active: osType === 'windows' }"
              @click="osType = 'windows'"
            >
              <OsLogo os="windows" :show-badge="false" :size="14" class="btn-logo win-logo" />
              <span class="btn-text">Windows</span>
              <span class="btn-sub">x64</span>
            </button>
          </div>
        </div>
      </template>

      <NSpace vertical :size="16">
        <!-- Token display -->
        <div v-if="enrollToken" class="token-box">
          <span class="token-label">注册令牌:</span>
          <code class="token-value">{{ enrollToken }}</code>
          <NButton size="tiny" quaternary @click="genToken">重新生成</NButton>
        </div>
        <NButton v-else type="primary" @click="genToken">生成注册令牌</NButton>

        <!-- Install command -->
        <div class="cmd-section">
          <div class="cmd-label">在被管主机上执行以下命令（一键安装）：</div>
          <div class="cmd-box">
            <code class="cmd-text">{{ installCmd }}</code>
            <NButton size="small" type="primary" class="copy-btn" @click="copyCmd">复制</NButton>
          </div>
        </div>

        <!-- What the script does -->
        <div class="info-section">
          <div class="info-title">该命令会自动完成：</div>
          <ul class="info-list">
            <li v-if="osType === 'linux'">
              下载对应架构的 agent 二进制（amd64 / arm64 自动检测）
            </li>
            <li v-if="osType === 'linux'">
              安装 systemd 服务，开机自启、自动重连
            </li>
            <li v-if="osType === 'windows'">
              下载 agent 到 <code>C:\Program Files\Watchman\</code>
            </li>
            <li v-if="osType === 'windows'">
              注册为计划任务，开机自启、自动重连
            </li>
            <li>Agent 主动反向回连控制端，被管主机无需开放入站端口</li>
            <li>注册成功后自动出现在主机列表中</li>
          </ul>
        </div>

        <!-- Manual script URL -->
        <div class="manual-section">
          <span class="muted">或手动查看安装脚本: </span>
          <a :href="installScriptUrl" target="_blank" class="script-link">{{ installScriptUrl }}</a>
        </div>
      </NSpace>
    </NCard>

    <!-- Binary list -->
    <NCard title="已编译 Agent 二进制" :bordered="false" size="small">
      <NSpace vertical :size="8">
        <div class="binary-row">
          <span class="binary-name">watchman-agent-linux-amd64</span>
          <NTag size="tiny">Linux x86_64</NTag>
          <a :href="`http://${serverHost}/api/v1/agent/binary?os=linux&arch=amd64`" target="_blank" class="dl-link">下载</a>
        </div>
        <div class="binary-row">
          <span class="binary-name">watchman-agent-linux-arm64</span>
          <NTag size="tiny">Linux ARM64</NTag>
          <a :href="`http://${serverHost}/api/v1/agent/binary?os=linux&arch=arm64`" target="_blank" class="dl-link">下载</a>
        </div>
        <div class="binary-row">
          <span class="binary-name">watchman-agent-windows-amd64.exe</span>
          <NTag size="tiny">Windows x86_64</NTag>
          <a :href="`http://${serverHost}/api/v1/agent/binary?os=windows&arch=amd64`" target="_blank" class="dl-link">下载</a>
        </div>
      </NSpace>
    </NCard>
  </NSpace>
  </div>
</template>

<style scoped lang="scss">
.settings-view {
  height: 100%;
  overflow-y: auto;
  box-sizing: border-box;
}

.settings-toolbar {
  margin-bottom: 16px;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 700;
  color: var(--text-primary);
}

.muted { color: var(--text-secondary); font-size: 13px; }
code { font-size: 12px; }

.os-selector-wrap {
  display: flex;
  align-items: center;
  gap: 10px;

  .selector-label {
    font-size: 13px;
    color: var(--text-secondary);
    font-weight: 500;
  }

  .os-segment-group {
    display: inline-flex;
    align-items: center;
    background-color: var(--code-box-bg, rgba(0, 0, 0, 0.2));
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 3px;
    gap: 4px;

    .os-segment-btn {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 5px 12px;
      border: 1px solid transparent;
      border-radius: 6px;
      background: transparent;
      color: var(--text-secondary);
      font-size: 13px;
      font-weight: 500;
      cursor: pointer;
      transition: all 0.2s cubic-bezier(0.4, 0, 0.2, 1);
      user-select: none;
      outline: none;

      .btn-logo {
        transition: transform 0.2s ease;
      }

      .btn-text {
        font-weight: 600;
      }

      .btn-sub {
        font-size: 10.5px;
        color: var(--text-tertiary, #9ca3af);
        padding: 0 4px;
        border-radius: 3px;
        background-color: rgba(255, 255, 255, 0.06);
        line-height: 1.4;
      }

      &:hover:not(.active) {
        color: var(--text-primary);
        background-color: rgba(255, 255, 255, 0.05);
      }

      &.active {
        background-color: var(--bg-card);
        color: #6366f1;
        border-color: rgba(99, 102, 241, 0.35);
        box-shadow: 0 2px 8px rgba(0, 0, 0, 0.24);

        .btn-logo {
          transform: scale(1.1);
        }

        .win-logo {
          color: #0078d6;
        }

        .btn-sub {
          color: #6366f1;
          background-color: rgba(99, 102, 241, 0.12);
        }
      }
    }
  }
}

.token-box {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 16px;
  background: var(--bg-card-subtle);
  border-radius: 6px;
  border: 1px solid var(--border-color);
}
.token-label { font-size: 13px; color: var(--text-secondary); flex-shrink: 0; }
.token-value {
  font-family: 'SFMono-Regular', Consolas, monospace;
  font-size: 13px;
  word-break: break-all;
  flex: 1;
  color: var(--text-primary);
}

.cmd-section { margin-top: 4px; }
.cmd-label { font-size: 14px; font-weight: 600; margin-bottom: 8px; color: var(--text-primary); }
.cmd-box {
  position: relative;
  background: var(--cmd-box-bg);
  border-radius: 6px;
  padding: 16px 80px 16px 16px;
  overflow-x: auto;
  border: 1px solid var(--border-color);
}
.cmd-text {
  color: var(--cmd-text-color);
  font-family: 'SFMono-Regular', Consolas, monospace;
  font-size: 13px;
  white-space: pre-wrap;
  word-break: break-all;
}
.copy-btn {
  position: absolute;
  top: 12px;
  right: 12px;
}

.info-section { margin-top: 4px; }
.info-title { font-size: 14px; font-weight: 600; margin-bottom: 8px; color: var(--text-primary); }
.info-list {
  list-style: none;
  padding: 0;
  margin: 0;
}
.info-list li {
  padding: 4px 0;
  font-size: 13px;
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--text-secondary);
}

.manual-section {
  padding-top: 12px;
  border-top: 1px solid var(--border-color);
  font-size: 13px;
}
.script-link {
  color: #3b82f6;
  text-decoration: none;
  word-break: break-all;
}
.script-link:hover { text-decoration: underline; }

.binary-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 0;
  border-bottom: 1px solid var(--border-color);
}
.binary-name {
  font-family: 'SFMono-Regular', Consolas, monospace;
  font-size: 13px;
  flex: 1;
  color: var(--text-primary);
}
.dl-link {
  color: #3b82f6;
  text-decoration: none;
  font-size: 13px;
}
.dl-link:hover { text-decoration: underline; }

.git-provider-row {
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 14px 16px;
  background: var(--bg-card-subtle);
}
</style>
