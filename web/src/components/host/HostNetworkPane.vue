<script setup lang="ts">
import { ref, onMounted } from 'vue'
import {
  NCard, NSpace, NTag, NButton, NInput, NSwitch, NAlert, NPopconfirm,
  NIcon, useMessage, NDescriptions, NDescriptionsItem, NSpin, NForm, NFormItem,
  NModal, NText,
} from 'naive-ui'
import {
  GitNetworkOutline, RefreshOutline, CheckmarkCircleOutline,
  PlayOutline, CloudUploadOutline, RadioOutline,
  CopyOutline,
} from '@vicons/ionicons5'
import {
  checkNetworkNode, installNetworkNode, joinNetworkNode, leaveNetworkNode,
  pingNetworkNode, getNetworkConfig, type NetworkNode, type NetworkConfig,
  type PingResult,
} from '../../api/network'
import { copyToClipboard } from '../../utils/clipboard'

const props = defineProps<{
  hostId: string
  hostname?: string
  os?: string
}>()

const message = useMessage()
const loading = ref(true)
const node = ref<NetworkNode | null>(null)
const config = ref<NetworkConfig | null>(null)

const checking = ref(false)
const installing = ref(false)
const joining = ref(false)
const leaving = ref(false)

// 加入网络弹窗
const showJoinModal = ref(false)
const joinForm = ref({
  auth_key: '',
  server_url: '',
  accept_routes: true,
  advertise_routes: '',
  advertise_exit_node: false,
  reset: false,
})

// Ping 测速弹窗
const showPingModal = ref(false)
const pingTarget = ref('100.64.0.1')
const pinging = ref(false)
const pingResult = ref<PingResult | null>(null)

async function copyText(text: string, label: string) {
  if (!text) return
  const ok = await copyToClipboard(text)
  if (ok) {
    message.success(`已复制${label}: ${text}`)
  } else {
    message.warning('复制失败')
  }
}

async function loadData() {
  loading.value = true
  try {
    const [cfg, st] = await Promise.all([
      getNetworkConfig().catch(() => null),
      checkNetworkNode(props.hostId).catch(() => null),
    ])
    config.value = cfg
    node.value = st
  } catch (e: any) {
    message.error(e.message || '加载组网状态失败')
  } finally {
    loading.value = false
  }
}

async function handleCheck() {
  checking.value = true
  try {
    const updated = await checkNetworkNode(props.hostId)
    node.value = updated
    if (updated.online) {
      message.success(`组网正常连接中 (${updated.ip})`)
    } else if (updated.installed) {
      message.info('已安装 Tailscale，尚未加入网络')
    } else {
      message.warning('未检测到 Tailscale 客户端')
    }
  } catch (e: any) {
    message.error(`探活失败: ${e.message}`)
  } finally {
    checking.value = false
  }
}

async function handleInstall() {
  installing.value = true
  try {
    const res = await installNetworkNode(props.hostId)
    message.success(res.message || '安装成功')
    await handleCheck()
  } catch (e: any) {
    message.error(`安装失败: ${e.message}`)
  } finally {
    installing.value = false
  }
}

function openJoinModal() {
  joinForm.value = {
    auth_key: config.value?.auth_key || '',
    server_url: config.value?.server_url || '',
    accept_routes: config.value?.accept_routes ?? true,
    advertise_routes: '',
    advertise_exit_node: config.value?.advertise_exit_node ?? false,
    reset: false,
  }
  showJoinModal.value = true
}

async function doJoin() {
  if (!joinForm.value.auth_key.trim()) {
    message.warning('请输入 Auth Key')
    return
  }
  joining.value = true
  try {
    const res = await joinNetworkNode(props.hostId, {
      auth_key: joinForm.value.auth_key.trim(),
      server_url: joinForm.value.server_url.trim(),
      accept_routes: joinForm.value.accept_routes,
      advertise_routes: joinForm.value.advertise_routes.trim(),
      advertise_exit_node: joinForm.value.advertise_exit_node,
      reset: joinForm.value.reset,
    })
    if (res.ip_changed) {
      message.warning(
        `注意：组网 IP 已从 ${res.previous_ip} 变为 ${res.ip}（本次启用了状态重置）`,
        { duration: 8000 },
      )
    } else {
      message.success(res.message || '成功加入虚拟局域网！')
    }
    showJoinModal.value = false
    await handleCheck()
  } catch (e: any) {
    message.error(`加入失败: ${e.message}`)
  } finally {
    joining.value = false
  }
}

async function handleLeave(action: 'down' | 'logout') {
  leaving.value = true
  try {
    const res = await leaveNetworkNode(props.hostId, action)
    message.success(res.message)
    await handleCheck()
  } catch (e: any) {
    message.error(`操作失败: ${e.message}`)
  } finally {
    leaving.value = false
  }
}

function openPing() {
  pingResult.value = null
  showPingModal.value = true
}

async function doPing() {
  if (!pingTarget.value.trim()) {
    message.warning('请输入要探测的目标虚拟 IP')
    return
  }
  pinging.value = true
  pingResult.value = null
  try {
    const res = await pingNetworkNode(props.hostId, pingTarget.value.trim(), 3)
    pingResult.value = res
  } catch (e: any) {
    message.error(`Ping 测速失败: ${e.message}`)
  } finally {
    pinging.value = false
  }
}

onMounted(loadData)
</script>

<template>
  <div class="host-network-pane">
    <NSpin v-if="loading" style="padding: 40px" />

    <div v-else class="network-content">
      <!-- 状态概览卡片 -->
      <NCard :bordered="false" class="overview-card">
        <div class="header-box">
          <div class="left-box">
            <div class="title-row">
              <NIcon size="20" color="#10b981"><GitNetworkOutline /></NIcon>
              <h3 class="pane-title">异地组网状态 (Tailscale / Headscale)</h3>
              <NTag v-if="node?.online" type="success" size="small" round :bordered="false">
                <template #icon><NIcon :component="CheckmarkCircleOutline" /></template>
                已建立 WireGuard 安全连接
              </NTag>
              <NTag v-else-if="node?.installed" type="warning" size="small" round :bordered="false">
                已部署客户端 · 未加入网络
              </NTag>
              <NTag v-else type="default" size="small" round :bordered="false">
                未部署 Tailscale 客户端
              </NTag>
            </div>
            <p class="pane-desc">
              无公网 IP 节点之间自动打洞加密直连，通过虚拟网段 (100.64.0.0/10) 实现跨机房、家庭内网免端口映射双向互通。
            </p>
          </div>

          <div class="action-box">
            <NSpace :size="8">
              <NButton v-if="!node?.installed" type="primary" :loading="installing" @click="handleInstall">
                <template #icon><NIcon :component="CloudUploadOutline" /></template>
                一键安装 Tailscale
              </NButton>

              <template v-else-if="!node?.online">
                <NButton type="primary" @click="openJoinModal">
                  <template #icon><NIcon :component="PlayOutline" /></template>
                  一键连接 Up
                </NButton>
                <NButton type="info" secondary @click="openPing">
                  <template #icon><NIcon :component="RadioOutline" /></template>
                  Ping 测速
                </NButton>
              </template>

              <template v-else>
                <NButton type="info" secondary @click="openPing">
                  <template #icon><NIcon :component="RadioOutline" /></template>
                  Ping 测速
                </NButton>

                <NPopconfirm @positive-click="() => handleLeave('down')">
                  <template #trigger>
                    <NButton type="warning" secondary :loading="leaving">
                      断开连接
                    </NButton>
                  </template>
                  确认暂时断开本机的虚拟局域网连接 (tailscale down) 吗？
                </NPopconfirm>

                <NPopconfirm @positive-click="() => handleLeave('logout')">
                  <template #trigger>
                    <NButton type="error" quaternary :loading="leaving">
                      注销节点
                    </NButton>
                  </template>
                  确认注销本机节点并清除授权凭证吗？
                </NPopconfirm>
              </template>

              <NButton quaternary :loading="checking" @click="handleCheck">
                <template #icon><NIcon :component="RefreshOutline" /></template>
                探测状态
              </NButton>
            </NSpace>
          </div>
        </div>

        <NDescriptions :columns="3" label-placement="left" class="info-grid">
          <NDescriptionsItem label="虚拟 IPv4">
            <span v-if="node?.ip" class="mono-ip is-clickable" title="点击复制虚拟 IP" @click="copyText(node.ip, '虚拟 IP')">
              {{ node.ip }}
              <NIcon size="13"><CopyOutline /></NIcon>
            </span>
            <span v-else class="text-muted">未分配</span>
          </NDescriptionsItem>

          <NDescriptionsItem label="虚拟 IPv6">
            <span v-if="node?.ipv6" class="mono-text">{{ node.ipv6 }}</span>
            <span v-else class="text-muted">-</span>
          </NDescriptionsItem>

          <NDescriptionsItem label="节点内部域名">
            <span v-if="node?.node_name" class="mono-text">{{ node.node_name }}</span>
            <span v-else class="text-muted">-</span>
          </NDescriptionsItem>

          <NDescriptionsItem label="Tailscale 版本">
            <span v-if="node?.version">v{{ node.version }}</span>
            <span v-else class="text-muted">未安装</span>
          </NDescriptionsItem>

          <NDescriptionsItem label="打洞直连状态">
            <span v-if="!node?.online" class="text-muted">-</span>
            <span v-else-if="node?.direct" class="text-success font-semibold">P2P 直连</span>
            <span v-else class="text-info">{{ node?.derp || 'DERP 转发' }}</span>
          </NDescriptionsItem>

          <NDescriptionsItem label="控制面地址">
            <span v-if="config?.server_url" class="mono-text">{{ config.server_url }}</span>
            <span v-else class="text-muted">Tailscale 官方控制面</span>
          </NDescriptionsItem>
        </NDescriptions>

        <NAlert v-if="node?.error_message && !node.online" type="warning" class="error-alert">
          {{ node.error_message }}
        </NAlert>
      </NCard>
    </div>

    <!-- 加入网络 Modal -->
    <NModal
      v-model:show="showJoinModal"
      preset="card"
      title="加入异地虚拟局域网"
      style="width: 520px; max-width: 90vw"
    >
      <NForm label-placement="top">
        <NFormItem label="Auth Key (预授权入网密钥)" required>
          <NInput
            v-model:value="joinForm.auth_key"
            type="password"
            show-password-on="click"
            placeholder="输入 Auth Key (已默认读取控制面预设密钥)"
          />
        </NFormItem>

        <NFormItem label="Login Server (控制端接入地址)">
          <NInput
            v-model:value="joinForm.server_url"
            placeholder="留空默认使用全局预设地址"
          />
        </NFormItem>

        <NFormItem label="自动接受其他节点广播的局域网路由 (--accept-routes)">
          <NSwitch v-model:value="joinForm.accept_routes" />
        </NFormItem>

        <NFormItem label="广播本机局域网子网 (可选，例如: 192.168.1.0/24)">
          <NInput
            v-model:value="joinForm.advertise_routes"
            placeholder="留空表示不广播子网路由"
          />
        </NFormItem>

        <NFormItem>
          <template #label>
            重置节点状态 (--reset)
            <NText depth="3" style="font-weight: normal; margin-left: 6px; font-size: 12px">
              默认关闭。开启后会丢弃原节点身份重新注册，组网 IP 会发生变化
            </NText>
          </template>
          <NSwitch v-model:value="joinForm.reset" />
        </NFormItem>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="showJoinModal = false">取消</NButton>
          <NButton type="primary" :loading="joining" @click="doJoin">
            确认连接 Up
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- Ping 测速 Modal -->
    <NModal
      v-model:show="showPingModal"
      preset="card"
      title="WireGuard 链路 Ping 测速"
      style="width: 540px; max-width: 90vw"
    >
      <NForm label-placement="top">
        <NFormItem label="目标节点虚拟 IP (100.x.x.x) 或节点名" required>
          <NInput
            v-model:value="pingTarget"
            placeholder="例如: 100.64.0.2"
          />
        </NFormItem>

        <NButton
          type="primary"
          :loading="pinging"
          style="width: 100%; margin-bottom: 12px"
          @click="doPing"
        >
          <template #icon><NIcon :component="RadioOutline" /></template>
          开始打洞链路探测 (tailscale ping)
        </NButton>

        <div v-if="pingResult" class="ping-box">
          <div class="result-bar">
            <NTag :type="pingResult.ok ? 'success' : 'error'" size="small">
              {{ pingResult.ok ? (pingResult.direct ? '连通 (P2P 直连)' : '连通 (DERP 中继)') : '探测超时或失败' }}
            </NTag>
            <span v-if="pingResult.latency_ms > 0" class="latency-lbl">
              往返耗时: {{ pingResult.latency_ms }} ms
            </span>
            <NTag v-if="pingResult.derp" size="small" type="info">
              链路: {{ pingResult.derp }}
            </NTag>
          </div>
          <NAlert
            v-if="pingResult.hint || pingResult.error"
            :type="pingResult.ok ? 'warning' : 'error'"
            size="small"
            style="margin-bottom: 8px"
            :show-icon="true"
          >
            {{ pingResult.hint }}
            <template v-if="pingResult.error">
              <br />{{ pingResult.error }}
            </template>
          </NAlert>
          <pre class="ping-raw">{{ pingResult.output || '(命令无输出)' }}</pre>
        </div>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="showPingModal = false">关闭</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.host-network-pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;

  .network-content {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .overview-card {
    background: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 16px;

    .header-box {
      display: flex;
      justify-content: space-between;
      align-items: flex-start;
      margin-bottom: 16px;
      gap: 16px;
      flex-wrap: wrap;

      .title-row {
        display: flex;
        align-items: center;
        gap: 10px;

        .pane-title {
          margin: 0;
          font-size: 16px;
          font-weight: 700;
        }
      }

      .pane-desc {
        margin: 6px 0 0;
        font-size: 12px;
        color: var(--n-text-color-3);
        max-width: 600px;
        line-height: 1.5;
      }
    }

    .info-grid {
      margin-top: 12px;
      background: var(--n-color-modal);
      padding: 12px 16px;
      border-radius: 6px;
      border: 1px solid var(--n-border-color);
    }

    .error-alert {
      margin-top: 12px;
    }
  }
}

.mono-ip {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-weight: 600;
  color: #10b981;
  display: inline-flex;
  align-items: center;
  gap: 4px;

  &.is-clickable {
    cursor: pointer;
    border-radius: 4px;
    padding: 1px 4px;
    margin-left: -4px;
    transition: all 0.15s;

    &:hover {
      background: rgba(16, 185, 129, 0.1);
    }
  }
}

.mono-text {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.text-muted {
  color: var(--n-text-color-3);
  font-size: 12px;
}

.text-success {
  color: #10b981;
}

.text-info {
  color: #3b82f6;
}

.ping-box {
  background: var(--n-color-modal);
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  padding: 10px;

  .result-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;

    .latency-lbl {
      font-size: 12px;
      font-weight: 600;
      color: #10b981;
    }
  }

  .ping-raw {
    margin: 0;
    font-family: monospace;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 160px;
    overflow-y: auto;
    color: var(--n-text-color-2);
  }
}
</style>
