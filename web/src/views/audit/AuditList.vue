<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import {
  NButton,
  NCard,
  NDataTable,
  NDatePicker,
  NIcon,
  NInput,
  NModal,
  NPagination,
  NSelect,
  NTag,
  NTooltip,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { RefreshOutline, SearchOutline, DownloadOutline } from '@vicons/ionicons5'
import {
  actionLabel,
  auditExportUrl,
  listAudit,
  resultLabels,
  riskLabels,
  targetTypeLabel,
  type AuditEntry,
  type AuditStats,
} from '../../api/audit'
import { listHosts } from '../../api/hosts'
import type { Host } from '../../api/types'
import { fmtDateTime } from '../../utils/time'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'AuditList' })

const message = useMessage()

const entries = ref<AuditEntry[]>([])
const stats = ref<AuditStats>({ total: 0, today: 0, high_risk: 0, failed: 0 })
const loading = ref(false)
const total = ref(0)
const page = ref(1)
const pageSize = ref(50)

// 主机映射字典：以 host_id / agent_id 为 key，映射 hostname 与 IP
const hostMap = ref<Record<string, Host>>({})

// Filters
const filterUser = ref('')
const filterAction = ref('')
const filterResult = ref('')
const filterRisk = ref('')
const filterRange = ref<[number, number] | null>(null)

const actionOptions = [
  { label: '全部操作', value: '' },
  { label: '登录控制台', value: 'login' },
  { label: '终端会话', value: 'terminal_*' },
  { label: '内网隧道', value: 'tunnel_*' },
  { label: '执行命令', value: 'exec' },
  { label: '批量推送命令', value: 'batch_exec' },
  { label: '文件操作', value: 'file_*' },
  { label: '解绑主机', value: 'host_unbind' },
  { label: 'Docker 操作', value: 'docker_*' },
  { label: '升级 Agent', value: 'agent_upgrade' },
  { label: '应用中心', value: 'app_*' },
  { label: '证书中心', value: 'cert_*' },
  { label: '用户管理', value: 'user_*' },
  { label: '分组与授权', value: 'group_*' },
  { label: '告警管理', value: 'alert_*' },
  { label: '备份任务', value: 'backup_*' },
  { label: '安全扫描', value: 'scan_trigger' },
  { label: '更新高危策略', value: 'policy_update' },
  { label: '凭证库操作', value: 'vault_op' },
]

const resultOptions = [
  { label: '全部结果', value: '' },
  { label: '成功', value: 'success' },
  { label: '失败', value: 'failed' },
  { label: '已拦截', value: 'blocked' },
]

const riskOptions = [
  { label: '全部风险', value: '' },
  { label: '高危', value: 'high' },
  { label: '中危', value: 'medium' },
  { label: '低危', value: 'low' },
]

const detailEntry = ref<AuditEntry | null>(null)
const showDetailModal = ref(false)

function fmtTime(ts?: string) {
  if (!ts) return '-'
  return fmtDateTime(ts)
}

function formatTarget(row?: AuditEntry | null) {
  if (!row || !row.target_id) return '-'
  const tid = row.target_id
  const hInfo = hostMap.value[tid]
  if (hInfo) {
    const ip = hInfo.public_ip || hInfo.internal_ip
    return ip ? `${hInfo.hostname} (${ip})` : hInfo.hostname
  }
  return tid
}

function targetTooltip(row?: AuditEntry | null) {
  if (!row || !row.target_id) return ''
  const tid = row.target_id
  const hInfo = hostMap.value[tid]
  if (hInfo) {
    const ip = hInfo.public_ip || hInfo.internal_ip
    return `主机名: ${hInfo.hostname} | IP: ${ip || '-'} | ID: ${tid}`
  }
  return tid
}

function resultTagType(result: string) {
  if (result === 'success') return 'success'
  if (result === 'blocked') return 'warning'
  return 'error'
}

function resultLabel(result: string) {
  return resultLabels[result] || result
}

function riskTag(risk: string) {
  const label = riskLabels[risk] || risk
  if (risk === 'high') return h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => label })
  if (risk === 'medium') return h(NTag, { size: 'small', type: 'warning', bordered: false }, { default: () => label })
  return h(NTag, { size: 'small', type: 'default', bordered: false }, { default: () => label })
}

const columns: DataTableColumns<AuditEntry> = [
  {
    title: '时间',
    key: 'timestamp',
    width: 170,
    render: (row) => h('span', { class: 'mono-font' }, fmtTime(row.timestamp)),
  },
  { title: '用户', key: 'username', width: 110 },
  {
    title: '操作',
    key: 'action',
    width: 140,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => actionLabel(row.action, row.target_type) }),
  },
  {
    title: '目标',
    key: 'target_id',
    width: 170,
    ellipsis: { tooltip: true },
    render: (row) => {
      const text = formatTarget(row)
      const tip = targetTooltip(row)
      if (tip && tip !== text) {
        return h(
          NTooltip,
          { trigger: 'hover' },
          {
            trigger: () => h('span', { class: 'target-text' }, text),
            default: () => tip,
          },
        )
      }
      return h('span', { class: 'target-text' }, text)
    },
  },
  {
    title: '详情',
    key: 'detail',
    minWidth: 220,
    ellipsis: { tooltip: true },
    render: (row) => row.detail || '-',
  },
  {
    title: '来源 IP',
    key: 'ip',
    width: 130,
    render: (row) => h('span', { class: 'mono-font' }, row.ip || '-'),
  },
  { title: '风险', key: 'risk_level', width: 80, render: (row) => riskTag(row.risk_level) },
  {
    title: '结果',
    key: 'result',
    width: 90,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: resultTagType(row.result) }, { default: () => resultLabel(row.result) }),
  },
]

function buildQuery(offset = (page.value - 1) * pageSize.value) {
  const q: Record<string, string | number> = {
    offset,
    page_size: pageSize.value,
  }
  if (filterUser.value.trim()) q.username = filterUser.value.trim()
  if (filterAction.value) q.action = filterAction.value
  if (filterResult.value) q.result = filterResult.value
  if (filterRisk.value) q.risk = filterRisk.value
  if (filterRange.value) {
    q.from = new Date(filterRange.value[0]).toISOString()
    q.to = new Date(filterRange.value[1]).toISOString()
  }
  return q
}

async function loadHostsMap() {
  try {
    const res = await listHosts()
    const list = res.data || res
    const map: Record<string, Host> = {}
    for (const hItem of list) {
      if (hItem.id) map[hItem.id] = hItem
      if (hItem.hostname) map[hItem.hostname] = hItem
    }
    hostMap.value = map
  } catch {
    // 忽略加载主机失败，降级展示原始 ID
  }
}

async function load() {
  loading.value = true
  try {
    const [res] = await Promise.all([
      listAudit(buildQuery()),
      Object.keys(hostMap.value).length === 0 ? loadHostsMap() : Promise.resolve(),
    ])
    entries.value = res.data
    stats.value = res.stats
    total.value = res.stats.total
  } catch (e: any) {
    message.error(e.message || '加载审计日志失败')
  } finally {
    loading.value = false
  }
}

function search() {
  page.value = 1
  load()
}

function resetFilters() {
  filterUser.value = ''
  filterAction.value = ''
  filterResult.value = ''
  filterRisk.value = ''
  filterRange.value = null
  page.value = 1
  load()
}

function handlePageChange(p: number) {
  page.value = p
  load()
}

function exportCSV() {
  const url = auditExportUrl(buildQuery(0))
  window.open(url, '_blank')
}

function showDetail(row: AuditEntry) {
  detailEntry.value = row
  showDetailModal.value = true
}

onMounted(load)
</script>

<template>
  <div class="audit-view">
    <!-- Stats cards -->
    <div class="stats-row">
      <NCard size="small" class="stat-card">
        <div class="stat-value">{{ stats.total }}</div>
        <div class="stat-label">审计记录</div>
      </NCard>
      <NCard size="small" class="stat-card">
        <div class="stat-value">{{ stats.today }}</div>
        <div class="stat-label">今日操作</div>
      </NCard>
      <NCard size="small" class="stat-card stat-high">
        <div class="stat-value">{{ stats.high_risk }}</div>
        <div class="stat-label">高危操作</div>
      </NCard>
      <NCard size="small" class="stat-card stat-fail">
        <div class="stat-value">{{ stats.failed }}</div>
        <div class="stat-label">失败/拦截</div>
      </NCard>
    </div>

    <!-- Filter bar -->
    <div class="filter-bar">
      <NInput v-model:value="filterUser" placeholder="用户名" size="small" clearable style="width: 140px" @keyup.enter="search" />
      <NSelect v-model:value="filterAction" :options="actionOptions" size="small" style="width: 150px" placeholder="操作类型" />
      <NSelect v-model:value="filterResult" :options="resultOptions" size="small" style="width: 120px" placeholder="结果" />
      <NSelect v-model:value="filterRisk" :options="riskOptions" size="small" style="width: 110px" placeholder="风险" />
      <NDatePicker
        v-model:value="filterRange"
        type="datetimerange"
        size="small"
        clearable
        style="width: 340px"
      />
      <NButton type="primary" size="small" @click="search">
        <template #icon><NIcon><SearchOutline /></NIcon></template>
        查询
      </NButton>
      <NButton size="small" @click="resetFilters">重置</NButton>
      <NButton size="small" secondary type="info" @click="exportCSV">
        <template #icon><NIcon><DownloadOutline /></NIcon></template>
        导出 CSV
      </NButton>
      <NButton quaternary size="small" :loading="loading" @click="load">
        <template #icon><NIcon><RefreshOutline /></NIcon></template>
      </NButton>
    </div>

    <!-- Table：表头与分页固定，仅数据区滚动（AGENTS.md 8.2） -->
    <NCard size="small" class="table-card table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="entries"
        :loading="loading"
        :bordered="false"
        size="small"
        :row-props="(row: AuditEntry) => ({ style: 'cursor: pointer', onClick: () => showDetail(row) })"
        :scroll-x="1100"
      />
      <div class="table-pagination-bar">
        <NPagination
          :page="page"
          :page-size="pageSize"
          :item-count="total"
          :page-sizes="[20, 50, 100]"
          show-size-picker
          @update:page="handlePageChange"
          @update:page-size="(ps: number) => { pageSize = ps; page = 1; load() }"
        />
      </div>
    </NCard>

    <!-- Detail modal -->
    <NModal v-model:show="showDetailModal" preset="card" title="审计详情" style="width: 560px">
      <template v-if="detailEntry">
        <div class="detail-grid">
          <div class="detail-label">记录 ID</div>
          <div class="mono-font">{{ detailEntry.id }}</div>
          <div class="detail-label">时间</div>
          <div class="mono-font">{{ fmtTime(detailEntry.timestamp) }}</div>
          <div class="detail-label">用户</div>
          <div>{{ detailEntry.username || '-' }}</div>
          <div class="detail-label">操作</div>
          <div>{{ actionLabel(detailEntry.action, detailEntry.target_type) }}</div>
          <div class="detail-label">目标类型</div>
          <div>{{ targetTypeLabel(detailEntry.target_type) }}</div>
          <div class="detail-label">目标</div>
          <div class="mono-font">{{ formatTarget(detailEntry) }}</div>
          <div class="detail-label">风险等级</div>
          <div>{{ riskLabels[detailEntry.risk_level] || detailEntry.risk_level }}</div>
          <div class="detail-label">结果</div>
          <div>{{ resultLabel(detailEntry.result) }}</div>
          <div class="detail-label">来源 IP</div>
          <div class="mono-font">{{ detailEntry.ip || '-' }}</div>
          <div class="detail-label">User-Agent</div>
          <div class="mono-font detail-ua">{{ detailEntry.user_agent || '-' }}</div>
          <div class="detail-label">详情</div>
          <div class="detail-text">{{ detailEntry.detail || '-' }}</div>
        </div>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.audit-view {
  display: flex;
  flex-direction: column;
  gap: 12px;
  height: 100%;
  min-height: 0;
  /* 滚动交给表格自身，外层不再滚动，避免出现双层滚动条 */
  overflow: hidden;
}

.stats-row {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 12px;
  flex-shrink: 0;

  .stat-card {
    text-align: center;

    .stat-value {
      font-size: 24px;
      font-weight: 600;
      line-height: 1.2;
    }

    .stat-label {
      font-size: 12px;
      color: var(--text-secondary, #888);
      margin-top: 4px;
    }

    &.stat-high .stat-value {
      color: #ef4444;
    }

    &.stat-fail .stat-value {
      color: #f59e0b;
    }
  }
}

.filter-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

/* flex 撑满与内部滚动由全局 .table-flex-fill 提供，这里只补页面特有的间距 */
.table-card {
  :deep(.n-card-content) {
    gap: 10px;
    padding: 0;
  }
}

.mono-font {
  font-family: 'SFMono-Regular', Consolas, monospace;
  font-size: 12px;
}

.target-text {
  font-weight: 500;
  color: var(--text-primary);
}

.detail-grid {
  display: grid;
  grid-template-columns: 90px 1fr;
  gap: 8px 12px;
  font-size: 13px;

  .detail-label {
    color: var(--text-secondary, #888);
  }

  .detail-ua {
    word-break: break-all;
    font-size: 11px;
  }

  .detail-text {
    word-break: break-all;
    white-space: pre-wrap;
  }
}

@media (max-width: 768px) {
  .stats-row {
    grid-template-columns: repeat(2, 1fr);
  }

  .filter-bar {
    :deep(.n-date-picker) {
      width: 100% !important;
    }
  }
}
</style>
