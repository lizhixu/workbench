<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard,
  NInput,
  NButton,
  NTag,
  NSpace,
  NIcon,
  useMessage,
} from 'naive-ui'
import {
  TerminalOutline,
  EyeOutline,
  CreateOutline,
  TimeOutline,
} from '@vicons/ionicons5'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import '@xterm/xterm/css/xterm.css'
import { getShareInfo, type ShareInfoResponse } from '../../api/hosts'
import { fmtDateTime } from '../../utils/time'

const route = useRoute()
const message = useMessage()

const inputTokenOrCode = ref((route.query.token as string) || (route.query.code as string) || '')
const shareInfo = ref<ShareInfoResponse | null>(null)
const loading = ref(false)
const connected = ref(false)
const errorMsg = ref('')

const containerEl = ref<HTMLElement | null>(null)
let term: Terminal | null = null
let fit: FitAddon | null = null
let ws: WebSocket | null = null
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
  if (!term || !ws || ws.readyState !== WebSocket.OPEN || !shareInfo.value) return
  if (shareInfo.value.mode === 'control') {
    ws.send(
      JSON.stringify({
        type: 'resize',
        sid: shareInfo.value.session_id,
        cols: term.cols,
        rows: term.rows,
      }),
    )
  }
}

function onWinResize() {
  fit?.fit()
}

async function joinSession() {
  const key = inputTokenOrCode.value.trim()
  if (!key) {
    message.warning('请输入协同口令或 Token')
    return
  }

  loading.value = true
  errorMsg.value = ''
  try {
    const info = await getShareInfo(key)
    shareInfo.value = info
    loading.value = false
    connectWs(info, key)
  } catch (e: any) {
    loading.value = false
    errorMsg.value = e.message || '无效或已过期的协同口令'
  }
}

function connectWs(info: ShareInfoResponse, tokenOrCode: string) {
  if (!containerEl.value) return

  term = new Terminal({
    fontSize: 14,
    fontFamily: 'Consolas, "Cascadia Code", "Courier New", monospace',
    cursorBlink: info.mode === 'control',
    disableStdin: info.mode !== 'control',
    theme: { background: '#0d1117', foreground: '#c9d1d9' },
  })
  fit = new FitAddon()
  term.loadAddon(fit)
  try {
    term.loadAddon(new WebglAddon())
  } catch {
    // WebGL fallback
  }
  term.open(containerEl.value)
  fit.fit()

  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const wsUrl = `${proto}://${location.host}/api/v1/ws/terminal/${info.session_id}?share_token=${encodeURIComponent(tokenOrCode)}`

  ws = new WebSocket(wsUrl)
  ws.binaryType = 'arraybuffer'

  ws.onopen = () => {
    connected.value = true
    term?.writeln(`\x1b[32m● 已连接协同终端会话 [${info.hostname}]\x1b[0m`)
    term?.writeln(
      info.mode === 'control'
        ? '\x1b[33m● 当前权限：完全控制 (您可以直接输入命令协同操作)\x1b[0m\r\n'
        : '\x1b[36m● 当前权限：只读观看 (仅允许观看操作，键盘输入已被锁定)\x1b[0m\r\n',
    )
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
          `\x1b[31m\r\n● 终端会话已结束${f.reason ? ': ' + f.reason : ''}\x1b[0m`,
        )
      }
    } catch {
      // ignore
    }
  }

  ws.onclose = () => {
    connected.value = false
    term?.writeln('\x1b[31m\r\n● 协同连接已断开\x1b[0m')
    if (pingTimer) clearInterval(pingTimer)
  }

  ws.onerror = () => {
    connected.value = false
    term?.writeln('\x1b[31m● 连接异常\x1b[0m')
  }

  if (info.mode === 'control') {
    term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', sid: info.session_id, data: b64encode(data) }))
      }
    })
    term.onResize(() => sendResize())
  }

  window.addEventListener('resize', onWinResize)
}

onMounted(() => {
  if (inputTokenOrCode.value) {
    joinSession()
  }
})

onBeforeUnmount(() => {
  if (pingTimer) clearInterval(pingTimer)
  window.removeEventListener('resize', onWinResize)
  ws?.close()
  term?.dispose()
  term = null
  ws = null
})
</script>

<template>
  <div class="shared-terminal-page">
    <!-- 未连接时的口令输入卡片 -->
    <div v-if="!shareInfo" class="join-card-wrapper">
      <NCard title="加入终端协同会话" class="join-card" :bordered="true">
        <template #header-extra>
          <NIcon size="20" color="#6366f1"><TerminalOutline /></NIcon>
        </template>
        <p class="desc">
          输入管理员提供的 6 位口令或分享链接，即可即时接入正在运行的远程终端会话。
        </p>

        <NSpace vertical size="large" style="margin-top: 16px">
          <NInput
            v-model:value="inputTokenOrCode"
            size="large"
            placeholder="输入 6 位口令（如 2gw937）或完整 Token"
            @keyup.enter="joinSession"
          />
          <NButton
            type="primary"
            size="large"
            block
            :loading="loading"
            @click="joinSession"
          >
            立即连接
          </NButton>

          <div v-if="errorMsg" class="error-msg">
            {{ errorMsg }}
          </div>
        </NSpace>
      </NCard>
    </div>

    <!-- 已连接时的终端界面 -->
    <div v-else class="terminal-workspace">
      <!-- 顶栏状态信息 -->
      <div class="workspace-header">
        <div class="header-left">
          <NIcon size="18" color="#6366f1"><TerminalOutline /></NIcon>
          <span class="host-title">{{ shareInfo.hostname }}</span>
          <NTag
            :type="shareInfo.mode === 'control' ? 'warning' : 'info'"
            size="small"
            round
          >
            <template #icon>
              <NIcon :component="shareInfo.mode === 'control' ? CreateOutline : EyeOutline" />
            </template>
            {{ shareInfo.mode === 'control' ? '协同控制模式' : '只读观看模式' }}
          </NTag>
        </div>

        <div class="header-right">
          <NTag type="default" size="small" :bordered="false">
            <template #icon><NIcon :component="TimeOutline" /></template>
            有效期至 {{ fmtDateTime(shareInfo.expires_at) }}
          </NTag>
          <NTag :type="connected ? 'success' : 'error'" size="small">
            {{ connected ? '在线协同中' : '已断开' }}
          </NTag>
        </div>
      </div>

      <!-- 终端主体 -->
      <div class="terminal-container">
        <div ref="containerEl" class="xterm-view" />
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.shared-terminal-page {
  width: 100vw;
  height: 100vh;
  background-color: var(--bg-app);
  display: flex;
  flex-direction: column;

  .join-card-wrapper {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 20px;

    .join-card {
      width: 440px;
      background-color: var(--bg-card);
      border: 1px solid var(--border-color);

      .desc {
        font-size: 13px;
        color: var(--text-secondary);
        line-height: 1.6;
        margin: 0;
      }

      .error-msg {
        color: #ef4444;
        font-size: 13px;
        text-align: center;
      }
    }
  }

  .terminal-workspace {
    display: flex;
    flex-direction: column;
    height: 100%;

    .workspace-header {
      height: 44px;
      background-color: var(--bg-card);
      border-bottom: 1px solid var(--border-color);
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0 16px;

      .header-left {
        display: flex;
        align-items: center;
        gap: 10px;

        .host-title {
          font-weight: 600;
          font-size: 14px;
          color: var(--text-primary);
        }
      }

      .header-right {
        display: flex;
        align-items: center;
        gap: 8px;
      }
    }

    .terminal-container {
      flex: 1;
      min-height: 0;
      background: var(--code-box-bg);
      padding: 4px;

      .xterm-view {
        width: 100%;
        height: 100%;
      }
    }
  }
}
</style>
