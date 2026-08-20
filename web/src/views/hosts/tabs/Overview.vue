<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from 'vue'
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
            <div class="muted">实时采样</div>
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
}

.mono-font {
  font-family: monospace;
}
</style>
