<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import {
  NButton, NSpace, NTag, NModal, NForm, NFormItem,
  NInput, NSelect, NSwitch, useMessage, NIcon, NAlert, NPopconfirm, NText,
  NSpin, NEmpty,
} from 'naive-ui'
import {
  RefreshOutline,
  CheckmarkCircleOutline, CloseCircleOutline, PlayOutline,
  CloudUploadOutline,
  SettingsOutline, CopyOutline, SearchOutline, RadioOutline,
} from '@vicons/ionicons5'
import {
  getNetworkConfig, updateNetworkConfig, listNetworkNodes,
  checkNetworkNode, installNetworkNode, joinNetworkNode, leaveNetworkNode,
  pingNetworkNode, type NetworkConfig, type NetworkNode, type PingResult,
} from '../../api/network'
import { copyToClipboard } from '../../utils/clipboard'

defineOptions({ name: 'NetworkList' })

const message = useMessage()
const nodes = ref<NetworkNode[]>([])
const loading = ref(false)
const searchKeyword = ref('')
const statusFilter = ref<'all' | 'connected' | 'not_joined' | 'not_installed'>('all')

// 全局组网配置
const config = ref<NetworkConfig>({
  control_plane: 'headscale',
  server_url: '',
  auth_key: '',
  accept_routes: true,
  advertise_exit_node: false,
})
const showConfigModal = ref(false)
const savingConfig = ref(false)
// 后端 GET 不再回显密钥，只告诉是否已配置
const authKeySet = ref(false)

// 节点加入网络弹窗
const showJoinModal = ref(false)
const targetNode = ref<NetworkNode | null>(null)
const joinForm = ref({
  auth_key: '',
  server_url: '',
  accept_routes: true,
  advertise_routes: '',
  advertise_exit_node: false,
  // 默认关闭：--reset 会丢弃节点状态并以新节点身份重新注册，
  // Headscale 会重新分配 100.x.y.z，导致组网 IP 变化。
  reset: false,
})
const joining = ref(false)

// Ping 连通性测速弹窗
const showPingModal = ref(false)
const pingSourceNode = ref<NetworkNode | null>(null)
const pingTarget = ref('')
const pinging = ref(false)
const pingResult = ref<PingResult | null>(null)

// 正在执行操作的主机 ID
const checkingIds = ref<Record<string, boolean>>({})
const installingIds = ref<Record<string, boolean>>({})
const leavingIds = ref<Record<string, boolean>>({})

// 拓扑统计
const stats = computed(() => {
  const total = nodes.value.length
  const connected = nodes.value.filter((n) => n.online).length
  const installed = nodes.value.filter((n) => n.installed).length
  return { total, connected, installed }
})

// 过滤后的数据列表
const filteredNodes = computed(() => {
  let list = nodes.value
  if (statusFilter.value === 'connected') {
    list = list.filter((n) => n.online)
  } else if (statusFilter.value === 'not_joined') {
    list = list.filter((n) => n.installed && !n.online)
  } else if (statusFilter.value === 'not_installed') {
    list = list.filter((n) => !n.installed)
  }

  const q = searchKeyword.value.trim().toLowerCase()
  if (!q) return list
  return list.filter((n) =>
    (n.hostname && n.hostname.toLowerCase().includes(q)) ||
    (n.ip && n.ip.includes(q)) ||
    (n.internal_ip && n.internal_ip.includes(q)) ||
    (n.node_name && n.node_name.toLowerCase().includes(q))
  )
})

async function copyText(text: string, label: string) {
  if (!text) return
  const ok = await copyToClipboard(text)
  if (ok) {
    message.success(`已复制${label}: ${text}`)
  } else {
    message.warning('复制失败，请手动复制')
  }
}

async function loadData() {
  loading.value = true
  try {
    const [cfg, list] = await Promise.all([
      getNetworkConfig().catch(() => ({
        control_plane: 'headscale' as const,
        server_url: '',
        auth_key: '',
        accept_routes: true,
        advertise_exit_node: false,
      })),
      listNetworkNodes().catch(() => []),
    ])
    config.value = cfg as NetworkConfig
    authKeySet.value = !!(cfg as NetworkConfig).auth_key_set
    nodes.value = list as NetworkNode[]
  } catch (e: any) {
    message.error(e.message || '获取网络拓扑失败')
  } finally {
    loading.value = false
  }
}

async function saveConfig() {
  savingConfig.value = true
  try {
    const saved = await updateNetworkConfig(config.value)
    authKeySet.value = !!saved.auth_key_set
    // 密钥只写不读：保存后清空本地输入框，不在内存里留存
    config.value.auth_key = ''
    message.success('组网配置已保存')
    showConfigModal.value = false
  } catch (e: any) {
    message.error(e.message || '保存组网配置失败')
  } finally {
    savingConfig.value = false
  }
}

async function handleCheck(node: NetworkNode) {
  checkingIds.value[node.host_id] = true
  try {
    const updated = await checkNetworkNode(node.host_id)
    const idx = nodes.value.findIndex((n) => n.host_id === node.host_id)
    if (idx !== -1) {
      nodes.value[idx] = { ...nodes.value[idx], ...updated }
    }
    if (updated.online) {
      message.success(`[${node.hostname}] 组网正常连接中 (${updated.ip})`)
    } else if (updated.installed) {
      message.info(`[${node.hostname}] 已安装 Tailscale，尚未加入网络`)
    } else {
      message.warning(`[${node.hostname}] 未检测到 Tailscale 客户端`)
    }
  } catch (e: any) {
    message.error(`探活失败: ${e.message}`)
  } finally {
    checkingIds.value[node.host_id] = false
  }
}

async function handleInstall(node: NetworkNode) {
  installingIds.value[node.host_id] = true
  try {
    const res = await installNetworkNode(node.host_id)
    message.success(`[${node.hostname}] ${res.message}`)
    await handleCheck(node)
  } catch (e: any) {
    message.error(`安装失败: ${e.message || '执行出错'}`)
  } finally {
    installingIds.value[node.host_id] = false
  }
}

function openJoinModal(node: NetworkNode) {
  targetNode.value = node
  joinForm.value = {
    // 后端不再回显全局密钥；留空则服务端用已存储的预设密钥
    auth_key: '',
    server_url: config.value.server_url || '',
    accept_routes: config.value.accept_routes,
    advertise_routes: '',
    advertise_exit_node: config.value.advertise_exit_node,
    reset: false,
  }
  showJoinModal.value = true
}

async function doJoin() {
  if (!targetNode.value) return
  joining.value = true
  try {
    const res = await joinNetworkNode(targetNode.value.host_id, {
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
      message.success(res.message || '成功加入虚拟网络！')
    }
    showJoinModal.value = false
    await handleCheck(targetNode.value)
  } catch (e: any) {
    message.error(`加入失败: ${e.message}`)
  } finally {
    joining.value = false
  }
}

async function handleLeave(node: NetworkNode, action: 'down' | 'logout') {
  leavingIds.value[node.host_id] = true
  try {
    const res = await leaveNetworkNode(node.host_id, action)
    message.success(res.message)
    await handleCheck(node)
  } catch (e: any) {
    message.error(`退出组网失败: ${e.message}`)
  } finally {
    leavingIds.value[node.host_id] = false
  }
}

function openPingModal(node: NetworkNode) {
  pingSourceNode.value = node
  // 智能预选一个其他已在线的节点 IP
  const other = nodes.value.find((n) => n.host_id !== node.host_id && n.ip)
  pingTarget.value = other ? other.ip : ''
  pingResult.value = null
  showPingModal.value = true
}

async function doPing() {
  if (!pingSourceNode.value || !pingTarget.value.trim()) {
    message.warning('请指定要 Ping 的目标虚拟 IP 或节点名')
    return
  }
  pinging.value = true
  pingResult.value = null
  try {
    const res = await pingNetworkNode(pingSourceNode.value.host_id, pingTarget.value.trim(), 3)
    pingResult.value = res
  } catch (e: any) {
    message.error(`Ping 执行失败: ${e.message}`)
  } finally {
    pinging.value = false
  }
}

onMounted(loadData)
</script>

<template>
  <div class="network-view page-flex-column">
    <!-- Header: 标题 + 统计指标 + 控制面配置 -->
    <div class="network-header">
      <div class="header-left">
        <div class="title-wrap">
          <h2 class="page-title">异地组网</h2>
          <NTag size="small" :bordered="false" round type="success">
            {{ config.control_plane === 'headscale' ? 'Headscale 自托管' : 'Tailscale 官方' }}
          </NTag>
          <NTag v-if="config.server_url" size="small" :bordered="false" round type="info">
            {{ config.server_url }}
          </NTag>
        </div>
      </div>

      <div class="header-right">
        <NSpace align="center" :size="12">
          <div class="stat-group">
            <div class="stat-item">
              <span class="stat-num">{{ stats.total }}</span>
              <span class="stat-lbl">全部主机</span>
            </div>
            <div class="stat-sep" />
            <div class="stat-item">
              <span class="stat-num text-success">{{ stats.connected }}</span>
              <span class="stat-lbl">已组网在线</span>
            </div>
            <div class="stat-sep" />
            <div class="stat-item">
              <span class="stat-num text-info">{{ stats.installed }}</span>
              <span class="stat-lbl">已安装客户端</span>
            </div>
          </div>

          <NButton secondary type="primary" @click="showConfigModal = true">
            <template #icon><NIcon :component="SettingsOutline" /></template>
            控制面设置
          </NButton>
          <NButton :loading="loading" @click="loadData">
            <template #icon><NIcon :component="RefreshOutline" /></template>
            刷新拓扑
          </NButton>
        </NSpace>
      </div>
    </div>

    <!-- 筛选条 -->
    <div class="network-toolbar">
      <NSpace align="center" :size="12">
        <NInput
          v-model:value="searchKeyword"
          placeholder="搜索主机名、物理 IP 或虚拟组网 IP..."
          clearable
          style="width: 280px"
        >
          <template #prefix><NIcon :component="SearchOutline" /></template>
        </NInput>

        <div class="filter-pills">
          <button
            class="pill-btn"
            :class="{ active: statusFilter === 'all' }"
            @click="statusFilter = 'all'"
          >
            全部节点 ({{ stats.total }})
          </button>
          <button
            class="pill-btn"
            :class="{ active: statusFilter === 'connected' }"
            @click="statusFilter = 'connected'"
          >
            已连接 ({{ stats.connected }})
          </button>
          <button
            class="pill-btn"
            :class="{ active: statusFilter === 'not_joined' }"
            @click="statusFilter = 'not_joined'"
          >
            未加入 ({{ stats.installed - stats.connected }})
          </button>
          <button
            class="pill-btn"
            :class="{ active: statusFilter === 'not_installed' }"
            @click="statusFilter = 'not_installed'"
          >
            未安装 ({{ stats.total - stats.installed }})
          </button>
        </div>
      </NSpace>
    </div>

    <!-- 采用与主机列表完全一致的卡片行列表 (清晰、不压缩、不挤压) -->
    <div class="network-card-wrap">
      <NSpin v-if="loading && nodes.length === 0" style="padding: 60px" />

      <div v-else-if="filteredNodes.length === 0" class="empty-box">
        <NEmpty description="未找到匹配的主机节点" />
      </div>

      <div v-else class="node-list">
        <div
          v-for="row in filteredNodes"
          :key="row.host_id"
          class="node-row"
        >
          <!-- 1. 纯色扁平主机状态圆点徽标 (去除彩色背景大 Logo) -->
          <div class="col-logo">
            <div class="host-avatar-box">
              <span class="status-dot-indicator" :class="{ 'is-online': row.agent_online, 'is-offline': !row.agent_online }"></span>
              <span class="host-avatar-text">{{ (row.hostname || 'H').slice(0, 2).toUpperCase() }}</span>
            </div>
          </div>

          <!-- 2. 主机名与物理 IP -->
          <div class="col-host">
            <div class="host-title-row">
              <span class="host-name">{{ row.hostname || row.host_id.slice(0, 8) }}</span>
              <NTag v-if="row.agent_online" size="tiny" :bordered="false" type="success">在线</NTag>
              <NTag v-else size="tiny" :bordered="false" type="error">离线</NTag>
            </div>
            <div class="host-phy-ip" title="物理网卡 IP">
              物理 IP: {{ row.internal_ip || row.public_ip || '-' }}
            </div>
          </div>

          <!-- 3. 组网状态 -->
          <div class="col-status">
            <div v-if="!row.agent_online" class="status-pill is-offline">
              主机离线
            </div>
            <div v-else-if="row.online" class="status-pill is-connected">
              <NIcon size="15" :component="CheckmarkCircleOutline" style="margin-right: 4px;" />
              已连接 WireGuard
            </div>
            <div v-else-if="row.installed" class="status-pill is-unjoined">
              <NIcon size="15" :component="CloseCircleOutline" style="margin-right: 4px;" />
              未加入网络
            </div>
            <div v-else class="status-pill is-not-installed">
              未安装客户端
            </div>
          </div>

          <!-- 4. 虚拟 IP -->
          <div class="col-ip">
            <div class="field-label">虚拟组网 IP (100.x.x.x)</div>
            <div
              v-if="row.ip"
              class="virtual-ip-tag"
              title="点击复制虚拟 IP"
              @click="copyText(row.ip, '虚拟 IP')"
            >
              <NIcon size="14" :component="CopyOutline" />
              <span>{{ row.ip }}</span>
            </div>
            <div v-else class="text-muted">-</div>
          </div>

          <!-- 5. 节点域名与客户端版本 -->
          <div class="col-node">
            <div class="field-label">节点内部域名 / 版本</div>
            <div v-if="row.installed" class="node-fqdn-box">
              <span class="fqdn-text">{{ row.node_name || '-' }}</span>
              <span class="ver-text">{{ row.version ? `v${row.version}` : '' }}</span>
            </div>
            <div v-else class="text-muted">未部署客户端</div>
          </div>

          <!-- 6. 通道直连状态 -->
          <div class="col-derp">
            <div class="field-label">打洞通道</div>
            <div v-if="row.online">
              <NTag v-if="row.direct" size="small" type="success" :bordered="false">P2P 直连</NTag>
              <NTag v-else size="small" type="info" :bordered="false">{{ row.derp || 'DERP 转发' }}</NTag>
            </div>
            <div v-else class="text-muted">-</div>
          </div>

          <!-- 7. 操作按钮 -->
          <div class="col-actions">
            <!-- 未安装 -->
            <template v-if="!row.installed">
              <NButton
                size="small"
                type="primary"
                secondary
                :loading="installingIds[row.host_id]"
                :disabled="!row.agent_online"
                @click="handleInstall(row)"
              >
                <template #icon><NIcon :component="CloudUploadOutline" /></template>
                安装 Tailscale
              </NButton>
              <NButton
                size="small"
                quaternary
                :loading="checkingIds[row.host_id]"
                :disabled="!row.agent_online"
                @click="handleCheck(row)"
              >
                <template #icon><NIcon :component="RefreshOutline" /></template>
              </NButton>
            </template>

            <!-- 已安装但未连接 -->
            <template v-else-if="!row.online">
              <NButton
                size="small"
                type="primary"
                :disabled="!row.agent_online"
                @click="openJoinModal(row)"
              >
                <template #icon><NIcon :component="PlayOutline" /></template>
                加入组网
              </NButton>
              <NButton
                size="small"
                type="info"
                secondary
                :disabled="!row.agent_online"
                @click="openPingModal(row)"
              >
                <template #icon><NIcon :component="RadioOutline" /></template>
                Ping 测速
              </NButton>
              <NButton
                size="small"
                quaternary
                :loading="checkingIds[row.host_id]"
                :disabled="!row.agent_online"
                @click="handleCheck(row)"
              >
                <template #icon><NIcon :component="RefreshOutline" /></template>
                探测
              </NButton>
            </template>

            <!-- 已在线已连接 -->
            <template v-else>
              <NButton
                size="small"
                type="info"
                secondary
                @click="openPingModal(row)"
              >
                <template #icon><NIcon :component="RadioOutline" /></template>
                Ping 测速
              </NButton>

              <NPopconfirm @positive-click="() => handleLeave(row, 'down')">
                <template #trigger>
                  <NButton size="small" type="warning" secondary :loading="leavingIds[row.host_id]">
                    断开
                  </NButton>
                </template>
                确认将主机 [{{ row.hostname }}] 从虚拟局域网断开吗？
              </NPopconfirm>

              <NButton
                size="small"
                quaternary
                :loading="checkingIds[row.host_id]"
                @click="handleCheck(row)"
              >
                <template #icon><NIcon :component="RefreshOutline" /></template>
              </NButton>
            </template>
          </div>
        </div>
      </div>
    </div>

    <!-- 弹窗 1: 控制面设置 -->
    <NModal
      v-model:show="showConfigModal"
      preset="card"
      title="组网控制面配置 (Headscale / Tailscale)"
      style="width: 540px; max-width: 90vw"
    >
      <NForm label-placement="top">
        <NFormItem label="控制面类型" required>
          <NSelect
            v-model:value="config.control_plane"
            :options="[
              { label: '自建 Headscale (完全自控自托管)', value: 'headscale' },
              { label: 'Tailscale 官方控制面 (login.tailscale.com)', value: 'tailscale' },
            ]"
          />
        </NFormItem>

        <NFormItem
          v-if="config.control_plane === 'headscale'"
          label="Headscale 服务端地址 (Login Server URL)"
          required
        >
          <NInput
            v-model:value="config.server_url"
            placeholder="例如: http://192.255.178.173:18090"
          />
        </NFormItem>

        <NFormItem label="预设 Auth Key (节点一键入网密钥)">
          <NInput
            v-model:value="config.auth_key"
            type="password"
            show-password-on="click"
            :placeholder="authKeySet ? '已设置，留空保持不变，输入新值则替换' : '例如: hskey-auth-xxxxxxx'"
          />
        </NFormItem>

        <NFormItem label="自动接受子网路由 (--accept-routes)">
          <NSwitch v-model:value="config.accept_routes" />
        </NFormItem>

        <NFormItem label="允许作为公网出口网关 (--advertise-exit-node)">
          <NSwitch v-model:value="config.advertise_exit_node" />
        </NFormItem>

        <NAlert type="info" :bordered="false">
          提示：配置保存后，所有主机在执行「加入组网」时将默认使用上述控制端地址与预设密钥。
        </NAlert>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="showConfigModal = false">取消</NButton>
          <NButton type="primary" :loading="savingConfig" @click="saveConfig">
            保存配置
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 弹窗 2: 加入网络 -->
    <NModal
      v-model:show="showJoinModal"
      preset="card"
      :title="`加入异地组网: ${targetNode?.hostname || ''}`"
      style="width: 520px; max-width: 90vw"
    >
      <NForm v-if="targetNode" label-placement="top">
        <NFormItem label="Auth Key (入网预授权密钥)">
          <NInput
            v-model:value="joinForm.auth_key"
            type="password"
            show-password-on="click"
            placeholder="留空则使用全局预设密钥"
          />
        </NFormItem>

        <NFormItem
          v-if="config.control_plane === 'headscale'"
          label="Login Server (Headscale 节点接入地址)"
        >
          <NInput
            v-model:value="joinForm.server_url"
            placeholder="留空则复用全局配置地址"
          />
        </NFormItem>

        <NFormItem label="接受其他节点广播的局域网路由 (--accept-routes)">
          <NSwitch v-model:value="joinForm.accept_routes" />
        </NFormItem>

        <NFormItem label="广播本机局域网子网 (可选，例如: 192.168.1.0/24)">
          <NInput
            v-model:value="joinForm.advertise_routes"
            placeholder="留空表示不广播子网，多段用逗号分隔"
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

    <!-- 弹窗 3: Ping 测速 -->
    <NModal
      v-model:show="showPingModal"
      preset="card"
      :title="`节点连通性测速 (源: ${pingSourceNode?.hostname || ''})`"
      style="width: 560px; max-width: 90vw"
    >
      <NForm label-placement="top">
        <NFormItem label="目标虚拟 IP 或主机名" required>
          <NInput
            v-model:value="pingTarget"
            placeholder="输入 100.x.x.x 虚拟 IP 或节点名"
          />
        </NFormItem>

        <NButton
          type="primary"
          :loading="pinging"
          style="width: 100%; margin-bottom: 12px"
          @click="doPing"
        >
          <template #icon><NIcon :component="RadioOutline" /></template>
          开始探测打洞链路 (tailscale ping)
        </NButton>

        <div v-if="pingResult" class="ping-result-box">
          <div class="result-header">
            <NTag :type="pingResult.ok ? 'success' : 'error'" size="small">
              {{ pingResult.ok ? (pingResult.direct ? '连通 (P2P 直连)' : '连通 (DERP 中继)') : '探测超时或失败' }}
            </NTag>
            <span v-if="pingResult.latency_ms > 0" class="latency-tag">
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
          <pre class="ping-output">{{ pingResult.output || '(命令无输出)' }}</pre>
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
.network-view {
  gap: 16px;
  overflow-y: auto;
}

.network-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  flex-wrap: wrap;

  .header-left {
    .title-wrap {
      display: flex;
      align-items: center;
      gap: 10px;

      .page-title {
        margin: 0;
        font-size: 18px;
        font-weight: 600;
        color: var(--text-primary);
      }
    }

    .page-desc {
      margin: 6px 0 0;
      font-size: 13px;
      color: var(--n-text-color-3);
      max-width: 680px;
      line-height: 1.5;
    }
  }

  .header-right {
    display: flex;
    align-items: center;
  }
}

.stat-group {
  display: flex;
  align-items: center;
  background: var(--n-color-modal);
  border: 1px solid var(--n-border-color);
  border-radius: 8px;
  padding: 6px 14px;
  gap: 12px;

  .stat-item {
    display: flex;
    flex-direction: column;
    align-items: center;

    .stat-num {
      font-size: 17px;
      font-weight: 700;
      line-height: 1.2;
    }

    .stat-lbl {
      font-size: 11px;
      color: var(--n-text-color-3);
    }
  }

  .stat-sep {
    width: 1px;
    height: 22px;
    background: var(--n-border-color);
  }
}

.network-toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;

  .filter-pills {
    display: flex;
    background: var(--n-color-modal);
    border: 1px solid var(--n-border-color);
    border-radius: 8px;
    padding: 3px;
    gap: 4px;

    .pill-btn {
      background: transparent;
      border: none;
      outline: none;
      cursor: pointer;
      font-size: 12px;
      padding: 4px 10px;
      border-radius: 6px;
      color: var(--n-text-color-2);
      transition: all 0.2s;

      &:hover {
        color: var(--n-text-color-1);
      }

      &.active {
        background: var(--n-color-hover);
        color: var(--n-primary-color);
        font-weight: 600;
      }
    }
  }
}

.network-card-wrap {
  flex: 1;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  overflow: hidden;

  .empty-box {
    padding: 60px 20px;
    display: flex;
    justify-content: center;
  }

  .node-list {
    display: flex;
    flex-direction: column;

    .node-row {
      display: flex;
      align-items: center;
      padding: 16px 20px;
      border-bottom: 1px solid var(--border-color);
      transition: background-color 0.15s ease;

      &:hover {
        background-color: var(--hover-bg, rgba(99, 102, 241, 0.04));
      }

      &:last-child {
        border-bottom: none;
      }

      .col-logo {
        flex: 0 0 54px;
        display: flex;
        align-items: center;

        .host-avatar-box {
          position: relative;
          width: 38px;
          height: 38px;
          border-radius: 8px;
          background: var(--n-color-modal);
          border: 1px solid var(--border-color);
          display: flex;
          align-items: center;
          justify-content: center;

          .host-avatar-text {
            font-size: 13px;
            font-weight: 700;
            color: var(--text-secondary);
            letter-spacing: 0.5px;
          }

          .status-dot-indicator {
            position: absolute;
            bottom: -2px;
            right: -2px;
            width: 9px;
            height: 9px;
            border-radius: 50%;
            border: 2px solid var(--bg-card);

            &.is-online {
              background-color: #10b981;
            }

            &.is-offline {
              background-color: #94a3b8;
            }
          }
        }
      }

      .col-host {
        flex: 0 0 200px;
        display: flex;
        flex-direction: column;
        gap: 4px;
        padding-right: 12px;

        .host-title-row {
          display: flex;
          align-items: center;
          gap: 6px;

          .host-name {
            font-weight: 600;
            font-size: 14px;
            color: var(--text-primary);
          }
        }

        .host-phy-ip {
          font-size: 12px;
          color: var(--text-secondary);
        }
      }

      .col-status {
        flex: 0 0 170px;
        display: flex;
        align-items: center;
        padding-right: 12px;

        .status-pill {
          display: inline-flex;
          align-items: center;
          padding: 4px 12px;
          border-radius: 14px;
          font-size: 12px;
          font-weight: 500;

          &.is-connected {
            background-color: #e6f8f0;
            color: #059669;
          }

          &.is-unjoined {
            background-color: #fef3c7;
            color: #d97706;
          }

          &.is-not-installed,
          &.is-offline {
            background-color: rgba(148, 163, 184, 0.15);
            color: #94a3b8;
          }
        }
      }

      .col-ip {
        flex: 0 0 200px;
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding-right: 12px;

        .field-label {
          font-size: 11px;
          color: var(--text-tertiary, #8c8c8c);
        }

        .virtual-ip-tag {
          display: inline-flex;
          align-items: center;
          gap: 6px;
          cursor: pointer;
          font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
          font-size: 13px;
          font-weight: 600;
          color: #10b981;
          padding: 2px 6px;
          border-radius: 4px;
          width: fit-content;
          transition: all 0.15s;

          &:hover {
            background-color: rgba(16, 185, 129, 0.1);
            color: #059669;
          }
        }
      }

      .col-node {
        flex: 1;
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding-right: 12px;

        .field-label {
          font-size: 11px;
          color: var(--text-tertiary, #8c8c8c);
        }

        .node-fqdn-box {
          display: flex;
          align-items: baseline;
          gap: 6px;

          .fqdn-text {
            font-size: 12.5px;
            font-weight: 500;
            color: var(--text-primary);
          }

          .ver-text {
            font-size: 11px;
            color: var(--text-secondary);
          }
        }
      }

      .col-derp {
        flex: 0 0 130px;
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding-right: 12px;

        .field-label {
          font-size: 11px;
          color: var(--text-tertiary, #8c8c8c);
        }
      }

      .col-actions {
        flex: 0 0 210px;
        display: flex;
        align-items: center;
        justify-content: flex-end;
        gap: 8px;
      }
    }
  }
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

.ping-result-box {
  background: var(--n-color-modal);
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  padding: 10px;

  .result-header {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;

    .latency-tag {
      font-size: 12px;
      font-weight: 600;
      color: #10b981;
    }
  }

  .ping-output {
    margin: 0;
    font-family: monospace;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 180px;
    overflow-y: auto;
    color: var(--n-text-color-2);
  }
}
</style>
