<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, watch } from 'vue'
import {
  NCard, NDataTable, NEmpty, NButton, NModal, NSpace, NTag,
  NPopconfirm, useMessage, NIcon, NPagination, NSpin,
} from 'naive-ui'
import { PlayOutline, TrashOutline, RefreshOutline } from '@vicons/ionicons5'
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

// Replay modal
const showReplay = ref(false)
const replaySession = ref<SessionRecord | null>(null)
const replayContainer = ref<HTMLElement | null>(null)
const replayLoading = ref(false)
const replayError = ref('')
let playerInstance: any = null
let replayRequestSeq = 0

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
}

function fmtDuration(sec: number): string {
  if (!sec) return '-'
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

function initReplay(row: SessionRecord) {
  const requestSeq = ++replayRequestSeq
  replayLoading.value = true
  replayError.value = ''

  void (async () => {
    try {
      // 通过 replayContainer watcher 启动时，容器已经挂载；这里仅保留一次
      // 轻量确认，避免弹窗过渡卸载/切换时误写入旧节点。
      if (!replayContainer.value) throw new Error('回放容器尚未就绪')
      const [{ create }, url] = await Promise.all([
        import('asciinema-player'),
        Promise.resolve(sessionRecordingUrl(row.id)),
      ])
      if (!replayContainer.value) throw new Error('回放容器尚未就绪')
      const response = await fetch(url, { cache: 'no-store' })
      if (!response.ok) throw new Error(`录像接口返回 HTTP ${response.status}`)
      const castText = await response.text()
      const lines = castText.trim().split(/\r?\n/).filter(Boolean)
      if (lines.length < 2) throw new Error('录像没有可回放的输出事件')
      let header: unknown
      try {
        header = JSON.parse(lines[0])
      } catch {
        throw new Error('录像头不是有效 JSON')
      }
      if (!header || typeof header !== 'object' || (header as { version?: number }).version !== 2) {
        throw new Error('录像不是 asciicast v2 格式')
      }
      for (const line of lines.slice(1)) {
        let event: unknown
        try {
          event = JSON.parse(line)
        } catch {
          throw new Error('录像事件不是有效 JSON')
        }
        if (!Array.isArray(event) || event.length !== 3 || typeof event[1] !== 'string') {
          throw new Error('录像事件格式无效')
        }
      }
      if (requestSeq !== replayRequestSeq || !replayContainer.value) return
      if (playerInstance) {
        try { playerInstance.dispose() } catch {}
        playerInstance = null
      }
      replayContainer.value.replaceChildren()
      // 直接传 data，播放器不再自行 fetch；避免异步网络失败只显示一个“💥”。
      playerInstance = create(
        { data: castText, parser: 'asciicast' },
        replayContainer.value,
        {
          cols: Number((header as { width?: number }).width) || 120,
          rows: Number((header as { height?: number }).height) || 30,
          autoPlay: true,
          theme: 'monokai',
          controls: true,
        },
      )
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
}

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
    :title="`终端回放 - ${replaySession?.hostname || ''}`"
    style="width: 900px"
    @after-enter="handleReplayEnter"
    @after-leave="closeReplay"
  >
    <div class="replay-stage">
      <div ref="replayContainer" class="replay-container" />
      <div v-if="replayLoading" class="replay-loading">
        <NSpin size="medium" />
        <span>正在加载录像…</span>
      </div>
      <div v-if="replayError" class="replay-error">{{ replayError }}</div>
    </div>
    <div class="replay-meta" v-if="replaySession">
      <span>操作者: {{ replaySession.operator }}</span>
      <span>开始: {{ fmtTime(replaySession.started_at) }}</span>
      <span>时长: {{ fmtDuration(replaySession.duration_sec) }}</span>
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
.replay-stage {
  position: relative;
  min-height: 400px;
}
.replay-container {
  min-height: 400px;
  background: #000;
  border-radius: 4px;
  overflow: hidden;
  :deep(.asciinema-player) {
    width: 100%;
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
.replay-meta {
  margin-top: 12px;
  display: flex;
  gap: 24px;
  font-size: 13px;
  color: #9ca3af;
}
</style>