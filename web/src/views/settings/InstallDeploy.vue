<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NIcon,
  NInput,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import { DownloadOutline, CubeOutline, LinkOutline } from '@vicons/ionicons5'
import { enroll } from '../../api/hosts'
import { SETTING_KEYS, getSystemSettings, saveSystemSettings } from '../../api/settings'
import { useAuthStore } from '../../stores/auth'
import { copyToClipboard } from '../../utils/clipboard'
import OsLogo from '../../components/common/OsLogo.vue'

// 从旧 Settings.vue 抽取：一键安装 Agent + 已编译二进制
defineOptions({ name: 'InstallDeploy' })

const message = useMessage()
const enrollToken = ref('')
// 令牌加载态：初始 true，首屏只显示菊花，不渲染「生成注册令牌」按钮与
// <TOKEN> 占位命令，避免令牌未加载完时闪现未生成态。
const tokenLoading = ref(true)
const osType = ref('linux')

// Agent 直连接入地址（仅管理员可见）：空表示跟随面板地址；面板域名走
// Cloudflare 等不转发 9090 的代理时填服务端直连地址；保存后立即向所有
// 在线 Agent 下发新地址，离线 Agent 下次重连时收到。
const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')
const grpcHost = ref('')
const grpcSaving = ref(false)
const grpcLoaded = ref(false)

const effectiveGrpc = computed(() => {
  const v = grpcHost.value.trim()
  if (v) {
    return v.includes(':') ? v : `${v}:9090`
  }
  if (typeof window !== 'undefined') {
    return `${window.location.hostname}:9090`
  }
  return 'localhost:9090'
})

async function loadGrpcHost() {
  if (!isAdmin.value) return
  try {
    const res = await getSystemSettings()
    grpcHost.value = String(res.data?.[SETTING_KEYS.agentGRPCHost] ?? '')
  } catch {
    // 加载失败不阻塞安装命令展示
  } finally {
    grpcLoaded.value = true
  }
}

async function saveGrpcHost() {
  grpcSaving.value = true
  try {
    const trimmed = grpcHost.value.trim()
    await saveSystemSettings({ [SETTING_KEYS.agentGRPCHost]: trimmed })
    if (trimmed) {
      message.success('已保存，已向在线 Agent 下发新地址，新安装的 Agent 也将使用该地址回连')
    } else {
      message.success('已保存：新安装的 Agent 将默认跟随面板地址回连（已迁走或已重定向的 Agent 保持原回连地址不变）')
    }
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    grpcSaving.value = false
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

async function genToken() {
  tokenLoading.value = true
  try {
    const res = await enroll()
    enrollToken.value = res.enroll_token
  } catch (e: any) {
    message.error(e.message)
  } finally {
    tokenLoading.value = false
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

onMounted(() => {
  genToken()
  loadGrpcHost()
})
</script>

<template>
  <NSpace vertical :size="16">
    <NCard :bordered="false">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">
          <NIcon style="vertical-align: middle; margin-right: 6px"><DownloadOutline /></NIcon>
          一键安装 Agent
        </span>
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
              <OsLogo os="linux" :size="16" class="btn-logo" />
              <span class="btn-text">Linux</span>
              <span class="btn-sub">x86 / arm</span>
            </button>
            <button
              type="button"
              class="os-segment-btn"
              :class="{ active: osType === 'windows' }"
              @click="osType = 'windows'"
            >
              <OsLogo os="windows" :size="14" class="btn-logo win-logo" />
              <span class="btn-text">Windows</span>
              <span class="btn-sub">x64</span>
            </button>
          </div>
        </div>
      </template>

      <NSpace vertical :size="16">
        <!-- Token display：加载中只显示菊花；加载失败才显示「生成注册令牌」按钮供重试 -->
        <NSpin v-if="tokenLoading" size="small" class="token-loading" />
        <div v-else-if="enrollToken" class="token-box">
          <span class="token-label">注册令牌:</span>
          <code class="token-value">{{ enrollToken }}</code>
          <NButton size="tiny" quaternary @click="genToken">重新生成</NButton>
        </div>
        <NButton v-else type="primary" @click="genToken">生成注册令牌</NButton>

        <!-- Install command：只有拿到真实令牌才渲染，加载中与加载失败时不展示 <TOKEN> 占位命令 -->
        <div v-if="enrollToken" class="cmd-section">
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

        <!-- Manual script URL：同安装命令，只有拿到真实令牌才渲染 -->
        <div v-if="enrollToken" class="manual-section">
          <span class="muted">或手动查看安装脚本: </span>
          <a :href="installScriptUrl" target="_blank" class="script-link">{{ installScriptUrl }}</a>
        </div>
      </NSpace>
    </NCard>

    <NCard v-if="isAdmin" :bordered="false">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">
          <NIcon style="vertical-align: middle; margin-right: 6px"><LinkOutline /></NIcon>
          Agent 直连接入地址
        </span>
      </template>
      <NSpin v-if="!grpcLoaded" size="small" class="token-loading" />
      <NSpace v-else vertical :size="12">
        <div class="muted">Agent 回连控制面的地址，新安装与已安装共用。留空则跟随面板地址（域名或访问 IP）+ :9090；面板域名走 Cloudflare 等不转发 9090 的代理时，在此填服务端直连地址。保存后立即向所有在线 Agent 下发新地址，Agent 先预检目标地址，通了才切换过去；离线 Agent 下次连回时自动收到。想把已迁走的 Agent 迁回来时，在此填回旧地址。重新保存同一地址可再次下发。</div>
        <NSpace :size="8" align="center">
          <NInput v-model:value="grpcHost" placeholder="例如 direct.example.com（留空跟随面板地址）" clearable style="width: 320px" />
          <NButton type="primary" :loading="grpcSaving" @click="saveGrpcHost">保存</NButton>
        </NSpace>
        <div class="muted">当前安装脚本将使用的地址：<code>{{ effectiveGrpc }}</code></div>
        <div class="muted">主机详情横幅「回连地址」可查看每台 Agent 当前实际拨号的地址。</div>
      </NSpace>
    </NCard>

    <NCard :bordered="false">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">
          <NIcon style="vertical-align: middle; margin-right: 6px"><CubeOutline /></NIcon>
          已编译 Agent 二进制
        </span>
      </template>
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
</template>

<style scoped lang="scss">
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

.token-loading { display: flex; padding: 12px 16px; }
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
</style>
