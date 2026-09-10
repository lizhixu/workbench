<script setup lang="ts">
import { ref, nextTick, onMounted } from 'vue'
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
})
</script>

<template>
  <div class="ai-chat-view">
    <div class="chat-header">
      <h2 class="page-title">AI 运维助手</h2>
    </div>

    <div class="chat-body">
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
        <div class="input-row">
          <NInput
            v-model:value="inputText"
            type="textarea"
            :rows="2"
            placeholder="输入你的运维问题，例如：哪台机器磁盘快满了？最近有哪些异常告警？（Shift+Enter 换行，Enter 发送）"
            class="fixed-chat-input"
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
      font-weight: 700;
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
    padding: 10px 0 2px;
    border-top: 1px solid var(--border-color);

    .input-row {
      display: flex;
      align-items: stretch;
      gap: 10px;
      width: 100%;

      .fixed-chat-input {
        flex: 1;
        min-width: 0;
        height: 64px;

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
      :deep(.n-input-group) {
        flex-wrap: wrap;
      }
    }
  }
}
</style>