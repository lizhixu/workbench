<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, computed } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard,
  NGrid,
  NGridItem,
  NSpin,
  NProgress,
  NStatistic,
  NTag,
  useMessage,
} from 'naive-ui'
import { getMetrics } from '../../../api/hosts'
import type { Metrics } from '../../../api/types'

const route = useRoute()
const message = useMessage()

const hostId = ref(route.params.id as string)
const metrics = ref<Metrics | null>(null)
const loading = ref(true)
let timer: ReturnType<typeof setInterval> | null = null

async function fetchMetrics() {
  try {
    const data = await getMetrics(hostId.value)
    metrics.value = data
    loading.value = false
  } catch (e: any) {
    message.error(e.message || '获取监控数据失败')
    loading.value = false
  }
}

function fmtBytes(n: number): string {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + ' GB'
  if (n >= 1e6) return (n / 1e6).toFixed(1) + ' MB'
  if (n >= 1e3) return (n / 1e3).toFixed(1) + ' KB'
  return n + ' B'
}

const swapPct = computed(() => {
  const m = metrics.value
  if (!m || !m.swap_total) return null
  return ((m.swap_used ?? 0) / m.swap_total) * 100
})

onMounted(() => {
  fetchMetrics()
  timer = setInterval(fetchMetrics, 3000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="overview-view">
    <NSpin v-if="loading" />
    <div v-else-if="metrics" class="overview-content">
      <NGrid :cols="4" :x-gap="16" :y-gap="16" responsive="screen" :cols-s="2" :cols-m="4">
        <!-- CPU -->
        <NGridItem>
          <NCard title="CPU 使用率" size="small" :bordered="true">
            <NStatistic label="CPU %" :value="metrics.cpu_usage.toFixed(1)" />
            <NProgress
              :percentage="metrics.cpu_usage"
              :show-indicator="false"
              :color="metrics.cpu_usage > 80 ? '#e88080' : '#2080f0'"
              style="margin-top: 12px"
            />
            <div class="muted" :title="metrics.cpu_model || ''">
              {{ metrics.cpu_model || '实时采样' }}
            </div>
          </NCard>
        </NGridItem>

        <!-- 内存 -->
        <NGridItem>
          <NCard title="内存使用率" size="small" :bordered="true">
            <NStatistic label="内存 %" :value="metrics.mem_usage.toFixed(1)" />
            <NProgress
              :percentage="metrics.mem_usage"
              :show-indicator="false"
              :color="metrics.mem_usage > 80 ? '#e88080' : '#52c41a'"
              style="margin-top: 12px"
            />
            <div class="muted">
              {{ fmtBytes(metrics.mem_used) }} / {{ fmtBytes(metrics.mem_total) }}
            </div>
          </NCard>
        </NGridItem>

        <!-- 网络吞吐 -->
        <NGridItem>
          <NCard title="网络吞吐" size="small" :bordered="true">
            <NStatistic label="双向 (KB/s)" :value="((metrics.net_rx + metrics.net_tx) / 1024).toFixed(1)" />
            <NProgress
              :percentage="Math.min(100, (metrics.net_rx + metrics.net_tx) / 1000)"
              :show-indicator="false"
              color="#3b82f6"
              style="margin-top: 12px"
            />
            <div class="muted">上行 / 下行 · 实时</div>
          </NCard>
        </NGridItem>

        <!-- 磁盘吞吐 -->
        <NGridItem>
          <NCard title="磁盘吞吐" size="small" :bordered="true">
            <NStatistic label="读写 (KB/s)" :value="((metrics.disk_read + metrics.disk_write) / 1024).toFixed(1)" />
            <NProgress
              :percentage="Math.min(100, (metrics.disk_read + metrics.disk_write) / 1000)"
              :show-indicator="false"
              color="#f59e0b"
              style="margin-top: 12px"
            />
            <div class="muted">读 / 写 · 实时</div>
          </NCard>
        </NGridItem>
      </NGrid>

      <!-- 扩展指标：负载 / Swap / 连接 / 进程 -->
      <NGrid :cols="4" :x-gap="16" :y-gap="16" responsive="screen" :cols-s="2" :cols-m="4" style="margin-top: 16px">
        <NGridItem>
          <NCard title="系统负载" size="small" :bordered="true">
            <NStatistic label="load avg (1m)" :value="(metrics.load1 ?? 0).toFixed(2)" />
            <div class="muted mono-font">
              1m {{ (metrics.load1 ?? 0).toFixed(2) }} · 5m {{ (metrics.load5 ?? 0).toFixed(2) }} · 15m {{ (metrics.load15 ?? 0).toFixed(2) }}
            </div>
          </NCard>
        </NGridItem>

        <NGridItem>
          <NCard title="Swap 交换分区" size="small" :bordered="true">
            <template v-if="swapPct !== null">
              <NStatistic label="Swap %" :value="swapPct.toFixed(1)" />
              <div class="muted">{{ fmtBytes(metrics.swap_used ?? 0) }} / {{ fmtBytes(metrics.swap_total ?? 0) }}</div>
            </template>
            <template v-else>
              <NStatistic label="未启用" value="—" />
            </template>
          </NCard>
        </NGridItem>

        <NGridItem>
          <NCard title="网络连接" size="small" :bordered="true">
            <NStatistic label="TCP ESTABLISHED" :value="metrics.tcp_established ?? 0" />
            <div class="muted">UDP 套接字 {{ metrics.udp_count ?? 0 }} 个</div>
          </NCard>
        </NGridItem>

        <NGridItem>
          <NCard title="进程数" size="small" :bordered="true">
            <NStatistic label="运行中进程" :value="metrics.process_count ?? 0" />
            <div class="muted">实时快照</div>
          </NCard>
        </NGridItem>
      </NGrid>

      <!-- 月度流量 -->
      <NCard
        v-if="metrics.month_rx || metrics.month_tx"
        title="本月流量"
        size="small"
        :bordered="true"
        style="margin-top: 16px"
      >
        <div class="traffic-row">
          <span class="traffic-item">↓ 下行 <b class="mono-font">{{ fmtBytes(metrics.month_rx ?? 0) }}</b></span>
          <span class="traffic-item">↑ 上行 <b class="mono-font">{{ fmtBytes(metrics.month_tx ?? 0) }}</b></span>
          <span class="traffic-item">Σ 合计 <b class="mono-font">{{ fmtBytes((metrics.month_rx ?? 0) + (metrics.month_tx ?? 0)) }}</b></span>
          <span class="traffic-hint">按账单周期累计 · 重启自动校正</span>
        </div>
      </NCard>

      <!-- 挂载点容量条 -->
      <NCard title="挂载点容量" size="small" :bordered="true">
        <NGrid :cols="3" :x-gap="12" :y-gap="8">
          <NGridItem v-for="m in metrics.mounts" :key="m.path">
            <div class="mount-bar">
              <div class="mount-label">{{ m.path }}</div>
              <div class="mount-progress">
                <NProgress
                  :percentage="m.total ? (m.used / m.total) * 100 : 0"
                  :show-indicator="false"
                  :height="6"
                />
                <div class="mount-info">
                  {{ fmtBytes(m.used) }} / {{ fmtBytes(m.total) }}
                </div>
              </div>
            </div>
          </NGridItem>
        </NGrid>
      </NCard>
    </div>
    <NTag v-else type="warning">暂无监控数据</NTag>
  </div>
</template>

<style scoped lang="scss">
.overview-content {
  .mount-bar {
    margin-bottom: 8px;

    .mount-label {
      font-size: 12px;
      color: #9ca3af;
      margin-bottom: 2px;
    }

    .mount-progress {
      display: flex;
      align-items: center;
      gap: 8px;
    }
  }
}

.muted {
  font-size: 12px;
  color: #9ca3af;
  margin-top: 6px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.mono-font {
  font-family: monospace;
}

.traffic-row {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 24px;

  .traffic-item {
    font-size: 13px;

    b {
      font-size: 16px;
      margin-left: 4px;
    }
  }

  .traffic-hint {
    font-size: 11px;
    color: #9ca3af;
    margin-left: auto;
  }
}
</style>
