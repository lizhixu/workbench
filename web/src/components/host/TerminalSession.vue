<script setup lang="ts">
// One component instance == one PTY session (one WebSocket, one xterm).
// Keeping the live state per-instance is what lets TerminalPane mount several
// of these side by side as tabs.
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'
import { openTerminal } from '../../api/hosts'
import { useTermPrefsStore } from '../../stores/termPrefs'
import { resolveTermTheme } from '../../utils/termThemes'

const props = withDefaults(
  defineProps<{
    hostId: string
    shell?: string
    /** A hidden tab keeps running; it just stops being laid out. */
    visible?: boolean
  }>(),
  {
    shell: '',
    visible: true,
  },
)
const emit = defineEmits<{
  (e: 'ended'): void
  (e: 'opened', sid: string): void
}>()

const termPrefs = useTermPrefsStore()
const containerEl = ref<HTMLElement | null>(null)

// All live state lives in one closure so the event handlers can see it.
let term: Terminal | null = null
let fit: FitAddon | null = null
let ws: WebSocket | null = null
let sid = ''
let pingTimer: ReturnType<typeof setInterval> | null = null

// ---- 断线重连 ----
// 服务端在最后一个客户端离开后保留 PTY 60 秒，所以网络抖动、合盖休眠之后重连
// 同一个 sid 就能拿回原来的 shell（滚屏与运行中的进程都在）。重连必须复用现有
// 的 xterm 实例，重建终端等于清屏。
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectAttempt = 0
let closedByUs = false
let sessionEnded = false
const RECONNECT_MAX_DELAY = 15000
// 超过服务端宽限期还没连上，PTY 已被回收，再重试只会连到一个不存在的会话。
const RECONNECT_GIVE_UP_MS = 60000
let firstDisconnectAt = 0

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

// Exposed so the pane's toolbar (AI bar, paste, future actions) can drive
// whichever session is currently on screen.
function sendInput(data: string): boolean {
  if (!ws || ws.readyState !== WebSocket.OPEN || !sid) return false
  ws.send(JSON.stringify({ type: 'input', sid, data: b64encode(data) }))
  return true
}

function refit() {
  fit?.fit()
}

function focus() {
  term?.focus()
}

// Lets the AI Copilot echo progress markers into the live terminal so the
// operator sees automation activity inline with the shell output. This does
// NOT send anything to the PTY — it only writes to the local xterm view.
function writeToScreen(text: string) {
  term?.write(text)
}

defineExpose({ sendInput, refit, focus, writeToScreen })

async function start() {
  if (!containerEl.value) return
  // Preferences decide the palette/font and the shell used when the caller
  // didn't pin one, so they must resolve before the terminal is constructed.
  const p = await termPrefs.load()
  const res = await openTerminal(props.hostId, props.shell || p.default_shell)
  sid = res.session_id
  emit('opened', sid)

  term = new Terminal({
    fontSize: p.font_size,
    fontFamily: p.font_family,
    cursorBlink: p.cursor_blink,
    scrollback: p.scrollback,
    theme: resolveTermTheme(p.theme),
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

  // 键盘输入与尺寸变化只绑一次：重连换的是 socket，不是终端。
  term.onData((data) => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'input', sid, data: b64encode(data) }))
    }
  })
  term.onResize(() => sendResize())
  window.addEventListener('resize', onWinResize)

  connect(res.ws_url)
}

/** 用当前 sid 建立（或重建）WebSocket。term 已存在，这里只换连接。 */
function connect(wsPath: string) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  // The WS gateway requires ?token=<jwt> for auth; append it if the server
  // didn't already include it in ws_url.
  let wsUrl = `${proto}://${location.host}${wsPath}`
  if (!wsUrl.includes('token=')) {
    const tok = localStorage.getItem('watchman_token') || ''
    const sep = wsUrl.includes('?') ? '&' : '?'
    wsUrl += `${sep}token=${encodeURIComponent(tok)}`
  }
  ws = new WebSocket(wsUrl)
  ws.binaryType = 'arraybuffer'

  ws.onopen = () => {
    if (reconnectAttempt > 0) {
      term?.writeln('\x1b[32m● 已重新连接，会话已恢复\x1b[0m')
    } else {
      term?.writeln('\x1b[32m● 已成功建立 SSH/gRPC 终端会话\x1b[0m')
    }
    reconnectAttempt = 0
    firstDisconnectAt = 0
    // 重连后终端尺寸要重新告知远端 PTY，否则回显按旧尺寸换行。
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
        // 会话是真的结束了（用户 exit、PTY 退出），不要再重连。
        sessionEnded = true
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
    stopPing()
    if (closedByUs || sessionEnded) return
    scheduleReconnect(wsPath)
  }

  ws.onerror = () => {
    // onerror 之后浏览器一定会再触发 onclose，重连逻辑统一放在那里。
  }
}

/**
 * 指数退避重连。服务端保留 PTY 60 秒，所以在这个窗口内连回来就能拿到原来的
 * shell；超过窗口 PTY 已被回收，继续重试只会连上一个不存在的会话，因此放弃
 * 并提示用户新开标签。
 */
function scheduleReconnect(wsPath: string) {
  if (reconnectTimer) return
  const now = Date.now()
  if (firstDisconnectAt === 0) firstDisconnectAt = now

  if (now - firstDisconnectAt > RECONNECT_GIVE_UP_MS) {
    term?.writeln('\x1b[31m● 连接中断超过 60 秒，会话已被回收，请新开一个终端标签\x1b[0m')
    emit('ended')
    return
  }

  const delay = Math.min(1000 * 2 ** reconnectAttempt, RECONNECT_MAX_DELAY)
  reconnectAttempt += 1
  term?.writeln(
    `\x1b[33m● 连接中断，${Math.round(delay / 1000)} 秒后尝试重连（第 ${reconnectAttempt} 次）\x1b[0m`,
  )
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    connect(wsPath)
  }, delay)
}

// A hidden container measures zero, so xterm has to re-measure and re-send its
// size when the tab comes back on screen.
watch(
  () => props.visible,
  (v) => {
    if (!v) return
    nextTick(() => {
      fit?.fit()
      sendResize()
      term?.focus()
    })
  }
)

// The theme picker writes to the shared preference store; every open session
// repaints, not just the one whose toolbar was used.
watch(
  () => termPrefs.prefs.theme,
  (name) => {
    if (term) term.options.theme = resolveTermTheme(name)
  }
)

onMounted(() => {
  start().catch((e) => {
    term?.writeln(`\x1b[31m● 打开终端失败: ${e}\x1b[0m`)
  })
})

onBeforeUnmount(() => {
  // 组件真的被销毁（关闭终端标签、关闭页签）时才主动断开，并标记为「我们关的」，
  // 免得 onclose 又去安排一次重连。
  closedByUs = true
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
  stopPing()
  window.removeEventListener('resize', onWinResize)
  ws?.close()
  term?.dispose()
  term = null
  ws = null
})
</script>

<template>
  <div ref="containerEl" class="xterm-container" />
</template>

<style scoped lang="scss">
.xterm-container {
  width: 100%;
  height: 100%;
}
</style>
