<script setup lang="ts">
// Terminal AI Copilot: a side panel that turns a natural-language operational
// intent into an ordered, host-tailored plan and (after confirmation) runs it
// step by step, echoing colored progress into the live terminal and auto-
// diagnosing failures. See AGENTS.md 3.16 — AI never auto-runs a high-risk
// step; those pause for explicit human confirmation.
import { ref, computed, nextTick } from 'vue'
import { NButton, NIcon, NInput, NSpin, NTag, useMessage } from 'naive-ui'
import {
  CloseOutline,
  SparklesOutline,
  PlayOutline,
  RefreshOutline,
  ChevronDownOutline,
  ChevronForwardOutline,
  CheckmarkCircle,
  CloseCircle,
  EllipseOutline,
  WarningOutline,
  TerminalOutline,
} from '@vicons/ionicons5'
import { planTask, analyzeExec, type TaskPlan } from '../../api/ai'
import { execCommand } from '../../api/hosts'

const props = defineProps<{
  hostId: string
  hostLabel: string
  osLabel?: string
}>()
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'echo', text: string): void
  (e: 'send', command: string): void
}>()

const message = useMessage()

type StepStatus = 'pending' | 'running' | 'success' | 'failed' | 'skipped' | 'confirm'

interface StepState {
  status: StepStatus
  stdout: string
  stderr: string
  exitCode: number | null
  durationMs: number
  expanded: boolean
  diagnosing: boolean
  diagnosis: string
}

const QUICK_INTENTS = [
  '安装 Docker',
  '排查 80 端口占用',
  '排查 CPU 负载最高的 5 个进程',
  '清理未使用的 Docker 镜像与缓存',
  '排查大于 100M 的大文件',
]

const input = ref('')
const planning = ref(false)
const plan = ref<TaskPlan | null>(null)
const stepStates = ref<StepState[]>([])
const running = ref(false)
const currentStep = ref(-1)
const finished = ref(false)
const aborted = ref(false)
// Guards a stale run: bumping the token makes any in-flight step loop bail.
let runToken = 0

const osTag = computed(() => props.osLabel || '')

const riskTagType = computed(() => {
  const r = plan.value?.risk_level
  return r === 'high' ? 'error' : r === 'medium' ? 'warning' : 'success'
})

function riskLabel(r?: string): string {
  return r === 'high' ? '高风险' : r === 'medium' ? '中风险' : '低风险'
}

function statusIcon(s: StepStatus) {
  switch (s) {
    case 'success':
      return CheckmarkCircle
    case 'failed':
      return CloseCircle
    case 'running':
      return RefreshOutline
    case 'confirm':
      return WarningOutline
    case 'skipped':
      return ChevronForwardOutline
    default:
      return EllipseOutline
  }
}

function statusColor(s: StepStatus): string {
  switch (s) {
    case 'success':
      return '#18a058'
    case 'failed':
      return '#d03050'
    case 'running':
      return '#2080f0'
    case 'confirm':
      return '#f0a020'
    case 'skipped':
      return '#909399'
    default:
      return '#c0c4cc'
  }
}

function resetPlanState(p: TaskPlan) {
  stepStates.value = p.steps.map(() => ({
    status: 'pending' as StepStatus,
    stdout: '',
    stderr: '',
    exitCode: null,
    durationMs: 0,
    expanded: false,
    diagnosing: false,
    diagnosis: '',
  }))
  currentStep.value = -1
  finished.value = false
  aborted.value = false
}

async function generatePlan(prompt?: string) {
  const text = (prompt ?? input.value).trim()
  if (!text) return
  if (running.value) {
    message.warning('当前任务执行中，请先等待完成或中止')
    return
  }
  input.value = text
  planning.value = true
  plan.value = null
  try {
    const p = await planTask(text, props.hostId)
    if (!p.steps || p.steps.length === 0) {
      message.warning('AI 未能生成有效的执行步骤')
      return
    }
    plan.value = p
    resetPlanState(p)
  } catch (e: any) {
    message.error(e.message || 'AI 规划失败')
  } finally {
    planning.value = false
  }
}

function echo(text: string) {
  emit('echo', text)
}

// Runs the plan from `startIdx`, honoring per-step confirmation. Returns when
// it finishes, aborts, or pauses on a step needing confirmation.
async function runFrom(startIdx: number) {
  if (!plan.value) return
  const token = ++runToken
  running.value = true
  aborted.value = false
  echo(`\r\n\x1b[36m╭─ AI 助手开始执行任务：${plan.value.title}\x1b[0m\r\n`)

  for (let i = startIdx; i < plan.value.steps.length; i++) {
    if (token !== runToken) return // superseded by a newer run
    const step = plan.value.steps[i]
    const st = stepStates.value[i]

    // High-risk steps pause for explicit human confirmation.
    if ((step.needs_confirm || step.risk_level === 'high') && st.status !== 'confirm') {
      st.status = 'confirm'
      currentStep.value = i
      running.value = false
      echo(`\r\n\x1b[33m⚠ 步骤 ${i + 1}「${step.title}」为高危操作，等待人工确认：\x1b[0m\r\n\x1b[33m  ${step.command}\x1b[0m\r\n`)
      return
    }

    currentStep.value = i
    st.status = 'running'
    st.expanded = true
    echo(`\r\n\x1b[36m▶ [${i + 1}/${plan.value.steps.length}] ${step.title}\x1b[0m\r\n\x1b[90m$ ${step.command}\x1b[0m\r\n`)

    try {
      const res = await execCommand(
        props.hostId,
        step.command,
        plan.value.shell === 'powershell' ? 'powershell' : '',
        180,
        false,
        step.needs_confirm || step.risk_level === 'high',
      )
      if (token !== runToken) return
      st.stdout = res.stdout || ''
      st.stderr = res.stderr || ''
      st.exitCode = res.exit_code
      st.durationMs = res.duration_ms || 0

      if (res.stdout) echo(dim(res.stdout))
      if (res.stderr) echo(`\x1b[31m${res.stderr}\x1b[0m`.replace(/\n/g, '\r\n'))

      if (res.exit_code === 0 && !res.error) {
        st.status = 'success'
        echo(`\x1b[32m✔ 步骤完成 (${fmtMs(st.durationMs)})\x1b[0m\r\n`)
        st.expanded = false
      } else {
        st.status = 'failed'
        echo(`\x1b[31m✗ 步骤失败，退出码 ${res.exit_code}${res.error ? ' (' + res.error + ')' : ''}\x1b[0m\r\n`)
        if (step.continue_on_error) {
          st.status = 'skipped'
          echo(`\x1b[33m→ 该步骤允许失败，继续执行后续步骤\x1b[0m\r\n`)
          continue
        }
        // Abort and auto-diagnose.
        running.value = false
        aborted.value = true
        void diagnoseStep(i)
        return
      }
    } catch (e: any) {
      if (token !== runToken) return
      st.status = 'failed'
      st.stderr = e.message || '执行请求失败'
      st.exitCode = -1
      echo(`\x1b[31m✗ 执行请求失败：${e.message || ''}\x1b[0m\r\n`)
      if (step.continue_on_error) {
        st.status = 'skipped'
        continue
      }
      running.value = false
      aborted.value = true
      return
    }
  }

  running.value = false
  finished.value = true
  currentStep.value = -1
  echo(`\r\n\x1b[32m╰─ 任务「${plan.value.title}」全部步骤执行完成 🎉\x1b[0m\r\n`)
  message.success('任务执行完成')
}

function startAuto() {
  if (!plan.value) return
  runFrom(0)
}

// Confirms a paused high-risk step, then resumes automation from it.
function confirmStep(i: number) {
  const st = stepStates.value[i]
  if (st.status !== 'confirm') return
  st.status = 'pending'
  // Temporarily clear the confirm gate for THIS step so runFrom executes it.
  const step = plan.value!.steps[i]
  ;(step as any).__confirmed = true
  runFromConfirmed(i)
}

// Like runFrom but the first step is already confirmed.
async function runFromConfirmed(idx: number) {
  if (!plan.value) return
  const step = plan.value.steps[idx]
  // Execute the confirmed step directly, then continue with the normal loop.
  const token = ++runToken
  running.value = true
  const st = stepStates.value[idx]
  currentStep.value = idx
  st.status = 'running'
  st.expanded = true
  echo(`\r\n\x1b[36m▶ [${idx + 1}/${plan.value.steps.length}] ${step.title}（已确认）\x1b[0m\r\n\x1b[90m$ ${step.command}\x1b[0m\r\n`)
  try {
    const res = await execCommand(props.hostId, step.command, plan.value.shell === 'powershell' ? 'powershell' : '', 180, false, true)
    if (token !== runToken) return
    st.stdout = res.stdout || ''
    st.stderr = res.stderr || ''
    st.exitCode = res.exit_code
    st.durationMs = res.duration_ms || 0
    if (res.stdout) echo(dim(res.stdout))
    if (res.stderr) echo(`\x1b[31m${res.stderr}\x1b[0m`.replace(/\n/g, '\r\n'))
    if (res.exit_code === 0 && !res.error) {
      st.status = 'success'
      st.expanded = false
      echo(`\x1b[32m✔ 步骤完成 (${fmtMs(st.durationMs)})\x1b[0m\r\n`)
      // Continue with the rest.
      runFrom(idx + 1)
    } else {
      st.status = 'failed'
      running.value = false
      aborted.value = true
      echo(`\x1b[31m✗ 步骤失败，退出码 ${res.exit_code}\x1b[0m\r\n`)
      void diagnoseStep(idx)
    }
  } catch (e: any) {
    if (token !== runToken) return
    st.status = 'failed'
    st.stderr = e.message || '执行请求失败'
    running.value = false
    aborted.value = true
  }
}

function skipStep(i: number) {
  const st = stepStates.value[i]
  st.status = 'skipped'
  echo(`\r\n\x1b[33m→ 已跳过步骤 ${i + 1}\x1b[0m\r\n`)
  runFrom(i + 1)
}

async function diagnoseStep(i: number) {
  const st = stepStates.value[i]
  const step = plan.value?.steps[i]
  if (!step) return
  st.diagnosing = true
  try {
    const d = await analyzeExec(step.command, st.stdout, st.stderr, st.exitCode ?? -1)
    st.diagnosis = d.answer
    st.expanded = true
  } catch (e: any) {
    st.diagnosis = ''
  } finally {
    st.diagnosing = false
  }
}

function retryStep(i: number) {
  const st = stepStates.value[i]
  st.status = 'pending'
  st.diagnosis = ''
  runFrom(i)
}

function abort() {
  runToken++
  running.value = false
  aborted.value = true
  echo(`\r\n\x1b[31m■ 已中止任务执行\x1b[0m\r\n`)
}

function pushToTerminal(cmd: string) {
  emit('send', cmd)
  message.success('命令已推送到终端')
}

function toggleStep(i: number) {
  stepStates.value[i].expanded = !stepStates.value[i].expanded
}

function newTask() {
  runToken++
  plan.value = null
  stepStates.value = []
  running.value = false
  finished.value = false
  aborted.value = false
  input.value = ''
  nextTick(() => {
    /* focus handled by input autofocus */
  })
}

function dim(text: string): string {
  return `\x1b[37m${text}\x1b[0m`.replace(/\n/g, '\r\n')
}

function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

// seed lets the parent (TerminalPane's bottom bar) hand a draft intent to the
// copilot and immediately generate a plan.
function seed(prompt: string) {
  generatePlan(prompt)
}

defineExpose({ seed })
</script>

<!-- PLACEHOLDER_TEMPLATE -->
<template>
  <div class="ai-copilot">
    <!-- Header -->
    <div class="copilot-header">
      <div class="header-title">
        <NIcon size="18" color="#6366f1"><SparklesOutline /></NIcon>
        <span>AI 运维助手</span>
      </div>
      <div class="header-host">
        <NTag size="tiny" :bordered="false" round>{{ hostLabel }}</NTag>
        <NTag v-if="osTag" size="tiny" :bordered="false" round type="info">{{ osTag }}</NTag>
      </div>
      <NButton quaternary circle size="tiny" title="关闭" @click="emit('close')">
        <template #icon><NIcon><CloseOutline /></NIcon></template>
      </NButton>
    </div>

    <!-- Body -->
    <div class="copilot-body">
      <!-- Empty state: quick intents -->
      <div v-if="!plan && !planning" class="copilot-empty">
        <p class="empty-hint">
          用自然语言描述运维目标，AI 会拆解为多步骤方案并可全自动执行。
        </p>
        <div class="quick-intents">
          <div class="quick-title">快捷任务</div>
          <NButton
            v-for="q in QUICK_INTENTS"
            :key="q"
            size="small"
            secondary
            block
            class="quick-btn"
            @click="generatePlan(q)"
          >
            {{ q }}
          </NButton>
        </div>
      </div>

      <!-- Planning spinner -->
      <div v-if="planning" class="copilot-loading">
        <NSpin size="medium" />
        <p>AI 正在分析目标主机环境并生成执行方案…</p>
      </div>

      <!-- Plan overview + steps -->
      <div v-if="plan && !planning" class="copilot-plan">
        <div class="plan-card">
          <div class="plan-card-head">
            <span class="plan-title">{{ plan.title }}</span>
            <NTag :type="riskTagType" size="tiny" round>{{ riskLabel(plan.risk_level) }}</NTag>
          </div>
          <p class="plan-summary">{{ plan.summary }}</p>
          <div class="plan-meta">
            <NTag size="tiny" :bordered="false">{{ plan.os }}/{{ plan.arch }}</NTag>
            <NTag size="tiny" :bordered="false">{{ plan.shell }}</NTag>
            <NTag size="tiny" :bordered="false" :type="plan.source === 'blueprint' ? 'success' : 'info'">
              {{ plan.source === 'blueprint' ? '内置蓝图' : 'AI 生成' }}
            </NTag>
            <span class="step-count">{{ plan.steps.length }} 个步骤</span>
          </div>
        </div>

        <!-- Steps -->
        <div class="steps">
          <div
            v-for="(step, i) in plan.steps"
            :key="i"
            class="step"
            :class="{ active: currentStep === i, [stepStates[i].status]: true }"
          >
            <div class="step-head" @click="toggleStep(i)">
              <NIcon
                size="16"
                :color="statusColor(stepStates[i].status)"
                :class="{ spin: stepStates[i].status === 'running' }"
              >
                <component :is="statusIcon(stepStates[i].status)" />
              </NIcon>
              <span class="step-idx">{{ i + 1 }}</span>
              <span class="step-title">{{ step.title }}</span>
              <NTag v-if="step.risk_level === 'high'" type="error" size="tiny" round>高危</NTag>
              <NTag v-else-if="step.probe" size="tiny" :bordered="false" round>检查</NTag>
              <span v-if="stepStates[i].durationMs" class="step-dur">{{ fmtMs(stepStates[i].durationMs) }}</span>
              <NIcon size="14" class="step-caret">
                <component :is="stepStates[i].expanded ? ChevronDownOutline : ChevronForwardOutline" />
              </NIcon>
            </div>

            <div v-show="stepStates[i].expanded" class="step-detail">
              <p class="step-desc">{{ step.description }}</p>
              <div class="step-cmd">
                <NIcon size="12"><TerminalOutline /></NIcon>
                <code>{{ step.command }}</code>
              </div>

              <!-- Confirm gate for high-risk steps -->
              <div v-if="stepStates[i].status === 'confirm'" class="step-confirm">
                <span class="confirm-warn">此步骤为高危操作，需要你确认后才会执行。</span>
                <div class="confirm-actions">
                  <NButton size="tiny" type="error" @click="confirmStep(i)">确认执行</NButton>
                  <NButton size="tiny" secondary @click="skipStep(i)">跳过</NButton>
                  <NButton size="tiny" quaternary @click="pushToTerminal(step.command)">仅推送到终端</NButton>
                </div>
              </div>

              <!-- Output -->
              <pre v-if="stepStates[i].stdout" class="step-output">{{ stepStates[i].stdout }}</pre>
              <pre v-if="stepStates[i].stderr" class="step-output stderr">{{ stepStates[i].stderr }}</pre>

              <!-- Failure diagnosis -->
              <div v-if="stepStates[i].status === 'failed'" class="step-diag">
                <div v-if="stepStates[i].diagnosing" class="diag-loading">
                  <NSpin size="small" /> <span>AI 正在分析失败原因…</span>
                </div>
                <div v-else-if="stepStates[i].diagnosis" class="diag-content">
                  <div class="diag-title">
                    <NIcon size="14" color="#f0a020"><WarningOutline /></NIcon> AI 诊断建议
                  </div>
                  <div class="diag-text">{{ stepStates[i].diagnosis }}</div>
                </div>
                <div class="diag-actions">
                  <NButton size="tiny" type="primary" @click="retryStep(i)">重试该步骤</NButton>
                  <NButton v-if="!stepStates[i].diagnosis && !stepStates[i].diagnosing" size="tiny" secondary @click="diagnoseStep(i)">
                    AI 诊断
                  </NButton>
                  <NButton size="tiny" quaternary @click="skipStep(i)">跳过继续</NButton>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- Action bar -->
        <div class="plan-actions">
          <NButton
            v-if="!running && !finished"
            type="primary"
            block
            @click="startAuto"
          >
            <template #icon><NIcon><PlayOutline /></NIcon></template>
            开始全自动执行
          </NButton>
          <NButton v-if="running" type="error" block secondary @click="abort">
            中止执行
          </NButton>
          <NButton v-if="finished || aborted" block secondary @click="newTask">
            <template #icon><NIcon><RefreshOutline /></NIcon></template>
            新任务
          </NButton>
        </div>
      </div>
    </div>

    <!-- Footer input -->
    <div class="copilot-input">
      <NInput
        v-model:value="input"
        type="text"
        size="small"
        placeholder="描述运维目标，如：安装 docker"
        :disabled="planning || running"
        @keyup.enter="generatePlan()"
      />
      <NButton
        type="primary"
        size="small"
        :loading="planning"
        :disabled="running"
        @click="generatePlan()"
      >
        {{ plan ? '重新规划' : '生成方案' }}
      </NButton>
    </div>
  </div>
</template>

<style scoped lang="scss">
.ai-copilot {
  display: flex;
  flex-direction: column;
  height: 100%;
  width: 100%;
  background-color: var(--bg-card);
  border-left: 1px solid var(--border-color);
  overflow: hidden;
}

.copilot-header {
  flex-shrink: 0;
  height: 40px;
  padding: 0 10px;
  display: flex;
  align-items: center;
  gap: 8px;
  border-bottom: 1px solid var(--border-color);

  .header-title {
    display: flex;
    align-items: center;
    gap: 6px;
    font-weight: 600;
    font-size: 13px;
    color: var(--text-primary);
  }

  .header-host {
    display: flex;
    align-items: center;
    gap: 4px;
    margin-left: auto;
    overflow: hidden;
  }
}

.copilot-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
  -webkit-overflow-scrolling: touch;
  padding: 10px;
}

.copilot-empty {
  .empty-hint {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.6;
    margin: 4px 0 14px;
  }

  .quick-title {
    font-size: 12px;
    color: var(--text-secondary);
    margin-bottom: 8px;
  }

  .quick-btn {
    margin-bottom: 8px;
    justify-content: flex-start;
  }
}

.copilot-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 40px 12px;
  color: var(--text-secondary);
  font-size: 12px;
  text-align: center;
}

.plan-card {
  background-color: var(--bg-active);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 10px;
  margin-bottom: 12px;

  .plan-card-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 6px;

    .plan-title {
      font-weight: 600;
      font-size: 13px;
      color: var(--text-primary);
    }
  }

  .plan-summary {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
    margin: 0 0 8px;
  }

  .plan-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;

    .step-count {
      font-size: 11px;
      color: var(--text-secondary);
      margin-left: auto;
    }
  }
}

.steps {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.step {
  border: 1px solid var(--border-color);
  border-radius: 6px;
  overflow: hidden;
  transition: border-color 0.2s;

  &.active {
    border-color: rgba(99, 102, 241, 0.5);
  }
  &.success {
    border-color: rgba(24, 160, 88, 0.35);
  }
  &.failed {
    border-color: rgba(208, 48, 80, 0.45);
  }
  &.confirm {
    border-color: rgba(240, 160, 32, 0.5);
  }

  .step-head {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 7px 8px;
    cursor: pointer;
    font-size: 12px;

    &:hover {
      background-color: var(--tab-hover);
    }

    .step-idx {
      color: var(--text-secondary);
      font-variant-numeric: tabular-nums;
    }

    .step-title {
      flex: 1;
      color: var(--text-primary);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .step-dur {
      font-size: 11px;
      color: var(--text-secondary);
    }

    .step-caret {
      color: var(--text-secondary);
    }
  }

  .step-detail {
    padding: 0 8px 8px 8px;
    border-top: 1px solid var(--border-color);

    .step-desc {
      font-size: 12px;
      color: var(--text-secondary);
      line-height: 1.5;
      margin: 8px 0;
    }

    .step-cmd {
      display: flex;
      align-items: center;
      gap: 6px;
      background-color: var(--code-box-bg);
      border-radius: 4px;
      padding: 6px 8px;
      margin-bottom: 8px;

      code {
        font-family: var(--font-mono, monospace);
        font-size: 11.5px;
        color: #10b981;
        word-break: break-all;
      }
    }
  }
}

.step-confirm {
  background-color: rgba(240, 160, 32, 0.08);
  border-radius: 4px;
  padding: 8px;
  margin-bottom: 8px;

  .confirm-warn {
    display: block;
    font-size: 12px;
    color: #d98e00;
    margin-bottom: 8px;
  }

  .confirm-actions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
}

.step-output {
  background-color: var(--code-box-bg);
  border-radius: 4px;
  padding: 6px 8px;
  margin: 0 0 6px;
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  line-height: 1.45;
  color: var(--text-secondary);
  max-height: 180px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;

  &.stderr {
    color: #e88;
  }
}

.step-diag {
  .diag-loading {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    color: var(--text-secondary);
    padding: 4px 0;
  }

  .diag-content {
    background-color: rgba(240, 160, 32, 0.08);
    border-radius: 4px;
    padding: 8px;
    margin-bottom: 6px;

    .diag-title {
      display: flex;
      align-items: center;
      gap: 4px;
      font-size: 12px;
      font-weight: 600;
      color: var(--text-primary);
      margin-bottom: 4px;
    }

    .diag-text {
      font-size: 12px;
      color: var(--text-secondary);
      line-height: 1.55;
      white-space: pre-wrap;
    }
  }

  .diag-actions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
}

.plan-actions {
  margin-top: 12px;
}

.copilot-input {
  flex-shrink: 0;
  height: 52px;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-top: 1px solid var(--border-color);
  background-color: var(--bg-card);
  box-sizing: border-box;

  :deep(.n-input) {
    flex: 1;
    min-width: 0;
    height: 34px;

    .n-input-wrapper {
      padding: 0 10px;
    }

    input {
      height: 34px;
      line-height: 34px;
    }
  }

  .n-button {
    flex-shrink: 0;
    height: 34px;
    padding: 0 12px;
  }
}

.spin {
  animation: copilot-spin 1s linear infinite;
}
@keyframes copilot-spin {
  from {
    transform: rotate(0deg);
  }
  to {
    transform: rotate(360deg);
  }
}
</style>
