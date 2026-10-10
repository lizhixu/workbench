<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert, NButton, NCard, NIcon, NInput, NSpin, NTabPane, NTabs, NTag, useMessage,
} from 'naive-ui'
import {
  ChatbubbleEllipsesOutline, CheckmarkOutline, SendOutline, SparklesOutline,
} from '@vicons/ionicons5'
import {
  chatAppAssistant, generateAppDraft, getAIConfig,
  type AppChatResponse, type AppDraftResponse, type AppFormDraft, type AppFormSnapshot,
} from '../../api/ai'

const props = defineProps<{
  // Live form state (reactive in the parent); read fresh on every request
  // so the model always sees what the user currently has in the form.
  snapshot: AppFormSnapshot
}>()

const emit = defineEmits<{
  (e: 'apply-draft', draft: AppFormDraft): void
}>()

const router = useRouter()
const message = useMessage()

const aiChecked = ref(false)
const aiEnabled = ref(false)
const tab = ref<'fill' | 'chat'>('fill')

const QUICK_REQUIREMENTS = [
  '在这台主机上部署一个 Redis，只允许组网内访问，密码生成强口令',
  '用 nginx:alpine 起一个静态站点，宿主端口 8080',
  '部署 WordPress + MySQL，绑定域名 blog.example.com',
]

// ---- One-shot draft fill ----
const requirement = ref('')
const generating = ref(false)
const draftResult = ref<AppDraftResponse | null>(null)
const draftApplied = ref(false)
const fillError = ref('')

async function doGenerate() {
  const req = requirement.value.trim()
  if (!req) {
    message.warning('先描述一下你想部署什么')
    return
  }
  generating.value = true
  fillError.value = ''
  try {
    draftResult.value = await generateAppDraft(req, props.snapshot)
    draftApplied.value = false
  } catch (e: any) {
    fillError.value = e.message || '生成草稿失败'
  } finally {
    generating.value = false
  }
}

function applyDraftResult() {
  if (!draftResult.value) return
  emit('apply-draft', draftResult.value.draft)
  draftApplied.value = true
  message.success('草稿已填充到表单，请检查后点击「创建应用」')
}

// ---- Conversational assistance ----
interface ChatEntry {
  role: 'user' | 'assistant'
  content: string
  patch?: AppFormDraft | null
  patchWarnings?: string[]
  patchApplied?: boolean
}

const chatMessages = ref<ChatEntry[]>([])
const chatInput = ref('')
const sending = ref(false)
const chatError = ref('')
const chatScrollEl = ref<HTMLElement | null>(null)

const patchLabels: Record<string, string> = {
  source_type: '部署来源',
  name: '应用名称',
  container_name: '容器名',
  host_id: '目标主机',
  template_id: '应用模板',
  image: '镜像地址',
  compose_content: 'Compose 内容',
  repo_url: '仓库地址',
  branch: '分支',
  build_type: '构建模式',
  dockerfile: 'Dockerfile 路径',
  build_context: '构建上下文',
  build_timeout_sec: '构建超时',
  healthcheck_url: '健康检查 URL',
  domain: '域名',
  proxy_mode: '反代模式',
  gateway_host_id: '网关主机',
  auto_deploy: '推送自动部署',
}

function describePatch(patch: AppFormDraft): string[] {
  const lines: string[] = []
  const rec = patch as Record<string, unknown>
  for (const [key, value] of Object.entries(rec)) {
    if (value === undefined || value === null) continue
    if (key === 'ports' && Array.isArray(value)) {
      for (const p of value as { host: number; container: number; bind_scope: string }[]) {
        lines.push(`端口 ${p.host} → ${p.container}（${p.bind_scope === 'mesh' ? '仅组网' : '公网'}）`)
      }
    } else if (key === 'template_params' || key === 'env_vars') {
      const keys = Object.keys(value as Record<string, string>)
      if (keys.length) lines.push(`${key === 'env_vars' ? '环境变量' : '模板参数'}：${keys.join('、')}`)
    } else if (key === 'volumes' && Array.isArray(value)) {
      if (value.length) lines.push(`挂载卷：${(value as string[]).join('；')}`)
    } else if (key === 'compose_content') {
      lines.push('Compose 内容（已生成/更新）')
    } else {
      const label = patchLabels[key] || key
      lines.push(`${label} → ${String(value)}`)
    }
  }
  return lines
}

async function scrollChatToBottom() {
  await nextTick()
  const el = chatScrollEl.value
  if (el) el.scrollTop = el.scrollHeight
}

async function doSend() {
  const q = chatInput.value.trim()
  if (!q || sending.value) return
  chatInput.value = ''
  chatError.value = ''
  const history = chatMessages.value.slice(-10).map((m) => ({ role: m.role, content: m.content }))
  chatMessages.value.push({ role: 'user', content: q })
  sending.value = true
  await scrollChatToBottom()
  try {
    const resp: AppChatResponse = await chatAppAssistant(q, history, props.snapshot)
    chatMessages.value.push({
      role: 'assistant',
      content: resp.answer,
      patch: resp.form_patch ?? null,
      patchWarnings: resp.patch_warnings,
      patchApplied: false,
    })
  } catch (e: any) {
    chatError.value = e.message || 'AI 对话失败'
  } finally {
    sending.value = false
    await scrollChatToBottom()
  }
}

function applyPatch(entry: ChatEntry) {
  if (!entry.patch) return
  emit('apply-draft', entry.patch)
  entry.patchApplied = true
  message.success('修改已应用到表单，请检查后继续')
}

const hasDraftContent = computed(() => {
  const d = draftResult.value?.draft
  if (!d) return false
  return Object.values(d).some((v) => v !== undefined && v !== null && v !== '' &&
    !(Array.isArray(v) && v.length === 0) &&
    !(typeof v === 'object' && Object.keys(v as object).length === 0))
})

onMounted(async () => {
  try {
    const cfg = await getAIConfig()
    aiEnabled.value = !!cfg.enabled && !!cfg.base_url
  } catch {
    aiEnabled.value = false
  } finally {
    aiChecked.value = true
  }
})
</script>

<template>
  <NCard :bordered="false" class="ai-assistant-card" content-style="padding: 0;">
    <!-- Header -->
    <div class="ai-header">
      <div class="ai-icon-chip">
        <NIcon size="18"><SparklesOutline /></NIcon>
      </div>
      <div class="ai-header-text">
        <div class="ai-title">AI 部署助手</div>
        <div class="ai-subtitle">只生成草稿，创建前由你确认</div>
      </div>
      <NTag size="tiny" round :bordered="false" type="info" class="ai-header-tag">草稿模式</NTag>
    </div>

    <div v-if="!aiChecked" class="ai-loading">
      <NSpin size="small" />
      <span>正在检查 AI 配置…</span>
    </div>

    <div v-else-if="!aiEnabled" class="ai-disabled">
      <NAlert type="warning" :show-icon="true">
        <div>AI 尚未启用或未配置模型端点，智能填充与对话不可用（手动填表不受影响）。</div>
        <NButton size="small" secondary style="margin-top: 10px" @click="router.push('/settings')">
          前往系统设置 - AI 大模型
        </NButton>
      </NAlert>
    </div>

    <NTabs v-else v-model:value="tab" type="line" size="small" animated class="ai-tabs">
      <!-- 智能填充 -->
      <NTabPane name="fill" tab="智能填充">
        <div class="pane">
          <div class="composer">
            <NInput
              v-model:value="requirement"
              type="textarea"
              :rows="3"
              :autosize="{ minRows: 3, maxRows: 5 }"
              placeholder="描述你想部署的应用，例如：在测试服上跑一个 Redis，只允许组网内访问"
              class="composer-input"
              @keydown.enter.ctrl="doGenerate"
            />
            <div class="composer-foot">
              <span class="composer-hint">Ctrl+Enter 生成</span>
              <NButton type="primary" size="small" round :loading="generating" @click="doGenerate">
                <template #icon><NIcon><SparklesOutline /></NIcon></template>
                生成草稿
              </NButton>
            </div>
          </div>

          <div v-if="!draftResult && !generating" class="quick-block">
            <div class="quick-title">试试这样描述</div>
            <button
              v-for="q in QUICK_REQUIREMENTS"
              :key="q"
              type="button"
              class="quick-chip"
              @click="requirement = q"
            >
              {{ q }}
            </button>
          </div>

          <div v-if="generating" class="thinking-row">
            <span class="thinking-dots"><i /><i /><i /></span>
            <span>AI 正在结合主机与模板目录生成草稿…</span>
          </div>

          <NAlert v-if="fillError" type="error" :show-icon="true">{{ fillError }}</NAlert>

          <div v-if="draftResult" class="draft-result">
            <div v-if="draftResult.explanation" class="explain-card">
              <div class="block-label">AI 的选型说明</div>
              <div class="explain-text">{{ draftResult.explanation }}</div>
            </div>

            <div v-if="draftResult.missing.length" class="note-block warn">
              <div class="block-label">还需要你补充</div>
              <div class="note-tags">
                <NTag v-for="m in draftResult.missing" :key="m" size="small" type="warning" :bordered="false" round>{{ m }}</NTag>
              </div>
            </div>

            <div v-if="draftResult.warnings.length" class="note-block info">
              <div class="block-label">注意事项</div>
              <div v-for="w in draftResult.warnings" :key="w" class="note-line">{{ w }}</div>
            </div>

            <NAlert v-if="!hasDraftContent" type="default" :show-icon="false">
              AI 没有给出可填充的字段，可以换个说法再试。
            </NAlert>

            <div v-else class="draft-actions">
              <NButton size="small" quaternary @click="draftResult = null">丢弃</NButton>
              <NButton
                size="small"
                round
                :type="draftApplied ? 'success' : 'primary'"
                :disabled="draftApplied"
                @click="applyDraftResult"
              >
                <template #icon>
                  <NIcon><CheckmarkOutline v-if="draftApplied" /><SparklesOutline v-else /></NIcon>
                </template>
                {{ draftApplied ? '已填充到表单' : '填充到表单' }}
              </NButton>
            </div>
          </div>
        </div>
      </NTabPane>

      <!-- 对话辅助 -->
      <NTabPane name="chat" tab="对话辅助">
        <div class="pane chat-pane">
          <div ref="chatScrollEl" class="chat-scroll">
            <div v-if="!chatMessages.length" class="chat-empty">
              <div class="ai-icon-chip small">
                <NIcon size="16"><ChatbubbleEllipsesOutline /></NIcon>
              </div>
              <p>边填表边问我，当前表单内容会自动带给我参考。也可以直接让我改，比如「把宿主端口改成 9000」。</p>
            </div>

            <div
              v-for="(m, idx) in chatMessages"
              :key="idx"
              class="chat-msg"
              :class="m.role"
            >
              <div v-if="m.role === 'assistant'" class="msg-avatar">
                <NIcon size="13"><SparklesOutline /></NIcon>
              </div>
              <div class="msg-main">
                <div class="chat-body">{{ m.content }}</div>
                <div v-if="m.role === 'assistant' && m.patch" class="patch-card">
                  <div class="patch-title">
                    <NIcon size="13"><ChatbubbleEllipsesOutline /></NIcon>
                    建议修改
                  </div>
                  <div v-for="line in describePatch(m.patch)" :key="line" class="patch-line">{{ line }}</div>
                  <div v-for="w in m.patchWarnings || []" :key="w" class="patch-warn">{{ w }}</div>
                  <div class="patch-actions">
                    <NButton
                      size="tiny"
                      round
                      :type="m.patchApplied ? 'success' : 'primary'"
                      :disabled="m.patchApplied"
                      @click="applyPatch(m)"
                    >
                      {{ m.patchApplied ? '已应用到表单' : '应用到表单' }}
                    </NButton>
                  </div>
                </div>
              </div>
            </div>

            <div v-if="sending" class="chat-msg assistant">
              <div class="msg-avatar">
                <NIcon size="13"><SparklesOutline /></NIcon>
              </div>
              <div class="msg-main">
                <div class="chat-body thinking">
                  <span class="thinking-dots"><i /><i /><i /></span>
                </div>
              </div>
            </div>
          </div>

          <NAlert v-if="chatError" type="error" :show-icon="true">{{ chatError }}</NAlert>

          <div class="chat-composer">
            <NInput
              v-model:value="chatInput"
              type="textarea"
              :rows="1"
              :autosize="{ minRows: 1, maxRows: 4 }"
              placeholder="问点什么，或让我调整表单…（Enter 发送）"
              class="chat-input"
              @keydown.enter.exact.prevent="doSend"
            />
            <NButton type="primary" circle :loading="sending" class="send-btn" @click="doSend">
              <template #icon><NIcon><SendOutline /></NIcon></template>
            </NButton>
          </div>
        </div>
      </NTabPane>
    </NTabs>
  </NCard>
</template>

<style scoped lang="scss">
.ai-assistant-card {
  width: 100%;
  background-color: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 12px;
  overflow: hidden;
}

/* ---------- Header ---------- */
.ai-header {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 16px 12px;
  border-bottom: 1px solid var(--border-color);
}

.ai-icon-chip {
  flex-shrink: 0;
  width: 34px;
  height: 34px;
  border-radius: 10px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  background: linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%);
  box-shadow: 0 4px 10px rgba(99, 102, 241, 0.35);

  &.small {
    width: 28px;
    height: 28px;
    border-radius: 8px;
  }
}

.ai-header-text {
  flex: 1;
  min-width: 0;

  .ai-title {
    font-size: 14px;
    font-weight: 600;
    line-height: 1.3;
    color: var(--text-primary);
  }

  .ai-subtitle {
    font-size: 11.5px;
    line-height: 1.4;
    color: var(--text-secondary);
  }
}

.ai-header-tag {
  flex-shrink: 0;
}

.ai-loading {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 18px 16px;
  font-size: 12.5px;
  color: var(--text-secondary);
}

.ai-disabled {
  padding: 14px 16px 16px;
}

/* ---------- Tabs / panes ---------- */
.ai-tabs {
  :deep(.n-tabs-nav) {
    padding: 0 16px;
  }
  :deep(.n-tab-pane) {
    padding: 0;
  }
}

.pane {
  padding: 14px 16px 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

/* ---------- Fill composer ---------- */
.composer {
  border: 1px solid var(--border-color);
  border-radius: 10px;
  background-color: var(--bg-card-subtle);
  transition: border-color 0.2s, box-shadow 0.2s;

  &:focus-within {
    border-color: rgba(99, 102, 241, 0.55);
    box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.12);
  }
}

.composer-input {
  :deep(.n-input) {
    background: transparent;
  }
  :deep(.n-input__border),
  :deep(.n-input__state-border) {
    display: none;
  }
  :deep(textarea) {
    font-size: 13px;
    line-height: 1.6;
    padding: 4px 2px;
  }
}

.composer-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 10px 10px;

  .composer-hint {
    font-size: 11px;
    color: var(--text-secondary);
  }
}

.quick-block {
  .quick-title {
    font-size: 11.5px;
    color: var(--text-secondary);
    margin-bottom: 8px;
  }
}

.quick-chip {
  display: block;
  width: 100%;
  text-align: left;
  margin-bottom: 6px;
  padding: 7px 10px;
  font-size: 12px;
  line-height: 1.45;
  color: var(--text-primary);
  background-color: var(--bg-card-subtle);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  cursor: pointer;
  transition: border-color 0.15s, background-color 0.15s;

  &:hover {
    border-color: rgba(99, 102, 241, 0.5);
    background-color: var(--bg-active);
  }

  &:last-child {
    margin-bottom: 0;
  }
}

/* ---------- Draft result ---------- */
.draft-result {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.block-label {
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.4px;
  color: var(--text-secondary);
  margin-bottom: 6px;
}

.explain-card {
  background-color: var(--bg-active);
  border: 1px solid rgba(99, 102, 241, 0.25);
  border-radius: 10px;
  padding: 10px 12px;

  .explain-text {
    font-size: 12.5px;
    line-height: 1.65;
    color: var(--text-primary);
    white-space: pre-wrap;
  }
}

.note-block {
  border-radius: 10px;
  padding: 10px 12px;
  border: 1px solid var(--border-color);

  &.warn {
    background-color: rgba(240, 160, 32, 0.07);
    border-color: rgba(240, 160, 32, 0.3);
  }
  &.info {
    background-color: var(--bg-card-subtle);
  }

  .note-tags {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }
  .note-line {
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-primary);
  }
}

.draft-actions {
  display: flex;
  justify-content: flex-end;
  align-items: center;
  gap: 6px;
}

/* ---------- Thinking dots ---------- */
.thinking-row {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  color: var(--text-secondary);
  padding: 2px;
}

.thinking-dots {
  display: inline-flex;
  align-items: center;
  gap: 3px;

  i {
    width: 5px;
    height: 5px;
    border-radius: 50%;
    background-color: #8b8fa3;
    animation: ai-dot-bounce 1.2s ease-in-out infinite;

    &:nth-child(2) {
      animation-delay: 0.15s;
    }
    &:nth-child(3) {
      animation-delay: 0.3s;
    }
  }
}

@keyframes ai-dot-bounce {
  0%, 60%, 100% {
    transform: translateY(0);
    opacity: 0.45;
  }
  30% {
    transform: translateY(-3px);
    opacity: 1;
  }
}

/* ---------- Chat ---------- */
.chat-pane {
  gap: 10px;
}

.chat-scroll {
  height: 350px;
  overflow-y: auto;
  overscroll-behavior: contain;
  -webkit-overflow-scrolling: touch;
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 12px;
  background-color: var(--bg-card-subtle);
  border: 1px solid var(--border-color);
  border-radius: 10px;
}

.chat-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  text-align: center;
  padding: 26px 14px 10px;

  p {
    margin: 0;
    font-size: 12px;
    line-height: 1.65;
    color: var(--text-secondary);
    max-width: 280px;
  }
}

.chat-msg {
  display: flex;
  gap: 7px;
  max-width: 100%;

  &.user {
    flex-direction: row-reverse;

    .msg-main {
      align-items: flex-end;
    }
    .chat-body {
      background-color: rgba(99, 102, 241, 0.14);
      color: var(--text-primary);
      border-radius: 12px 12px 4px 12px;
    }
  }

  &.assistant {
    .chat-body {
      background-color: var(--bg-card);
      border: 1px solid var(--border-color);
      border-radius: 4px 12px 12px 12px;
    }
  }

  .msg-avatar {
    flex-shrink: 0;
    width: 22px;
    height: 22px;
    margin-top: 2px;
    border-radius: 7px;
    display: flex;
    align-items: center;
    justify-content: center;
    color: #fff;
    background: linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%);
  }

  .msg-main {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    min-width: 0;
    max-width: calc(100% - 29px);
  }

  .chat-body {
    padding: 8px 11px;
    font-size: 12.5px;
    line-height: 1.65;
    white-space: pre-wrap;
    word-break: break-word;

    &.thinking {
      padding: 10px 12px;
    }
  }
}

.patch-card {
  margin-top: 7px;
  width: 100%;
  border: 1px solid rgba(99, 102, 241, 0.3);
  border-left: 3px solid #6366f1;
  border-radius: 8px;
  background-color: rgba(99, 102, 241, 0.06);
  padding: 8px 10px;

  .patch-title {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11.5px;
    font-weight: 600;
    color: #6366f1;
    margin-bottom: 5px;
  }

  .patch-line {
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-primary);
    word-break: break-all;
  }

  .patch-warn {
    font-size: 11.5px;
    line-height: 1.55;
    color: #d98e00;
  }

  .patch-actions {
    display: flex;
    justify-content: flex-end;
    margin-top: 7px;
  }
}

.chat-composer {
  display: flex;
  align-items: flex-end;
  gap: 8px;
  border: 1px solid var(--border-color);
  border-radius: 10px;
  background-color: var(--bg-card-subtle);
  padding: 6px 6px 6px 10px;
  transition: border-color 0.2s, box-shadow 0.2s;

  &:focus-within {
    border-color: rgba(99, 102, 241, 0.55);
    box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.12);
  }

  .chat-input {
    flex: 1;
    min-width: 0;

    :deep(.n-input) {
      background: transparent;
    }
    :deep(.n-input__border),
    :deep(.n-input__state-border) {
      display: none;
    }
    :deep(textarea) {
      font-size: 12.5px;
      line-height: 1.55;
    }
  }

  .send-btn {
    flex-shrink: 0;
  }
}
</style>
