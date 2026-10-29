<script setup lang="ts">
import { ref, nextTick, onBeforeUnmount, onMounted } from 'vue'
import {
  NInput,
  NButton,
  NIcon,
  NSpin,
  NEmpty,
  useMessage,
} from 'naive-ui'
import { ChatbubbleEllipsesOutline, PaperPlaneOutline, SparklesOutline, CopyOutline } from '@vicons/ionicons5'
import { chat, type ChatMessage, type ChatResponse } from '../../api/ai'
import { useWorkspaceStore } from '../../stores/workspace'
import { copyToClipboard } from '../../utils/clipboard'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'AiChat' })

const message = useMessage()
const workspace = useWorkspaceStore()

const inputText = ref('')
const loading = ref(false)
const conversations = ref<{ role: 'user' | 'assistant'; content: string }[]>([])
const chatListRef = ref<HTMLElement | null>(null)
const chatBodyRef = ref<HTMLElement | null>(null)

// ---- 输入框高度拖拽 ----
// 输入框默认单行高度；拖住上方把手向上拉可增高，消息列表相应让出空间。
// 下限 MIN_INPUT_H，上限保证消息列表至少留 INPUT_MIN_LIST_H，避免拖到把对话区挤没。
const MIN_INPUT_H = 64
const INPUT_MIN_LIST_H = 180
const inputH = ref(MIN_INPUT_H)
const resizingInput = ref(false)

function maxInputHeight(): number {
  const bodyH = chatBodyRef.value?.clientHeight ?? 0
  return Math.max(bodyH - INPUT_MIN_LIST_H, MIN_INPUT_H)
}

function startInputResize(e: PointerEvent) {
  if (e.pointerType === 'mouse' && e.button !== 0) return
  // 窄屏下输入区固定高度，不参与拖拽（把手也被 CSS 隐藏）。
  if (window.innerWidth <= 768) return
  const handle = e.currentTarget as HTMLElement
  const startY = e.clientY
  const startH = inputH.value
  const maxH = maxInputHeight()
  // 拖动前列表若停在底部，拖动后继续保持贴底，避免压缩后看不到最新消息。
  const listEl = chatListRef.value
  const stickBottom = listEl
    ? listEl.scrollHeight - listEl.scrollTop - listEl.clientHeight < 8
    : false

  // 必须做指针捕获：鼠标拖出窗口后直接松开时仍能收到 pointerup，
  // 否则监听残留会让之后的鼠标移动继续改高度（表现为「对话框自己越拖越大」）。
  try {
    handle.setPointerCapture(e.pointerId)
  } catch {
    /* 部分浏览器对合成事件不支持捕获，退化为普通监听 */
  }
  resizingInput.value = true
  document.body.style.cursor = 'row-resize'
  document.body.style.userSelect = 'none'

  const onMove = (ev: PointerEvent) => {
    // 向上拖（clientY 变小）增高，向下拖还原。
    const next = startH + (startY - ev.clientY)
    inputH.value = Math.min(Math.max(next, MIN_INPUT_H), maxH)
  }
  const onEnd = (ev: PointerEvent) => {
    try {
      handle.releasePointerCapture(ev.pointerId)
    } catch {
      /* 捕获已被浏览器释放 */
    }
    handle.removeEventListener('pointermove', onMove)
    handle.removeEventListener('pointerup', onEnd)
    handle.removeEventListener('pointercancel', onEnd)
    resizingInput.value = false
    document.body.style.cursor = ''
    document.body.style.userSelect = ''
    if (stickBottom) void scrollToBottom()
  }

  handle.addEventListener('pointermove', onMove)
  handle.addEventListener('pointerup', onEnd)
  handle.addEventListener('pointercancel', onEnd)
}

// 双击把手恢复单行高度（拖大后想快速复位）。
function resetInputHeight() {
  inputH.value = MIN_INPUT_H
}

// 窗口/容器变矮时把已拖高的输入框收敛回可用范围，避免撑破布局。
let bodyObserver: ResizeObserver | null = null

const suggestedQuestions = [
  '哪台机器磁盘快满了？',
  '最近有哪些异常告警？',
  '当前有多少台主机在线？',
  '哪台主机 CPU 负载最高？',
]

async function send(question?: string) {
  const q = (question || inputText.value).trim()
  if (!q || loading.value) return

  conversations.value.push({ role: 'user', content: q })
  inputText.value = ''
  loading.value = true
  await scrollToBottom()

  try {
    // Build history from prior messages (last 10 to keep prompt reasonable).
    const history: ChatMessage[] = conversations.value
      .slice(-11, -1)
      .map((m) => ({ role: m.role, content: m.content }))

    const resp: ChatResponse = await chat(q, history)
    conversations.value.push({ role: 'assistant', content: resp.answer })
  } catch (e: any) {
    conversations.value.push({ role: 'assistant', content: `抱歉，查询失败：${e.message}` })
    message.error(e.message || 'AI 问答失败')
  } finally {
    loading.value = false
    await scrollToBottom()
  }
}

async function scrollToBottom() {
  await nextTick()
  if (chatListRef.value) {
    chatListRef.value.scrollTop = chatListRef.value.scrollHeight
  }
}

async function copyMsg(content: string) {
  if (!content) return
  const ok = await copyToClipboard(content)
  if (ok) {
    message.success('已复制内容到剪贴板')
  } else {
    message.error('复制失败，请手动选中复制')
  }
}

// 仅覆盖 AI 回答中的加粗、行内代码、代码块与换行。
// 需要标题/列表/链接时改用 utils/markdown.ts 的 renderMarkdown（AGENTS.md §8.11），
// 迁移时传 codeBlockClass: 'chat-code-block'、inlineCodeClass: 'chat-inline-code' 保持现有样式。
function renderMarkdown(text: string): string {
  // Lightweight markdown rendering: code blocks, bold, line breaks.
  let html = text
    // Escape HTML
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    // Code blocks
    .replace(/```(\w*)\n([\s\S]*?)```/g, '<pre class="chat-code-block">$2</pre>')
    // Inline code
    .replace(/`([^`]+)`/g, '<code class="chat-inline-code">$1</code>')
    // Bold
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    // Line breaks
    .replace(/\n/g, '<br>')
  return html
}

onMounted(() => {
  workspace.openTab({
    key: '/ai/chat',
    title: 'AI 助手',
    path: '/ai/chat',
    closable: true,
  })

  // 容器高度变化时（窗口缩放、侧栏展开）收敛输入框高度。
  if (chatBodyRef.value) {
    bodyObserver = new ResizeObserver(() => {
      // KeepAlive 切走时容器高度会变成 0，此时不能拿它当上限，否则
      // 用户拖高的输入框会被重置（违反 §8.4 页签状态不丢）。
      const bodyH = chatBodyRef.value?.clientHeight ?? 0
      if (bodyH === 0) return
      const max = maxInputHeight()
      if (inputH.value > max) inputH.value = max
    })
    bodyObserver.observe(chatBodyRef.value)
  }
})

onBeforeUnmount(() => {
  bodyObserver?.disconnect()
  bodyObserver = null
  document.body.style.cursor = ''
  document.body.style.userSelect = ''
})
</script>

<template>
  <div class="ai-chat-view">
    <div class="chat-header">
      <h2 class="page-title">AI 运维助手</h2>
    </div>

    <div ref="chatBodyRef" class="chat-body">
      <!-- Conversation list -->
      <div ref="chatListRef" class="chat-list">
        <NEmpty v-if="conversations.length === 0" description="开始提问吧" class="chat-empty">
          <template #extra>
            <div class="suggestions">
              <div class="suggestion-label">试试这些问题：</div>
              <NButton
                v-for="q in suggestedQuestions"
                :key="q"
                size="small"
                secondary
                @click="send(q)"
              >
                {{ q }}
              </NButton>
            </div>
          </template>
        </NEmpty>

        <div
          v-for="(msg, i) in conversations"
          :key="i"
          class="chat-message"
          :class="msg.role"
        >
          <div class="msg-avatar">
            <NIcon v-if="msg.role === 'user'" size="16"><ChatbubbleEllipsesOutline /></NIcon>
            <NIcon v-else size="16" color="#6366f1"><SparklesOutline /></NIcon>
          </div>
          <div class="msg-bubble-wrap">
            <div class="msg-content" v-html="renderMarkdown(msg.content)" />
            <button
              class="msg-copy-btn"
              title="复制内容"
              @click="copyMsg(msg.content)"
            >
              <NIcon size="14"><CopyOutline /></NIcon>
            </button>
          </div>
        </div>

        <div v-if="loading" class="chat-message assistant">
          <div class="msg-avatar">
            <NIcon size="16" color="#6366f1"><SparklesOutline /></NIcon>
          </div>
          <div class="msg-content">
            <NSpin size="small" />
            <span class="typing-hint">正在思考...</span>
          </div>
        </div>
      </div>

      <!-- Input area -->
      <div class="chat-input-area">
        <div
          class="chat-resize-handle"
          :class="{ active: resizingInput }"
          role="separator"
          aria-orientation="horizontal"
          :aria-valuemin="MIN_INPUT_H"
          :aria-valuemax="maxInputHeight()"
          :aria-valuenow="Math.round(inputH)"
          title="上下拖拽调整输入框高度，双击恢复"
          @pointerdown.prevent="startInputResize"
          @dblclick="resetInputHeight"
        >
          <span class="grip" />
        </div>
        <div class="input-row">
          <NInput
            v-model:value="inputText"
            type="textarea"
            :rows="2"
            placeholder="输入你的运维问题，例如：哪台机器磁盘快满了？最近有哪些异常告警？（Shift+Enter 换行，Enter 发送）"
            class="fixed-chat-input"
            :style="{ height: `${inputH}px` }"
            @keydown.enter.exact.prevent="send()"
          />
          <NButton
            type="primary"
            class="send-btn"
            :loading="loading"
            :disabled="!inputText.trim()"
            @click="send()"
          >
            <template #icon><NIcon><PaperPlaneOutline /></NIcon></template>
            发送
          </NButton>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.ai-chat-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  box-sizing: border-box;
  gap: 12px;

  .chat-header {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-shrink: 0;

    .page-title {
      margin: 0;
      font-size: 18px;
      font-weight: 600;
      color: var(--text-primary);
    }
  }

  .chat-body {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .chat-list {
    flex: 1;
    overflow-y: auto;
    padding: 8px 4px;
    display: flex;
    flex-direction: column;
    gap: 16px;

    .chat-empty {
      margin-top: 60px;

      .suggestions {
        display: flex;
        flex-direction: column;
        gap: 8px;
        align-items: center;
      }

      .suggestion-label {
        font-size: 13px;
        color: var(--text-secondary);
        margin-bottom: 4px;
      }
    }
  }

  .chat-message {
    display: flex;
    gap: 10px;
    max-width: 85%;

    &.user {
      align-self: flex-end;
      flex-direction: row-reverse;

      .msg-avatar {
        background: var(--bg-card-subtle);
      }

      .msg-bubble-wrap {
        flex-direction: row-reverse;

        .msg-content {
          background: rgba(99, 102, 241, 0.1);
          border-color: rgba(99, 102, 241, 0.2);
        }
      }
    }

    &.assistant {
      .msg-avatar {
        background: rgba(99, 102, 241, 0.1);
      }

      .msg-bubble-wrap {
        .msg-content {
          background: var(--bg-card-subtle);
        }
      }
    }

    .msg-bubble-wrap {
      display: flex;
      align-items: flex-start;
      gap: 6px;
      position: relative;

      &:hover .msg-copy-btn {
        opacity: 1;
      }

      .msg-copy-btn {
        opacity: 0;
        transition: opacity 0.15s ease;
        background: transparent;
        border: none;
        cursor: pointer;
        padding: 4px;
        color: var(--text-tertiary, #9ca3af);
        border-radius: 4px;
        display: flex;
        align-items: center;
        justify-content: center;

        &:hover {
          color: #6366f1;
          background: rgba(99, 102, 241, 0.1);
        }
      }
    }

    .msg-avatar {
      width: 28px;
      height: 28px;
      border-radius: 50%;
      flex-shrink: 0;
      display: flex;
      align-items: center;
      justify-content: center;
    }

    .msg-content {
      padding: 10px 14px;
      border-radius: 8px;
      border: 1px solid var(--border-color);
      font-size: 13px;
      line-height: 1.6;
      color: var(--text-primary);
      word-break: break-word;

      :deep(.chat-code-block) {
        background: var(--code-box-bg);
        padding: 8px 12px;
        border-radius: 4px;
        font-family: 'JetBrains Mono', Consolas, monospace;
        font-size: 12px;
        overflow-x: auto;
        margin: 4px 0;
      }

      :deep(.chat-inline-code) {
        background: var(--code-box-bg);
        padding: 1px 4px;
        border-radius: 3px;
        font-family: 'JetBrains Mono', Consolas, monospace;
        font-size: 12px;
      }

      .typing-hint {
        margin-left: 8px;
        color: var(--text-secondary);
        font-size: 12px;
      }
    }
  }

  .chat-input-area {
    flex-shrink: 0;
    padding: 0 0 2px;

    /* 输入框上方的拖拽把手：向上拖增高，双击复位。分隔线由把手绘制，
       避免与 input-area 的 border-top 叠成双线。 */
    .chat-resize-handle {
      position: relative;
      flex-shrink: 0;
      height: 12px;
      margin-bottom: 4px;
      display: flex;
      align-items: center;
      justify-content: center;
      cursor: row-resize;
      touch-action: none;
      user-select: none;

      &::before {
        content: '';
        position: absolute;
        top: 0;
        left: 0;
        right: 0;
        height: 1px;
        background: var(--border-color);
      }

      .grip {
        width: 36px;
        height: 3px;
        border-radius: 2px;
        background: var(--border-color);
        transition: background-color 0.15s;
      }

      &:hover .grip,
      &.active .grip {
        background: rgba(99, 102, 241, 0.6);
      }
    }

    .input-row {
      display: flex;
      align-items: flex-end;
      gap: 10px;
      width: 100%;

      .fixed-chat-input {
        flex: 1;
        min-width: 0;
        min-height: 64px;

        :deep(.n-input__textarea) {
          height: 100%;
        }

        :deep(.n-input-wrapper) {
          height: 100%;
          padding: 8px 12px;
        }

        :deep(textarea) {
          height: 100% !important;
          resize: none !important;
          line-height: 1.5;
          font-size: 13.5px;
        }
      }

      .send-btn {
        flex-shrink: 0;
        width: 88px;
        height: 64px;
        font-size: 14px;
        font-weight: 500;
      }
    }
  }
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .ai-chat-view {
    gap: 8px;

    .chat-header {
      .page-title {
        font-size: 16px;
      }
    }

    /* 气泡加宽 + 代码块可横向滚动 */
    .chat-message {
      max-width: 94%;
    }

    .chat-input-area {
      /* 窄屏不提供拖拽把手，恢复原来的分隔线与内边距 */
      padding-top: 10px;
      border-top: 1px solid var(--border-color);

      .chat-resize-handle {
        display: none;
      }

      :deep(.n-input-group) {
        flex-wrap: wrap;
      }
    }
  }
}
</style>