<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import {
  NAlert, NButton, NCheckbox, NIcon, NModal, NSpace, NSpin, NTag, useMessage,
} from 'naive-ui'
import {
  CheckmarkCircleOutline, CloseCircleOutline, CopyOutline,
  RefreshOutline, SyncOutline,
} from '@vicons/ionicons5'
import { getCertLog, getIssueTask } from '../../api/certs'
import { fmtDateTime } from '../../utils/time'

const props = withDefaults(
  defineProps<{
    show: boolean
    certId?: string
    taskId?: string
    title?: string
    domains?: string[]
  }>(),
  {
    certId: '',
    taskId: '',
    title: '',
    domains: () => [],
  },
)

const emit = defineEmits<{
  (e: 'update:show', val: boolean): void
  (e: 'completed', certId?: string): void
}>()

const message = useMessage()

const logText = ref('')
const loading = ref(false)
const refreshing = ref(false)
const isRunning = ref(false)
const taskStatus = ref<'running' | 'success' | 'failed' | ''>('')
const errorMessage = ref('')
const updatedAt = ref('')
const autoScroll = ref(true)
const logPreRef = ref<HTMLPreElement | null>(null)

let pollTimer: ReturnType<typeof setInterval> | null = null

function scrollToBottom() {
  if (!autoScroll.value || !logPreRef.value) return
  nextTick(() => {
    if (logPreRef.value) {
      logPreRef.value.scrollTop = logPreRef.value.scrollHeight
    }
  })
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

async function fetchLog(silent = false) {
  if (!silent) {
    if (!logText.value) loading.value = true
    else refreshing.value = true
  }

  try {
    if (props.taskId) {
      const task = await getIssueTask(props.taskId)
      logText.value = task.log || ''
      taskStatus.value = task.status
      isRunning.value = task.status === 'running'
      errorMessage.value = task.error || ''
      updatedAt.value = task.updated_at || task.created_at
      scrollToBottom()

      if (task.status !== 'running') {
        stopPolling()
        if (task.status === 'success') {
          emit('completed', task.cert_id)
        }
      }
    } else if (props.certId) {
      const res = await getCertLog(props.certId)
      logText.value = res.log || ''
      isRunning.value = res.running
      taskStatus.value = res.running ? 'running' : 'success'
      updatedAt.value = res.updated_at
      scrollToBottom()

      if (!res.running) {
        stopPolling()
      }
    }
  } catch (err: any) {
    // If not found yet during rapid initial poll, don't spam errors
    if (!silent) {
      message.error(err?.message || '获取日志失败')
    }
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

function startPolling() {
  stopPolling()
  fetchLog(false)
  pollTimer = setInterval(() => {
    fetchLog(true)
  }, 2000)
}

watch(
  () => props.show,
  (val) => {
    if (val) {
      logText.value = ''
      errorMessage.value = ''
      taskStatus.value = ''
      isRunning.value = false
      startPolling()
    } else {
      stopPolling()
    }
  },
  { immediate: true },
)

watch(
  () => [props.certId, props.taskId],
  () => {
    if (props.show) {
      startPolling()
    }
  },
)

async function copyLog() {
  if (!logText.value) return
  try {
    await navigator.clipboard.writeText(logText.value)
    message.success('日志已复制到剪贴板')
  } catch {
    message.error('复制失败')
  }
}

function handleClose() {
  stopPolling()
  emit('update:show', false)
}

onBeforeUnmount(() => {
  stopPolling()
})
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :title="title || '证书执行日志'"
    style="width: 840px; max-width: 95vw"
    :mask-closable="true"
    @update:show="handleClose"
  >
    <template #header-extra>
      <NSpace :size="10" align="center">
        <NCheckbox v-model:checked="autoScroll" size="small">自动滚动</NCheckbox>
        <NButton size="tiny" secondary :loading="refreshing" @click="() => fetchLog(false)">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          刷新
        </NButton>
        <NButton size="tiny" secondary :disabled="!logText" @click="copyLog">
          <template #icon><NIcon><CopyOutline /></NIcon></template>
          复制
        </NButton>
      </NSpace>
    </template>

    <div class="cert-log-meta">
      <NSpace :size="10" align="center" :wrap="true">
        <NTag
          v-if="taskStatus === 'running' || isRunning"
          size="small"
          type="info"
          :bordered="false"
        >
          <template #icon><NIcon class="rotating"><SyncOutline /></NIcon></template>
          执行中
        </NTag>
        <NTag
          v-else-if="taskStatus === 'success'"
          size="small"
          type="success"
          :bordered="false"
        >
          <template #icon><NIcon><CheckmarkCircleOutline /></NIcon></template>
          已完成
        </NTag>
        <NTag
          v-else-if="taskStatus === 'failed' || errorMessage"
          size="small"
          type="error"
          :bordered="false"
        >
          <template #icon><NIcon><CloseCircleOutline /></NIcon></template>
          失败
        </NTag>

        <span v-if="domains && domains.length" class="meta-item">
          {{ domains.join(', ') }}
        </span>
        <span v-else-if="certId" class="meta-item mono">
          {{ certId }}
        </span>
        <span v-if="taskId" class="meta-item mono">
          任务: {{ taskId }}
        </span>
        <span v-if="updatedAt" class="meta-item">
          更新于 {{ fmtDateTime(updatedAt) }}
        </span>
      </NSpace>
    </div>

    <div class="cert-log-body">
      <div v-if="isRunning" class="live-hint">
        <NSpin :size="12" />
        <span>ACME 签发/续期进行中，日志每 2 秒实时刷新...</span>
      </div>

      <div v-if="loading && !logText" class="loading-box">
        <NSpin size="medium" />
        <span style="margin-top: 8px; color: var(--n-text-color-3)">加载日志中...</span>
      </div>
      <pre v-else ref="logPreRef" class="cert-log-pre">{{ logText || '等待日志输出...' }}</pre>

      <NAlert v-if="errorMessage" type="error" style="margin-top: 10px" :show-icon="true">
        <template #header>执行失败原因</template>
        {{ errorMessage }}
      </NAlert>
    </div>
  </NModal>
</template>

<style scoped>
.cert-log-meta {
  padding: 0 0 12px;
  border-bottom: 1px solid var(--n-border-color);
  margin-bottom: 12px;
  font-size: 13px;
  color: var(--n-text-color-3);
}

.meta-item {
  font-size: 12px;
}

.meta-item.mono {
  font-family: 'JetBrains Mono', Consolas, monospace;
}

.cert-log-body {
  display: flex;
  flex-direction: column;
  min-height: 280px;
  max-height: 65vh;
}

.live-hint {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  color: var(--n-primary-color);
  margin-bottom: 8px;
}

.loading-box {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  min-height: 240px;
  background: #18181c;
  border-radius: 6px;
}

.cert-log-pre {
  flex: 1;
  min-height: 240px;
  max-height: 55vh;
  overflow: auto;
  overscroll-behavior: contain;
  background: #18181c;
  color: #e6e6e6;
  border: 1px solid rgba(255, 255, 255, 0.08);
  border-radius: 6px;
  padding: 12px 14px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}

.rotating {
  animation: spin 1.2s linear infinite;
}

@keyframes spin {
  from { transform: rotate(0deg); }
  to { transform: rotate(360deg); }
}
</style>
