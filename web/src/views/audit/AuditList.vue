<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
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
  NSpace,
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
  // 批量勾选列：NDataTable 勾选列宽 40px（BatchExec.vue 同款；28px 是
  // DESIGN.md §9.20 自定义行列表的口径，NDataTable 复选框 + 单元格内边距放不下）。
  { type: 'selection', width: 40 },
  {
    title: '时间',
    key: 'timestamp',
    width: 170,
    render: (row) => h('span', { class: 'mono-font' }, fmtTime(row.timestamp)),
  },
  { title: '用户', key: 'username', width: 110, ellipsis: { tooltip: true } },
  {
    title: '操作',
    key: 'action',
    width: 160,
    render: (row) => {
      const label = actionLabel(row.action, row.target_type)
      // NTag 是 inline-flex + nowrap，标签本身不会截断；Naive 的 td 默认
      // overflow: visible，超长中文 action（如「切换应用到 GitHub App 触发」）
      // 会画出单元格直接盖住「目标」列。这里让标签吃满列宽上限、在标签内部
      // 省略，完整文案走 title 悬停（单元格样式由 render 生成，需 :deep 穿透）。
      return h(
        NTag,
        { size: 'small', bordered: false, type: 'info', class: 'audit-action-tag', title: label },
        { default: () => label },
      )
    },
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
  // 筛选条件变化：已勾选的行可能已不在结果集内，按口径清空勾选
  clearSelection()
  load()
}

function resetFilters() {
  filterUser.value = ''
  filterAction.value = ''
  filterResult.value = ''
  filterRisk.value = ''
  filterRange.value = null
  page.value = 1
  clearSelection()
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

// ---- 批量导出（只导出已勾选的行，不做批量删除）-------------------------
// 服务端导出接口只支持时间范围等过滤、不支持按 id 导出，故批量导出在
// 前端对已选行生成 CSV；字段口径与服务端 ToCSV 完全一致（原始 action 码
// 与字段值，不做中文翻译；user_agent 不进导出列）。
// 勾选按 id 跨页保留：服务端分页下当前页只渲染本页勾选态，翻页不丢已选；
// 筛选条件变化时清空（search/resetFilters 里调 clearSelection）。
const selectedEntries = ref(new Map<string, AuditEntry>())
const selectedCount = computed(() => selectedEntries.value.size)
const pageCheckedKeys = computed(() =>
  entries.value.filter((e) => selectedEntries.value.has(e.id)).map((e) => e.id),
)

function onCheckedRowKeys(keys: Array<string | number>) {
  const keySet = new Set(keys.map((k) => String(k)))
  entries.value.forEach((e) => {
    if (keySet.has(e.id)) selectedEntries.value.set(e.id, e)
    else selectedEntries.value.delete(e.id)
  })
}

function clearSelection() {
  selectedEntries.value.clear()
}

// 服务端 ToCSV 同款字段引用规则：含逗号/引号/换行才加引号，引号双写。
function csvField(s: string): string {
  if (s.includes(',') || s.includes('"') || s.includes('\n')) {
    return '"' + s.replaceAll('"', '""') + '"'
  }
  return s
}

// 服务端导出时间取 RFC3339（秒级）：去掉列表接口可能带的小数秒，保留原时区偏移。
function csvTimestamp(ts: string): string {
  return ts.replace(/\.\d+/, '')
}

function exportSelectedCSV() {
  if (selectedEntries.value.size === 0) return
  const header = 'id,timestamp,username,action,target_type,target_id,risk_level,result,ip,detail'
  const rows = [...selectedEntries.value.values()].sort((a, b) =>
    a.timestamp < b.timestamp ? -1 : a.timestamp > b.timestamp ? 1 : 0,
  )
  const lines = rows.map((e) =>
    [
      e.id,
      csvTimestamp(e.timestamp),
      e.username,
      e.action,
      e.target_type,
      e.target_id || '',
      e.risk_level,
      e.result,
      e.ip || '',
      e.detail || '',
    ]
      .map(csvField)
      .join(','),
  )
  // UTF-8 BOM，与服务端导出一致，保证 Excel 打开中文不乱码。
  const blob = new Blob(['\uFEFF' + header + '\n' + lines.join('\n') + '\n'], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  const now = new Date()
  const pad = (n: number) => String(n).padStart(2, '0')
  a.href = url
  a.download = `watchman-audit-selected-${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}.csv`
  document.body.appendChild(a)
  a.click()
  window.setTimeout(() => {
    a.remove()
    URL.revokeObjectURL(url)
  }, 0)
  message.success(`已导出 ${rows.length} 条勾选记录`)
}

function showDetail(row: AuditEntry) {
  detailEntry.value = row
  showDetailModal.value = true
}

onMounted(load)
</script>

<template>
  <div class="audit-view page-flex-column">
    <div class="page-header">
      <h2 class="page-title">操作审计</h2>
      <NSpace align="center" :size="8">
        <NButton size="small" secondary type="info" @click="exportCSV">
          <template #icon><NIcon><DownloadOutline /></NIcon></template>
          导出 CSV
        </NButton>
        <NButton quaternary size="small" :loading="loading" @click="load">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          刷新
        </NButton>
      </NSpace>
    </div>

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
    </div>

    <!-- 批量操作栏：有勾选时出现，只做批量导出（批量删审计是高危操作，明确排除） -->
    <div v-if="selectedCount > 0" class="batch-bar">
      <span class="batch-info">
        已选 {{ selectedCount }} 条
        <span class="batch-sub">（勾选跨页保留，切换筛选条件会清空）</span>
      </span>
      <NButton size="small" type="primary" @click="exportSelectedCSV">
        <template #icon><NIcon><DownloadOutline /></NIcon></template>
        导出选中（{{ selectedCount }} 条）
      </NButton>
      <NButton size="small" quaternary @click="clearSelection">
        取消选择
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
        :row-key="(row: AuditEntry) => row.id"
        :checked-row-keys="pageCheckedKeys"
        :row-props="(row: AuditEntry) => ({ style: 'cursor: pointer', onClick: (e: MouseEvent) => { if ((e.target as HTMLElement).closest('.n-checkbox')) return; showDetail(row) } })"
        :scroll-x="1170"
        @update:checked-row-keys="onCheckedRowKeys"
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
  gap: 12px;
  /* 滚动交给表格自身，外层不再滚动，避免出现双层滚动条 */
  overflow: hidden;
}

.page-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: var(--text-primary);
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

/* 批量操作栏：与 NetworkList.vue 同款（有勾选时出现，贴在筛选条下方） */
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 8px 12px;
  flex-shrink: 0;

  .batch-info {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .batch-sub {
    font-size: 12px;
    font-weight: 400;
    color: var(--text-secondary);
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

/* 操作列标签：外层限制在列宽内，内层文本省略，长 action 绝不溢出到「目标」列
   （NDataTable 的 td 默认 overflow: visible，不做约束就会画出单元格）。 */
:deep(.audit-action-tag) {
  max-width: 100%;
  vertical-align: middle;

  .n-tag__content {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
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
