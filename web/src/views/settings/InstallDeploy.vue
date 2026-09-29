<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NIcon,
  NTag,
  useMessage,
} from 'naive-ui'
import { DownloadOutline, CubeOutline } from '@vicons/ionicons5'
import { enroll } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import OsLogo from '../../components/common/OsLogo.vue'

// 从旧 Settings.vue 抽取：一键安装 Agent + 已编译二进制
defineOptions({ name: 'InstallDeploy' })

const message = useMessage()
const enrollToken = ref('')
const osType = ref('linux')

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

onMounted(() => {
  genToken()
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

    <NCard :bordered="false" size="small">
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
