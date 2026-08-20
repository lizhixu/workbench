<script setup lang="ts">
import { ref, onMounted, nextTick } from 'vue'
import {
  NCard, NDataTable, NEmpty, NButton, NModal, NSpace, NTag,
  NPopconfirm, useMessage, NIcon,
} from 'naive-ui'
import { PlayOutline, TrashOutline, RefreshOutline } from '@vicons/ionicons5'
import { listSessions, deleteSession, sessionRecordingUrl } from '../../api/hosts'
import type { SessionRecord } from '../../api/types'

const message = useMessage()
const sessions = ref<SessionRecord[]>([])
const loading = ref(false)

// Replay modal
const showReplay = ref(false)
const replaySession = ref<SessionRecord | null>(null)
const replayContainer = ref<HTMLElement | null>(null)
let playerInstance: any = null

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
    const res = await listSessions()
    sessions.value = res.data || []
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

async function openReplay(row: SessionRecord) {
  replaySession.value = row
  showReplay.value = true
  await nextTick()
  await nextTick()

  // Dynamically import asciinema-player to avoid bundling it for all users.
  try {
    const AsciinemaPlayer = await import('asciinema-player')
    const url = sessionRecordingUrl(row.id)
    if (playerInstance) {
      try { playerInstance.dispose() } catch {}
    }
    if (replayContainer.value) {
      replayContainer.value.innerHTML = ''
      playerInstance = AsciinemaPlayer.create(
        { url },
        replayContainer.value,
        {
          cols: 120,
          rows: 30,
          autoPlay: true,
          theme: 'monokai',
          controls: true,
        },
      )
    }
  } catch (e: any) {
    message.error('回放加载失败: ' + e.message)
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
    load()
  } catch (e: any) {
    message.error(e.message)
  }
}

onMounted(load)
</script>

<template>
  <NSpace vertical :size="16">
    <NSpace align="center" justify="space-between">
      <h2 class="page-title">会话审计</h2>
      <NButton @click="load" :loading="loading">
        <template #icon><NIcon :component="RefreshOutline" /></template>
        刷新
      </NButton>
    </NSpace>

    <NCard>
      <NDataTable
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
    </NCard>
  </NSpace>

  <!-- Replay modal -->
  <NModal
    v-model:show="showReplay"
    preset="card"
    :title="`终端回放 - ${replaySession?.hostname || ''}`"
    style="width: 900px"
    @after-leave="closeReplay"
  >
    <div ref="replayContainer" class="replay-container" />
    <div class="replay-meta" v-if="replaySession">
      <span>操作者: {{ replaySession.operator }}</span>
      <span>开始: {{ fmtTime(replaySession.started_at) }}</span>
      <span>时长: {{ fmtDuration(replaySession.duration_sec) }}</span>
    </div>
  </NModal>
</template>

<style scoped lang="scss">
.page-title {
  margin: 0;
  font-size: 18px;
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
.replay-meta {
  margin-top: 12px;
  display: flex;
  gap: 24px;
  font-size: 13px;
  color: #9ca3af;
}
</style>