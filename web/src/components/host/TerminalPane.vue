<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, computed } from 'vue'
import { useRoute } from 'vue-router'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'
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
} from '@vicons/ionicons5'
import { openTerminal } from '../../api/hosts'
import { nl2command } from '../../api/ai'
import { useWorkspaceStore } from '../../stores/workspace'
import RemoteAssistModal from '../terminal/RemoteAssistModal.vue'

const props = withDefaults(
  defineProps<{
    hostId: string
    shell?: string
  }>(),
  {
    shell: '',
  },
)
const emit = defineEmits<{(e: 'ended'): void }>()
const message = useMessage()
const route = useRoute()

// Derive a real label from the route/host. The HostDetail view sets the
// workspace tab title to the hostname, and the route param :id is the host id.
const hostLabel = computed(() => {
  const ws = useWorkspaceStore()
  const tab = ws.tabs.find((t) => t.key === route.path)
  return tab?.title || props.hostId.slice(0, 8)
})

const containerEl = ref<HTMLElement | null>(null)
const selectedTheme = ref('Dracula')
const showRemoteAssist = ref(false)
const currentSid = ref('')

const aiInput = ref('')
const aiGenerating = ref(false)

const themeOptions = [
  { label: 'Dracula', value: 'Dracula' },
  { label: 'Miku', value: 'Miku' },
  { label: 'Solarized Dark', value: 'Solarized Dark' },
  { label: 'Monokai', value: 'Monokai' },
]

// All live state lives in one closure so the event handlers can see it.
let term: Terminal | null = null
let fit: FitAddon | null = null
let ws: WebSocket | null = null
let sid = ''
let pingTimer: ReturnType<typeof setInterval> | null = null

function b64encode(s: string): string {
  const bytes = new TextEncoder().encode(s)
  let bin = ''
  bytes.forEach((b) => (bin += String.fromCharCode(b)))
  return btoa(bin)
}
function b64decode(s: string): string {
  const bin = atob(s)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return new TextDecoder().decode(bytes)
}

function sendResize() {
  if (!term || !ws || ws.readyState !== WebSocket.OPEN || !sid) return
  ws.send(
    JSON.stringify({ type: 'resize', sid, cols: term.cols, rows: term.rows })
  )
}

function onWinResize() {
  fit?.fit()
}

function stopPing() {
  if (pingTimer) {
    clearInterval(pingTimer)
    pingTimer = null
  }
}

async function handleAiGenerate() {
  if (!aiInput.value.trim()) return
  aiGenerating.value = true
  try {
    const res = await nl2command(aiInput.value, props.hostId)
    if (!res.command) {
      message.warning('AI 未能生成有效命令')
      return
    }
    // High-risk commands require explicit human confirmation before sending
    // (Agent.md 3.16.7: AI never auto-runs destructive commands).
    if (res.needs_confirm || res.risk_level === 'high') {
      const ok = window.confirm(
        `AI 生成了高风险命令，请确认后执行：\n\n${res.command}\n\n${res.explanation ? '说明: ' + res.explanation + '\n\n' : ''}点击「确定」发送到终端，「取消」放弃。`
      )
      if (!ok) return
    } else if (res.explanation) {
      message.info(`已生成: ${res.command} (${res.explanation})`)
    }
    if (term && ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'input', sid, data: b64encode(res.command + '\n') }))
      message.success('命令已发送到终端')
      aiInput.value = ''
    }
  } catch (e: any) {
    message.error(e.message || 'AI 命令生成失败')
  } finally {
    aiGenerating.value = false
  }
}

async function start() {
  if (!containerEl.value) return
  const res = await openTerminal(props.hostId, props.shell)
  sid = res.session_id
  currentSid.value = sid

  term = new Terminal({
    fontSize: 14,
    fontFamily: 'Consolas, "Cascadia Code", "Courier New", monospace',
    cursorBlink: true,
    theme: { background: '#0d1117', foreground: '#c9d1d9' },
  })
  fit = new FitAddon()
  term.loadAddon(fit)
  term.loadAddon(new SearchAddon())
  try {
    term.loadAddon(new WebglAddon())
  } catch {
    // WebGL fallback
  }
  term.open(containerEl.value)
  fit.fit()

  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  // The WS gateway requires ?token=<jwt> for auth; append it if the server
  // didn't already include it in ws_url.
  let wsUrl = `${proto}://${location.host}${res.ws_url}`
  if (!wsUrl.includes('token=')) {
    const tok = localStorage.getItem('watchman_token') || ''
    const sep = wsUrl.includes('?') ? '&' : '?'
    wsUrl += `${sep}token=${encodeURIComponent(tok)}`
  }
  ws = new WebSocket(wsUrl)
  ws.binaryType = 'arraybuffer'

  ws.onopen = () => {
    term?.writeln('\x1b[32m● 已成功建立 SSH/gRPC 终端会话\x1b[0m')
    sendResize()
    pingTimer = setInterval(() => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'ping' }))
      }
    }, 25000)
  }

  ws.onmessage = (ev) => {
    try {
      const raw =
        typeof ev.data === 'string'
          ? ev.data
          : new TextDecoder().decode(ev.data)
      const f = JSON.parse(raw)
      if (f.type === 'output' && f.data) {
        term?.write(b64decode(f.data))
      } else if (f.type === 'ended') {
        term?.writeln(
          `\x1b[31m\n● 会话结束${f.reason ? ': ' + f.reason : ''}\x1b[0m`
        )
        emit('ended')
      }
    } catch {
      // ignore
    }
  }

  ws.onclose = () => {
    term?.writeln('\x1b[31m\n● 连接已关闭\x1b[0m')
    stopPing()
  }

  ws.onerror = () => {
    term?.writeln('\x1b[31m● 连接错误\x1b[0m')
  }

  term.onData((data) => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'input', sid, data: b64encode(data) }))
    }
  })

  term.onResize(() => sendResize())
  window.addEventListener('resize', onWinResize)
}

onMounted(() => {
  start().catch((e) => {
    term?.writeln(`\x1b[31m● 打开终端失败: ${e}\x1b[0m`)
  })
})

onBeforeUnmount(() => {
  stopPing()
  window.removeEventListener('resize', onWinResize)
  ws?.close()
  term?.dispose()
  term = null
  ws = null
})
</script>

<template>
  <div class="terminal-pane">
    <!-- 顶栏 Shell Tabs + 主题选择 + 远程协助 Modal -->
    <div class="terminal-top-toolbar">
      <div class="shell-tabs">
        <div class="shell-tab active">
          <span>{{ hostLabel }}</span>
        </div>
        <button class="add-tab-btn" title="多标签支持开发中" disabled>
          <NIcon size="14"><AddOutline /></NIcon>
        </button>
      </div>

      <div class="toolbar-right">
        <NSelect
          v-model:value="selectedTheme"
          :options="themeOptions"
          size="tiny"
          style="width: 120px"
        />

        <NButton
          secondary
          type="primary"
          size="tiny"
          @click="showRemoteAssist = true"
        >
          <template #icon>
            <NIcon><ShareSocialOutline /></NIcon>
          </template>
          远程协助
        </NButton>
      </div>
    </div>

    <!-- xterm.js Canvas 主体 -->
    <div class="terminal-body">
      <div ref="containerEl" class="xterm-container" />
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
        padding: 4px 10px;
        border-radius: 4px;
        font-size: 12px;
        background-color: var(--tab-bg);
        color: var(--text-secondary);
        cursor: pointer;
        border: 1px solid var(--border-color);

        &.active {
          background-color: var(--tab-active-bg);
          color: #6366f1;
          font-weight: 600;
          border-color: rgba(99, 102, 241, 0.4);
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

  .terminal-body {
    flex: 1;
    min-height: 0;
    padding: 4px;
  }

  .xterm-container {
    width: 100%;
    height: 100%;
  }

  .ai-cmd-bar {
    padding: 8px 12px;
    background-color: var(--bg-card);
    border-top: 1px solid var(--border-color);
    display: flex;
    gap: 8px;
  }
}
</style>
