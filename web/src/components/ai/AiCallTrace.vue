<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import type { CallTrace, LLMUsage } from '../../api/ai'

// Codex-style call timeline: the server measures each real stage of an AI
// call (facts probe, model call, sanitize) and this component renders the
// steps with their durations and expandable details. While the request is
// still in flight (trace is null but startedAt is set) it shows only a
// live elapsed timer — never fabricated per-step progress, because without
// streaming the frontend cannot know which stage is running.
const props = defineProps<{
  trace?: CallTrace | null
  model?: string
  usage?: LLMUsage | null
  startedAt?: number | null
  defaultExpanded?: boolean
}>()

const expanded = ref(!!props.defaultExpanded)
const openStep = ref<string | null>(null)
const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | null = null

function startTimer() {
  if (timer) return
  now.value = Date.now()
  timer = setInterval(() => {
    now.value = Date.now()
  }, 500)
}

function stopTimer() {
  if (timer) {
    clearInterval(timer)
    timer = null
  }
}

watch(
  () => [props.trace, props.startedAt] as const,
  ([trace, startedAt]) => {
    if (!trace && startedAt) startTimer()
    else stopTimer()
  },
  { immediate: true },
)
onUnmounted(stopTimer)

const pending = computed(() => !props.trace && !!props.startedAt)
const elapsedText = computed(() => {
  if (!props.startedAt) return ''
  return fmtDuration(Math.max(0, now.value - props.startedAt))
})

function fmtDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

function statusText(status: string): string {
  if (status === 'done') return '已完成'
  if (status === 'failed') return '失败'
  return status
}

const usageText = computed(() => {
  const u = props.usage
  if (!u) return ''
  if (u.prompt_tokens || u.completion_tokens) {
    return `输入 ${u.prompt_tokens ?? 0} / 输出 ${u.completion_tokens ?? 0} tokens`
  }
  if (u.total_tokens) return `共 ${u.total_tokens} tokens`
  return ''
})

function toggleStep(key: string) {
  openStep.value = openStep.value === key ? null : key
}
</script>

<template>
  <div class="ai-call-trace">
    <!-- Pending: honest elapsed timer only, no fake step progress. -->
    <div v-if="pending" class="trace-pending">
      <span class="thinking-dots"><i /><i /><i /></span>
      <span class="pending-text">调用进行中，已用 {{ elapsedText }}</span>
      <span class="pending-hint">完成后显示各步骤耗时</span>
    </div>

    <div v-else-if="trace" class="trace-done">
      <button class="trace-head" type="button" @click="expanded = !expanded">
        <span class="trace-title">调用过程</span>
        <span class="trace-meta">
          共耗时 {{ fmtDuration(trace.total_ms) }}
          <span v-if="model"> · {{ model }}</span>
          <span v-if="usageText"> · {{ usageText }}</span>
        </span>
        <span class="trace-chevron">{{ expanded ? '▾' : '▸' }}</span>
      </button>

      <div v-if="expanded" class="trace-steps">
        <div
          v-for="step in trace.steps"
          :key="step.key"
          class="trace-step"
          :class="step.status"
        >
          <button
            class="step-main"
            type="button"
            :disabled="!step.detail"
            @click="step.detail && toggleStep(step.key)"
          >
            <span class="step-label">{{ step.label }}</span>
            <span class="step-status">{{ statusText(step.status) }}</span>
            <span class="step-time">{{ fmtDuration(step.duration_ms) }}</span>
            <span v-if="step.detail" class="step-chevron">
              {{ openStep === step.key ? '▾' : '▸' }}
            </span>
          </button>
          <div v-if="step.detail && openStep === step.key" class="step-detail">
            {{ step.detail }}
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.ai-call-trace {
  margin-top: 8px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
}
.trace-pending {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.pending-hint {
  opacity: 0.7;
}
.thinking-dots {
  display: inline-flex;
  gap: 3px;
  align-items: center;
}
.thinking-dots i {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: currentColor;
  opacity: 0.4;
  animation: trace-blink 1.2s infinite;
}
.thinking-dots i:nth-child(2) {
  animation-delay: 0.2s;
}
.thinking-dots i:nth-child(3) {
  animation-delay: 0.4s;
}
@keyframes trace-blink {
  0%,
  100% {
    opacity: 0.25;
  }
  50% {
    opacity: 1;
  }
}
.trace-head {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 0;
  border: none;
  background: none;
  color: inherit;
  font-size: 12px;
  cursor: pointer;
  text-align: left;
}
.trace-head:focus-visible,
.step-main:focus-visible {
  outline: 2px solid rgba(99, 102, 241, 0.55);
  outline-offset: 2px;
  border-radius: 4px;
}
.trace-title {
  font-weight: 600;
  color: var(--text-primary);
}
.trace-meta {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.trace-steps {
  margin-top: 8px;
  border-left: 2px solid var(--border-color);
  padding-left: 12px;
}
.trace-step {
  margin-bottom: 2px;
}
.step-main {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 0;
  border: none;
  background: none;
  color: inherit;
  font-size: 12px;
  cursor: pointer;
  text-align: left;
}
.step-main:disabled {
  cursor: default;
}
.step-label {
  color: var(--text-primary);
}
.trace-step.failed .step-label,
.trace-step.failed .step-status {
  color: #ef4444;
}
.step-status {
  font-size: 11px;
  opacity: 0.8;
}
.step-time {
  font-variant-numeric: tabular-nums;
  opacity: 0.8;
}
.step-detail {
  margin: 4px 0 8px;
  padding: 8px 10px;
  border: 1px solid var(--border-color);
  border-radius: 8px;
  background: var(--bg-card-subtle);
  color: var(--text-primary);
  word-break: break-all;
  white-space: pre-wrap;
}
</style>
