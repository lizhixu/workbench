<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import {
  NCard, NDataTable, NEmpty, NButton, NModal, NSpace, NTag,
  NPopconfirm, useMessage, NIcon, NPagination, NSpin, NSwitch,
  NRadioGroup, NRadioButton, NTooltip,
} from 'naive-ui'
import {
  PlayOutline, TrashOutline, RefreshOutline, FlashOutline, InformationCircleOutline, StopOutline,
} from '@vicons/ionicons5'
import { listSessions, deleteSession, closeTerminal, sessionRecordingUrl } from '../../api/hosts'
import type { SessionRecord } from '../../api/types'
import { fmtDateTime } from '../../utils/time'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'SessionList' })

const message = useMessage()
const sessions = ref<SessionRecord[]>([])
const loading = ref(false)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

// ---- 批量结束 / 批量删除（范式见 NetworkList.vue） ----
// 勾选按会话 id 存、跨页保留；执行期 3 路并发、逐行显示进度、失败可重试。
// 服务端分页：全量行不在内存，勾选时把行快照进 selectedRows，批量执行才能
// 覆盖跨页已选；切回某页时 load() 的数据会刷新快照里的 live 状态；
// 跨页行的 live 快照可能过期，服务端会以真实状态拒绝（逐条记失败原因）。
const selectedRows = ref(new Map<string, SessionRecord>())
const selectedSessionIds = computed(() => new Set(selectedRows.value.keys()))
const BATCH_CONCURRENCY = 3
type BatchPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchKind = ref<'close' | 'delete' | null>(null)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
const showBatchDeleteModal = ref(false)

// Replay modal & Player controls
const showReplay = ref(false)
const replaySession = ref<SessionRecord | null>(null)
const replayContainer = ref<HTMLElement | null>(null)
const replayLoading = ref(false)
const replayError = ref('')
const cachedCastText = ref('')
const cachedHeader = ref<any>(null)
let playerInstance: any = null
let replayRequestSeq = 0

// 播放控制状态
const playbackSpeed = ref<number>(1.0)
const skipIdle = ref<boolean>(true) // 默认开启智能空闲压缩

// 录像元数据分析
interface ReplayStats {
  sessionSec: number   // 会话挂载总时长 (EndedAt - StartedAt)
  activeSec: number    // 录像实际有输出的活跃时刻
  idleSec: number      // 空闲挂机时长
  eventCount: number   // 事件帧数
}
const replayStats = ref<ReplayStats | null>(null)


function fmtDuration(sec: number): string {
  if (sec === 0) return '0s'
  if (!sec || isNaN(sec)) return '-'
  if (sec < 1) return '<1s'
  if (sec < 60) return `${sec}s`
  const m = Math.floor(sec / 60)
  const s = sec % 60
  if (m < 60) return `${m}m${s}s`
  const h = Math.floor(m / 60)
  return `${h}h${m % 60}m`
}

const columns = [
  {
    // 批量勾选列：NDataTable 复选框列取 40px（BatchExec.vue 同款口径；
    // DESIGN.md §9.20 的 28px 是 flex 行列表口径，复选框 + 单元格内边距放不下）。
    type: 'selection' as const,
    width: 40,
    disabled: () => batchRunning.value,
  },
  { title: '主机', key: 'hostname', width: 150, ellipsis: { tooltip: true } },
  { title: '操作者', key: 'operator', width: 100 },
  {
    title: '状态',
    key: 'live',
    width: 80,
    render: (row: SessionRecord) =>
      h('div', null, [
        row.live
          ? h(NTag, { type: 'success', size: 'small', round: true }, { default: () => '进行中' })
          : h(NTag, { type: 'default', size: 'small', round: true }, { default: () => '已结束' }),
        batchProgressNode(row),
      ]),
  },
  { title: '开始时间', key: 'started_at', width: 170, render: (row: SessionRecord) => fmtDateTime(row.started_at) },
  { title: '结束时间', key: 'ended_at', width: 170, render: (row: SessionRecord) => fmtDateTime(row.ended_at) },
  { title: '时长', key: 'duration_sec', width: 80, render: (row: SessionRecord) => fmtDuration(row.duration_sec) },
  {
    title: '操作',
    key: 'actions',
    width: 190,
    render: (row: SessionRecord) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(NButton, {
            size: 'small',
            quaternary: true,
            disabled: row.live,
            onClick: () => openReplay(row),
          }, {
            icon: () => h(NIcon, { component: PlayOutline }),
            default: () => '回放',
          }),
          ...(row.live ? [h(NPopconfirm, { onPositiveClick: () => doClose(row.id) }, {
            trigger: () => h(NButton, { size: 'small', quaternary: true, type: 'warning' }, {
              icon: () => h(NIcon, { component: StopOutline }),
              default: () => '结束',
            }),
            default: () => '确认结束该终端会话？正在连接的浏览器会断开，Agent 上的 shell 会被关闭。',
          })] : []),
          h(NPopconfirm, { onPositiveClick: () => doDelete(row.id) }, {
            trigger: () => h(NButton, { size: 'small', quaternary: true, type: 'error' }, {
              icon: () => h(NIcon, { component: TrashOutline }),
            }),
            default: () => '确认删除该会话记录？',
          }),
        ],
      }),
  },
]

import { h } from 'vue'

async function load() {
  loading.value = true
  try {
    const res = await listSessions({ offset: (page.value - 1) * pageSize.value, page_size: pageSize.value })
    sessions.value = res.data || []
    total.value = res.total || 0
    // 切回某页时用最新数据刷新勾选快照里的 live 状态。
    sessions.value.forEach((s) => {
      if (selectedRows.value.has(s.id)) selectedRows.value.set(s.id, s)
    })
    // 刷新清掉上一批的旧进度；批量执行中不清除（runBatch 在执行完才刷新，
    // 此时 batchRunning 仍为 true，进度得以保留到汇总展示）。
    if (!batchRunning.value) {
      opStatus.value = {}
      batchSummary.value = null
    }
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

function handlePageChange(p: number) {
  page.value = p
  load()
}

function handlePageSizeChange(ps: number) {
  pageSize.value = ps
  page.value = 1
  load()
}

async function mountPlayer() {
  if (!replayContainer.value || !cachedCastText.value || !cachedHeader.value) return
  if (playerInstance) {
    try { playerInstance.dispose() } catch {}
    playerInstance = null
  }
  replayContainer.value.replaceChildren()
  const { create } = await import('asciinema-player')
  playerInstance = create(
    { data: cachedCastText.value, parser: 'asciicast' },
    replayContainer.value,
    {
      cols: Number(cachedHeader.value.width) || 120,
      rows: Number(cachedHeader.value.height) || 30,
      autoPlay: true,
      theme: 'monokai',
      controls: true,
      fit: 'width',
      terminalFontSize: '14px',
      // 当开启空闲压缩时，超过 2 秒的挂机停顿自动压缩为 2 秒跳过，避免长时间静止
      idleTimeLimit: skipIdle.value ? 2 : undefined,
      speed: playbackSpeed.value,
    },
  )
}

function initReplay(row: SessionRecord) {
  const requestSeq = ++replayRequestSeq
  replayLoading.value = true
  replayError.value = ''
  replayStats.value = null

  void (async () => {
    try {
      if (!replayContainer.value) throw new Error('回放容器尚未就绪')
      const url = sessionRecordingUrl(row.id)
      const response = await fetch(url, { cache: 'no-store' })
      if (!response.ok) throw new Error(`录像接口返回 HTTP ${response.status}`)
      const castText = await response.text()
      const lines = castText.trim().split(/\r?\n/).filter(Boolean)
      if (lines.length < 2) throw new Error('录像没有可回放的输出事件')

      let header: any
      try {
        header = JSON.parse(lines[0])
      } catch {
        throw new Error('录像头不是有效 JSON')
      }
      if (!header || typeof header !== 'object' || header.version !== 2) {
        throw new Error('录像不是 asciicast v2 格式')
      }

      // 分析录像的真实活跃时间与空闲停顿
      let maxOutputTime = 0
      let eventCount = 0
      for (const line of lines.slice(1)) {
        let event: any
        try {
          event = JSON.parse(line)
        } catch {
          throw new Error('录像事件不是有效 JSON')
        }
        if (!Array.isArray(event) || event.length !== 3 || typeof event[1] !== 'string') {
          throw new Error('录像事件格式无效')
        }
        if (event[1] === 'o') {
          eventCount++
          const t = Number(event[0])
          // 如果该帧有实际文本输出内容（非心跳空帧），记录实际操作的最晚时刻
          if (t > maxOutputTime && event[2] && event[2].length > 0) {
            maxOutputTime = t
          }
        }
      }

      const sessionSec = row.duration_sec || 0
      // 真实活跃操作时长（最后一笔有数据的输出时刻）
      const activeSec = maxOutputTime > 0 && maxOutputTime < 1 ? Number(maxOutputTime.toFixed(1)) : Math.round(maxOutputTime)
      const totalSec = Math.max(sessionSec, Math.round(activeSec))
      const idleSec = Math.max(0, totalSec - Math.round(activeSec))
      replayStats.value = {
        sessionSec: totalSec,
        activeSec,
        idleSec,
        eventCount,
      }

      cachedCastText.value = castText
      cachedHeader.value = header

      if (requestSeq !== replayRequestSeq || !replayContainer.value) return
      await mountPlayer()
      replayLoading.value = false
    } catch (e: any) {
      if (requestSeq !== replayRequestSeq) return
      replayLoading.value = false
      replayError.value = e?.message || '录像文件不可用'
      message.error('回放加载失败: ' + replayError.value)
    }
  })()
}

function openReplay(row: SessionRecord) {
  replaySession.value = row
  replayError.value = ''
  replayLoading.value = true
  showReplay.value = true
}

// NModal 内容挂载后 replayContainer 才从 null 变为元素。监听这个事实比 after-enter
// 或固定 nextTick 更可靠，也不会启动两个 initReplay 互相取消。
watch(replayContainer, (container) => {
  if (container && replaySession.value && !playerInstance) {
    initReplay(replaySession.value)
  }
})

function handleReplayEnter() {
  // 兼容某些过渡配置下 ref watcher 延迟的情况；已有 player 时不重复初始化。
  if (replaySession.value && replayContainer.value && !playerInstance) {
    initReplay(replaySession.value)
  }
}

function closeReplay() {
  if (playerInstance) {
    try { playerInstance.dispose() } catch {}
    playerInstance = null
  }
  showReplay.value = false
  replaySession.value = null
  cachedCastText.value = ''
  cachedHeader.value = null
  replayStats.value = null
}

// 切换播放倍速或切换空闲压缩时，平滑重新应用到当前播放器
watch([playbackSpeed, skipIdle], () => {
  if (showReplay.value && cachedCastText.value && !replayLoading.value) {
    mountPlayer()
  }
})

async function doClose(id: string) {
  try {
    await closeTerminal(id)
    message.success('会话已结束')
    selectedRows.value.delete(id)
    load()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doDelete(id: string) {
  try {
    await deleteSession(id)
    message.success('已删除')
    selectedRows.value.delete(id)
    // If the last row on a non-first page was removed, step back a page.
    if (sessions.value.length === 1 && page.value > 1) {
      page.value -= 1
    }
    load()
  } catch (e: any) {
    message.error(e.message)
  }
}

// ---- 批量结束 / 批量删除（NetworkList.vue 范式：前端并发编排、复用单机接口） ----

// 批量执行行内进度（NetworkList .op-progress 同款文案与色板）：render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透（见文末样式）。
function batchProgressNode(row: { id: string }) {
  const st = opStatus.value[row.id]
  if (!st) return null
  const text =
    st.phase === 'queued' ? '排队中…'
    : st.phase === 'running' ? '执行中…'
    : st.phase === 'done' ? '已完成'
    : `失败：${st.message || '执行出错'}`
  return h('div', { class: ['batch-op-progress', `is-${st.phase}`], title: st.message || '' }, text)
}

const selectedSessions = computed(() => [...selectedRows.value.values()])
// 只有进行中的会话能结束，单机「结束」按钮同款准入（见 columns 操作列）。
// 跨页行的 live 取勾选时的快照，过期时由服务端最终校验、逐条记失败原因。
const batchCloseable = computed(() => selectedSessions.value.filter((s) => s.live))

function onSessionChecked(keys: (string | number)[]) {
  const checked = new Set(keys.map((k) => String(k)))
  sessions.value.forEach((s) => {
    if (checked.has(s.id)) selectedRows.value.set(s.id, s)
    else selectedRows.value.delete(s.id)
  })
}

async function runBatch(kind: 'close' | 'delete', targets: SessionRecord[]) {
  const eligible = kind === 'close' ? targets.filter((s) => s.live) : targets
  const skipped = targets.length - eligible.length
  if (eligible.length === 0) {
    message.warning(kind === 'close' ? '所选会话中没有进行中的（只有进行中的会话能结束）' : '所选会话中没有可删除的')
    return
  }
  batchRunning.value = true
  batchKind.value = kind
  batchSummary.value = null
  opStatus.value = {}
  eligible.forEach((s) => {
    opStatus.value[s.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...eligible]
  async function worker() {
    while (queue.length > 0) {
      const session = queue.shift()!
      opStatus.value[session.id] = { phase: 'running' }
      try {
        // 结束走既有关闭终端会话接口（AGENTS.md §8.5 第 9 条：只有会话
        // 操作人或 admin 能调，越权时服务端 403、逐条记失败原因）；
        // 删除与结束的审计走既有单机接口的审计，无需新埋点。
        if (kind === 'close') await closeTerminal(session.id)
        else await deleteSession(session.id)
        opStatus.value[session.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[session.id] = { phase: 'failed', message: e.message || '执行出错' }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, eligible.length) }, () => worker()),
  )
  // 批量执行完再刷新列表（batchRunning 仍为 true，load 的清进度守卫不触发）。
  // 删除成功后已删 id 从勾选中移除（服务端分页无法靠刷新核验远端是否存在，
  // 成功即已知不存在）；结束成功后行保留在列表内，勾选不动。
  if (kind === 'delete') {
    Object.entries(opStatus.value).forEach(([id, st]) => {
      if (st.phase === 'done') selectedRows.value.delete(id)
    })
  }
  await load()
  // 删空当前页时退回上一页，避免空白页（单机 doDelete 同款处理）。
  if (sessions.value.length === 0 && page.value > 1) {
    page.value -= 1
    await load()
  }
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped }
  const verb = kind === 'close' ? '结束' : '删除'
  const skipText = skipped > 0 ? `，跳过 ${skipped} 条` : ''
  if (failed === 0) {
    message.success(`批量${verb}完成：成功 ${done} 条${skipText}`)
  } else {
    message.warning(`批量${verb}完成：成功 ${done} 条，失败 ${failed} 条${skipText}，可点「重试失败」只重跑失败的会话`)
  }
}

function retryBatchFailed() {
  if (!batchKind.value || !batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  // 重试只重跑失败项：快照里有的直接取，当前页新出现失败行时以页内行为准。
  const failedRows = sessions.value.filter((s) => failedIds.includes(s.id))
  const snapshotRows = selectedSessions.value.filter((s) => failedIds.includes(s.id))
  const merged = new Map<string, SessionRecord>()
  snapshotRows.forEach((s) => merged.set(s.id, s))
  failedRows.forEach((s) => merged.set(s.id, s))
  runBatch(batchKind.value, [...merged.values()])
}

function confirmBatchDelete() {
  showBatchDeleteModal.value = false
  runBatch('delete', selectedSessions.value)
}

function clearBatchSelection() {
  selectedRows.value.clear()
  opStatus.value = {}
  batchSummary.value = null
  batchKind.value = null
}

onMounted(load)

onBeforeUnmount(() => {
  if (playerInstance) {
    try { playerInstance.dispose() } catch {}
    playerInstance = null
  }
})
</script>

<template>
  <div class="session-view page-flex-column">
    <div class="session-toolbar">
      <h2 class="page-title">会话审计</h2>
      <NSpace align="center" :size="12">
        <span class="result-count">共 {{ total }} 条会话记录</span>
        <NButton @click="load" :loading="loading">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
      </NSpace>
    </div>

    <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐行显示进度 -->
    <div v-if="selectedSessionIds.size > 0" class="batch-bar">
      <span class="batch-info">
        已选 {{ selectedSessionIds.size }} 条会话
        <span class="text-muted">（进行中 {{ batchCloseable.length }} 条）</span>
      </span>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'close'"
        :disabled="batchRunning || batchCloseable.length === 0"
        @click="runBatch('close', selectedSessions)"
      >
        批量结束
      </NButton>
      <NButton
        size="small"
        type="error"
        secondary
        :loading="batchRunning && batchKind === 'delete'"
        :disabled="batchRunning"
        @click="showBatchDeleteModal = true"
      >
        批量删除
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
      <NButton size="small" quaternary :disabled="batchRunning" @click="clearBatchSelection">
        取消选择
      </NButton>
    </div>

    <NCard class="table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="sessions"
        :bordered="false"
        size="small"
        :loading="loading"
        :row-key="(row: SessionRecord) => row.id"
        :checked-row-keys="[...selectedSessionIds]"
        @update:checked-row-keys="onSessionChecked"
      >
        <template #empty>
          <NEmpty description="暂无会话记录。打开终端会话后，录像将自动出现在这里。" />
        </template>
      </NDataTable>
      <div class="table-pagination-bar">
        <NPagination
          :page="page"
          :page-size="pageSize"
          :item-count="total"
          :page-sizes="[20, 50, 100]"
          show-size-picker
          @update:page="handlePageChange"
          @update:page-size="handlePageSizeChange"
        />
      </div>
    </NCard>
  </div>

  <!-- Replay modal -->
  <NModal
    v-model:show="showReplay"
    preset="card"
    :title="`终端会话回放 - ${replaySession?.hostname || ''}`"
    style="width: 960px"
    @after-enter="handleReplayEnter"
    @after-leave="closeReplay"
  >
    <!-- 控制与元数据栏 -->
    <div class="replay-control-bar">
      <!-- 左侧：会话与活跃时长分析 -->
      <div class="stat-group" v-if="replayStats">
        <div class="stat-item">
          <span class="stat-label">会话总长:</span>
          <strong class="stat-val">{{ fmtDuration(replayStats.sessionSec) }}</strong>
        </div>
        <div class="stat-item">
          <span class="stat-label">键盘操作:</span>
          <span class="stat-val active-val">{{ fmtDuration(replayStats.activeSec) }}</span>
          <span class="stat-sub">({{ replayStats.eventCount }} 帧输出)</span>
        </div>
        <div class="stat-item" v-if="replayStats.idleSec > 5">
          <NTag size="small" type="warning" :bordered="false" round>
            <template #icon><NIcon :component="FlashOutline" /></template>
            含闲置挂机 {{ fmtDuration(replayStats.idleSec) }}
          </NTag>
        </div>
      </div>
      <div v-else-if="replaySession" class="stat-group">
        <div class="stat-item">
          <span class="stat-label">会话时长:</span>
          <strong class="stat-val">{{ fmtDuration(replaySession.duration_sec) }}</strong>
        </div>
      </div>

      <!-- 右侧：播放增强选项 -->
      <div class="action-group">
        <NTooltip trigger="hover">
          <template #trigger>
            <div class="option-switch">
              <span class="option-label">跳过挂机等待</span>
              <NSwitch v-model:value="skipIdle" size="small" />
            </div>
          </template>
          开启后自动平滑跳过超过 2 秒的挂机空白停顿，避免长达数分钟的静止画面
        </NTooltip>

        <div class="speed-selector">
          <span class="option-label">倍速</span>
          <NRadioGroup v-model:value="playbackSpeed" size="small">
            <NRadioButton :value="1.0">1.0x</NRadioButton>
            <NRadioButton :value="1.5">1.5x</NRadioButton>
            <NRadioButton :value="2.0">2.0x</NRadioButton>
          </NRadioGroup>
        </div>
      </div>
    </div>

    <!-- 播放器视窗 -->
    <div class="replay-stage">
      <div ref="replayContainer" class="replay-container" />
      <div v-if="replayLoading" class="replay-loading">
        <NSpin size="medium" />
        <span>正在加载并分析会话录像…</span>
      </div>
      <div v-if="replayError" class="replay-error">{{ replayError }}</div>
    </div>

    <!-- 底部操作者与说明 -->
    <div class="replay-footer-meta" v-if="replaySession">
      <div class="meta-left">
        <span>操作账号: <code>{{ replaySession.operator }}</code></span>
        <span>连接开始: {{ fmtDateTime(replaySession.started_at) }}</span>
        <span v-if="replaySession.ended_at">连接结束: {{ fmtDateTime(replaySession.ended_at) }}</span>
      </div>
      <div class="meta-right" v-if="replayStats && replayStats.idleSec > 5 && skipIdle">
        <span class="skip-hint">
          <NIcon :component="InformationCircleOutline" size="14" style="vertical-align: -2px" />
          已启用智能空闲压缩：无操作时的挂机停顿已自动快进跳过
        </span>
      </div>
    </div>
  </NModal>

  <!-- 批量删除二次确认（DESIGN.md §9.17：破坏性确认在弹窗内就地展开） -->
  <NModal
    v-model:show="showBatchDeleteModal"
    preset="card"
    :title="`批量删除会话记录（${selectedSessions.length} 条）`"
    style="width: 520px"
  >
    <div class="batch-confirm-body">
      <div class="batch-target-list">
        <div
          v-for="s in selectedSessions.slice(0, 5)"
          :key="s.id"
          class="batch-target-row"
        >
          {{ s.hostname }} · {{ s.operator }} · {{ fmtDateTime(s.started_at) }}
        </div>
        <div v-if="selectedSessions.length > 5" class="batch-target-row muted">
          等 {{ selectedSessions.length - 5 }} 条…
        </div>
      </div>
      <p class="field-hint">删除后会话记录与录像无法恢复，请确认。</p>
    </div>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showBatchDeleteModal = false">取消</NButton>
        <NButton type="error" :loading="batchRunning" @click="confirmBatchDelete">
          确认删除
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.session-view {
  gap: 16px;
}
.session-toolbar {
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
.result-count {
  font-size: 12px;
  color: var(--text-secondary);
}

.replay-control-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
  padding: 8px 12px;
  background-color: var(--bg-card-subtle, rgba(255, 255, 255, 0.04));
  border: 1px solid var(--border-color);
  border-radius: 6px;
  flex-wrap: wrap;
  gap: 10px;

  .stat-group {
    display: flex;
    align-items: center;
    gap: 16px;
    font-size: 13px;

    .stat-item {
      display: flex;
      align-items: center;
      gap: 6px;

      .stat-label {
        color: var(--text-secondary);
      }
      .stat-val {
        color: var(--text-primary);
        font-family: var(--font-mono, monospace);
      }
      .active-val {
        color: #10b981;
        font-weight: 600;
      }
      .stat-sub {
        font-size: 11.5px;
        color: var(--text-secondary);
      }
    }
  }

  .action-group {
    display: flex;
    align-items: center;
    gap: 16px;

    .option-switch {
      display: flex;
      align-items: center;
      gap: 6px;
      cursor: pointer;
    }
    .speed-selector {
      display: flex;
      align-items: center;
      gap: 6px;
    }
    .option-label {
      font-size: 12.5px;
      color: var(--text-secondary);
    }
  }
}

.replay-stage {
  position: relative;
  min-height: 420px;
}
.replay-container {
  min-height: 420px;
  background: #000;
  border-radius: 6px;
  overflow: hidden;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
  display: flex;
  justify-content: center;

  :deep(.ap-wrapper) {
    width: 100%;
  }
  :deep(.ap-player) {
    width: 100% !important;
  }
}
.replay-loading,
.replay-error {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: #9ca3af;
  pointer-events: none;
}
.replay-error {
  color: #ef4444;
}

.replay-footer-meta {
  margin-top: 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 12.5px;
  color: var(--text-secondary);
  flex-wrap: wrap;
  gap: 8px;

  .meta-left {
    display: flex;
    align-items: center;
    gap: 18px;

    code {
      color: #6366f1;
      font-weight: 500;
    }
  }

  .skip-hint {
    color: #f59e0b;
    font-size: 12px;
  }
}

// 批量操作栏（NetworkList.vue 同款）
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--n-color-modal);
  border: 1px solid var(--n-border-color);
  border-radius: 8px;
  padding: 8px 12px;
  flex-shrink: 0;

  .batch-info {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .text-muted {
    color: var(--text-secondary);
    font-weight: 400;
  }

  .batch-summary {
    font-size: 12px;
    color: var(--text-secondary);
  }
}

// 批量删除确认弹窗：目标清单内部滚动，行数多也不撑出视口
.batch-confirm-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.batch-target-list {
  max-height: 180px;
  overflow-y: auto;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 6px 10px;

  .batch-target-row {
    padding: 3px 0;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;

    &.muted {
      color: var(--text-secondary);
    }
  }
}

.field-hint {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
  margin: 0;
}

// 批量执行行内进度（NetworkList .op-progress 同款色板；render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透）。
:deep(.batch-op-progress) {
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
</style>