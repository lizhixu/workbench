<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, watch } from 'vue'
import {
  NCard, NDataTable, NEmpty, NButton, NModal, NSpace, NTag,
  NPopconfirm, useMessage, NIcon, NPagination, NSpin, NSwitch,
  NRadioGroup, NRadioButton, NTooltip,
} from 'naive-ui'
import {
  PlayOutline, TrashOutline, RefreshOutline, FlashOutline, InformationCircleOutline,
} from '@vicons/ionicons5'
import { listSessions, deleteSession, sessionRecordingUrl } from '../../api/hosts'
import type { SessionRecord } from '../../api/types'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'SessionList' })

const message = useMessage()
const sessions = ref<SessionRecord[]>([])
const loading = ref(false)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

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

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
}

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
  { title: '主机', key: 'hostname', width: 150, ellipsis: { tooltip: true } },
  { title: '操作者', key: 'operator', width: 100 },
  {
    title: '状态',
    key: 'live',
    width: 80,
    render: (row: SessionRecord) =>
      row.live
        ? h(NTag, { type: 'success', size: 'small', round: true }, { default: () => '进行中' })
        : h(NTag, { type: 'default', size: 'small', round: true }, { default: () => '已结束' }),
  },
  { title: '开始时间', key: 'started_at', width: 170, render: (row: SessionRecord) => fmtTime(row.started_at) },
  { title: '结束时间', key: 'ended_at', width: 170, render: (row: SessionRecord) => fmtTime(row.ended_at) },
  { title: '时长', key: 'duration_sec', width: 80, render: (row: SessionRecord) => fmtDuration(row.duration_sec) },
  {
    title: '操作',
    key: 'actions',
    width: 120,
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

async function doDelete(id: string) {
  try {
    await deleteSession(id)
    message.success('已删除')
    // If the last row on a non-first page was removed, step back a page.
    if (sessions.value.length === 1 && page.value > 1) {
      page.value -= 1
    }
    load()
  } catch (e: any) {
    message.error(e.message)
  }
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

    <NCard class="table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="sessions"
        :bordered="false"
        size="small"
        :loading="loading"
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
        <span>连接开始: {{ fmtTime(replaySession.started_at) }}</span>
        <span v-if="replaySession.ended_at">连接结束: {{ fmtTime(replaySession.ended_at) }}</span>
      </div>
      <div class="meta-right" v-if="replayStats && replayStats.idleSec > 5 && skipIdle">
        <span class="skip-hint">
          <NIcon :component="InformationCircleOutline" size="14" style="vertical-align: -2px" />
          已启用智能空闲压缩：无操作时的挂机停顿已自动快进跳过
        </span>
      </div>
    </div>
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
</style>