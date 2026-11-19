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
import { ChatbubbleEllipsesOutline, SparklesOutline, CopyOutline } from '@vicons/ionicons5'
import { chat, type CallTrace, type ChatMessage, type ChatResponse, type LLMUsage } from '../../api/ai'
import AiCallTrace from '../../components/ai/AiCallTrace.vue'
import { useWorkspaceStore } from '../../stores/workspace'
import { copyToClipboard } from '../../utils/clipboard'
import { renderMarkdown } from '../../utils/markdown'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'AiChat' })

const message = useMessage()
const workspace = useWorkspaceStore()

const inputText = ref('')
const loading = ref(false)
const loadingStartedAt = ref<number | null>(null)
const conversations = ref<{
  role: 'user' | 'assistant'
  content: string
  trace?: CallTrace | null
  model?: string
  usage?: LLMUsage | null
}[]>([])
const chatListRef = ref<HTMLElement | null>(null)
const chatBodyRef = ref<HTMLElement | null>(null)

// ---- 输入框高度拖拽 ----
// 输入框默认单行高度；拖住上方把手向上拉可增高，消息列表相应让出空间。
// 下限 MIN_INPUT_H，上限保证消息列表至少留 INPUT_MIN_LIST_H，避免拖到把对话区挤没。
// 上限同时扣除输入卡底栏（快捷键提示 + 发送按钮）的固定高度，保证的列表高度才是真的。
const MIN_INPUT_H = 64
const INPUT_MIN_LIST_H = 180
const COMPOSER_EXTRA_H = 46
const inputH = ref(MIN_INPUT_H)
const resizingInput = ref(false)

function maxInputHeight(): number {
  const bodyH = chatBodyRef.value?.clientHeight ?? 0
  return Math.max(bodyH - INPUT_MIN_LIST_H - COMPOSER_EXTRA_H, MIN_INPUT_H)
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
  loadingStartedAt.value = Date.now()
  await scrollToBottom()

  try {
    // Build history from prior messages (last 10 to keep prompt reasonable).
    const history: ChatMessage[] = conversations.value
      .slice(-11, -1)
      .map((m) => ({ role: m.role, content: m.content }))

    const resp: ChatResponse = await chat(q, history)
    conversations.value.push({
      role: 'assistant',
      content: resp.answer,
      trace: resp.trace ?? null,
      model: resp.model,
      usage: resp.usage ?? null,
    })
  } catch (e: any) {
    conversations.value.push({
      role: 'assistant',
      content: `抱歉，查询失败：${e.message}`,
      trace: e.trace ?? null,
    })
    message.error(e.message || 'AI 问答失败')
  } finally {
    loading.value = false
    loadingStartedAt.value = null
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

// 消息内容走 utils/markdown.ts 的共享渲染（AGENTS.md §8.11、DESIGN.md §9.12）：
// 标题/列表/表格/链接/引用/删除线全量支持，先转义 HTML 再解析，链接只放行
// http(s)/mailto/锚点/站内路径。样式见下方 .markdown-body，与部署助手同款。

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
          <div class="msg-main">
            <div class="msg-bubble-wrap">
              <div class="msg-content markdown-body" v-html="renderMarkdown(msg.content)" />
              <button
                class="msg-copy-btn"
                title="复制内容"
                @click="copyMsg(msg.content)"
              >
                <NIcon size="14"><CopyOutline /></NIcon>
              </button>
            </div>
            <!-- 调用过程置于每条对话下方（气泡之外）、默认折叠 -->
            <AiCallTrace
              v-if="msg.role === 'assistant' && msg.trace"
              :trace="msg.trace"
              :model="msg.model"
              :usage="msg.usage"
            />
          </div>
        </div>

        <div v-if="loading" class="chat-message assistant">
          <div class="msg-avatar">
            <NIcon size="16" color="#6366f1"><SparklesOutline /></NIcon>
          </div>
          <div class="msg-main">
            <div class="msg-bubble-wrap">
              <div class="msg-content">
                <NSpin size="small" />
                <span class="typing-hint">正在思考...</span>
              </div>
            </div>
            <AiCallTrace :started-at="loadingStartedAt" />
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
        <div class="chat-composer">
          <NInput
            v-model:value="inputText"
            type="textarea"
            :rows="2"
            :bordered="false"
            placeholder="输入你的运维问题，例如：哪台机器磁盘快满了？最近有哪些异常告警？"
            class="composer-input"
            :style="{ height: `${inputH}px` }"
            @keydown.enter.exact.prevent="send()"
          />
          <div class="composer-footer">
            <div class="composer-hints">
              <span class="hint-key">Enter</span>
              <span>发送</span>
              <span class="hint-dot">·</span>
              <span class="hint-key">Shift+Enter</span>
              <span>换行</span>
            </div>
            <NButton
              type="primary"
              size="small"
              class="send-btn"
              :loading="loading"
              :disabled="!inputText.trim()"
              @click="send()"
            >
              发送
            </NButton>
          </div>
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
    padding: 8px 12px;
    display: flex;
    flex-direction: column;
    gap: 16px;

    .chat-empty {
      margin-top: 60px;

      .suggestions {
        display: flex;
        flex-direction: row;
        flex-wrap: wrap;
        gap: 8px;
        justify-content: center;
        max-width: 560px;
      }

      .suggestion-label {
        width: 100%;
        text-align: center;
        font-size: 13px;
        color: var(--text-secondary);
        margin-bottom: 4px;
      }
    }
  }

  .chat-message {
    display: flex;
    gap: 10px;
    /* 助手消息占满屏幕；用户消息（右侧）按内容自适应，见 &.user */

    &.user {
      /* 用户消息按内容自适应：行宽上限 85%、靠右对齐，
         气泡只占内容宽度，不撑满行宽 */
      max-width: 85%;
      align-self: flex-end;
      flex-direction: row-reverse;

      .msg-avatar {
        background: var(--bg-card-subtle);
      }

      .msg-bubble-wrap {
        flex-direction: row-reverse;

        .msg-content {
          flex: none;
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

    /* 气泡 + 下方调用过程的纵向列：气泡行（气泡与复制按钮）与调用过程上下排布 */
    .msg-main {
      flex: 1;
      min-width: 0;
      display: flex;
      flex-direction: column;
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
      flex: 1;
      min-width: 0;
      padding: 10px 14px;
      border-radius: 8px;
      border: 1px solid var(--border-color);
      font-size: 13px;
      line-height: 1.6;
      color: var(--text-primary);
      word-break: break-word;

      .typing-hint {
        margin-left: 8px;
        color: var(--text-secondary);
        font-size: 12px;
      }
    }
  }

  /* 消息气泡内的 Markdown 排版：与部署助手（AppAiAssistant.vue）的
     .markdown-body 同款，只按本页 13px 正文字号微调。v-html 输出的元素
     不带 scoped 标记，全部用 :deep() 下发；只用主题变量，不引入硬编码色。 */
  .markdown-body {
    white-space: normal;
    word-break: break-word;

    :deep(> :first-child) {
      margin-top: 0;
    }
    :deep(> :last-child) {
      margin-bottom: 0;
    }

    :deep(p) {
      margin: 6px 0;
      line-height: 1.65;
    }

    :deep(h4),
    :deep(h5),
    :deep(h6) {
      margin: 10px 0 4px;
      font-weight: 600;
      color: var(--text-primary);
      line-height: 1.4;
    }

    :deep(h4) {
      font-size: 14px;
    }
    :deep(h5) {
      font-size: 13px;
    }
    :deep(h6) {
      font-size: 12.5px;
    }

    :deep(ul),
    :deep(ol) {
      margin: 6px 0;
      padding-left: 18px;
      line-height: 1.6;
    }

    :deep(li) {
      margin: 3px 0;
    }

    :deep(strong) {
      font-weight: 600;
      color: var(--text-primary);
    }

    :deep(em) {
      font-style: italic;
    }

    :deep(del) {
      text-decoration: line-through;
      opacity: 0.75;
    }

    :deep(a) {
      color: var(--primary-color, #6366f1);
      text-decoration: none;

      &:hover {
        text-decoration: underline;
      }
    }

    :deep(blockquote) {
      margin: 8px 0;
      padding: 6px 10px;
      border-left: 3px solid var(--primary-color, #6366f1);
      background: rgba(99, 102, 241, 0.06);
      border-radius: 0 4px 4px 0;
      color: var(--text-secondary);
      font-size: 12px;
    }

    :deep(hr) {
      border: none;
      border-top: 1px solid var(--border-color);
      margin: 10px 0;
    }

    :deep(code.md-inline-code) {
      padding: 1px 5px;
      margin: 0 2px;
      border-radius: 4px;
      background: rgba(125, 125, 125, 0.12);
      font-family: var(--font-family-mono, monospace);
      font-size: 12px;
      color: var(--text-primary);
    }

    :deep(pre.md-code-block) {
      margin: 8px 0;
      padding: 8px 10px;
      border-radius: 6px;
      background: var(--code-box-bg, rgba(0, 0, 0, 0.28));
      border: 1px solid var(--code-border, var(--border-color));
      overflow-x: auto;
      font-family: var(--font-family-mono, monospace);
      font-size: 12px;
      line-height: 1.5;
      color: var(--text-primary);
      position: relative;

      /* 语言标记贴在代码块右上角（renderMarkdown 已把语言写进 data-lang） */
      &[data-lang]::before {
        content: attr(data-lang);
        position: absolute;
        top: 4px;
        right: 8px;
        font-size: 10px;
        text-transform: uppercase;
        color: var(--text-secondary);
        opacity: 0.7;
        font-weight: 600;
      }

      code {
        font-family: inherit;
        font-size: inherit;
        background: transparent;
        padding: 0;
        color: inherit;
      }
    }

    :deep(.md-table-wrap) {
      margin: 8px 0;
      max-width: 100%;
      overflow-x: auto;
      border: 1px solid var(--border-color);
      border-radius: 6px;
    }

    :deep(table.md-table) {
      width: 100%;
      border-collapse: collapse;
      font-size: 12px;
      line-height: 1.5;

      th,
      td {
        padding: 5px 8px;
        border: 1px solid var(--border-color);
      }

      th {
        background: rgba(125, 125, 125, 0.08);
        font-weight: 600;
        color: var(--text-primary);
        text-align: left;
      }

      tr:nth-child(even) td {
        background: rgba(125, 125, 125, 0.03);
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
      height: 16px;
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

      /* 抓手线加长加粗（64×5px），仅约 30px 的细线视觉提示过弱；
         仍只用主题变量，不引入硬编码色。 */
      .grip {
        width: 64px;
        height: 5px;
        border-radius: 3px;
        background: var(--border-color);
        transition: background-color 0.15s;
      }

      &:hover .grip,
      &.active .grip {
        background: rgba(99, 102, 241, 0.6);
      }
    }

    /* CherryStudio 风格一体化输入卡：圆角容器承载无边框 textarea 与底部工具行，
       聚焦时容器整体点亮，而非输入框自身描边。 */
    .chat-composer {
      border: 1px solid var(--border-color);
      border-radius: 14px;
      background: var(--bg-card);
      box-shadow: var(--shadow-sm);
      transition: border-color 0.2s ease, box-shadow 0.2s ease;
      overflow: hidden;

      &:focus-within {
        border-color: rgba(99, 102, 241, 0.55);
        box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.12);
      }

      .composer-input {
        background-color: transparent;
        --n-border-radius: 0;

        :deep(.n-input-wrapper) {
          padding: 10px 14px 4px;
        }

        :deep(.n-input__textarea) {
          height: 100%;
        }

        :deep(textarea) {
          height: 100% !important;
          resize: none !important;
          line-height: 1.5;
          font-size: 13.5px;
          background: transparent;
        }
      }

      .composer-footer {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 8px;
        padding: 4px 8px 8px 12px;
      }

      .composer-hints {
        display: flex;
        align-items: center;
        gap: 5px;
        font-size: 12px;
        color: var(--text-muted, #9ca3af);
        min-width: 0;
        white-space: nowrap;
        overflow: hidden;

        .hint-key {
          font-family: 'JetBrains Mono', Consolas, monospace;
          font-size: 11px;
          line-height: 1;
          padding: 3px 6px;
          border-radius: 4px;
          border: 1px solid var(--border-color);
          background: var(--bg-card-subtle);
          color: var(--text-secondary);
        }

        .hint-dot {
          color: var(--text-muted);
        }
      }

      .send-btn {
        flex-shrink: 0;
        margin-left: auto;
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

    .chat-input-area {
      /* 窄屏不提供拖拽把手，恢复原来的分隔线与内边距 */
      padding-top: 10px;
      border-top: 1px solid var(--border-color);

      .chat-resize-handle {
        display: none;
      }

      .chat-composer {
        .composer-hints {
          /* 窄屏空间紧张，快捷键提示隐藏，只保留发送按钮 */
          display: none;
        }
      }
    }
  }
}
</style>