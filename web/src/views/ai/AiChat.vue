<script setup lang="ts">
import { ref, nextTick, onMounted } from 'vue'
import {
  NSpace,
  NInput,
  NButton,
  NIcon,
  NSpin,
  NEmpty,
  useMessage,
} from 'naive-ui'
import { ChatbubbleEllipsesOutline, PaperPlaneOutline, SparklesOutline } from '@vicons/ionicons5'
import { chat, type ChatMessage, type ChatResponse } from '../../api/ai'
import { useWorkspaceStore } from '../../stores/workspace'

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
      <NIcon size="20" color="#6366f1"><SparklesOutline /></NIcon>
      <span class="chat-title">AI 运维助手</span>
      <span class="chat-desc">用自然语言查询主机状态、告警、资源使用等信息</span>
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
          <div class="msg-content" v-html="renderMarkdown(msg.content)" />
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
        <NSpace align="flex-end" :size="8">
          <NInput
            v-model:value="inputText"
            type="textarea"
            :autosize="{ minRows: 1, maxRows: 4 }"
            placeholder="输入你的问题，例如：哪台机器磁盘快满了？"
            @keydown.enter.prevent="send()"
          />
          <NButton
            type="primary"
            :loading="loading"
            :disabled="!inputText.trim()"
            @click="send()"
          >
            <template #icon><NIcon><PaperPlaneOutline /></NIcon></template>
            发送
          </NButton>
        </NSpace>
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.ai-chat-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  padding: 16px;
  box-sizing: border-box;
  gap: 12px;

  .chat-header {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-shrink: 0;

    .chat-title {
      font-size: 18px;
      font-weight: 700;
      color: var(--text-primary);
    }

    .chat-desc {
      font-size: 12px;
      color: var(--text-secondary);
      margin-left: 8px;
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

      .msg-content {
        background: rgba(99, 102, 241, 0.1);
        border-color: rgba(99, 102, 241, 0.2);
      }
    }

    &.assistant {
      .msg-avatar {
        background: rgba(99, 102, 241, 0.1);
      }

      .msg-content {
        background: var(--bg-card-subtle);
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
    padding-top: 8px;
    border-top: 1px solid var(--border-color);
  }
}
</style>