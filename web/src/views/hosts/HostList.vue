<script setup lang="ts">
import { onMounted, ref, computed, h } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NModal,
  NCode,
  NSpin,
  NDropdown,
  NSelect,
  NSpace,
  NTag,
  NCheckbox,
  useMessage,
  useDialog,
  NIcon,
} from 'naive-ui'
import {
  AddCircleOutline,
  RefreshOutline,
  SearchOutline,
  DesktopOutline,
  PaperPlaneOutline,
  EllipsisHorizontalCircleOutline,
  TerminalOutline,
  FolderOutline,
  CubeOutline,
  TrashOutline,
  PulseOutline,
  SparklesOutline,
  DocumentTextOutline,
  ShieldCheckmarkOutline,
  CopyOutline,
  CheckmarkCircleOutline,
  LayersOutline,
  ArrowUpCircleOutline,
  GitNetworkOutline,
  CardOutline,
  LinkOutline,
} from '@vicons/ionicons5'
import OsLogo from '../../components/common/OsLogo.vue'
import HostBillingModal from '../../components/host/HostBillingModal.vue'
import { useHostsStore } from '../../stores/hosts'
import { useWorkspaceStore } from '../../stores/workspace'
import { useAuthStore } from '../../stores/auth'
import { enroll, deleteHost, setHostGroup, upgradeAgent } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import { listGroups, type HostGroup } from '../../api/groups'
import { copyToClipboard } from '../../utils/clipboard'
import type { Host } from '../../api/types'
import type { NetworkNode } from '../../api/network'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'HostList' })

const router = useRouter()
const store = useHostsStore()
const workspace = useWorkspaceStore()
const auth = useAuthStore()
const message = useMessage()
const dialog = useDialog()

const search = ref('')
const showEnroll = ref(false)
const enrollToken = ref('')
const enrollCmd = ref('')
const enrollWinCmd = ref('')
const enrolling = ref(false)
const showBillingModal = ref(false)
const selectedBillingHost = ref<Host | null>(null)

// Host grouping: filter bar + per-host assignment modal.
const groups = ref<HostGroup[]>([])
const groupFilter = ref<string>('')
const showGroupModal = ref(false)
const groupTarget = ref<Host | null>(null)
const groupChoice = ref<string>('')
const networkNodesMap = ref<Record<string, NetworkNode>>({})

async function loadNetworkNodes() {
  try {
    const list = await listNetworkNodes()
    const map: Record<string, NetworkNode> = {}
    for (const node of list) {
      map[node.host_id] = node
    }
    networkNodesMap.value = map
  } catch {
    networkNodesMap.value = {}
  }
}

const groupFilterOptions = computed(() => [
  { label: '全部分组', value: '' },
  { label: '未分组', value: '__none__' },
  ...groups.value.map((g) => ({ label: `${g.name} (${g.host_count})`, value: g.name })),
])

const groupAssignOptions = computed(() => [
  { label: '（不属于任何分组）', value: '' },
  ...groups.value.map((g) => ({ label: g.name, value: g.name })),
])

const filteredHosts = computed(() => {
  const q = search.value.trim().toLowerCase()
  const gf = groupFilter.value
  return store.hosts.filter((h) => {
    if (gf === '__none__' && h.group) return false
    if (gf && gf !== '__none__' && h.group !== gf) return false
    if (!q) return true
    return (
      h.hostname.toLowerCase().includes(q) ||
      h.id.toLowerCase().includes(q) ||
      (h.os || '').toLowerCase().includes(q) ||
      (h.distro || '').toLowerCase().includes(q) ||
      (h.internal_ip || '').toLowerCase().includes(q) ||
      (h.public_ip || '').toLowerCase().includes(q) ||
      (h.group || '').toLowerCase().includes(q) ||
      (h.notes || '').toLowerCase().includes(q)
    )
  })
})

async function loadGroups() {
  try {
    groups.value = await listGroups()
  } catch {
    // Grouping is optional; a failure here must not block the host list.
  }
}

function openGroupModal(host: Host) {
  groupTarget.value = host
  groupChoice.value = host.group || ''
  showGroupModal.value = true
}

async function submitGroup() {
  const host = groupTarget.value
  if (!host) return
  try {
    await setHostGroup(host.id, groupChoice.value)
    message.success(groupChoice.value ? `已将 ${host.hostname} 移入 ${groupChoice.value}` : `已将 ${host.hostname} 移出分组`)
    showGroupModal.value = false
    await Promise.all([store.fetchList(), loadGroups()])
  } catch (e: any) {
    message.error(e.message || '设置分组失败')
  }
}


function formatArch(arch?: string): string {
  const a = (arch || '').toLowerCase()
  if (a === 'amd64' || a === 'x86_64' || a === 'x64') return 'x64'
  if (a === 'arm64' || a === 'aarch64') return 'arm64'
  if (a === '386' || a === 'x86') return 'x86'
  return arch || 'x64'
}

function formatMem(bytes?: number): string {
  if (!bytes || bytes <= 0) return '-'
  const gb = bytes / (1024 * 1024 * 1024)
  if (gb >= 1) {
    return gb.toFixed(1) + ' GB'
  }
  const mb = bytes / (1024 * 1024)
  return Math.round(mb) + ' MB'
}

function fmtUptime(row: Host): string {
  let secs = 0
  if (row.uptime && row.uptime > 0) {
    secs = row.uptime
  } else {
    const end = row.status === 'online' ? Date.now() : new Date(row.last_seen || row.registered).getTime()
    const start = new Date(row.registered || row.last_seen || end).getTime()
    secs = Math.max(0, Math.floor((end - start) / 1000))
  }
  if (secs < 60) return `${Math.max(1, secs)} 秒`
  const m = Math.floor(secs / 60)
  if (m < 60) return `${m} 分钟`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时 ${m % 60} 分`
  const d = Math.floor(h / 24)
  if (d >= 30) {
    const months = Math.floor(d / 30)
    const remDays = d % 30
    return `${months} 月 ${remDays} 天`
  }
  return `${d} 天 ${h % 24} 小时`
}

function goDetail(host: Host, tab = 'metrics') {
  workspace.openTab({
    key: `/hosts/${host.id}`,
    title: host.hostname,
    path: `/hosts/${host.id}`,
    closable: true,
  })
  router.push({ path: `/hosts/${host.id}`, query: { tab } })
}

function hasPrice(h: Host): boolean {
  return h.price !== undefined && h.price !== null && !isNaN(Number(h.price))
}

function hasBilling(h: Host): boolean {
  return (
    hasPrice(h) ||
    (h.traffic_limit_gb !== undefined && h.traffic_limit_gb !== null && Number(h.traffic_limit_gb) > 0) ||
    !!h.expires_at ||
    (!!h.notes && h.notes.trim() !== '')
  )
}

function formatPrice(h: Host): string {
  if (!hasPrice(h)) return '-'
  const curMap: Record<string, string> = { CNY: '¥', USD: '$', EUR: '€', HKD: 'HK$', JPY: '¥', GBP: '£', USDT: 'USDT ' }
  const cur = curMap[h.currency || 'CNY'] || `${h.currency || ''} `
  const p = `${cur}${h.price}`
  const c = h.billing_cycle ? ` / ${h.billing_cycle}` : ''
  return `${p}${c}`.trim()
}

function formatHostTraffic(h: Host): string {
  if (!h.traffic_limit_gb) return '-'
  let used = (h.month_rx || 0) + (h.month_tx || 0)
  if (h.traffic_calc_type === 'out') used = h.month_tx || 0
  else if (h.traffic_calc_type === 'in') used = h.month_rx || 0
  const usedGB = used / (1024 * 1024 * 1024)
  const pct = Math.round((usedGB / h.traffic_limit_gb) * 100)
  return `${usedGB.toFixed(1)}G/${h.traffic_limit_gb}G (${pct}%)`
}

function getTrafficTooltip(h: Host): string {
  if (!h.traffic_limit_gb) return ''
  const rxGB = ((h.month_rx || 0) / (1024 * 1024 * 1024)).toFixed(2)
  const txGB = ((h.month_tx || 0) / (1024 * 1024 * 1024)).toFixed(2)
  return `本月入向: ${rxGB} GB | 本月出向: ${txGB} GB | 重置日: 每月 ${h.traffic_reset_day || 1} 号`
}

function getExpiryClass(exp?: string): string {
  if (!exp) return ''
  const days = (new Date(exp).getTime() - Date.now()) / (1000 * 3600 * 24)
  if (days < 0) return 'text-error'
  if (days <= 30) return 'text-warning'
  return ''
}

function getExpiryDaysText(exp?: string): string {
  if (!exp) return ''
  const diffDays = Math.ceil((new Date(exp).getTime() - Date.now()) / (1000 * 3600 * 24))
  if (diffDays < 0) return `已逾期 ${Math.abs(diffDays)} 天`
  if (diffDays === 0) return '今天到期'
  return `剩 ${diffDays} 天`
}

function goExec(host?: Host) {
  const query = host && typeof host === 'object' && host.id ? `?host_id=${host.id}` : ''
  workspace.openTab({
    key: '/batch-exec',
    title: '推送命令',
    path: `/batch-exec${query}`,
    closable: true,
  })
  router.push(`/batch-exec${query}`)
}

function goAiChat() {
  workspace.openTab({
    key: '/ai/chat',
    title: 'AI 助手',
    path: '/ai/chat',
    closable: true,
  })
  router.push('/ai/chat')
}

function goAiReport() {
  workspace.openTab({
    key: '/ai/report',
    title: '运维报告',
    path: '/ai/report',
    closable: true,
  })
  router.push('/ai/report')
}

const isAdmin = computed(() => auth.role === 'admin')

function goAudit() {
  workspace.openTab({
    key: '/audit',
    title: '操作审计',
    path: '/audit',
    closable: true,
  })
  router.push('/audit')
}

function handleMenuSelect(key: string, host: Host) {
  if (key === 'detail') {
    goDetail(host, 'metrics')
  } else if (key === 'network') {
    workspace.openTab({
      key: '/network',
      title: '异地组网',
      path: '/network',
      closable: true,
      viewName: 'NetworkList',
    })
    router.push('/network')
  } else if (key === 'terminal') {
    goDetail(host, 'terminal')
  } else if (key === 'files') {
    goDetail(host, 'files')
  } else if (key === 'docker') {
    goDetail(host, 'docker')
  } else if (key === 'exec') {
    goExec(host)
  } else if (key === 'billing') {
    selectedBillingHost.value = host
    showBillingModal.value = true
  } else if (key === 'group') {
    openGroupModal(host)
  } else if (key === 'upgrade') {
    if (host.status !== 'online') {
      message.warning('主机已离线，无法下发在线升级指令')
      return
    }
    dialog.info({
      title: '升级 Agent 确认',
      content: `确定将主机 "${host.hostname}" 的 Agent 升级至控制端最新版本吗？\n升级过程中 Agent 将下载最新对应平台二进制，校验并平滑重启服务。`,
      positiveText: '开始升级',
      negativeText: '取消',
      onPositiveClick: async () => {
        message.loading('正在下发升级指令并等待 Agent 替换重启…', { duration: 6000 })
        try {
          const res = await upgradeAgent(host.id)
          message.success(res.message || 'Agent 升级成功，正在重启自愈连线！')
          setTimeout(() => store.fetchList(), 4000)
        } catch (e: any) {
          message.error(e.message || 'Agent 升级失败')
        }
      },
    })
  } else if (key === 'unbind') {
    const uninstallAgent = ref(false)
    dialog.warning({
      title: '解绑主机确认',
      content: () =>
        h('div', [
          h('div', `确定要解绑主机 "${host.hostname}" (${host.id}) 吗？解绑后该主机将从控制台移除。`),
          h(
            'div',
            { style: 'margin-top: 12px;' },
            h(
              NCheckbox,
              {
                checked: uninstallAgent.value,
                'onUpdate:checked': (v: boolean) => {
                  uninstallAgent.value = v
                },
              },
              { default: () => '同时卸载 Agent 并清除其数据（不可恢复，需主机在线）' },
            ),
          ),
        ]),
      positiveText: '确认解绑',
      negativeText: '取消',
      onPositiveClick: async () => {
        try {
          const res = await deleteHost(host.id, uninstallAgent.value)
          if (res.warning) {
            message.warning(`已解绑主机，但${res.warning}`)
          } else if (uninstallAgent.value && res.uninstalled) {
            message.success('已解绑主机并远程卸载 Agent')
          } else {
            message.success('已成功解绑主机')
          }
          await store.fetchList()
        } catch (e: any) {
          const errMsg = e.message || '解绑失败'
          if (errMsg.includes('agent offline')) {
            message.error('该主机处于离线状态，无法远程卸载 Agent。请取消勾选“同时卸载 Agent”直接解绑，或等待主机上线后再操作。')
          } else {
            message.error(errMsg)
          }
        }
      },
    })
  }
}

const menuOptions = [
  {
    label: '运维监控',
    key: 'detail',
    icon: () => h(NIcon, null, { default: () => h(PulseOutline) }),
  },
  {
    label: '异地组网 (Tailscale)',
    key: 'network',
    icon: () => h(NIcon, { color: '#10b981' }, { default: () => h(GitNetworkOutline) }),
  },
  {
    label: '在线终端',
    key: 'terminal',
    icon: () => h(NIcon, null, { default: () => h(TerminalOutline) }),
  },
  {
    label: '文件管理',
    key: 'files',
    icon: () => h(NIcon, null, { default: () => h(FolderOutline) }),
  },
  {
    label: 'Docker 管理',
    key: 'docker',
    icon: () => h(NIcon, null, { default: () => h(CubeOutline) }),
  },
  {
    label: '推送命令',
    key: 'exec',
    icon: () => h(NIcon, null, { default: () => h(PaperPlaneOutline) }),
  },
  {
    label: '财务与规格',
    key: 'billing',
    icon: () => h(NIcon, { color: '#6366f1' }, { default: () => h(CardOutline) }),
  },
  {
    type: 'divider',
    key: 'd1',
  },
  {
    label: '设置分组',
    key: 'group',
    icon: () => h(NIcon, null, { default: () => h(LayersOutline) }),
  },
  {
    label: '升级 Agent',
    key: 'upgrade',
    icon: () => h(NIcon, { color: '#6366f1' }, { default: () => h(ArrowUpCircleOutline) }),
  },
  {
    label: '解绑主机',
    key: 'unbind',
    icon: () => h(NIcon, { color: '#ef4444' }, { default: () => h(TrashOutline) }),
  },
]

const copied = ref(false)
const copiedWin = ref(false)
let copyTimer: any = null
let copyWinTimer: any = null

async function openEnroll() {
  showEnroll.value = true
  enrolling.value = true
  copied.value = false
  copiedWin.value = false
  try {
    const res = await enroll()
    enrollToken.value = res.enroll_token
    const proto = window.location.protocol || 'http:'
    const host = window.location.hostname || 'localhost'
    const port = window.location.port ? `:${window.location.port}` : ''
    enrollCmd.value = res.install || `curl -kfsSL '${proto}//${host}${port}/install?token=${res.enroll_token}' | sudo bash`
    enrollWinCmd.value = res.install_win || `irm '${proto}//${host}${port}/install?os_type=windows^&token=${res.enroll_token}' | iex`
  } catch (e: any) {
    message.error(e.message || '获取安装脚本失败')
  } finally {
    enrolling.value = false
  }
}

async function copyCmd() {
  if (!enrollCmd.value) return
  const ok = await copyToClipboard(enrollCmd.value)
  if (ok) {
    copied.value = true
    if (copyTimer) clearTimeout(copyTimer)
    copyTimer = setTimeout(() => {
      copied.value = false
    }, 2500)
    message.success('已复制 Linux 安装命令到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

async function copyWinCmd() {
  if (!enrollWinCmd.value) return
  const ok = await copyToClipboard(enrollWinCmd.value)
  if (ok) {
    copiedWin.value = true
    if (copyWinTimer) clearTimeout(copyWinTimer)
    copyWinTimer = setTimeout(() => {
      copiedWin.value = false
    }, 2500)
    message.success('已复制 Windows 安装命令到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

async function copyIp(ip?: string) {
  if (!ip || ip === '-') return
  const ok = await copyToClipboard(ip)
  if (ok) {
    message.success(`已复制 IP 地址 (${ip}) 到剪贴板`)
  } else {
    message.error('复制失败，请手动选中复制')
  }
}

async function refresh() {
  await Promise.all([store.fetchList(), loadGroups(), loadNetworkNodes()])
}

onMounted(() => {
  workspace.setActiveKey('/hosts')
  store.fetchList().catch((e) => message.error(e.message))
  loadGroups()
  loadNetworkNodes()
})
</script>

<template>
  <div class="baichuan-host-list-view">
    <div class="main-table-area">
      <!-- 顶部工具栏 Toolbar -->
      <div class="toolbar-bar">
        <div class="toolbar-left">
          <NInput
            v-model:value="search"
            placeholder="快速搜索"
            clearable
            size="small"
            style="width: 240px"
          >
            <template #prefix>
              <NIcon :component="SearchOutline" />
            </template>
          </NInput>
          <NSelect
            v-model:value="groupFilter"
            :options="groupFilterOptions"
            size="small"
            style="width: 170px"
          />
          <span class="total-count-text">共 {{ store.hosts.length }} 台主机</span>
        </div>

        <div class="toolbar-right">
          <NButton type="primary" size="small" @click="openEnroll">
            <template #icon>
              <NIcon :component="AddCircleOutline" />
            </template>
            绑定主机
          </NButton>

          <NButton secondary type="primary" size="small" @click="() => goExec()">
            <template #icon>
              <NIcon :component="PaperPlaneOutline" />
            </template>
            推送命令
          </NButton>

          <NButton secondary type="info" size="small" @click="goAiChat">
            <template #icon>
              <NIcon :component="SparklesOutline" />
            </template>
            AI 助手
          </NButton>

          <NButton secondary type="info" size="small" @click="goAiReport">
            <template #icon>
              <NIcon :component="DocumentTextOutline" />
            </template>
            运维报告
          </NButton>

          <NButton v-if="isAdmin" secondary size="small" @click="goAudit">
            <template #icon>
              <NIcon :component="ShieldCheckmarkOutline" />
            </template>
            操作审计
          </NButton>

          <NButton quaternary size="small" @click="refresh" :loading="store.loading">
            <template #icon>
              <NIcon :component="RefreshOutline" />
            </template>
          </NButton>
        </div>
      </div>

      <!-- 主机列表卡片容器 -->
      <div class="host-list-container">
        <NSpin v-if="store.loading && store.hosts.length === 0" style="padding: 40px" />

        <div v-else-if="filteredHosts.length === 0" class="empty-placeholder">
          <NIcon :component="DesktopOutline" size="48" style="color: var(--text-tertiary); margin-bottom: 12px;" />
          <div style="color: var(--text-secondary); font-size: 14px;">暂无匹配的主机设备</div>
        </div>

        <div v-else class="host-row-list">
          <div
            v-for="host in filteredHosts"
            :key="host.id"
            class="host-row-item"
            @click="goDetail(host)"
          >
	            <!-- 1. OS Logo 图标 -->
	            <div class="col-os-logo">
	              <OsLogo :os="host.os" :distro="host.distro" :badge-size="38" :size="22" />
	            </div>

            <!-- 2. 主机名称与系统版本 -->
            <div class="col-host-info">
              <div class="host-name-row">
                <span class="host-name">{{ host.hostname }}</span>
                <NTag v-if="host.group" size="tiny" :bordered="false" type="info">{{ host.group }}</NTag>
              </div>
              <div class="host-distro">
                {{ host.distro || host.os || 'Linux' }}
              </div>
            </div>

            <!-- 3. 开机状态 Pill 标签 -->
            <div class="col-status">
              <div
                class="uptime-pill"
                :class="{ 'is-online': host.status === 'online', 'is-offline': host.status !== 'online' }"
              >
                {{ host.status === 'online' ? `已开机 ${fmtUptime(host)}` : '当前已离线' }}
              </div>
            </div>

            <!-- 4. CPU 与内存规格 -->
            <div class="col-specs">
              <div class="spec-line">
                <span class="spec-label">CPU</span>
                <span class="spec-value">{{ (host.cpu_cores || 1) }} 核 ({{ formatArch(host.arch) }})</span>
              </div>
              <div class="spec-line">
                <span class="spec-label">内存</span>
                <span class="spec-value">{{ formatMem(host.mem_total) }}</span>
              </div>
            </div>

            <!-- 5. 内网与外网 IP + 组网 IP + 地理位置 -->
            <div class="col-ips">
              <div
                v-if="networkNodesMap[host.id]?.online && networkNodesMap[host.id]?.ip"
                class="ip-line is-copyable text-emerald-wrap"
                :title="`点击复制异地组网虚拟 IP (${networkNodesMap[host.id].ip})`"
                @click.stop="copyIp(networkNodesMap[host.id].ip)"
              >
                <span class="ip-label ip-label-net">网</span>
                <span class="ip-value ip-value-net">{{ networkNodesMap[host.id].ip }}</span>
              </div>
              <div
                class="ip-line"
                :class="{ 'is-copyable': host.internal_ip && host.internal_ip !== '-' }"
                :title="host.internal_ip && host.internal_ip !== '-' ? '点击复制内网 IP' : ''"
                @click.stop="copyIp(host.internal_ip)"
              >
                <span class="ip-label">内</span>
                <span class="ip-value">{{ host.internal_ip || '-' }}</span>
              </div>
              <div
                class="ip-line"
                :class="{ 'is-copyable': host.public_ip && host.public_ip !== '-' }"
                :title="host.public_ip && host.public_ip !== '-' ? '点击复制外网 IP' : ''"
                @click.stop="copyIp(host.public_ip)"
              >
                <span class="ip-label">外</span>
                <span class="ip-value">
                  {{ host.public_ip || '-' }}
                  <span v-if="host.location" class="ip-location">({{ host.location }})</span>
                </span>
              </div>
            </div>

            <!-- 6. 财务与规格概要（配置什么展示什么，未配置不展示） -->
            <div v-if="hasBilling(host)" class="col-billing">
              <div v-if="hasPrice(host)" class="billing-line">
                <span class="billing-label">资费</span>
                <span class="billing-value" :title="formatPrice(host)">{{ formatPrice(host) }}</span>
              </div>
              <div v-if="host.traffic_limit_gb" class="billing-line" :title="getTrafficTooltip(host)">
                <span class="billing-label">流量</span>
                <span class="billing-value">{{ formatHostTraffic(host) }}</span>
              </div>
              <div v-if="host.expires_at" class="billing-line">
                <span class="billing-label">到期</span>
                <div class="expiry-group" :class="getExpiryClass(host.expires_at)" :title="`${host.expires_at} (${getExpiryDaysText(host.expires_at)})`">
                  <span class="expiry-date">{{ host.expires_at }}</span>
                  <span class="expiry-badge">{{ getExpiryDaysText(host.expires_at) }}</span>
                  <span v-if="host.auto_renewal" class="auto-renew-badge">自续</span>
                  <a
                    v-if="host.renewal_url"
                    :href="host.renewal_url"
                    target="_blank"
                    class="renewal-link-icon"
                    title="点击跳转服务商控制台续费"
                    @click.stop
                  >
                    <NIcon :component="LinkOutline" size="12" />
                  </a>
                </div>
              </div>
              <!-- 备注行：配了就展示，没配隐藏 -->
              <div v-if="host.notes && host.notes.trim()" class="billing-line" :title="`备注: ${host.notes}`">
                <span class="billing-label">备注</span>
                <span class="billing-value muted-notes">{{ host.notes }}</span>
              </div>
            </div>

            <!-- 7. 右侧更多操作按钮 -->
            <div class="col-actions" @click.stop>
              <NDropdown
                trigger="click"
                placement="bottom-end"
                :options="menuOptions"
                @select="(key: string) => handleMenuSelect(key, host)"
              >
                <button class="more-action-btn" title="更多操作">
                  <NIcon :component="EllipsisHorizontalCircleOutline" size="22" />
                </button>
              </NDropdown>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 绑定主机 Modal -->
    <NModal
      v-model:show="showEnroll"
      preset="card"
      title="添加/绑定主机"
      style="width: 620px"
    >
      <NSpin v-if="enrolling" />
      <template v-else>
        <div class="enroll-modal-content">
          <p class="guide-text">
            在目标 Linux 服务器上执行以下安装指令（含 Token）：
          </p>
          <div class="code-container">
            <NCode :code="enrollCmd" language="bash" word-wrap />
          </div>
          <NSpace justify="end" style="margin-top: 10px">
            <NButton
              :type="copied ? 'success' : 'primary'"
              @click="copyCmd"
            >
              <template #icon>
                <NIcon :component="copied ? CheckmarkCircleOutline : CopyOutline" />
              </template>
              {{ copied ? '已复制命令' : '复制命令' }}
            </NButton>
          </NSpace>

          <p class="guide-text" style="margin-top: 6px">
            在目标 Windows 主机以管理员身份运行 PowerShell，执行以下安装指令：
          </p>
          <div class="code-container">
            <NCode :code="enrollWinCmd" language="powershell" word-wrap />
          </div>
          <NSpace justify="end" style="margin-top: 10px">
            <NButton
              :type="copiedWin ? 'success' : 'primary'"
              @click="copyWinCmd"
            >
              <template #icon>
                <NIcon :component="copiedWin ? CheckmarkCircleOutline : CopyOutline" />
              </template>
              {{ copiedWin ? '已复制命令' : '复制命令' }}
            </NButton>
          </NSpace>
        </div>
      </template>
    </NModal>

    <!-- 设置主机分组 Modal -->
    <NModal
      v-model:show="showGroupModal"
      preset="card"
      title="设置主机分组"
      style="width: 440px"
    >
      <NSpace vertical :size="12">
        <p class="guide-text">
          将主机 <code>{{ groupTarget?.hostname }}</code> 归入分组，用于按分组给用户授权访问。
        </p>
        <NSelect v-model:value="groupChoice" :options="groupAssignOptions" />
        <p v-if="!groups.length" class="guide-text">
          暂无可用分组，请先在「分组权限」页面创建分组。
        </p>
      </NSpace>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showGroupModal = false">取消</NButton>
          <NButton type="primary" @click="submitGroup">保存</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 财务与规格编辑弹窗 -->
    <HostBillingModal
      v-model:show="showBillingModal"
      :host="selectedBillingHost"
      @saved="refresh"
    />
  </div>
</template>

<style scoped lang="scss">
.baichuan-host-list-view {
  display: flex;
  height: 100%;
  box-sizing: border-box;

  .main-table-area {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 12px;
    background-color: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: var(--card-padding);
    box-shadow: var(--shadow-sm);

    .toolbar-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding-bottom: 4px;

      .toolbar-left,
      .toolbar-right {
        display: flex;
        align-items: center;
        gap: 12px;
      }

      .total-count-text {
        font-size: 13px;
        color: var(--text-secondary);
      }
    }

    .host-list-container {
      flex: 1;
      overflow-y: auto;

      .empty-placeholder {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        padding: 60px 20px;
      }

      .host-row-list {
        display: flex;
        flex-direction: column;

        .host-row-item {
          display: flex;
          align-items: center;
          padding: 18px 12px;
          border-bottom: 1px solid var(--border-color);
          cursor: pointer;
          transition: background-color 0.15s ease;

          &:hover {
            background-color: var(--hover-bg, rgba(99, 102, 241, 0.04));
          }

          &:last-child {
            border-bottom: none;
          }

          // 1. OS Logo
          .col-os-logo {
            flex: 0 0 54px;
            display: flex;
            align-items: center;
          }

          // 2. 主机名与系统
          .col-host-info {
            flex: 0 0 190px;
            display: flex;
            flex-direction: column;
            gap: 4px;
            padding-right: 12px;

            .host-name-row {
              display: flex;
              align-items: center;
              gap: 6px;

              .host-name {
                font-weight: 600;
                font-size: 15px;
                color: var(--text-primary);
                letter-spacing: 0.2px;
              }
            }

            .host-distro {
              font-size: 12px;
              color: var(--text-secondary);
            }
          }

          // 3. 运行时间 Pill 标签
          .col-status {
            flex: 0 0 170px;
            display: flex;
            align-items: center;
            padding-right: 12px;

            .uptime-pill {
              display: inline-flex;
              align-items: center;
              justify-content: center;
              padding: 4px 14px;
              border-radius: 14px;
              font-size: 12px;
              font-weight: 500;

              &.is-online {
                background-color: #e6f8f0;
                color: #059669;
              }

              &.is-offline {
                background-color: rgba(148, 163, 184, 0.15);
                color: #94a3b8;
              }
            }
          }

          // 4. CPU 与内存规格
          .col-specs {
            flex: 0 0 180px;
            display: flex;
            flex-direction: column;
            gap: 4px;
            font-size: 13px;
            padding-right: 12px;

            .spec-line {
              display: flex;
              align-items: center;

              .spec-label {
                color: var(--text-tertiary, #8c8c8c);
                width: 38px;
                flex-shrink: 0;
              }

              .spec-value {
                color: var(--text-primary);
                font-weight: 500;
              }
            }
          }

          // 5. 内网与外网 IP
          .col-ips {
            flex: 0 0 250px;
            min-width: 0;
            display: flex;
            flex-direction: column;
            gap: 4px;
            font-size: 13px;
            padding-right: 12px;

            .ip-line {
              display: flex;
              align-items: center;
              // 收缩为内容实际宽度，避免 hover 高亮铺满整个富余列宽
              width: fit-content;
              max-width: 100%;

              &.is-copyable {
                cursor: pointer;
                transition: color 0.15s ease, background-color 0.15s ease;
                border-radius: 4px;
                padding: 1px 4px;
                margin-left: -4px;

                &:hover {
                  color: #6366f1;
                  background-color: rgba(99, 102, 241, 0.08);

                  .ip-value {
                    color: #6366f1;
                  }
                }
              }

              &.text-emerald-wrap {
                .ip-label-net {
                  color: #10b981;
                  font-weight: 600;
                }
                .ip-value-net {
                  color: #10b981;
                  font-weight: 600;
                  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
                }
                &:hover {
                  color: #059669;
                  background-color: rgba(16, 185, 129, 0.08);
                  .ip-value-net {
                    color: #059669;
                  }
                }
              }

              .ip-label {
                color: var(--text-tertiary, #8c8c8c);
                width: 24px;
                flex-shrink: 0;
              }

              .ip-value {
                color: var(--text-primary);
                font-weight: 400;
                white-space: nowrap;
                overflow: hidden;
                text-overflow: ellipsis;

                .ip-location {
                  color: var(--text-secondary);
                  margin-left: 4px;
                  font-size: 12px;
                }
              }
            }
          }

          // 6. 财务与规格概要
          .col-billing {
            flex: 0 0 240px;
            margin-left: 28px;
            display: flex;
            flex-direction: column;
            gap: 4px;
            font-size: 13px;
            padding-right: 12px;
            min-width: 0;

            .billing-line {
              display: flex;
              align-items: center;
              gap: 4px;
              min-width: 0;

              .billing-label {
                color: var(--text-tertiary, #8c8c8c);
                width: 32px;
                flex-shrink: 0;
                font-size: 12px;
              }

              .billing-value {
                color: var(--text-primary);
                font-weight: 500;
                white-space: nowrap;
                overflow: hidden;
                text-overflow: ellipsis;

                &.muted-notes {
                  color: var(--text-secondary);
                  font-weight: 400;
                  font-size: 12px;
                }
              }

              .expiry-group {
                display: flex;
                align-items: center;
                gap: 4px;
                min-width: 0;
                overflow: hidden;
                white-space: nowrap;

                .expiry-date {
                  font-weight: 500;
                  color: var(--text-primary);
                }

                .expiry-badge {
                  font-size: 10px;
                  padding: 1px 4px;
                  border-radius: 3px;
                  background: rgba(148, 163, 184, 0.14);
                  color: var(--text-secondary);
                  flex-shrink: 0;
                  line-height: 1.2;
                }

                .auto-renew-badge {
                  font-size: 10px;
                  padding: 1px 4px;
                  border-radius: 3px;
                  background: rgba(16, 185, 129, 0.12);
                  color: #10b981;
                  font-weight: 600;
                  flex-shrink: 0;
                  line-height: 1.2;
                }

                .renewal-link-icon {
                  display: inline-flex;
                  align-items: center;
                  justify-content: center;
                  color: var(--primary-color, #6366f1);
                  padding: 2px;
                  border-radius: 3px;
                  flex-shrink: 0;
                  transition: background-color 0.15s ease;

                  &:hover {
                    background: rgba(99, 102, 241, 0.12);
                  }
                }

                &.text-error {
                  .expiry-date,
                  .expiry-badge {
                    color: #ef4444;
                    background: rgba(239, 68, 68, 0.14);
                    font-weight: 600;
                  }
                }

                &.text-warning {
                  .expiry-date,
                  .expiry-badge {
                    color: #f59e0b;
                    background: rgba(245, 158, 11, 0.14);
                    font-weight: 600;
                  }
                }
              }
            }
          }

          // 7. 更多操作按钮
          .col-actions {
            flex: 0 0 44px;
            margin-left: auto;
            display: flex;
            align-items: center;
            justify-content: flex-end;

            .more-action-btn {
              background: transparent;
              border: none;
              cursor: pointer;
              color: var(--text-tertiary, #94a3b8);
              padding: 4px;
              border-radius: 50%;
              display: flex;
              align-items: center;
              justify-content: center;
              transition: color 0.15s, background-color 0.15s;

              &:hover {
                color: var(--primary-color, #6366f1);
                background-color: var(--hover-bg, rgba(99, 102, 241, 0.08));
              }
            }
          }
        }
      }
    }
  }

  .enroll-modal-content {
    display: flex;
    flex-direction: column;
    gap: 10px;

    .code-container {
      background: var(--code-box-bg);
      border: 1px solid var(--border-color);
      padding: 12px;
      border-radius: 6px;
    }
  }
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .baichuan-host-list-view {
    .main-table-area {
      gap: 8px;

      /* 工具栏换行：搜索框占满首行，按钮组换到第二行 */
      .toolbar-bar {
        flex-wrap: wrap;
        gap: 8px;

        .toolbar-left {
          flex: 1 1 100%;
          min-width: 0;

          .n-input {
            width: 100% !important;
            flex: 1;
          }

          .total-count-text {
            font-size: 12px;
            flex-shrink: 0;
          }
        }

        .toolbar-right {
          flex: 1 1 100%;
          justify-content: flex-end;
          gap: 8px;
          flex-wrap: wrap;

          .n-button {
            padding: 0 10px;
          }
        }
      }

      .host-list-container {
        display: flex;
        flex-direction: column;
        gap: 8px;

        .host-row-item {
          /* 桌面端单行 6 列 → 移动端独立卡片布局 */
          flex-wrap: wrap;
          padding: 12px 14px;
          row-gap: 8px;
          background: var(--bg-card);
          border: 1px solid var(--border-color);
          border-radius: 8px;
          box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
          transition: all 0.15s ease;

          &:hover {
            border-color: var(--primary-color, #6366f1);
          }

          /* 重排：第一行 logo + 主机名 + 更多按钮；第二行 pill/规格；第三行 IP */
          .col-os-logo {
            order: 1;
            flex: 0 0 44px;
          }

          .col-host-info {
            order: 2;
            flex: 1 1 auto;
            min-width: 0;
            padding-right: 4px;

            .host-name {
              font-size: 14px;
              white-space: nowrap;
              overflow: hidden;
              text-overflow: ellipsis;
            }

            .host-distro {
              font-size: 11px;
              white-space: nowrap;
              overflow: hidden;
              text-overflow: ellipsis;
            }
          }

          .col-actions {
            order: 3;
            flex: 0 0 32px;
          }

          /* 运行时间 pill：第二行开头 */
          .col-status {
            order: 4;
            flex: 0 0 auto;
            padding-right: 6px;

            .uptime-pill {
              padding: 2px 8px;
              font-size: 11px;
              white-space: nowrap;
            }
          }

          /* 规格与 IP：第二行并排 */
          .col-specs {
            order: 5;
            flex: 1 1 auto;
            min-width: 0;
            font-size: 12px;

            .spec-line .spec-label {
              width: 32px;
            }
          }

          .col-ips {
            order: 6;
            flex: 1 1 100%;
            font-size: 11px;
            padding-top: 4px;
            border-top: 1px dashed var(--border-dashed, rgba(0, 0, 0, 0.06));
            display: flex;
            flex-direction: row;
            justify-content: space-between;
            gap: 6px;

            .ip-line {
              flex: 1;
              min-width: 0;
            }

            .ip-value {
              white-space: nowrap;
              overflow: hidden;
              text-overflow: ellipsis;
            }
          }

          .col-billing {
            order: 7;
            flex: 1 1 100%;
            font-size: 11px;
            padding-top: 4px;
            border-top: 1px dashed var(--border-dashed, rgba(0, 0, 0, 0.06));
            display: flex;
            flex-direction: row;
            flex-wrap: wrap;
            align-items: center;
            gap: 8px;

            .billing-line {
              margin-right: 6px;
            }
          }
        }
      }
    }
  }
}
</style>
