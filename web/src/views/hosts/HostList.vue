<script setup lang="ts">
import { onMounted, onUnmounted, watch, ref, computed, h } from 'vue'
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
  NPagination,
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
  AlertCircleOutline,
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
import { batchUpgradeAgents } from '../../api/upgrade'
import { listNetworkNodes } from '../../api/network'
import { listGroups, type HostGroup } from '../../api/groups'
import { copyToClipboard } from '../../utils/clipboard'
import { formatUpgradeStage } from '../../utils/upgrade'
import { useIsMobile } from '../../composables/useIsMobile'
import { FALLBACK_RATES, getCnyRates, toCny, formatMoney, formatCny, type CnyRates } from '../../utils/exchangeRate'
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

// 首屏加载完成标记：计数只在首次拉取完成后才渲染，避免加载瞬间闪「共 0 台主机」
const listLoaded = ref(false)

// Host grouping: filter bar + per-host assignment modal.
const groups = ref<HostGroup[]>([])
const groupFilter = ref<string>('')
const showGroupModal = ref(false)
const groupTarget = ref<Host | null>(null)
const groupChoice = ref<string[]>([])
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

const groupAssignOptions = computed(() => groups.value.map((g) => ({ label: g.name, value: g.name })))

const filteredHosts = computed(() => {
  const q = search.value.trim().toLowerCase()
  const gf = groupFilter.value
  return store.hosts.filter((h) => {
    const groups = h.groups || []
    if (gf === '__none__' && groups.length > 0) return false
    if (gf && gf !== '__none__' && !groups.includes(gf)) return false
    if (!q) return true
    return (
      h.hostname.toLowerCase().includes(q) ||
      h.id.toLowerCase().includes(q) ||
      (h.os || '').toLowerCase().includes(q) ||
      (h.distro || '').toLowerCase().includes(q) ||
      (h.internal_ip || '').toLowerCase().includes(q) ||
      (h.public_ip || '').toLowerCase().includes(q) ||
      groups.some((g) => g.toLowerCase().includes(q)) ||
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
  groupChoice.value = [...(host.groups || [])]
  showGroupModal.value = true
}

async function submitGroup() {
  const host = groupTarget.value
  if (!host) return
  try {
    await setHostGroup(host.id, groupChoice.value)
    message.success(
      groupChoice.value.length > 0
        ? `已将 ${host.hostname} 归入 ${groupChoice.value.join('、')}`
        : `已将 ${host.hostname} 移出全部分组`,
    )
    showGroupModal.value = false
    await Promise.all([store.fetchList(), loadGroups()])
  } catch (e: any) {
    message.error(e.message || '设置分组失败')
  }
}

// ---- 批量操作：多选 + 前端并发编排（3 台同时），零后端改动，复用单机接口
// （与 NetworkList.vue 同款范式：勾选按 id 跨页保留、行内逐台进度、
// 失败可重试、完成后给成功/失败汇总；审计走既有单机接口，无需新埋点）。
const selectedIds = ref<Set<string>>(new Set())
const BATCH_CONCURRENCY = 3
type BatchOpPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchOpPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchKind = ref<'group' | 'unbind' | 'upgrade' | null>(null)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)

// 前端分页：主机全量已在内存，按 AGENTS.md §8.2 不定长列表必须分页。
// 自定义行列表不用 NDataTable，分页条用独立 NPagination + 计算切片
// （与 NetworkList.vue 同款），数据区滚动、分页条固定在卡片底部。
const isMobile = useIsMobile()
const page = ref(1)
const pageSize = ref(20)
const pageSizes = [20, 50, 100]
const pagedHosts = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return filteredHosts.value.slice(start, start + pageSize.value)
})
// 搜索/筛选变化回第一页
watch([search, groupFilter], () => {
  page.value = 1
})
// 页码收敛：筛选收紧导致总行数变少时夹回最大有效页，杜绝空白页
watch([() => filteredHosts.value.length, pageSize], ([count, size]) => {
  const maxPage = Math.max(1, Math.ceil(count / size))
  if (page.value > maxPage) page.value = maxPage
})

function handlePageChange(p: number) {
  page.value = p
}
function handlePageSizeChange(size: number) {
  pageSize.value = size
  page.value = 1
}

// 批量操作的目标集合：勾选按 id 存、跨页保留，执行时按当时状态过滤。
const selectedHosts = computed(() => store.hosts.filter((h) => selectedIds.value.has(h.id)))
const batchOnline = computed(() => selectedHosts.value.filter((h) => h.status === 'online'))
const batchUpgradable = computed(() => batchOnline.value.filter((h) => h.agent_outdated))
const pageAllChecked = computed(() =>
  pagedHosts.value.length > 0 && pagedHosts.value.every((h) => selectedIds.value.has(h.id)),
)
const pageSomeChecked = computed(() =>
  !pageAllChecked.value && pagedHosts.value.some((h) => selectedIds.value.has(h.id)),
)

function toggleRow(hostId: string, checked: boolean) {
  if (checked) {
    selectedIds.value.add(hostId)
  } else {
    selectedIds.value.delete(hostId)
  }
}

function toggleSelectAllPage() {
  if (pageAllChecked.value) {
    pagedHosts.value.forEach((h) => selectedIds.value.delete(h.id))
  } else {
    pagedHosts.value.forEach((h) => selectedIds.value.add(h.id))
  }
}

function clearSelection() {
  selectedIds.value.clear()
  opStatus.value = {}
  batchSummary.value = null
  batchKind.value = null
}

function isOpBusy(hostId: string) {
  const st = opStatus.value[hostId]
  return !!st && (st.phase === 'queued' || st.phase === 'running')
}

// 刷新后清掉已不存在的主机勾选，避免批量操作带上幽灵 ID
function pruneSelection() {
  const alive = new Set(store.hosts.map((h) => h.id))
  selectedIds.value.forEach((id) => {
    if (!alive.has(id)) selectedIds.value.delete(id)
  })
}

// 逐台并发执行器：改分组 / 解绑共用。每台独立链路，失败一台不影响其他。
async function runBatchPerHost(
  kind: 'group' | 'unbind',
  targets: Host[],
  fn: (host: Host) => Promise<unknown>,
) {
  // 改分组/解绑不过滤目标：skipped 恒为 0（重试只重跑失败主机，不把已成功
  // 的计为跳过；升级的 skipped 另在 dispatchBatchUpgrade 里算）。
  const skipped = 0
  if (targets.length === 0) {
    message.warning('所选主机中没有可执行的目标')
    return
  }
  batchRunning.value = true
  batchKind.value = kind
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((h) => {
    opStatus.value[h.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...targets]
  async function worker() {
    while (queue.length > 0) {
      const host = queue.shift()!
      opStatus.value[host.id] = { phase: 'running' }
      try {
        await fn(host)
        opStatus.value[host.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        let msg = e.message || '执行出错'
        // 解绑时离线主机勾了卸载会报 agent offline：行内给中文原因，
        // 不直接展示服务端原文。
        if (kind === 'unbind' && msg.includes('agent offline')) {
          msg = '主机离线，无法远程卸载 Agent'
        }
        opStatus.value[host.id] = { phase: 'failed', message: msg }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, queue.length) }, () => worker()),
  )
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped }
  const verb = kind === 'group' ? '改分组' : '解绑'
  const skipText = skipped > 0 ? `，跳过 ${skipped} 台` : ''
  if (failed === 0) {
    message.success(`批量${verb}完成：成功 ${done} 台${skipText}`)
  } else {
    message.warning(`批量${verb}完成：成功 ${done} 台，失败 ${failed} 台${skipText}，可点「重试失败」只重跑失败的主机`)
  }
  await Promise.all([store.fetchList(), loadGroups(), loadNetworkNodes()])
  pruneSelection()
  checkPolling()
}

function retryBatchFailed() {
  if (!batchKind.value || !batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  const targets = store.hosts.filter((h) => failedIds.includes(h.id))
  if (targets.length === 0) return
  if (batchKind.value === 'group') {
    runBatchPerHost('group', targets, (host) => setHostGroup(host.id, lastBatchGroupChoice.value))
  } else if (batchKind.value === 'unbind') {
    runBatchPerHost('unbind', targets, (host) => deleteHost(host.id, lastBatchUninstall.value))
  } else if (batchKind.value === 'upgrade') {
    dispatchBatchUpgrade(targets)
  }
}

// ---- 批量改分组：弹窗选分组，逐台复用单机 setHostGroup
const showBatchGroupModal = ref(false)
const batchGroupChoice = ref<string[]>([])
const lastBatchGroupChoice = ref<string[]>([])

function openBatchGroupModal() {
  batchGroupChoice.value = []
  showBatchGroupModal.value = true
}

async function submitBatchGroup() {
  const groups = [...batchGroupChoice.value]
  lastBatchGroupChoice.value = groups
  showBatchGroupModal.value = false
  await runBatchPerHost('group', selectedHosts.value, (host) => setHostGroup(host.id, groups))
}

// ---- 批量解绑：弹窗内就地二次确认（DESIGN.md §9.17），逐台复用单机 deleteHost
const showBatchUnbindModal = ref(false)
const batchUninstallAgent = ref(false)
const lastBatchUninstall = ref(false)

function openBatchUnbindModal() {
  batchUninstallAgent.value = false
  showBatchUnbindModal.value = true
}

async function submitBatchUnbind() {
  const uninstall = batchUninstallAgent.value
  lastBatchUninstall.value = uninstall
  showBatchUnbindModal.value = false
  await runBatchPerHost('unbind', selectedHosts.value, (host) => deleteHost(host.id, uninstall))
}

// ---- 批量升级：复用系统升级页的批量升级模式（POST /system/upgrade-agents，
// 服务端一次下发到已选在线且版本过旧的主机；离线或已是最新版的跳过）。
function confirmBatchUpgrade() {
  const targets = batchUpgradable.value
  const skipped = selectedHosts.value.length - targets.length
  if (targets.length === 0) {
    message.warning('所选主机中没有可升级的（须在线且 Agent 版本过旧）')
    return
  }
  dialog.info({
    title: '批量升级 Agent 确认',
    content: `确定将 ${targets.length} 台主机的 Agent 升级至控制端最新版本吗？${skipped > 0 ? `其余 ${skipped} 台（离线或已是最新版）将跳过。` : ''}升级过程中 Agent 将下载最新对应平台二进制，校验并平滑重启服务。`,
    positiveText: '开始升级',
    negativeText: '取消',
    onPositiveClick: async () => {
      await dispatchBatchUpgrade(targets, skipped)
    },
  })
}

async function dispatchBatchUpgrade(targets: Host[], skipped = 0) {
  batchRunning.value = true
  batchKind.value = 'upgrade'
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((h) => {
    opStatus.value[h.id] = { phase: 'running' }
  })
  try {
    const res = await batchUpgradeAgents(targets.map((h) => h.id), false)
    targets.forEach((h) => {
      opStatus.value[h.id] = { phase: 'done' }
    })
    batchSummary.value = { done: targets.length, failed: 0, skipped }
    message.success(res.message || `已下发 ${targets.length} 台主机的升级任务`)
    await Promise.all([store.fetchList(), loadGroups(), loadNetworkNodes()])
    pruneSelection()
    checkPolling()
  } catch (e: any) {
    targets.forEach((h) => {
      opStatus.value[h.id] = { phase: 'failed', message: e.message || '下发失败' }
    })
    batchSummary.value = { done: 0, failed: targets.length, skipped }
    message.error(e.message || '批量升级失败')
  } finally {
    batchRunning.value = false
  }
}

// ---- 批量执行：跳到批量执行页并带上已选主机（不另造执行通道）。
// 单台走既有 host_id 参数；多台用 host_ids（逗号分隔），
// 待 BatchExec.vue 接上该参数后预选全部已选主机。
function goBatchExec() {
  const ids = selectedHosts.value.map((h) => h.id)
  if (ids.length === 0) return
  const query = ids.length === 1 ? `?host_id=${ids[0]}` : `?host_ids=${ids.join(',')}`
  workspace.openTab({
    key: '/batch-exec',
    title: '推送命令',
    path: `/batch-exec${query}`,
    closable: true,
  })
  router.push(`/batch-exec${query}`)
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
  // 内存按国内云厂商控制台口径：GB 标签、二进制数值（1 GB = 1024³ B），与
  // free / 任务管理器看到的数值一致（阿里云/腾讯云/华为云控制台同样写 GB，
  // 数值本身是二进制的）。小数位保留 2 位，与监控页 fmtMemBytes 一致，
  // 总量在横幅、列表与监控页三处必须显示完全一致。
  const gib = bytes / (1 << 30)
  if (gib >= 1) {
    return gib.toFixed(2) + ' GB'
  }
  const mib = bytes / (1 << 20)
  return mib.toFixed(2) + ' MB'
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
  const usedGB = used / (1 << 30)
  const pct = Math.round((usedGB / h.traffic_limit_gb) * 100)
  return `${usedGB.toFixed(1)}G/${h.traffic_limit_gb}G (${pct}%)`
}

function getTrafficTooltip(h: Host): string {
  if (!h.traffic_limit_gb) return ''
  const rxGB = ((h.month_rx || 0) / (1 << 30)).toFixed(2)
  const txGB = ((h.month_tx || 0) / (1 << 30)).toFixed(2)
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

// ---- 整页成本汇总（年化成本 + 剩余价值，统一换算为 CNY）----
// 计费周期年化系数：月付×12、季付×4、半年付×2、年付×1、两年付÷2、三年付÷3；
// 一次性 / 按量计费无法年化，不计入年化成本。
const ANNUAL_FACTOR: Record<string, number> = {
  '月付': 12, '季付': 4, '半年付': 2, '年付': 1, '两年付': 0.5, '三年付': 1 / 3,
}
// 计费周期天数（剩余价值 = 价格 × 剩余天数 ÷ 周期天数，剩余天数封顶为一个周期）。
const CYCLE_DAYS: Record<string, number> = {
  '月付': 30, '季付': 90, '半年付': 180, '年付': 365, '两年付': 730, '三年付': 1095,
}

// 汇率先用内置参考价即时出数，实时汇率到后再替换重算；实时接口不可用时保持参考价。
const cnyRates = ref<CnyRates>(FALLBACK_RATES)

const costSummary = computed(() => {
  let annualCost = 0
  let remainingValue = 0
  let counted = 0
  let excluded = 0
  let noExpiry = 0
  const annualByCur: Record<string, number> = {}
  const remainingByCur: Record<string, number> = {}
  for (const h of store.hosts) {
    if (!hasPrice(h)) continue
    const priceCny = toCny(Number(h.price), h.currency, cnyRates.value)
    if (priceCny === null) { excluded++; continue }
    const cur = (h.currency || 'CNY').toUpperCase()
    const factor = ANNUAL_FACTOR[h.billing_cycle || '']
    const cycleDays = CYCLE_DAYS[h.billing_cycle || '']
    let inAny = false
    if (factor !== undefined) {
      annualCost += priceCny * factor
      annualByCur[cur] = (annualByCur[cur] || 0) + Number(h.price) * factor
      inAny = true
    }
    if (cycleDays !== undefined) {
      if (!h.expires_at) {
        noExpiry++
      } else {
        const remainDays = Math.ceil((new Date(h.expires_at).getTime() - Date.now()) / (1000 * 3600 * 24))
        if (remainDays > 0) {
          const frac = Math.min(remainDays, cycleDays) / cycleDays
          remainingValue += priceCny * frac
          remainingByCur[cur] = (remainingByCur[cur] || 0) + Number(h.price) * frac
          inAny = true
        }
      }
    }
    if (inAny) counted++
    else excluded++
  }
  const rateNote = cnyRates.value.live
    ? `汇率 ${cnyRates.value.dateLabel}`
    : '汇率离线（内置参考价）'
  return {
    annualCost,
    remainingValue,
    note: `已计价 ${counted} 台 · 未计入 ${excluded} 台 · ${rateNote}`,
    tooltip: [
      `年化成本（已换算为 CNY）：${Object.entries(annualByCur).map(([c, v]) => `${formatMoney(v, c)}/年`).join('、') || '—'}`,
      `剩余价值（已换算为 CNY）：${Object.entries(remainingByCur).map(([c, v]) => formatMoney(v, c)).join('、') || '—'}`,
      `一次性 / 按量计费不计入年化成本；无到期日的主机不计入剩余价值${noExpiry ? `（${noExpiry} 台）` : ''}。`,
      rateNote,
    ].join('\n'),
  }
})

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
    const isOutdated = host.agent_outdated
    dialog.info({
      title: isOutdated ? '升级 Agent 确认' : '重装 Agent 确认',
      content: isOutdated
        ? `确定将主机 "${host.hostname}" 的 Agent 升级至控制端最新版本 (${host.agent_latest_version || '最新版'}) 吗？\n升级过程中 Agent 将下载最新对应平台二进制，校验并平滑重启服务。`
        : `当前主机 "${host.hostname}" 的 Agent 已是最新版本 (${host.agent_version || '最新版'})。确定重新下发安装包执行覆盖安装吗？`,
      positiveText: isOutdated ? '开始升级' : '确认重装',
      negativeText: '取消',
      onPositiveClick: async () => {
        message.loading('正在下发升级指令并等待 Agent 替换重启…', { duration: 6000 })
        try {
          const res = await upgradeAgent(host.id)
          message.success(res.message || '已成功下发升级指令，正在后台更新中！')
          await store.fetchList()
          checkPolling()
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

function getHostMenuOptions(host: Host) {
  const isOutdated = host.agent_outdated
  return [
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
      label: isOutdated ? '升级 Agent (可升级)' : '重装 Agent',
      key: 'upgrade',
      icon: () => h(NIcon, { color: isOutdated ? '#6366f1' : '#64748b' }, { default: () => h(isOutdated ? ArrowUpCircleOutline : RefreshOutline) }),
    },
    {
      label: '解绑主机',
      key: 'unbind',
      icon: () => h(NIcon, { color: '#ef4444' }, { default: () => h(TrashOutline) }),
    },
  ]
}

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

const isAnyUpgrading = computed(() => store.hosts.some((h) => h.upgrading && h.upgrade_stage !== 'error'))
let pollTimer: any = null

function checkPolling() {
  if (isAnyUpgrading.value) {
    if (!pollTimer) {
      pollTimer = setInterval(() => {
        store.fetchList().catch(() => {})
      }, 3000)
    }
  } else {
    if (pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  }
}

watch(isAnyUpgrading, () => {
  checkPolling()
})

async function refresh() {
  await Promise.all([store.fetchList(), loadGroups(), loadNetworkNodes()])
  pruneSelection()
  // 手动刷新拿到的是最新状态，旧的操作进度（已完成/失败）不再对应当前行；
  // 批量进行中时不动，等本轮跑完再由执行器收尾。
  if (!batchRunning.value) {
    opStatus.value = {}
    batchSummary.value = null
  }
  checkPolling()
}

onMounted(() => {
  workspace.setActiveKey('/hosts')
  store.fetchList().catch((e) => message.error(e.message)).finally(() => {
    listLoaded.value = true
    pruneSelection()
    checkPolling()
  })
  loadGroups()
  loadNetworkNodes()
  // 实时汇率到后替换内置参考价重算汇总；失败时静默保持参考价。
  getCnyRates().then((r) => { cnyRates.value = r })
})

onUnmounted(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
})
</script>

<template>
  <div class="baichuan-host-list-view">
    <!-- 页面顶栏：外层单文字标题 + 计数（DESIGN.md §9.3） -->
    <div class="page-header">
      <div class="header-left">
        <h2 class="page-title">主机列表</h2>
        <span v-if="listLoaded" class="total-count-text">共 {{ store.hosts.length }} 台主机</span>
      </div>
      <!-- 整页成本汇总：年化成本 + 剩余价值（统一换算为 CNY，悬停看分币种明细） -->
      <div v-if="listLoaded" class="header-right">
        <div class="cost-summary" :title="costSummary.tooltip">
          <span class="cost-item">年化成本 <strong>¥{{ formatCny(costSummary.annualCost) }}</strong>/年</span>
          <span class="cost-item">剩余价值 <strong>¥{{ formatCny(costSummary.remainingValue) }}</strong></span>
          <span class="cost-note">{{ costSummary.note }}</span>
        </div>
      </div>
    </div>

    <div class="main-table-area">
      <!-- 顶部工具栏 Toolbar -->
      <div class="toolbar-bar">
        <div class="toolbar-left">
          <NCheckbox
            :checked="pageAllChecked"
            :indeterminate="pageSomeChecked"
            :disabled="batchRunning"
            @update:checked="toggleSelectAllPage"
          >
            全选本页
          </NCheckbox>
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

      <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐台显示进度 -->
      <div v-if="selectedIds.size > 0" class="batch-bar">
        <span class="batch-info">
          已选 {{ selectedIds.size }} 台
          <span class="text-muted">（在线 {{ batchOnline.length }} 台 · 可升级 {{ batchUpgradable.length }} 台）</span>
        </span>
        <NButton
          size="small"
          type="primary"
          secondary
          :disabled="batchRunning || batchOnline.length === 0"
          @click="goBatchExec"
        >
          批量执行
        </NButton>
        <NButton
          size="small"
          type="primary"
          secondary
          :loading="batchRunning && batchKind === 'upgrade'"
          :disabled="batchRunning || batchUpgradable.length === 0"
          @click="confirmBatchUpgrade"
        >
          批量升级
        </NButton>
        <NButton
          size="small"
          :disabled="batchRunning"
          @click="openBatchGroupModal"
        >
          改分组
        </NButton>
        <NButton
          size="small"
          type="error"
          secondary
          :disabled="batchRunning"
          @click="openBatchUnbindModal"
        >
          批量解绑
        </NButton>
        <NButton
          v-if="batchSummary && batchSummary.failed > 0"
          size="small"
          type="warning"
          secondary
          :disabled="batchRunning"
          @click="retryBatchFailed"
        >
          重试失败（{{ batchSummary.failed }}）
        </NButton>
        <span v-if="batchSummary" class="batch-summary">
          成功 {{ batchSummary.done }} · 失败 {{ batchSummary.failed }}<span v-if="batchSummary.skipped > 0"> · 跳过 {{ batchSummary.skipped }}</span>
        </span>
        <NButton size="small" quaternary :disabled="batchRunning" @click="clearSelection">
          取消选择
        </NButton>
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
            v-for="host in pagedHosts"
            :key="host.id"
            :class="['host-row-item', { 'is-op-busy': isOpBusy(host.id) }]"
            @click="goDetail(host)"
          >
            <!-- 0. 批量勾选列：28px 窄列，共享列宽不变（见 DESIGN.md §9.20） -->
            <div class="col-check" @click.stop>
              <NCheckbox
                :checked="selectedIds.has(host.id)"
                :disabled="batchRunning"
                @update:checked="(v: boolean) => toggleRow(host.id, v)"
              />
            </div>

	            <!-- 1. OS Logo 图标 -->
	            <div class="col-os-logo">
	              <OsLogo :os="host.os" :distro="host.distro" :size="28" />
	            </div>

            <!-- 2. 主机名称与系统版本 -->
            <div class="col-host-info">
              <div class="host-name-row">
                <span class="host-name" :title="host.hostname">{{ host.hostname }}</span>
                <span v-if="host.groups?.length" class="group-tags">
                  <NTag v-for="g in host.groups" :key="g" size="tiny" :bordered="false" type="info">{{ g }}</NTag>
                </span>
              </div>
              <div class="host-distro">
                {{ host.distro || host.os || 'Linux' }}
              </div>
            </div>

            <!-- 3. 开机状态 Pill 标签 -->
            <div class="col-status">
              <div
                v-if="host.upgrading && host.upgrade_stage !== 'error'"
                class="uptime-pill is-upgrading"
                :title="`Agent 正在升级至 ${host.upgrade_target || '最新版'}${host.upgrade_stage ? ` (${formatUpgradeStage(host.upgrade_stage)})` : ''}`"
              >
                <NSpin :size="12" style="margin-right: 4px;" />
                升级中{{ host.upgrade_stage ? ` (${formatUpgradeStage(host.upgrade_stage)})` : '' }}
              </div>
              <div
                v-else-if="host.upgrade_stage === 'error'"
                class="uptime-pill is-error"
                :title="`Agent 升级失败: ${host.upgrade_error || '未知错误'}`"
              >
                <NIcon :component="AlertCircleOutline" style="margin-right: 4px;" />
                升级失败
              </div>
              <div
                v-else
                class="uptime-pill"
                :class="{ 'is-online': host.status === 'online', 'is-offline': host.status !== 'online' }"
              >
                {{ host.status === 'online' ? `已开机 ${fmtUptime(host)}` : '当前已离线' }}
              </div>
              <!-- 批量进度：执行期间逐行显示当前阶段，失败时显示原因（悬停看全文） -->
              <div
                v-if="opStatus[host.id]"
                class="op-progress"
                :class="`is-${opStatus[host.id].phase}`"
                :title="opStatus[host.id].message || ''"
              >
                <template v-if="opStatus[host.id].phase === 'queued'">排队中…</template>
                <template v-else-if="opStatus[host.id].phase === 'running'">执行中…</template>
                <template v-else-if="opStatus[host.id].phase === 'done'">已完成</template>
                <template v-else>失败：{{ opStatus[host.id].message || '执行出错' }}</template>
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
                :options="getHostMenuOptions(host)"
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

      <!-- 前端分页：自定义行列表不用 NDataTable 的 pagination，独立 NPagination
           固定在卡片底部（见 DESIGN.md §9.20），数据区滚动、分页条不参与滚动 -->
      <div v-if="filteredHosts.length > 0" class="table-pagination-bar">
        <NPagination
          :page="page"
          :page-size="pageSize"
          :item-count="filteredHosts.length"
          :page-sizes="pageSizes"
          :simple="isMobile"
          show-size-picker
          @update:page="handlePageChange"
          @update:page-size="handlePageSizeChange"
        />
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
          将主机 <code>{{ groupTarget?.hostname }}</code> 归入分组（可多选），用于按分组给用户授权访问；不选即移出全部分组。
        </p>
        <NSelect v-model:value="groupChoice" :options="groupAssignOptions" multiple placeholder="选择分组" />
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

    <!-- 批量改分组 Modal -->
    <NModal
      v-model:show="showBatchGroupModal"
      preset="card"
      title="批量改分组"
      style="width: 440px"
    >
      <NSpace vertical :size="12">
        <p class="guide-text">
          将已选 {{ selectedIds.size }} 台主机归入分组（可多选），用于按分组给用户授权访问；不选即移出全部分组。
        </p>
        <NSelect v-model:value="batchGroupChoice" :options="groupAssignOptions" multiple placeholder="选择分组" />
        <p v-if="!groups.length" class="guide-text">
          暂无可用分组，请先在「分组权限」页面创建分组。
        </p>
      </NSpace>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBatchGroupModal = false">取消</NButton>
          <NButton type="primary" :loading="batchRunning" @click="submitBatchGroup">保存</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 批量解绑 Modal：危险操作，弹窗内就地二次确认（DESIGN.md §9.17） -->
    <NModal
      v-model:show="showBatchUnbindModal"
      preset="card"
      title="批量解绑主机"
      style="width: 520px"
    >
      <NSpace vertical :size="12">
        <p class="guide-text">
          确定要解绑以下 {{ selectedIds.size }} 台主机吗？解绑后这些主机将从控制台移除。
        </p>
        <div class="batch-unbind-list">
          <div v-for="h in selectedHosts" :key="h.id">{{ h.hostname }}（{{ h.id }}）</div>
        </div>
        <NCheckbox v-model:checked="batchUninstallAgent">
          同时卸载 Agent 并清除其数据（不可恢复，需主机在线）
        </NCheckbox>
        <p v-if="batchUninstallAgent" class="guide-text">
          离线主机无法远程卸载，将记为失败，可稍后重试。
        </p>
      </NSpace>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBatchUnbindModal = false">取消</NButton>
          <NButton type="error" :loading="batchRunning" @click="submitBatchUnbind">确认解绑</NButton>
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
  flex-direction: column;
  height: 100%;
  min-height: 0;
  box-sizing: border-box;
  gap: 12px;

  .page-header {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;

    .header-left {
      display: flex;
      align-items: center;
      gap: 12px;

      .page-title {
        margin: 0;
        font-size: 18px;
        font-weight: 600;
        color: var(--text-primary);
      }

      .total-count-text {
        font-size: 13px;
        color: var(--text-secondary);
      }
    }

    // 整页成本汇总（年化成本 + 剩余价值，DESIGN.md §9.3 顶栏右侧汇总）
    .header-right {
      display: flex;
      align-items: center;
      min-width: 0;

      .cost-summary {
        display: flex;
        align-items: baseline;
        gap: 12px;
        font-size: 13px;
        color: var(--text-secondary);
        white-space: nowrap;

        .cost-item strong {
          color: var(--text-primary);
          font-weight: 600;
        }

        .cost-note {
          font-size: 12px;
          color: var(--text-tertiary);
        }
      }
    }
  }

  .main-table-area {
    flex: 1;
    min-width: 0;
    min-height: 0;
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
    }

    // 批量操作栏：有勾选时出现，贴在工具栏下方、列表上方（与 NetworkList.vue 同款）
    .batch-bar {
      display: flex;
      align-items: center;
      gap: 10px;
      flex-wrap: wrap;
      background: var(--n-color-modal);
      border: 1px solid var(--n-border-color);
      border-radius: 8px;
      padding: 8px 12px;

      .batch-info {
        font-size: 13px;
        font-weight: 600;
        color: var(--text-primary);

        .text-muted {
          color: var(--n-text-color-3);
          font-size: 12px;
          font-weight: 400;
        }
      }

      .batch-summary {
        font-size: 12px;
        color: var(--text-secondary);
      }
    }

    .host-list-container {
      flex: 1;
      min-height: 0;
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

          // 批量执行中的行：操作列整体置灰禁用，防单机操作与批量并发冲突
          &.is-op-busy {
            .col-actions {
              pointer-events: none;
              opacity: 0.5;
            }
          }

          // 0. 批量勾选列：28px 窄列，共享列宽不变（见 DESIGN.md §9.20）
          .col-check {
            flex: 0 0 28px;
            min-width: 0;
            display: flex;
            align-items: center;
          }

          // 1. OS Logo
          .col-os-logo {
            flex: 0 0 54px;
            display: flex;
            align-items: center;
          }

          // 2. 主机名与系统
          // 列宽必须锁死：min-width: 0 抵消 flex item 的 min-width: auto（自动最小值 =
          // 内容 min-content），否则长主机名会把整列撑大、顶偏后面所有列
          //（instance-20260909-083055 实测 253px 顶偏 +30px）。同列 .col-ips /
          // .col-billing 已有 min-width: 0，本列此前漏写。
          .col-host-info {
            flex: 0 0 190px;
            min-width: 0;
            display: flex;
            flex-direction: column;
            gap: 4px;
            padding-right: 12px;

            .host-name-row {
              display: flex;
              align-items: center;
              gap: 6px;
              min-width: 0;

              .host-name {
                font-weight: 600;
                font-size: 15px;
                color: var(--text-primary);
                letter-spacing: 0.2px;
                min-width: 0;
                white-space: nowrap;
                overflow: hidden;
                text-overflow: ellipsis;
              }

              .n-tag {
                flex-shrink: 0;
              }

              .group-tags {
                display: inline-flex;
                flex-wrap: wrap;
                gap: 4px;
                flex-shrink: 0;
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
            min-width: 0;
            display: flex;
            flex-direction: column;
            align-items: flex-start;
            justify-content: center;
            gap: 2px;
            padding-right: 12px;

            // 批量进度行：单行省略号、失败原因悬停看全文
            .op-progress {
              font-size: 12px;
              max-width: 100%;
              white-space: nowrap;
              overflow: hidden;
              text-overflow: ellipsis;

              &.is-queued,
              &.is-running {
                color: #3b82f6;
              }

              &.is-done {
                color: #059669;
              }

              &.is-failed {
                color: #dc2626;
              }
            }

            .uptime-pill {
              display: inline-flex;
              align-items: center;
              justify-content: center;
              padding: 4px 14px;
              border-radius: 14px;
              font-size: 12px;
              font-weight: 500;

              &.is-online {
                background-color: rgba(16, 185, 129, 0.12);
                color: #10b981;
              }

              &.is-offline {
                background-color: rgba(148, 163, 184, 0.15);
                color: #94a3b8;
              }

              &.is-upgrading {
                background-color: rgba(99, 102, 241, 0.12);
                color: #6366f1;
              }

              &.is-error {
                background-color: rgba(239, 68, 68, 0.12);
                color: #ef4444;
              }
            }
          }

          // 4. CPU 与内存规格（与异地组网第 4 列同宽 200px，见 DESIGN.md §9.20）
          .col-specs {
            flex: 0 0 200px;
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

  .batch-unbind-list {
    max-height: 200px;
    overflow-y: auto;
    font-size: 13px;
    color: var(--text-secondary);
    display: flex;
    flex-direction: column;
    gap: 2px;
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
    gap: 8px;

    .page-header {
      .header-left {
        .total-count-text {
          font-size: 12px;
        }
      }

      // 窄屏只保留汇总数字，隐藏说明文字
      .header-right {
        .cost-summary {
          font-size: 12px;
          gap: 8px;

          .cost-note {
            display: none;
          }
        }
      }
    }

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
          .col-check {
            order: 0;
            flex: 0 0 28px;
          }

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
