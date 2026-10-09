<script setup lang="ts">
import { onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch, nextTick, computed } from 'vue'
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
// 按需引入 echarts：整包 import 会把地图、3D、所有图表类型都打进来，这里只用到
// 折线图和 tooltip/grid/title 三个组件。
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, TitleComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([LineChart, GridComponent, TooltipComponent, TitleComponent, CanvasRenderer])
import { getMetrics, getMetricsHistory, type MetricPoint } from '../../api/hosts'
import type { Metrics } from '../../api/types'
import { useSettingsStore } from '../../stores/settings'
import { fmtDateTime, fmtTime as fmtTimeOfDay } from '../../utils/time'

const props = defineProps<{ hostId: string; probeEnabled?: boolean }>()
const settings = useSettingsStore()

type TimeSpan = 'realtime' | '1h' | '24h' | '7d'
const timeSpan = ref<TimeSpan>('realtime')

const loading = ref(true)
const errorMsg = ref('')
const currentMetrics = ref<Metrics | null>(null)

// Swap usage percent for the stats strip.
const swapPct = computed(() => {
  const m = currentMetrics.value
  if (!m || !m.swap_total) return null
  return (m.swap_used ?? 0) / m.swap_total * 100
})

// Load average color: warn when approaching/exceeding core count.
const loadColor = computed(() => {
  const m = currentMetrics.value
  if (!m || !m.load1) return 'var(--text-secondary)'
  return '#16a34a'
})

// DOM refs for charts
const cpuChartEl = ref<HTMLDivElement | null>(null)
const memChartEl = ref<HTMLDivElement | null>(null)
const netChartEl = ref<HTMLDivElement | null>(null)
const diskChartEl = ref<HTMLDivElement | null>(null)
const latencyChartEl = ref<HTMLDivElement | null>(null)
const lossChartEl = ref<HTMLDivElement | null>(null)

let cpuChart: echarts.ECharts | null
let memChart: echarts.ECharts | null = null
let netChart: echarts.ECharts | null = null
let diskChart: echarts.ECharts | null = null
let latencyChart: echarts.ECharts | null = null
let lossChart: echarts.ECharts | null = null

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
  return full ? fmtDateTime(d) : fmtTimeOfDay(d)
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
  if (latencyChartEl.value && !latencyChart) {
    latencyChart = echarts.init(latencyChartEl.value)
  }
  if (lossChartEl.value && !lossChart) {
    lossChart = echarts.init(lossChartEl.value)
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

  // 5 & 6. Network latency / packet loss (panel-configured probe target).
  // net_latency_ms <= 0 means "no probe data" (older agent, probing off, or
  // a round where every attempt failed); gaps are rendered as nulls.
  const hasProbeData =
    points.some((p) => (p.net_latency_ms ?? 0) > 0) ||
    points.some((p) => (p.net_loss_pct ?? 0) > 0)
  const noProbeHint = '暂无数据（等待探测或未配置测速目标）'
  const lastLatency = [...points].reverse().find((p) => (p.net_latency_ms ?? 0) > 0)
  const lastLoss = points[points.length - 1]

  if (latencyChart) {
    const opt: any = getBaseChartOption(
      '网络延迟',
      hasProbeData && lastLatency
        ? `当前: ${(lastLatency.net_latency_ms as number).toFixed(1)} ms`
        : noProbeHint,
      (v) => `${v} ms`,
    )
    opt.xAxis.data = times
    opt.tooltip.formatter = (params: any[]) => {
      const idx = params[0]?.dataIndex ?? 0
      const pt = points[idx]
      const v = pt.net_latency_ms
      return `${fmtTime(pt.ts, true)}<br/>延迟: ${v && v > 0 ? v.toFixed(1) + ' ms' : '无数据'}<br/>丢包率: ${(pt.net_loss_pct ?? 0).toFixed(1)}%`
    }
    opt.series = [
      {
        name: '网络延迟',
        type: 'line',
        smooth: true,
        showSymbol: false,
        connectNulls: false,
        data: points.map((p) =>
          p.net_latency_ms && p.net_latency_ms > 0 ? +p.net_latency_ms.toFixed(1) : null,
        ),
        itemStyle: { color: '#14b8a6' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(20, 184, 166, 0.40)' },
            { offset: 1, color: 'rgba(20, 184, 166, 0.02)' },
          ]),
        },
      },
    ]
    latencyChart.setOption(opt)
  }

  if (lossChart) {
    const opt: any = getBaseChartOption(
      '丢包率',
      hasProbeData ? `当前: ${(lastLoss.net_loss_pct ?? 0).toFixed(1)}%` : noProbeHint,
      (v) => `${v}%`,
      100,
    )
    opt.xAxis.data = times
    opt.tooltip.formatter = (params: any[]) => {
      const idx = params[0]?.dataIndex ?? 0
      const pt = points[idx]
      return `${fmtTime(pt.ts, true)}<br/>丢包率: ${(pt.net_loss_pct ?? 0).toFixed(1)}%`
    }
    opt.series = [
      {
        name: '丢包率',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: points.map((p) => (hasProbeData ? +(p.net_loss_pct ?? 0).toFixed(1) : null)),
        itemStyle: { color: '#ef4444' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(239, 68, 68, 0.40)' },
            { offset: 1, color: 'rgba(239, 68, 68, 0.02)' },
          ]),
        },
      },
    ]
    lossChart.setOption(opt)
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
      net_latency_ms: data.net_latency_ms ?? 0,
      net_loss_pct: data.net_loss_pct ?? 0,
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
  latencyChart?.resize()
  lossChart?.resize()
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

// 页签被 KeepAlive 缓存后组件不会卸载，实时轮询会在后台一直打接口。
// 切走时停掉，切回时重启并立刻取一次，图表不留一段空白。
onDeactivated(() => {
  if (timer) {
    clearInterval(timer)
    timer = null
  }
})

onActivated(() => {
  if (timeSpan.value === 'realtime' && !timer) {
    fetchRealtimeMetrics()
    timer = setInterval(fetchRealtimeMetrics, 3000)
  }
  // 隐藏期间容器尺寸可能变了，图表要重新按当前宽高绘制
  nextTick(handleResize)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
  window.removeEventListener('resize', handleResize)
  resizeObserver?.disconnect()
  cpuChart?.dispose()
  memChart?.dispose()
  netChart?.dispose()
  diskChart?.dispose()
  latencyChart?.dispose()
  lossChart?.dispose()
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

    <!-- 实时快捷指标条：负载 / Swap / 连接数 / 进程数 / CPU 型号 -->
    <div v-if="currentMetrics" class="quick-stats-strip">
      <div class="stat-chip">
        <span class="chip-label">负载 (1/5/15m)</span>
        <span class="chip-value mono" :style="{ color: loadColor }">
          {{ (currentMetrics.load1 ?? 0).toFixed(2) }} / {{ (currentMetrics.load5 ?? 0).toFixed(2) }} / {{ (currentMetrics.load15 ?? 0).toFixed(2) }}
        </span>
      </div>
      <div class="stat-chip">
        <span class="chip-label">Swap</span>
        <span class="chip-value mono">
          <template v-if="swapPct !== null">{{ fmtBytes(currentMetrics.swap_used ?? 0) }} / {{ fmtBytes(currentMetrics.swap_total ?? 0) }} ({{ swapPct.toFixed(1) }}%)</template>
          <template v-else>未启用</template>
        </span>
      </div>
      <div class="stat-chip">
        <span class="chip-label">TCP 连接</span>
        <span class="chip-value mono">{{ currentMetrics.tcp_established ?? 0 }} <span class="chip-sub">ESTABLISHED</span></span>
      </div>
      <div class="stat-chip">
        <span class="chip-label">UDP</span>
        <span class="chip-value mono">{{ currentMetrics.udp_count ?? 0 }}</span>
      </div>
      <div class="stat-chip">
        <span class="chip-label">进程数</span>
        <span class="chip-value mono">{{ currentMetrics.process_count ?? 0 }}</span>
      </div>
      <div v-if="currentMetrics.cpu_model" class="stat-chip chip-wide">
        <span class="chip-label">CPU 型号</span>
        <span class="chip-value chip-model" :title="currentMetrics.cpu_model">{{ currentMetrics.cpu_model }}</span>
      </div>
    </div>

    <!-- 月度流量统计 -->
    <div v-if="currentMetrics && (currentMetrics.month_rx || currentMetrics.month_tx)" class="monthly-traffic-card">
      <div class="traffic-header">
        <span class="traffic-title">本月流量</span>
        <span class="traffic-cycle">按账单周期累计 · Agent 重启/主机重启自动校正</span>
      </div>
      <div class="traffic-row">
        <div class="traffic-item">
          <span class="traffic-dir down">↓ 下行 (RX)</span>
          <span class="traffic-value mono">{{ fmtBytes(currentMetrics.month_rx ?? 0) }}</span>
        </div>
        <div class="traffic-item">
          <span class="traffic-dir up">↑ 上行 (TX)</span>
          <span class="traffic-value mono">{{ fmtBytes(currentMetrics.month_tx ?? 0) }}</span>
        </div>
        <div class="traffic-item">
          <span class="traffic-dir total">Σ 合计</span>
          <span class="traffic-value mono">{{ fmtBytes((currentMetrics.month_rx ?? 0) + (currentMetrics.month_tx ?? 0)) }}</span>
        </div>
      </div>
    </div>

    <div v-show="!loading || cpuChart" class="grid-4-container">
      <NGrid cols="1 s:2" :x-gap="14" :y-gap="14" responsive="screen">
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

        <!-- 5. 网络延迟 监控（需在主机配置中开启延迟/丢包监控） -->
        <NGridItem v-if="probeEnabled">
          <div class="chart-card">
            <div ref="latencyChartEl" class="echarts-box"></div>
          </div>
        </NGridItem>

        <!-- 6. 丢包率 监控（需在主机配置中开启延迟/丢包监控） -->
        <NGridItem v-if="probeEnabled">
          <div class="chart-card">
            <div ref="lossChartEl" class="echarts-box"></div>
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

  .quick-stats-strip {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;

    .stat-chip {
      display: flex;
      flex-direction: column;
      gap: 2px;
      padding: 8px 12px;
      background-color: var(--bg-card-subtle);
      border: 1px solid var(--border-color);
      border-radius: 6px;
      min-width: 120px;
      box-shadow: var(--shadow-sm);

      &.chip-wide {
        flex: 1;
        min-width: 200px;
      }

      .chip-label {
        font-size: 11px;
        color: var(--text-secondary);
      }

      .chip-value {
        font-size: 13px;
        font-weight: 600;
        color: var(--text-primary);

        &.mono {
          font-family: 'SFMono-Regular', Consolas, monospace;
        }

        .chip-sub {
          font-size: 10px;
          font-weight: 400;
          color: var(--text-secondary);
        }
      }

      .chip-model {
        font-weight: 500;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
      }
    }
  }

  .monthly-traffic-card {
    background-color: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    padding: 10px 14px;
    box-shadow: var(--shadow-sm);

    .traffic-header {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 8px;
      margin-bottom: 8px;

      .traffic-title {
        font-size: 13px;
        font-weight: 600;
      }

      .traffic-cycle {
        font-size: 11px;
        color: var(--text-secondary);
      }
    }

    .traffic-row {
      display: flex;
      flex-wrap: wrap;
      gap: 24px;

      .traffic-item {
        display: flex;
        align-items: baseline;
        gap: 8px;

        .traffic-dir {
          font-size: 12px;
          font-weight: 500;

          &.down { color: #3b82f6; }
          &.up { color: #8b5cf6; }
          &.total { color: var(--text-secondary); }
        }

        .traffic-value {
          font-size: 16px;
          font-weight: 600;
        }
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

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .metrics-pane-container {
    .metrics-sub-toolbar {
      flex-wrap: wrap;
      gap: 6px;

      .toolbar-left {
        flex-wrap: wrap;
        gap: 6px;

        .mode-hint {
          width: 100%;
        }
      }
    }

    .quick-stats-strip {
      .stat-chip {
        flex: 1 1 calc(50% - 8px);
        min-width: 0;

        &.chip-wide {
          flex: 1 1 100%;
        }
      }
    }

    .monthly-traffic-card {
      .traffic-row {
        gap: 12px;

        .traffic-item {
          flex: 1 1 100%;

          .traffic-value {
            font-size: 14px;
          }
        }
      }
    }

    .grid-4-container {
      .chart-card {
        height: 200px;
      }
    }
  }
}
</style>
