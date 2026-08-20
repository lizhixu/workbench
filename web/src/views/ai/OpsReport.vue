<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
  NSpace,
  NButton,
  NIcon,
  NSelect,
  NSpin,
  NEmpty,
  NCard,
  NTag,
  NProgress,
  useMessage,
} from 'naive-ui'
import {
  DocumentTextOutline,
  RefreshOutline,
  SparklesOutline,
  TimeOutline,
  AlertCircleOutline,
} from '@vicons/ionicons5'
import {
  generateOpsReport,
  listOpsReports,
  type OpsReport,
} from '../../api/ai'
import { listHosts } from '../../api/hosts'
import type { Host } from '../../api/types'
import { useWorkspaceStore } from '../../stores/workspace'

const message = useMessage()
const workspace = useWorkspaceStore()

const loading = ref(false)
const generating = ref(false)
const reports = ref<OpsReport[]>([])
const currentReport = ref<OpsReport | null>(null)
const hosts = ref<Host[]>([])
const selectedHostIds = ref<string[]>([])
const period = ref('24h')

const periodOptions = [
  { label: '最近 1 小时', value: '1h' },
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
]

const hostOptions = computed(() =>
  hosts.value.map((h) => ({ label: h.hostname, value: h.id })),
)

function healthColor(score: number): string {
  if (score >= 80) return '#10b981'
  if (score >= 60) return '#f59e0b'
  return '#ef4444'
}

function healthStatus(score: number): string {
  if (score >= 80) return '健康'
  if (score >= 60) return '需关注'
  return '风险'
}

async function loadReports() {
  loading.value = true
  try {
    reports.value = await listOpsReports()
    if (reports.value.length > 0 && !currentReport.value) {
      currentReport.value = reports.value[0]
    }
  } catch (e: any) {
    message.error(e.message || '加载报告列表失败')
  } finally {
    loading.value = false
  }
}

async function loadHosts() {
  try {
    const res = await listHosts()
    hosts.value = res.data || res
  } catch {
    // silent
  }
}

async function generate() {
  generating.value = true
  try {
    const report = await generateOpsReport(selectedHostIds.value, period.value)
    currentReport.value = report
    message.success('运维报告已生成')
    await loadReports()
  } catch (e: any) {
    message.error(e.message || '生成报告失败')
  } finally {
    generating.value = false
  }
}

function viewReport(r: OpsReport) {
  currentReport.value = r
}

function formatTime(t: string): string {
  if (!t) return '-'
  try {
    return new Date(t).toLocaleString('zh-CN')
  } catch {
    return t
  }
}

onMounted(() => {
  workspace.openTab({
    key: '/ai/report',
    title: '运维报告',
    path: '/ai/report',
    closable: true,
  })
  loadHosts()
  loadReports()
})
</script>

<template>
  <div class="ops-report-view">
    <!-- Header -->
    <div class="report-header">
      <div class="header-left">
        <NIcon size="20" color="#6366f1"><DocumentTextOutline /></NIcon>
        <span class="page-title">AI 运维报告</span>
        <span class="page-desc">自动聚合主机健康度、告警与资源趋势，AI 撰写摘要与建议</span>
      </div>
      <div class="header-right">
        <NSelect
          v-model:value="period"
          :options="periodOptions"
          size="small"
          style="width: 160px"
        />
        <NSelect
          v-model:value="selectedHostIds"
          :options="hostOptions"
          size="small"
          multiple
          clearable
          placeholder="全部主机"
          style="width: 240px"
        />
        <NButton type="primary" size="small" :loading="generating" @click="generate">
          <template #icon><NIcon><SparklesOutline /></NIcon></template>
          生成报告
        </NButton>
        <NButton quaternary size="small" :loading="loading" @click="loadReports">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
        </NButton>
      </div>
    </div>

    <div class="report-body">
      <!-- Left: report history list -->
      <div class="report-sidebar">
        <div class="sidebar-title">历史报告</div>
        <NSpin v-if="loading" size="small" class="sidebar-spin" />
        <NEmpty v-else-if="reports.length === 0" size="small" description="暂无报告" />
        <div
          v-for="r in reports"
          :key="r.id"
          class="report-list-item"
          :class="{ active: currentReport?.id === r.id }"
          @click="viewReport(r)"
        >
          <div class="item-time">
            <NIcon size="12"><TimeOutline /></NIcon>
            {{ formatTime(r.generated_at) }}
          </div>
          <div class="item-score">
            <span class="score-num" :style="{ color: healthColor(r.health_score) }">
              {{ r.health_score.toFixed(0) }}
            </span>
            <span class="score-unit">/100</span>
          </div>
          <div class="item-period">{{ r.period }}</div>
        </div>
      </div>

      <!-- Right: report detail -->
      <div class="report-detail">
        <NSpin v-if="generating" class="detail-spin" />
        <template v-else-if="currentReport">
          <!-- Health score card -->
          <div class="score-card">
            <div class="score-circle">
              <NProgress
                type="circle"
                :percentage="currentReport.health_score"
                :color="healthColor(currentReport.health_score)"
                :stroke-width="8"
                :show-indicator="false"
                style="width: 80px"
              />
              <div class="score-center">
                <div class="score-value" :style="{ color: healthColor(currentReport.health_score) }">
                  {{ currentReport.health_score.toFixed(0) }}
                </div>
                <div class="score-label">{{ healthStatus(currentReport.health_score) }}</div>
              </div>
            </div>
            <div class="score-info">
              <div class="info-row">
                <NTag size="small" :type="currentReport.health_score >= 80 ? 'success' : currentReport.health_score >= 60 ? 'warning' : 'error'" bordered="false">
                  {{ healthStatus(currentReport.health_score) }}
                </NTag>
                <span class="info-meta">报告周期: {{ currentReport.period }}</span>
                <span class="info-meta">主机数: {{ currentReport.host_reports.length }}</span>
                <span class="info-meta">生成时间: {{ formatTime(currentReport.generated_at) }}</span>
              </div>
              <div class="summary-text">{{ currentReport.summary }}</div>
            </div>
          </div>

          <!-- Host reports grid -->
          <div v-if="currentReport.host_reports.length" class="host-reports-section">
            <div class="section-title">各主机健康度</div>
            <div class="host-reports-grid">
              <div
                v-for="hr in currentReport.host_reports"
                :key="hr.host_id"
                class="host-report-card"
              >
                <div class="hr-header">
                  <span class="hr-hostname">{{ hr.hostname }}</span>
                  <NTag
                    v-if="hr.alerts > 0"
                    size="tiny"
                    type="error"
                    bordered="false"
                  >
                    <template #icon><NIcon><AlertCircleOutline /></NIcon></template>
                    {{ hr.alerts }}
                  </NTag>
                </div>
                <div class="hr-score-bar">
                  <NProgress
                    type="line"
                    :percentage="hr.score"
                    :color="healthColor(hr.score)"
                    :height="6"
                    :show-indicator="false"
                  />
                </div>
                <div class="hr-score-num" :style="{ color: healthColor(hr.score) }">
                  {{ hr.score.toFixed(0) }} / 100
                </div>
              </div>
            </div>
          </div>

          <!-- Suggestions -->
          <div v-if="currentReport.suggestions && currentReport.suggestions.length" class="suggestions-section">
            <div class="section-title">改进建议</div>
            <div class="suggestions-list">
              <div v-for="(s, i) in currentReport.suggestions" :key="i" class="suggestion-item">
                <span class="suggestion-num">{{ i + 1 }}</span>
                <span class="suggestion-text">{{ s }}</span>
              </div>
            </div>
          </div>
        </template>
        <NEmpty v-else description="点击「生成报告」创建运维健康报告" class="detail-empty" />
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.ops-report-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  padding: 16px;
  box-sizing: border-box;
  gap: 14px;

  .report-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-shrink: 0;

    .header-left {
      display: flex;
      align-items: center;
      gap: 8px;

      .page-title {
        font-size: 18px;
        font-weight: 700;
        color: var(--text-primary);
      }

      .page-desc {
        font-size: 12px;
        color: var(--text-secondary);
      }
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 10px;
    }
  }

  .report-body {
    flex: 1;
    min-height: 0;
    display: flex;
    gap: 14px;
  }

  .report-sidebar {
    width: 220px;
    flex-shrink: 0;
    background: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 12px;
    overflow-y: auto;

    .sidebar-title {
      font-size: 13px;
      font-weight: 600;
      color: var(--text-secondary);
      margin-bottom: 10px;
    }

    .sidebar-spin {
      display: flex;
      justify-content: center;
      padding: 20px;
    }

    .report-list-item {
      padding: 10px 12px;
      border-radius: 6px;
      cursor: pointer;
      border: 1px solid transparent;
      margin-bottom: 6px;
      transition: all 0.15s;

      &:hover {
        background: var(--bg-hover);
      }

      &.active {
        background: rgba(99, 102, 241, 0.08);
        border-color: rgba(99, 102, 241, 0.3);
      }

      .item-time {
        display: flex;
        align-items: center;
        gap: 4px;
        font-size: 11px;
        color: var(--text-secondary);
      }

      .item-score {
        margin-top: 4px;

        .score-num {
          font-size: 18px;
          font-weight: 700;
        }

        .score-unit {
          font-size: 11px;
          color: var(--text-secondary);
        }
      }

      .item-period {
        font-size: 11px;
        color: var(--text-secondary);
        margin-top: 2px;
      }
    }
  }

  .report-detail {
    flex: 1;
    min-width: 0;
    background: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 16px;
    overflow-y: auto;

    .detail-spin {
      display: flex;
      justify-content: center;
      padding: 80px;
    }

    .detail-empty {
      margin-top: 80px;
    }
  }

  .score-card {
    display: flex;
    gap: 20px;
    align-items: center;
    padding: 16px;
    background: var(--bg-card-subtle);
    border-radius: 8px;
    margin-bottom: 20px;

    .score-circle {
      position: relative;
      width: 80px;
      height: 80px;
      flex-shrink: 0;

      .score-center {
        position: absolute;
        top: 0;
        left: 0;
        width: 100%;
        height: 100%;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;

        .score-value {
          font-size: 22px;
          font-weight: 700;
          line-height: 1;
        }

        .score-label {
          font-size: 10px;
          color: var(--text-secondary);
          margin-top: 2px;
        }
      }
    }

    .score-info {
      flex: 1;

      .info-row {
        display: flex;
        align-items: center;
        gap: 12px;
        margin-bottom: 8px;

        .info-meta {
          font-size: 12px;
          color: var(--text-secondary);
        }
      }

      .summary-text {
        font-size: 13px;
        line-height: 1.7;
        color: var(--text-primary);
      }
    }
  }

  .host-reports-section {
    margin-bottom: 20px;

    .section-title {
      font-size: 14px;
      font-weight: 600;
      margin-bottom: 10px;
      color: var(--text-primary);
    }

    .host-reports-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 12px;
    }

    .host-report-card {
      padding: 12px;
      background: var(--bg-card-subtle);
      border-radius: 6px;
      border: 1px solid var(--border-color);

      .hr-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        margin-bottom: 8px;

        .hr-hostname {
          font-size: 13px;
          font-weight: 600;
          color: var(--text-primary);
        }
      }

      .hr-score-bar {
        margin-bottom: 4px;
      }

      .hr-score-num {
        font-size: 12px;
        font-weight: 600;
      }
    }
  }

  .suggestions-section {
    .section-title {
      font-size: 14px;
      font-weight: 600;
      margin-bottom: 10px;
      color: var(--text-primary);
    }

    .suggestions-list {
      display: flex;
      flex-direction: column;
      gap: 8px;
    }

    .suggestion-item {
      display: flex;
      gap: 10px;
      padding: 10px 14px;
      background: var(--bg-card-subtle);
      border-radius: 6px;

      .suggestion-num {
        flex-shrink: 0;
        width: 20px;
        height: 20px;
        border-radius: 50%;
        background: rgba(99, 102, 241, 0.15);
        color: #6366f1;
        font-size: 11px;
        font-weight: 700;
        display: flex;
        align-items: center;
        justify-content: center;
      }

      .suggestion-text {
        font-size: 13px;
        line-height: 1.6;
        color: var(--text-primary);
      }
    }
  }
}
</style>