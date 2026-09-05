<script setup lang="ts">
import { ref, computed, h, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NGrid,
  NGridItem,
  NButton,
  NSelect,
  NTag,
  NIcon,
  NEmpty,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NSpin,
  NInput,
  NSpace,
  NTooltip,
  NCollapse,
  NCollapseItem,
  useMessage,
} from 'naive-ui'
import {
  WarningOutline,
  CheckmarkCircleOutline,
  ShieldCheckmarkOutline,
  ScanOutline,
  DocumentTextOutline,
  PaperPlaneOutline,
  ChatbubbleEllipsesOutline,
  CopyOutline,
} from '@vicons/ionicons5'
import { copyToClipboard } from '../../../utils/clipboard'
import { useTablePagination } from '../../../composables/useTablePagination'
import {
  triggerScan,
  listScans,
  getLatestScan,
  analyzeScanReport,
  scanReportFollowup,
  type ScanType,
  type ScanJob,
  type ScanFinding,
  type ScanReportResponse,
  type ScanRecommendationItem,
} from '../../../api/scan'

const props = defineProps<{ hostId: string }>()

const message = useMessage()
const router = useRouter()

const scanType = ref<ScanType>('baseline')
const scanning = ref(false)
const jobs = ref<ScanJob[]>([])
const currentJob = ref<ScanJob | null>(null)
const severityFilter = ref<string>('all')

// AI report state
const showReportDrawer = ref(false)
const reportLoading = ref(false)
const report = ref<ScanReportResponse | null>(null)
const reportContext = ref('') // findings JSON sent to AI, for followup grounding
const followupQuestion = ref('')
const followupLoading = ref(false)
const followupHistory = ref<{ q: string; a: string }[]>([])

const scanTypeOptions = [
  { label: '安全基线扫描', value: 'baseline' },
  { label: '入侵痕迹排查', value: 'intrusion' },
  { label: '漏洞扫描', value: 'vuln' },
]

function severityTagType(s: string): 'error' | 'warning' | 'info' | 'success' | 'default' {
  switch (s) {
    case 'critical':
      return 'error'
    case 'high':
      return 'error'
    case 'medium':
      return 'warning'
    case 'low':
      return 'info'
    default:
      return 'default'
  }
}

function severityLabel(s: string): string {
  const m: Record<string, string> = { critical: '严重', high: '高危', medium: '中危', low: '低危', info: '提示' }
  return m[s] || s
}

const findings = computed<ScanFinding[]>(() => {
  const raw = currentJob.value?.findings_json
  if (!raw) return []
  return Array.isArray(raw) ? (raw as ScanFinding[]) : []
})

const filteredFindings = computed(() => {
  if (severityFilter.value === 'all') return findings.value
  return findings.value.filter((f) => f.severity === severityFilter.value)
})

// 一次基线/漏洞扫描可能产出上百条发现（AGENTS.md 8.2）；改筛选级别时回到第一页。
const findingCount = computed(() => filteredFindings.value.length)
const { pagination: findingPagination, resetPage: resetFindingPage } = useTablePagination({
  pageSize: 20,
  rowCount: findingCount,
})
watch(severityFilter, resetFindingPage)

const stats = computed(() => {
  const counts = { critical: 0, high: 0, medium: 0, low: 0, info: 0 }
  for (const f of findings.value) {
    counts[f.severity as keyof typeof counts] = (counts[f.severity as keyof typeof counts] || 0) + 1
  }
  return counts
})

const severityFilterOptions = [
  { label: '全部', value: 'all' },
  { label: '严重', value: 'critical' },
  { label: '高危', value: 'high' },
  { label: '中危', value: 'medium' },
  { label: '低危', value: 'low' },
  { label: '提示', value: 'info' },
]

async function loadHistory() {
  try {
    const list = await listScans({ host_id: props.hostId })
    jobs.value = list || []
    // Auto-load the latest scan of the current type if present.
    const latestOfType = jobs.value.find((j) => j.type === scanType.value && j.status === 'completed')
    if (latestOfType) {
      currentJob.value = latestOfType
    }
  } catch (e: any) {
    // Silent: history is best-effort.
  }
}

async function loadLatest() {
  try {
    const latest = await getLatestScan(props.hostId)
    if (latest) {
      currentJob.value = latest
      scanType.value = latest.type
    }
  } catch (e: any) {
    // ignore
  }
}

async function runScan() {
  scanning.value = true
  currentJob.value = null
  try {
    const job = await triggerScan(props.hostId, scanType.value)
    currentJob.value = job
    message.success('扫描完成')
    await loadHistory()
  } catch (e: any) {
    message.error(e.message || '扫描失败')
  } finally {
    scanning.value = false
  }
}

function viewJob(job: ScanJob) {
  currentJob.value = job
  scanType.value = job.type
  report.value = null
  followupHistory.value = []
}

async function openAIReport() {
  if (!currentJob.value || currentJob.value.status !== 'completed') {
    message.warning('请先完成一次扫描')
    return
  }
  showReportDrawer.value = true
  reportLoading.value = true
  report.value = null
  followupHistory.value = []
  // Build findings JSON context for grounding.
  const findingsJSON = JSON.stringify(findings.value)
  reportContext.value = findingsJSON
  try {
    report.value = await analyzeScanReport(currentJob.value.type, findingsJSON, props.hostId)
  } catch (e: any) {
    message.error(e.message || 'AI 报告解读失败')
  } finally {
    reportLoading.value = false
  }
}

async function askFollowup() {
  if (!followupQuestion.value.trim()) return
  const q = followupQuestion.value
  followupLoading.value = true
  try {
    const resp = await scanReportFollowup(reportContext.value, q)
    followupHistory.value.push({ q, a: resp.answer })
    followupQuestion.value = ''
  } catch (e: any) {
    message.error(e.message || '追问失败')
  } finally {
    followupLoading.value = false
  }
}

function sendToExec(rec: ScanRecommendationItem) {
  if (!rec.command) {
    message.info('该建议无可直接下发的命令')
    return
  }
  router.push({
    name: 'batch-exec',
    query: { command: rec.command, host_id: props.hostId },
  })
}

async function copyRecCommand(cmd?: string) {
  if (!cmd) return
  const ok = await copyToClipboard(cmd)
  if (ok) {
    message.success('已复制修复命令到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

function formatTime(t?: string): string {
  if (!t) return '-'
  try {
    return new Date(t).toLocaleString('zh-CN')
  } catch {
    return t
  }
}

const findingColumns = [
  {
    title: '等级',
    key: 'severity',
    width: 80,
    render(row: ScanFinding) {
      return h(NTag, { size: 'small', type: severityTagType(row.severity), bordered: false }, { default: () => severityLabel(row.severity) })
    },
  },
  { title: '类别', key: 'category', width: 120 },
  { title: '风险项', key: 'title' },
  { title: '详情', key: 'detail', ellipsis: { tooltip: true } },
  { title: '处置建议', key: 'suggestion', ellipsis: { tooltip: true } },
]

const historyColumns = [
  {
    title: '扫描类型',
    key: 'type',
    width: 120,
    render(row: ScanJob) {
      const m: Record<string, string> = { baseline: '安全基线', intrusion: '入侵痕迹', vuln: '漏洞扫描' }
      return m[row.type] || row.type
    },
  },
  {
    title: '状态',
    key: 'status',
    width: 90,
    render(row: ScanJob) {
      const m: Record<string, string> = { completed: '已完成', running: '进行中', failed: '失败' }
      const type = row.status === 'completed' ? 'success' : row.status === 'failed' ? 'error' : 'info'
      return h(NTag, { size: 'small', type: type as any, bordered: false }, { default: () => m[row.status] || row.status })
    },
  },
  { title: '风险数', key: 'findings_count', width: 80 },
  { title: '扫描时间', key: 'started_at', width: 170, render: (row: ScanJob) => formatTime(row.started_at) },
  {
    title: '操作',
    key: 'actions',
    width: 80,
    render(row: ScanJob) {
      return h(NButton, { size: 'tiny', text: true, type: 'primary', onClick: () => viewJob(row) }, { default: () => '查看' })
    },
  },
]

onMounted(async () => {
  await loadLatest()
  await loadHistory()
})
</script>

<template>
  <div class="vulnerabilities-view">
    <!-- 顶栏与扫描触发 -->
    <div class="vuln-header-bar">
      <div class="header-title">安全扫描</div>
      <div class="header-actions">
        <NSelect
          v-model:value="scanType"
          :options="scanTypeOptions"
          size="small"
          style="width: 160px"
          :disabled="scanning"
        />
        <NButton type="primary" size="small" :loading="scanning" @click="runScan">
          <template #icon>
            <NIcon><ScanOutline /></NIcon>
          </template>
          一键扫描
        </NButton>
        <NTooltip>
          <template #trigger>
            <NButton
              size="small"
              :disabled="!currentJob || currentJob.status !== 'completed'"
              @click="openAIReport"
            >
              <template #icon>
                <NIcon><DocumentTextOutline /></NIcon>
              </template>
              AI 报告解读
            </NButton>
          </template>
          使用 AI 生成中文安全报告，含修复优先级与可执行命令
        </NTooltip>
      </div>
    </div>

    <!-- 统计指标卡片 -->
    <NGrid cols="2 s:3 m:5" :x-gap="12" :y-gap="12" responsive="screen" class="stat-cards-grid">
      <NGridItem>
        <div class="vuln-stat-card critical">
          <div class="card-icon"><NIcon size="20"><WarningOutline /></NIcon></div>
          <div class="card-info">
            <div class="num">{{ stats.critical }}</div>
            <div class="label">严重</div>
          </div>
        </div>
      </NGridItem>
      <NGridItem>
        <div class="vuln-stat-card high">
          <div class="card-icon"><NIcon size="20"><WarningOutline /></NIcon></div>
          <div class="card-info">
            <div class="num">{{ stats.high }}</div>
            <div class="label">高危</div>
          </div>
        </div>
      </NGridItem>
      <NGridItem>
        <div class="vuln-stat-card medium">
          <div class="card-icon"><NIcon size="20"><WarningOutline /></NIcon></div>
          <div class="card-info">
            <div class="num">{{ stats.medium }}</div>
            <div class="label">中危</div>
          </div>
        </div>
      </NGridItem>
      <NGridItem>
        <div class="vuln-stat-card low">
          <div class="card-icon"><NIcon size="20"><CheckmarkCircleOutline /></NIcon></div>
          <div class="card-info">
            <div class="num">{{ stats.low }}</div>
            <div class="label">低危</div>
          </div>
        </div>
      </NGridItem>
      <NGridItem>
        <div class="vuln-stat-card info">
          <div class="card-icon"><NIcon size="20"><ShieldCheckmarkOutline /></NIcon></div>
          <div class="card-info">
            <div class="num">{{ findings.length }}</div>
            <div class="label">风险项总计</div>
          </div>
        </div>
      </NGridItem>
    </NGrid>

    <!-- 当前扫描结果：表头固定，仅数据区滚动（AGENTS.md 8.2） -->
    <div class="table-card findings-card">
      <div class="table-card-header">
        <span class="section-title">
          扫描结果
          <span v-if="currentJob" class="scan-meta">
            · {{ formatTime(currentJob.started_at) }}
            <NTag
              size="tiny"
              :type="currentJob.status === 'completed' ? 'success' : currentJob.status === 'failed' ? 'error' : 'info'"
              :bordered="false"
              style="margin-left: 8px"
            >
              {{ currentJob.status === 'completed' ? '已完成' : currentJob.status === 'failed' ? '失败' : '进行中' }}
            </NTag>
          </span>
        </span>
        <NSelect
          v-model:value="severityFilter"
          :options="severityFilterOptions"
          size="small"
          style="width: 110px"
        />
      </div>
      <NDataTable
        flex-height
        :columns="findingColumns"
        :data="filteredFindings"
        :pagination="findingPagination"
        size="small"
        :bordered="false"
        :row-key="(row: ScanFinding) => row.title + row.category"
      >
        <template #empty>
          <NEmpty :description="scanning ? '扫描进行中...' : '暂无扫描结果，点击「一键扫描」开始检测'" />
        </template>
      </NDataTable>
    </div>

    <!-- 历史扫描记录：条数有限，按辅助表格限高（AGENTS.md 8.2 例外） -->
    <div v-if="jobs.length > 0" class="table-card history-card">
      <div class="section-title" style="margin-bottom: 8px">历史扫描记录</div>
      <NDataTable
        :columns="historyColumns"
        :data="jobs"
        size="small"
        :bordered="false"
        :max-height="200"
        :row-key="(row: ScanJob) => row.id"
      />
    </div>

    <!-- AI 报告解读抽屉 -->
    <NDrawer v-model:show="showReportDrawer" :width="560" placement="right">
      <NDrawerContent title="AI 安全报告解读" closable>
        <NSpin v-if="reportLoading" class="report-spin" />
        <div v-else-if="report" class="report-body">
          <!-- 摘要 -->
          <div class="report-section">
            <div class="report-section-title">风险概述</div>
            <div class="report-summary">{{ report.summary }}</div>
          </div>

          <!-- 修复优先级 -->
          <div v-if="report.priorities && report.priorities.length" class="report-section">
            <div class="report-section-title">修复优先级</div>
            <div class="priority-list">
              <div v-for="(p, i) in report.priorities" :key="i" class="priority-item">
                <NTag size="small" :type="severityTagType(p.level)" :bordered="false">{{ severityLabel(p.level) }}</NTag>
                <div class="priority-content">
                  <div class="priority-title">{{ p.title }}</div>
                  <div class="priority-reason">{{ p.reason }}</div>
                </div>
              </div>
            </div>
          </div>

          <!-- 修复建议 -->
          <div v-if="report.recommendations && report.recommendations.length" class="report-section">
            <div class="report-section-title">修复建议</div>
            <NCollapse accordion>
              <NCollapseItem
                v-for="(rec, i) in report.recommendations"
                :key="i"
                :title="rec.title"
                :name="String(i)"
              >
                <template #header-extra>
                  <NTag size="tiny" :type="severityTagType(rec.severity)" :bordered="false">{{ severityLabel(rec.severity) }}</NTag>
                </template>
                <div class="rec-steps">{{ rec.steps }}</div>
                <div v-if="rec.command" class="rec-command-block">
                  <div class="rec-command-label">修复命令：</div>
                  <pre class="rec-command-pre">{{ rec.command }}</pre>
                  <NSpace :size="8" style="margin-top: 6px">
                    <NButton
                      size="tiny"
                      type="primary"
                      ghost
                      @click="sendToExec(rec)"
                    >
                      <template #icon>
                        <NIcon><PaperPlaneOutline /></NIcon>
                      </template>
                      下发执行
                    </NButton>
                    <NButton
                      size="tiny"
                      secondary
                      @click="copyRecCommand(rec.command)"
                    >
                      <template #icon>
                        <NIcon><CopyOutline /></NIcon>
                      </template>
                      复制命令
                    </NButton>
                  </NSpace>
                </div>
              </NCollapseItem>
            </NCollapse>
          </div>

          <!-- 追问对话 -->
          <div class="report-section">
            <div class="report-section-title">
              <NIcon style="vertical-align: middle; margin-right: 4px"><ChatbubbleEllipsesOutline /></NIcon>
              追问
            </div>
            <div v-if="followupHistory.length" class="followup-list">
              <div v-for="(item, i) in followupHistory" :key="i" class="followup-item">
                <div class="followup-q">问：{{ item.q }}</div>
                <div class="followup-a">答：{{ item.a }}</div>
              </div>
            </div>
            <div v-else class="followup-empty">暂无追问，可在下方输入问题对报告内容继续提问。</div>
            <NSpace align="flex-end">
              <NInput
                v-model:value="followupQuestion"
                type="textarea"
                :autosize="{ minRows: 1, maxRows: 3 }"
                placeholder="针对报告内容提问，例如：这个漏洞在这台机器上具体怎么修？"
                @keydown.enter.prevent="askFollowup"
              />
              <NButton type="primary" :loading="followupLoading" @click="askFollowup">发送</NButton>
            </NSpace>
          </div>
        </div>
        <NEmpty v-else description="点击「AI 报告解读」生成报告" />
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped lang="scss">
.vulnerabilities-view {
  display: flex;
  flex-direction: column;
  gap: 16px;
  /* 撑满面板高度，让扫描结果表按剩余空间滚动（AGENTS.md 8.2） */
  height: 100%;
  min-height: 0;
  overflow: hidden;

  .vuln-header-bar,
  .stat-cards-grid {
    flex-shrink: 0;
  }

  .vuln-header-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;

    .header-title {
      font-size: 16px;
      font-weight: 700;
    }

    .header-actions {
      display: flex;
      gap: 12px;
    }
  }

  .stat-cards-grid {
    .vuln-stat-card {
      background-color: var(--bg-card-subtle);
      border: 1px solid var(--border-color);
      border-radius: 6px;
      padding: 14px;
      display: flex;
      align-items: center;
      gap: 12px;

      &.critical .card-icon { color: #dc2626; }
      &.high .card-icon { color: #ef4444; }
      &.medium .card-icon { color: #f59e0b; }
      &.low .card-icon { color: #10b981; }
      &.info .card-icon { color: #3b82f6; }

      .card-info {
        .num {
          font-size: 20px;
          font-weight: 700;
          line-height: 1.2;
          color: var(--text-primary);
        }
        .label {
          font-size: 12px;
          color: var(--text-secondary);
          margin-top: 2px;
        }
      }
    }
  }

  /* 扫描结果表：吃掉页面剩余高度，表头随之固定 */
  .findings-card {
    flex: 1;
    min-height: 200px;
    display: flex;
    flex-direction: column;

    :deep(.n-data-table) {
      flex: 1;
      min-height: 0;
    }
  }

  /* 历史记录为辅助表格，固定限高、不与主表争抢空间 */
  .history-card {
    flex-shrink: 0;
  }

  .table-card {
    border-radius: 6px;

    .table-card-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 8px;
      flex-shrink: 0;
    }

    .section-title {
      font-size: 14px;
      font-weight: 600;
    }

    .scan-meta {
      font-size: 12px;
      font-weight: 400;
      color: var(--text-secondary);
      margin-left: 4px;
    }
  }

  .report-spin {
    display: flex;
    justify-content: center;
    padding: 60px 0;
  }

  .report-body {
    padding: 0 4px;

    .report-section {
      margin-bottom: 20px;
    }

    .report-section-title {
      font-size: 14px;
      font-weight: 600;
      margin-bottom: 8px;
      color: var(--text-primary);
    }

    .report-summary {
      font-size: 13px;
      line-height: 1.7;
      color: var(--text-secondary);
      background: var(--bg-card-subtle);
      border-radius: 6px;
      padding: 12px;
    }

    .priority-list {
      display: flex;
      flex-direction: column;
      gap: 10px;
    }

    .priority-item {
      display: flex;
      gap: 10px;
      align-items: flex-start;

      .priority-content {
        flex: 1;
      }

      .priority-title {
        font-size: 13px;
        font-weight: 600;
      }

      .priority-reason {
        font-size: 12px;
        color: var(--text-secondary);
        margin-top: 2px;
      }
    }

    .rec-steps {
      font-size: 13px;
      line-height: 1.7;
      color: var(--text-secondary);
      margin-bottom: 8px;
    }

    .rec-command-block {
      background: var(--code-box-bg);
      border-radius: 6px;
      padding: 10px;
      margin-top: 8px;

      .rec-command-label {
        font-size: 12px;
        color: var(--text-secondary);
        margin-bottom: 4px;
      }

      .rec-command-pre {
        font-family: 'JetBrains Mono', Consolas, monospace;
        font-size: 12px;
        margin: 0 0 8px;
        white-space: pre-wrap;
        word-break: break-all;
        color: var(--text-primary);
      }
    }

    .followup-list {
      margin-bottom: 12px;

      .followup-item {
        margin-bottom: 10px;
        font-size: 13px;
        line-height: 1.6;

        .followup-q {
          color: var(--text-primary);
          font-weight: 500;
        }

        .followup-a {
          color: var(--text-secondary);
          margin-top: 2px;
        }
      }
    }

    .followup-empty {
      font-size: 12px;
      color: var(--text-secondary);
      margin-bottom: 10px;
    }
  }
}
</style>