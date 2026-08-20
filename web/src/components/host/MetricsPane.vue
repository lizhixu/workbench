<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, watch, nextTick } from 'vue'
import {
  NGrid,
  NGridItem,
  NRadioGroup,
  NRadioButton,
  NSpin,
  NTag,
  NButton,
  NIcon,
} from 'naive-ui'
import { RefreshOutline } from '@vicons/ionicons5'
import * as echarts from 'echarts'
import { getMetrics, getMetricsHistory, type MetricPoint } from '../../api/hosts'
import type { Metrics } from '../../api/types'
import { useSettingsStore } from '../../stores/settings'

const props = defineProps<{ hostId: string }>()
const settings = useSettingsStore()

type TimeSpan = 'realtime' | '1h' | '24h' | '7d'
const timeSpan = ref<TimeSpan>('realtime')

const loading = ref(true)
const errorMsg = ref('')
const currentMetrics = ref<Metrics | null>(null)

// DOM refs for charts
const cpuChartEl = ref<HTMLDivElement | null>(null)
const memChartEl = ref<HTMLDivElement | null>(null)
const netChartEl = ref<HTMLDivElement | null>(null)
const diskChartEl = ref<HTMLDivElement | null>(null)

let cpuChart: echarts.ECharts | null = null
let memChart: echarts.ECharts | null = null
let netChart: echarts.ECharts | null = null
let diskChart: echarts.ECharts | null = null

let timer: ReturnType<typeof setInterval> | null = null
let resizeObserver: ResizeObserver | null = null

// Realtime rolling cache (max 60 points)
const realtimePoints = ref<MetricPoint[]>([])

function fmtBytes(n: number): string {
  if (!n || isNaN(n)) return '0 B'
  if (n >= 1e9) return (n / 1e9).toFixed(2) + ' GB'
  if (n >= 1e6) return (n / 1e6).toFixed(2) + ' MB'
  if (n >= 1e3) return (n / 1e3).toFixed(2) + ' KB'
  return n.toFixed(0) + ' B'
}

function fmtRate(n: number): string {
  return fmtBytes(n) + '/s'
}

function fmtTime(ts: number, full = false): string {
  const d = new Date(ts * 1000)
  const pad = (v: number) => String(v).padStart(2, '0')
  const timeStr = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  if (!full) return timeStr
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${timeStr}`
}

function getBaseChartOption(title: string, sub: string, yAxisFormatter?: (v: number) => string, max?: number) {
  const isDark = settings.themeMode === 'dark'
  return {
    backgroundColor: 'transparent',
    title: {
      text: title,
      subtext: sub,
      textStyle: { color: isDark ? '#f3f4f6' : '#1e293b', fontSize: 13, fontWeight: 'bold' },
      subtextStyle: { color: isDark ? '#9ca3af' : '#64748b', fontSize: 11 },
      top: 2,
      left: 6,
    },
    tooltip: {
      trigger: 'axis',
      backgroundColor: isDark ? '#1f2937' : '#ffffff',
      borderColor: isDark ? '#374151' : '#e2e8f0',
      textStyle: { color: isDark ? '#f3f4f6' : '#1e293b', fontSize: 12 },
      axisPointer: { type: 'line', lineStyle: { color: isDark ? 'rgba(255,255,255,0.2)' : 'rgba(0,0,0,0.15)' } },
    },
    grid: {
      top: 48,
      right: 18,
      bottom: 24,
      left: 54,
    },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      axisLine: { lineStyle: { color: isDark ? 'rgba(255,255,255,0.1)' : '#e2e8f0' } },
      axisLabel: { color: isDark ? '#9ca3af' : '#64748b', fontSize: 10 },
      splitLine: { show: false },
      data: [] as string[],
    },
    yAxis: {
      type: 'value',
      max: max,
      min: 0,
      axisLine: { show: false },
      axisLabel: {
        color: isDark ? '#9ca3af' : '#64748b',
        fontSize: 10,
        formatter: yAxisFormatter || ((v: number) => v.toString()),
      },
      splitLine: { lineStyle: { color: isDark ? 'rgba(255,255,255,0.06)' : '#f1f5f9' } },
    },
  }
}

function initCharts() {
  if (cpuChartEl.value && !cpuChart) {
    cpuChart = echarts.init(cpuChartEl.value)
  }
  if (memChartEl.value && !memChart) {
    memChart = echarts.init(memChartEl.value)
  }
  if (netChartEl.value && !netChart) {
    netChart = echarts.init(netChartEl.value)
  }
  if (diskChartEl.value && !diskChart) {
    diskChart = echarts.init(diskChartEl.value)
  }
}

function updateChartsWithPoints(points: MetricPoint[]) {
  if (!points || points.length === 0) return

  const times = points.map((p) => fmtTime(p.ts, timeSpan.value === '24h' || timeSpan.value === '7d'))
  const latest = points[points.length - 1]

  // 1. CPU
  if (cpuChart) {
    const opt: any = getBaseChartOption('CPU 使用率', `当前: ${latest.cpu_usage.toFixed(1)}%`, (v) => `${v}%`, 100)
    opt.xAxis.data = times
    opt.series = [
      {
        name: 'CPU使用率',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.cpu_usage.toFixed(1)),
        itemStyle: { color: '#6366f1' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(99, 102, 241, 0.45)' },
            { offset: 1, color: 'rgba(99, 102, 241, 0.02)' },
          ]),
        },
      },
    ]
    cpuChart.setOption(opt)
  }

  // 2. Memory
  if (memChart) {
    const opt: any = getBaseChartOption(
      '内存使用率',
      `${fmtBytes(latest.mem_used)} / ${fmtBytes(latest.mem_total)} (${latest.mem_usage.toFixed(1)}%)`,
      (v) => `${v}%`,
      100,
    )
    opt.xAxis.data = times
    opt.tooltip.formatter = (params: any[]) => {
      const idx = params[0]?.dataIndex ?? 0
      const pt = points[idx]
      return `${fmtTime(pt.ts, true)}<br/>已用: ${fmtBytes(pt.mem_used)} / ${fmtBytes(pt.mem_total)}<br/>使用率: ${pt.mem_usage.toFixed(1)}%`
    }
    opt.series = [
      {
        name: '内存使用率',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.mem_usage.toFixed(1)),
        itemStyle: { color: '#10b981' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(16, 185, 129, 0.45)' },
            { offset: 1, color: 'rgba(16, 185, 129, 0.02)' },
          ]),
        },
      },
    ]
    memChart.setOption(opt)
  }

  // 3. Network
  if (netChart) {
    const opt: any = getBaseChartOption(
      '网络吞吐',
      `↓ ${fmtRate(latest.net_rx)} · ↑ ${fmtRate(latest.net_tx)}`,
      (v) => fmtBytes(v) + '/s',
    )
    opt.xAxis.data = times
    opt.tooltip.formatter = (params: any[]) => {
      const idx = params[0]?.dataIndex ?? 0
      const pt = points[idx]
      return `${fmtTime(pt.ts, true)}<br/>↓ 下行: ${fmtRate(pt.net_rx)}<br/>↑ 上行: ${fmtRate(pt.net_tx)}`
    }
    opt.legend = {
      show: true,
      right: 10,
      top: 4,
      textStyle: { color: settings.themeMode === 'dark' ? '#9ca3af' : '#64748b', fontSize: 11 },
      data: ['下行 (Rx)', '上行 (Tx)'],
    }
    opt.series = [
      {
        name: '下行 (Rx)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.net_rx),
        itemStyle: { color: '#3b82f6' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(59, 130, 246, 0.35)' },
            { offset: 1, color: 'rgba(59, 130, 246, 0.02)' },
          ]),
        },
      },
      {
        name: '上行 (Tx)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.net_tx),
        itemStyle: { color: '#8b5cf6' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(139, 92, 246, 0.35)' },
            { offset: 1, color: 'rgba(139, 92, 246, 0.02)' },
          ]),
        },
      },
    ]
    netChart.setOption(opt)
  }

  // 4. Disk
  if (diskChart) {
    const opt: any = getBaseChartOption(
      '磁盘读写',
      `读 ${fmtRate(latest.disk_read)} · 写 ${fmtRate(latest.disk_write)}`,
      (v) => fmtBytes(v) + '/s',
    )
    opt.xAxis.data = times
    opt.tooltip.formatter = (params: any[]) => {
      const idx = params[0]?.dataIndex ?? 0
      const pt = points[idx]
      return `${fmtTime(pt.ts, true)}<br/>读速率: ${fmtRate(pt.disk_read)}<br/>写速率: ${fmtRate(pt.disk_write)}`
    }
    opt.legend = {
      show: true,
      right: 10,
      top: 4,
      textStyle: { color: settings.themeMode === 'dark' ? '#9ca3af' : '#64748b', fontSize: 11 },
      data: ['读取', '写入'],
    }
    opt.series = [
      {
        name: '读取',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.disk_read),
        itemStyle: { color: '#f59e0b' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(245, 158, 11, 0.35)' },
            { offset: 1, color: 'rgba(245, 158, 11, 0.02)' },
          ]),
        },
      },
      {
        name: '写入',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => p.disk_write),
        itemStyle: { color: '#ec4899' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(236, 72, 153, 0.35)' },
            { offset: 1, color: 'rgba(236, 72, 153, 0.02)' },
          ]),
        },
      },
    ]
    diskChart.setOption(opt)
  }
}

async function fetchRealtimeMetrics() {
  try {
    const data = await getMetrics(props.hostId)
    currentMetrics.value = data
    loading.value = false
    errorMsg.value = ''

    const pt: MetricPoint = {
      ts: data.ts || Math.floor(Date.now() / 1000),
      cpu_usage: data.cpu_usage || 0,
      mem_usage: data.mem_usage || 0,
      mem_total: data.mem_total || 0,
      mem_used: data.mem_used || 0,
      net_rx: data.net_rx || 0,
      net_tx: data.net_tx || 0,
      disk_read: data.disk_read || 0,
      disk_write: data.disk_write || 0,
    }

    realtimePoints.value.push(pt)
    if (realtimePoints.value.length > 60) {
      realtimePoints.value = realtimePoints.value.slice(-60)
    }

    await nextTick()
    initCharts()
    updateChartsWithPoints(realtimePoints.value)
  } catch (e: any) {
    loading.value = false
    errorMsg.value = e.message || '获取实时监控数据失败'
  }
}

async function fetchHistoricalMetrics() {
  loading.value = true
  try {
    const now = Math.floor(Date.now() / 1000)
    let from = now - 3600
    let step = 15

    if (timeSpan.value === '1h') {
      from = now - 3600
      step = 15
    } else if (timeSpan.value === '24h') {
      from = now - 86400
      step = 60
    } else if (timeSpan.value === '7d') {
      from = now - 7 * 86400
      step = 900
    }

    const res = await getMetricsHistory(props.hostId, { from, to: now, step })
    loading.value = false
    errorMsg.value = ''

    await nextTick()
    initCharts()
    if (res.points && res.points.length > 0) {
      updateChartsWithPoints(res.points)
    } else {
      // If historical points are empty yet, fallback to recent realtime point
      if (realtimePoints.value.length > 0) {
        updateChartsWithPoints(realtimePoints.value)
      }
    }
  } catch (e: any) {
    loading.value = false
    errorMsg.value = e.message || '获取历史监控数据失败'
  }
}

function handleTimeSpanChange() {
  if (timer) {
    clearInterval(timer)
    timer = null
  }

  if (timeSpan.value === 'realtime') {
    fetchRealtimeMetrics()
    timer = setInterval(fetchRealtimeMetrics, 3000)
  } else {
    fetchHistoricalMetrics()
  }
}

function handleResize() {
  cpuChart?.resize()
  memChart?.resize()
  netChart?.resize()
  diskChart?.resize()
}

onMounted(() => {
  handleTimeSpanChange()

  window.addEventListener('resize', handleResize)
  if (typeof ResizeObserver !== 'undefined') {
    resizeObserver = new ResizeObserver(() => handleResize())
    if (cpuChartEl.value?.parentElement) {
      resizeObserver.observe(cpuChartEl.value.parentElement)
    }
  }
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
  window.removeEventListener('resize', handleResize)
  resizeObserver?.disconnect()
  cpuChart?.dispose()
  memChart?.dispose()
  netChart?.dispose()
  diskChart?.dispose()
})

watch(() => props.hostId, () => {
  realtimePoints.value = []
  handleTimeSpanChange()
})

watch(() => settings.themeMode, () => {
  handleTimeSpanChange()
})
</script>

<template>
  <div class="metrics-pane-container">
    <!-- 顶部时间跨度切换栏 -->
    <div class="metrics-sub-toolbar">
      <div class="toolbar-left">
        <NRadioGroup v-model:value="timeSpan" size="small" @update:value="handleTimeSpanChange">
          <NRadioButton value="realtime">实时 (3s)</NRadioButton>
          <NRadioButton value="1h">近 1 小时</NRadioButton>
          <NRadioButton value="24h">近 24 小时</NRadioButton>
          <NRadioButton value="7d">近 7 天</NRadioButton>
        </NRadioGroup>
        <span class="mode-hint">
          {{
            timeSpan === 'realtime'
              ? '每 3 秒实时轮询采样'
              : timeSpan === '1h'
              ? '15 秒粒度聚合'
              : timeSpan === '24h'
              ? '1 分钟粒度聚合'
              : '15 分钟粒度聚合 (保存 7 天)'
          }}
        </span>
      </div>
      <div class="toolbar-right">
        <NButton size="tiny" quaternary @click="handleTimeSpanChange">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
      </div>
    </div>

    <NSpin v-if="loading && (!cpuChart || timeSpan !== 'realtime')" class="spin-box" />

    <div v-show="!loading || cpuChart" class="grid-4-container">
      <NGrid :cols="2" :x-gap="14" :y-gap="14" responsive="screen">
        <!-- 1. CPU 监控 -->
        <NGridItem>
          <div class="chart-card">
            <div ref="cpuChartEl" class="echarts-box"></div>
          </div>
        </NGridItem>

        <!-- 2. 内存 监控 -->
        <NGridItem>
          <div class="chart-card">
            <div ref="memChartEl" class="echarts-box"></div>
          </div>
        </NGridItem>

        <!-- 3. 网络吞吐 监控 -->
        <NGridItem>
          <div class="chart-card">
            <div ref="netChartEl" class="echarts-box"></div>
          </div>
        </NGridItem>

        <!-- 4. 磁盘吞吐 监控 -->
        <NGridItem>
          <div class="chart-card">
            <div ref="diskChartEl" class="echarts-box"></div>
          </div>
        </NGridItem>
      </NGrid>
    </div>

    <NTag v-if="errorMsg" type="warning" class="error-banner">
      {{ errorMsg }}
    </NTag>
  </div>
</template>

<style scoped lang="scss">
.metrics-pane-container {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .metrics-sub-toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 2px 0 8px;

    .toolbar-left {
      display: flex;
      align-items: center;
      gap: 10px;

      .mode-hint {
        font-size: 12px;
        color: var(--text-secondary);
      }
    }
  }

  .grid-4-container {
    .chart-card {
      background-color: var(--bg-card-subtle);
      border: 1px solid var(--border-color);
      border-radius: 6px;
      padding: 10px;
      height: 220px;
      position: relative;
      box-shadow: var(--shadow-sm);

      .echarts-box {
        width: 100%;
        height: 100%;
      }
    }
  }

  .spin-box {
    display: flex;
    justify-content: center;
    padding: 80px 0;
  }

  .error-banner {
    margin-top: 8px;
  }
}
</style>
