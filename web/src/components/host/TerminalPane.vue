<script setup lang="ts">
// Tab owner for a host's terminal. Each tab mounts its own TerminalSession
// (its own PTY / WebSocket / xterm), and inactive tabs stay mounted so their
// scrollback and running processes survive tab switches.
import { computed, nextTick, onActivated, ref } from 'vue'
import { useRoute } from 'vue-router'
import {
  NSelect,
  NButton,
  NIcon,
  NInput,
  useMessage,
} from 'naive-ui'
import {
  ShareSocialOutline,
  SparklesOutline,
  AddOutline,
  CloseOutline,
} from '@vicons/ionicons5'
import { nl2command } from '../../api/ai'
import { useWorkspaceStore } from '../../stores/workspace'
import { useTermPrefsStore } from '../../stores/termPrefs'
import TerminalSession from './TerminalSession.vue'
import RemoteAssistModal from '../terminal/RemoteAssistModal.vue'
import TerminalAiCopilot from '../terminal/TerminalAiCopilot.vue'

const props = withDefaults(
  defineProps<{
    hostId: string
    shell?: string
    os?: string
    distro?: string
  }>(),
  {
    shell: '',
    os: '',
    distro: '',
  },
)
const emit = defineEmits<{ (e: 'ended'): void }>()
const message = useMessage()
const route = useRoute()
const termPrefs = useTermPrefsStore()

// Each tab holds one PTY. The cap keeps a stray click from opening dozens of
// shells on the managed host.
const MAX_TABS = 8

interface TermTab {
  id: string
  seq: number
  sid: string
  ended: boolean
}

let seqCounter = 0
function makeTab(): TermTab {
  seqCounter += 1
  return { id: `t${Date.now().toString(36)}-${seqCounter}`, seq: seqCounter, sid: '', ended: false }
}

const tabs = ref<TermTab[]>([makeTab()])
const activeId = ref(tabs.value[0].id)

// Derive a real label from the route/host. The HostDetail view sets the
// workspace tab title to the hostname, and the route param :id is the host id.
const hostLabel = computed(() => {
  const ws = useWorkspaceStore()
  const tab = ws.tabs.find((t) => t.key === route.path)
  return tab?.title || props.hostId.slice(0, 8)
})

function tabTitle(t: TermTab): string {
  return t.seq === 1 ? hostLabel.value : `${hostLabel.value} #${t.seq}`
}

// A pinned shell (the Docker container-exec modal) is a single-purpose
// session, so it does not grow tabs.
const canAddTab = computed(() => !props.shell)

const showRemoteAssist = ref(false)
const aiInput = ref('')
const aiGenerating = ref(false)

// AI Copilot side panel: opens beside the terminal so the operator can watch a
// multi-step plan run while the shell output streams inline.
const showCopilot = ref(false)
const copilotRef = ref<InstanceType<typeof TerminalAiCopilot> | null>(null)

function toggleCopilot() {
  showCopilot.value = !showCopilot.value
  // The terminal canvas width changes when the side panel opens/closes, so the
  // active session must re-measure or its columns will be wrong.
  nextTick(() => {
    setTimeout(() => sessionRefs.get(activeId.value)?.refit(), 60)
  })
}

// The bottom bar's "多步骤任务" button hands the current draft to the copilot:
// open the panel and generate a plan from whatever the user typed (or let them
// pick a quick intent if the box is empty).
function handleComplexTask() {
  const draft = aiInput.value.trim()
  if (!showCopilot.value) {
    showCopilot.value = true
    nextTick(() => setTimeout(() => sessionRefs.get(activeId.value)?.refit(), 60))
  }
  if (draft) {
    nextTick(() => copilotRef.value?.seed(draft))
    aiInput.value = ''
  }
}

// Echoes AI progress markers into the live terminal view (local only — never
// sent to the PTY).
function copilotEcho(text: string) {
  sessionRefs.get(activeId.value)?.writeToScreen(text)
}

// Pushes a command into the PTY (used by the copilot's "仅推送到终端" action).
function copilotSend(cmd: string) {
  const ok = sessionRefs.get(activeId.value)?.sendInput(cmd + '\n')
  if (!ok) message.error('终端连接不可用，命令未发送')
}

// A human-readable OS label for the copilot header.
const osLabel = computed(() => {
  const parts = [props.distro || props.os].filter(Boolean)
  return parts.join(' ')
})

const activeTab = computed(() => tabs.value.find((t) => t.id === activeId.value) || null)
const currentSid = computed(() => activeTab.value?.sid || '')

const selectedTheme = computed(() => termPrefs.prefs.theme)
// Theme names come from the server (prefs.Themes) so the picker cannot offer
// a value the server would reject.
const themeOptions = computed(() =>
  termPrefs.themes.map((t) => ({ label: t, value: t })),
)

// Live session handles, so the toolbar can drive whichever tab is on screen.
interface SessionHandle {
  sendInput: (data: string) => boolean
  refit: () => void
  focus: () => void
  writeToScreen: (text: string) => void
}
const sessionRefs = new Map<string, SessionHandle>()
function setSessionRef(id: string, el: any) {
  if (el) sessionRefs.set(id, el as SessionHandle)
  else sessionRefs.delete(id)
}

// Persisting the theme repaints every open session (each one watches the
// store), so a switch is consistent across tabs and machines.
async function applyTheme(name: string) {
  const previous = termPrefs.prefs.theme
  termPrefs.prefs.theme = name
  try {
    await termPrefs.save({ ...termPrefs.prefs, theme: name })
  } catch (e: any) {
    termPrefs.prefs.theme = previous
    message.warning(e.message || '终端主题保存失败')
  }
}

function addTab() {
  if (tabs.value.length >= MAX_TABS) {
    message.warning(`单台主机最多同时打开 ${MAX_TABS} 个终端标签`)
    return
  }
  const t = makeTab()
  tabs.value.push(t)
  activeId.value = t.id
}

function selectTab(id: string) {
  activeId.value = id
  sessionRefs.get(id)?.focus()
}

// 页签被 KeepAlive 缓存时容器被移出布局，xterm 量到的尺寸是 0。切回来要让
// 当前会话按真实宽高重新测量并把尺寸同步给远端 PTY，否则回显会错行。
onActivated(() => {
  nextTick(() => {
    const s = sessionRefs.get(activeId.value)
    s?.refit()
    s?.focus()
  })
})

function closeTab(id: string) {
  const idx = tabs.value.findIndex((t) => t.id === id)
  if (idx < 0) return
  tabs.value.splice(idx, 1)
  sessionRefs.delete(id)
  if (activeId.value !== id) return
  // The close control is hidden on the last tab, so a neighbour always exists;
  // falling back to it keeps focus near where the user was.
  const next = tabs.value[idx] || tabs.value[idx - 1]
  activeId.value = next.id
}

function onOpened(id: string, sid: string) {
  const t = tabs.value.find((x) => x.id === id)
  if (t) t.sid = sid
}

function onEnded(id: string) {
  const t = tabs.value.find((x) => x.id === id)
  if (t) t.ended = true
  emit('ended')
}

async function handleAiGenerate() {
  if (!aiInput.value.trim()) return
  const target = activeId.value
  if (!target) {
    message.warning('请先新建一个终端标签')
    return
  }
  aiGenerating.value = true
  try {
    const res = await nl2command(aiInput.value, props.hostId)
    if (!res.command) {
      message.warning('AI 未能生成有效命令')
      return
    }
    // High-risk commands require explicit human confirmation before sending
    // (AGENTS.md 3.16.7: AI never auto-runs destructive commands).
    if (res.needs_confirm || res.risk_level === 'high') {
      const ok = window.confirm(
        `AI 生成了高风险命令，请确认后执行：\n\n${res.command}\n\n${res.explanation ? '说明: ' + res.explanation + '\n\n' : ''}点击「确定」发送到终端，「取消」放弃。`
      )
      if (!ok) return
    } else if (res.explanation) {
      message.info(`已生成: ${res.command} (${res.explanation})`)
    }
    if (sessionRefs.get(target)?.sendInput(res.command + '\n')) {
      message.success('命令已发送到终端')
      aiInput.value = ''
    } else {
      message.error('终端连接不可用，命令未发送')
    }
  } catch (e: any) {
    message.error(e.message || 'AI 命令生成失败')
  } finally {
    aiGenerating.value = false
  }
}
</script>

<template>
  <div class="terminal-pane">
    <!-- 顶栏 Shell Tabs + 主题选择 + 远程协助 Modal -->
    <div class="terminal-top-toolbar">
      <div class="shell-tabs">
        <div
          v-for="t in tabs"
          :key="t.id"
          class="shell-tab"
          :class="{ active: t.id === activeId, ended: t.ended }"
          @click="selectTab(t.id)"
        >
          <span class="tab-title">{{ tabTitle(t) }}</span>
          <NIcon
            v-if="tabs.length > 1"
            class="tab-close"
            size="12"
            title="关闭该终端标签"
            @click.stop="closeTab(t.id)"
          >
            <CloseOutline />
          </NIcon>
        </div>
        <button
          v-if="canAddTab"
          class="add-tab-btn"
          title="新建终端标签"
          @click="addTab"
        >
          <NIcon size="14"><AddOutline /></NIcon>
        </button>
      </div>

      <div class="toolbar-right">
        <NSelect
          :value="selectedTheme"
          :options="themeOptions"
          size="tiny"
          style="width: 130px"
          @update:value="applyTheme"
        />

        <NButton
          secondary
          type="primary"
          size="tiny"
          :disabled="!currentSid"
          @click="showRemoteAssist = true"
        >
          <template #icon>
            <NIcon><ShareSocialOutline /></NIcon>
          </template>
          远程协助
        </NButton>

        <NButton
          :type="showCopilot ? 'primary' : 'default'"
          :secondary="showCopilot"
          size="tiny"
          title="AI 运维助手：多步骤任务规划与自动执行"
          @click="toggleCopilot"
        >
          <template #icon>
            <NIcon><SparklesOutline /></NIcon>
          </template>
          AI 助手
        </NButton>
      </div>
    </div>

    <!-- 终端主体 + AI Copilot 侧栏并排 -->
    <div class="terminal-workspace">
      <!-- xterm.js Canvas 主体：每个标签一个独立会话，隐藏的标签保持运行 -->
      <div class="terminal-body">
        <div
          v-for="t in tabs"
          v-show="t.id === activeId"
          :key="t.id"
          class="session-slot"
        >
          <TerminalSession
            :ref="(el) => setSessionRef(t.id, el)"
            :host-id="hostId"
            :shell="shell"
            :visible="t.id === activeId"
            @opened="(sid: string) => onOpened(t.id, sid)"
            @ended="onEnded(t.id)"
          />
        </div>
      </div>

      <!-- AI 运维助手侧栏 -->
      <div v-if="showCopilot" class="copilot-slot">
        <TerminalAiCopilot
          ref="copilotRef"
          :host-id="hostId"
          :host-label="hostLabel"
          :os-label="osLabel"
          @close="toggleCopilot"
          @echo="copilotEcho"
          @send="copilotSend"
        />
      </div>
    </div>

    <!-- AI 命令行助手条 -->
    <div class="ai-cmd-bar">
      <NInput
        v-model:value="aiInput"
        placeholder="用自然语言描述意图（如：查看占用 CPU 最高的 5 个进程），按 Enter 自动生成命令并发送..."
        size="small"
        @keyup.enter="handleAiGenerate"
      >
        <template #prefix>
          <NIcon color="#6366f1"><SparklesOutline /></NIcon>
        </template>
      </NInput>
      <NButton
        type="primary"
        size="small"
        :loading="aiGenerating"
        @click="handleAiGenerate"
      >
        生成命令
      </NButton>
      <NButton
        secondary
        type="primary"
        size="small"
        title="多步骤任务：交给 AI 助手规划并自动执行"
        @click="handleComplexTask"
      >
        多步骤任务
      </NButton>
    </div>

    <!-- 远程协助 Modal -->
    <RemoteAssistModal
      v-model:show="showRemoteAssist"
      :host-id="hostId"
      :session-id="currentSid"
    />
  </div>
</template>

<style scoped lang="scss">
.terminal-pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  background-color: var(--code-box-bg);

  .terminal-top-toolbar {
    height: 38px;
    padding: 0 12px;
    background-color: var(--bg-card);
    display: flex;
    align-items: center;
    justify-content: space-between;
    border-bottom: 1px solid var(--border-color);

    .shell-tabs {
      display: flex;
      align-items: center;
      gap: 6px;

      .shell-tab {
        display: flex;
        align-items: center;
        gap: 6px;
        padding: 4px 10px;
        border-radius: 4px;
        font-size: 12px;
        background-color: var(--tab-bg);
        color: var(--text-secondary);
        cursor: pointer;
        border: 1px solid var(--border-color);
        max-width: 200px;

        .tab-title {
          overflow: hidden;
          text-overflow: ellipsis;
          white-space: nowrap;
        }

        .tab-close {
          flex-shrink: 0;
          opacity: 0.55;

          &:hover {
            opacity: 1;
          }
        }

        &:hover {
          background-color: var(--tab-hover);
        }

        &.active {
          background-color: var(--bg-active);
          color: #6366f1;
          font-weight: 600;
          border-color: rgba(99, 102, 241, 0.4);
        }

        &.ended .tab-title {
          text-decoration: line-through;
          opacity: 0.6;
        }
      }

      .add-tab-btn {
        background: transparent;
        border: none;
        color: var(--text-secondary);
        cursor: pointer;
        display: flex;
        align-items: center;
        padding: 4px;

        &:hover {
          color: var(--text-primary);
        }
      }
    }

    .toolbar-right {
      display: flex;
      align-items: center;
      gap: 10px;
    }
  }

  .terminal-workspace {
    flex: 1;
    min-height: 0;
    display: flex;
    overflow: hidden;
  }

  .terminal-body {
    flex: 1;
    min-width: 0;
    min-height: 0;
    padding: 4px;
    position: relative;

    .session-slot {
      width: 100%;
      height: 100%;
    }

    .empty-slot {
      display: flex;
      align-items: center;
      justify-content: center;
      height: 100%;
    }
  }

  .copilot-slot {
    flex-shrink: 0;
    width: 400px;
    max-width: 46%;
    height: 100%;
  }

  .ai-cmd-bar {
    flex-shrink: 0;
    height: 48px;
    padding: 8px 12px;
    background-color: var(--bg-card);
    border-top: 1px solid var(--border-color);
    display: flex;
    align-items: center;
    gap: 8px;
    box-sizing: border-box;

    :deep(.n-input) {
      flex: 1;
      min-width: 0;
      height: 32px;
    }

    .n-button {
      flex-shrink: 0;
      height: 32px;
    }
  }
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .terminal-pane {
    .terminal-top-toolbar {
      height: auto;
      min-height: 38px;
      flex-wrap: wrap;
      padding: 6px 8px;
      gap: 6px;

      .shell-tabs {
        overflow-x: auto;
        max-width: 100%;

        &::-webkit-scrollbar {
          display: none;
        }
      }

      .toolbar-right {
        margin-left: auto;
        gap: 6px;
      }
    }

    .ai-cmd-bar {
      flex-wrap: wrap;

      :deep(.n-select) {
        width: 100% !important;
      }
    }

    // 窄屏：AI 侧栏改为覆盖式全宽抽屉，避免终端被挤到不可用。
    .terminal-workspace {
      position: relative;
    }

    .copilot-slot {
      position: absolute;
      top: 0;
      right: 0;
      bottom: 0;
      width: 100%;
      max-width: 100%;
      z-index: 20;
      box-shadow: -4px 0 16px rgba(0, 0, 0, 0.28);
    }
  }
}
</style>
