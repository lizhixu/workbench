<!-- OpsTunnelModal: one-click entry for on-demand ops tunnels
     (kubectl port-forward style). Shows the ready-to-copy watchmanctl
     command, CLI download links, and the caller's live tunnels.
     Used from AppDetail (app mode: fixed port from the app's port mapping)
     and HostNetworkPane (host mode: target port editable). -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCode,
  NEmpty,
  NIcon,
  NInputNumber,
  NModal,
  NSpin,
  NSpace,
  NTag,
  useMessage,
} from 'naive-ui'
import { CheckmarkCircleOutline, CopyOutline, DownloadOutline, RefreshOutline } from '@vicons/ionicons5'

import { closeMyTunnel, getTunnelClientInfo, listMyTunnels, type MyTunnel, type TunnelClientInfo } from '../../api/tunnels'
import { copyToClipboard } from '../../utils/clipboard'

interface Prefill {
  appId?: string
  appName?: string
  hostId?: string
  hostname?: string
  port?: number
}

const props = defineProps<{
  show: boolean
  prefill: Prefill
}>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void }>()

const message = useMessage()

const cliPlatforms: [key: string, label: string][] = [
  ['linux_amd64', 'Linux x86_64'],
  ['linux_arm64', 'Linux ARM64'],
  ['windows_amd64', 'Windows x86_64 (.exe)'],
  ['darwin_amd64', 'macOS Intel'],
  ['darwin_arm64', 'macOS Apple Silicon'],
]

const loading = ref(true)
const info = ref<TunnelClientInfo | null>(null)
const tunnels = ref<MyTunnel[]>([])
const tunnelsLoading = ref(false)
const closing = ref('')
const copiedLogin = ref(false)
const copiedTunnel = ref(false)

// Host mode lets the operator edit the target port in place (default comes
// from the clicked row's port mapping); app mode pins it to the app port.
const targetPort = ref(0)
const isAppMode = computed(() => !!props.prefill.appId)

const panelOrigin = computed(() => window.location.origin)
const loginCmd = computed(() => `watchmanctl login --server ${panelOrigin.value}`)
const tunnelCmd = computed(() => {
  if (isAppMode.value) {
    return `watchmanctl tunnel --app ${props.prefill.appId} --port ${targetPort.value}`
  }
  return `watchmanctl tunnel --host ${props.prefill.hostId} --target 127.0.0.1:${targetPort.value}`
})

async function refreshTunnels() {
  tunnelsLoading.value = true
  try {
    tunnels.value = await listMyTunnels()
  } catch {
    tunnels.value = []
  } finally {
    tunnelsLoading.value = false
  }
}

async function load() {
  loading.value = true
  try {
    info.value = await getTunnelClientInfo()
  } catch (e: any) {
    info.value = null
    message.error(e?.message || '获取隧道服务信息失败')
  } finally {
    loading.value = false
  }
  refreshTunnels()
}

watch(
  () => props.show,
  (v) => {
    if (v) {
      targetPort.value = props.prefill.port ?? 8080
      copiedLogin.value = false
      copiedTunnel.value = false
      load()
    }
  },
)

function fmtIdle(s: number) {
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`
}

async function copy(text: string, which: 'login' | 'tunnel') {
  const ok = await copyToClipboard(text)
  if (ok) {
    if (which === 'login') copiedLogin.value = true
    else copiedTunnel.value = true
    message.success('已复制命令')
  } else {
    message.error('复制失败，请手动选择复制')
  }
}

async function closeTunnel(connID: string) {
  closing.value = connID
  try {
    await closeMyTunnel(connID)
    message.success(`已关闭隧道 ${connID}`)
    await refreshTunnels()
  } catch (e: any) {
    message.error(e?.message || '关闭失败')
  } finally {
    closing.value = ''
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    title="运维隧道（按需端口转发）"
    style="width: 640px"
    @update:show="(v) => emit('update:show', v)"
  >
    <NSpin v-if="loading" />
    <template v-else>
      <NAlert v-if="!info?.enabled" type="warning" style="margin-bottom: 14px">
        服务端未启用运维隧道端口（-opgrpc，默认 :9443，强制 TLS）。以下命令先保存好，让管理员以
        <code>-opgrpc :9443 -tls-cert ... -tls-key ...</code> 重启控制端后即可使用。
      </NAlert>
      <NAlert v-else type="info" :show-icon="false" style="margin-bottom: 14px">
        数据面端点 <span class="mono">{{ info.endpoint }}</span>
        · 隧道只监听本机 127.0.0.1，进程退出即关闭，目标主机零开放端口。
      </NAlert>

      <!-- Step 1: install + login -->
      <p class="guide-text">1. 首次使用：下载 CLI 并登录面板</p>
      <div class="cli-links">
        <template v-for="[key, label] in cliPlatforms" :key="key">
          <a v-if="info?.cli?.[key]" :href="info.cli[key]" target="_blank" rel="noopener">
            <NButton size="tiny" quaternary>
              <template #icon><NIcon :component="DownloadOutline" /></template>
              {{ label }}
            </NButton>
          </a>
        </template>
      </div>
      <div class="code-container">
        <NCode :code="loginCmd" language="bash" word-wrap />
      </div>
      <NSpace justify="end" style="margin-top: 8px">
        <NButton size="small" :type="copiedLogin ? 'success' : 'default'" @click="copy(loginCmd, 'login')">
          <template #icon><NIcon :component="copiedLogin ? CheckmarkCircleOutline : CopyOutline" /></template>
          {{ copiedLogin ? '已复制' : '复制' }}
        </NButton>
      </NSpace>

      <!-- Step 2: the tunnel command -->
      <p class="guide-text" style="margin-top: 16px">
        2. 打开隧道
        <template v-if="isAppMode">（应用 {{ prefill.appName || prefill.appId }} 的端口 {{ prefill.port }}）</template>
        <template v-else>（主机 {{ prefill.hostname || prefill.hostId }}）</template>
      </p>
      <div v-if="!isAppMode" style="margin-bottom: 8px">
        <NSpace align="center">
          <span>目标端口</span>
          <NInputNumber
            v-model:value="targetPort"
            :min="1"
            :max="65535"
            size="small"
            style="width: 140px"
            placeholder="8080"
          />
          <span style="color: var(--n-text-color-3); font-size: 12px">127.0.0.1:{{ targetPort }}</span>
        </NSpace>
      </div>
      <div class="code-container">
        <NCode :code="tunnelCmd" language="bash" word-wrap />
      </div>
      <NSpace justify="end" style="margin-top: 8px">
        <NButton size="small" type="primary" :disabled="!targetPort" @click="copy(tunnelCmd, 'tunnel')">
          <template #icon><NIcon :component="copiedTunnel ? CheckmarkCircleOutline : CopyOutline" /></template>
          {{ copiedTunnel ? '已复制命令' : '复制命令' }}
        </NButton>
      </NSpace>

      <!-- Live tunnels owned by the caller -->
      <div class="mine-header">
        <span class="guide-text" style="margin: 0">我的活动隧道</span>
        <NButton size="tiny" quaternary :loading="tunnelsLoading" @click="refreshTunnels">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
      </div>
      <NEmpty
        v-if="!tunnels.length"
        :description="info && !info.enabled ? '列表不可用（隧道数据面未启用）' : '无活动隧道'"
        size="small"
        style="padding: 12px 0"
      />
      <div v-else class="mine-list">
        <div v-for="t in tunnels" :key="t.conn_id" class="mine-row">
          <NTag size="small" :bordered="false" class="mono">{{ t.conn_id.slice(0, 8) }}</NTag>
          <span class="mono mine-target">{{ t.target }}</span>
          <span class="mine-idle">空闲 {{ fmtIdle(t.idle_seconds) }}</span>
          <NButton size="tiny" type="error" secondary :loading="closing === t.conn_id" @click="closeTunnel(t.conn_id)">
            关闭
          </NButton>
        </div>
      </div>
    </template>
  </NModal>
</template>

<style scoped>
.guide-text {
  color: var(--n-text-color-2);
  font-size: 13px;
  margin: 0 0 8px;
}
.code-container {
  padding: 10px 12px;
  border-radius: 8px;
  background: var(--n-action-color);
}
.cli-links {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin-bottom: 8px;
}
.cli-links a {
  text-decoration: none;
}
.mine-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 18px 0 8px;
  border-top: 1px solid var(--n-border-color);
  padding-top: 14px;
}
.mine-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.mine-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.mine-target {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mine-idle {
  color: var(--n-text-color-3);
  font-size: 12px;
}
</style>
