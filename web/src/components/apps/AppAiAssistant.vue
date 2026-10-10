<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert, NButton, NCard, NIcon, NInput, NSpace, NSpin, NTabPane, NTabs, NTag, useMessage,
} from 'naive-ui'
import { ChatbubbleEllipsesOutline, SendOutline, SparklesOutline } from '@vicons/ionicons5'
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
  <NCard :bordered="false" class="ai-assistant-card">
    <template #header>
      <NSpace align="center" :size="8">
        <NIcon size="18" color="#6366f1"><SparklesOutline /></NIcon>
        <span class="ai-title">AI 部署助手</span>
        <NTag size="tiny" :bordered="false" type="info">只填草稿，不会自动创建</NTag>
      </NSpace>
    </template>

    <div v-if="!aiChecked" class="ai-loading">
      <NSpin size="small" />
      <span>正在检查 AI 配置…</span>
    </div>

    <NAlert v-else-if="!aiEnabled" type="warning" :show-icon="true">
      <div>AI 尚未启用或未配置模型端点，智能填充与对话不可用（手动填表不受影响）。</div>
      <NButton size="small" secondary style="margin-top: 8px" @click="router.push('/settings')">
        前往系统设置 - AI 大模型
      </NButton>
    </NAlert>

    <NTabs v-else v-model:value="tab" type="line" size="small" animated>
      <NTabPane name="fill" tab="智能填充">
        <NSpace vertical size="medium">
          <NInput
            v-model:value="requirement"
            type="textarea"
            :rows="3"
            placeholder="描述你想部署的应用，例如：在测试服上跑一个 Redis，只允许组网内访问，密码生成一个强口令"
            @keydown.enter.ctrl="doGenerate"
          />
          <NSpace justify="space-between" align="center">
            <span class="hint">Ctrl+Enter 快速生成</span>
            <NButton type="primary" size="small" :loading="generating" @click="doGenerate">
              <template #icon>
                <NIcon><SparklesOutline /></NIcon>
              </template>
              生成草稿
            </NButton>
          </NSpace>

          <NAlert v-if="fillError" type="error" :show-icon="true">{{ fillError }}</NAlert>

          <div v-if="draftResult" class="draft-result">
            <div v-if="draftResult.explanation" class="draft-explain">{{ draftResult.explanation }}</div>

            <NAlert v-if="draftResult.missing.length" type="warning" :show-icon="false" class="draft-note">
              <div class="note-title">还需要你补充：</div>
              <div class="note-tags">
                <NTag v-for="m in draftResult.missing" :key="m" size="small" type="warning" :bordered="false">{{ m }}</NTag>
              </div>
            </NAlert>

            <NAlert v-if="draftResult.warnings.length" type="info" :show-icon="false" class="draft-note">
              <div class="note-title">注意事项：</div>
              <div v-for="w in draftResult.warnings" :key="w" class="note-line">{{ w }}</div>
            </NAlert>

            <NAlert v-if="!hasDraftContent" type="default" :show-icon="false">
              AI 没有给出可填充的字段，可以换个说法再试。
            </NAlert>

            <NSpace v-else justify="end">
              <NButton size="small" @click="draftResult = null">丢弃</NButton>
              <NButton size="small" type="primary" :disabled="draftApplied" @click="applyDraftResult">
                {{ draftApplied ? '已填充到表单' : '填充到表单' }}
              </NButton>
            </NSpace>
          </div>
        </NSpace>
      </NTabPane>

      <NTabPane name="chat" tab="对话辅助">
        <NSpace vertical size="small">
          <div ref="chatScrollEl" class="chat-scroll">
            <div v-if="!chatMessages.length" class="chat-empty">
              边填表边问我：当前表单内容会自动带给我参考。也可以直接让我改，比如「把宿主端口改成 9000」「仓库换成 dev 分支」。
            </div>
            <div
              v-for="(m, idx) in chatMessages"
              :key="idx"
              class="chat-msg"
              :class="m.role"
            >
              <div class="chat-body">{{ m.content }}</div>
              <div v-if="m.role === 'assistant' && m.patch" class="patch-card">
                <div class="patch-title">
                  <NIcon size="14"><ChatbubbleEllipsesOutline /></NIcon>
                  建议修改
                </div>
                <div v-for="line in describePatch(m.patch)" :key="line" class="patch-line">{{ line }}</div>
                <div v-for="w in m.patchWarnings || []" :key="w" class="patch-warn">{{ w }}</div>
                <NSpace justify="end" style="margin-top: 6px">
                  <NButton size="tiny" type="primary" :disabled="m.patchApplied" @click="applyPatch(m)">
                    {{ m.patchApplied ? '已应用' : '应用到表单' }}
                  </NButton>
                </NSpace>
              </div>
            </div>
            <div v-if="sending" class="chat-msg assistant">
              <div class="chat-body thinking">思考中…</div>
            </div>
          </div>

          <NAlert v-if="chatError" type="error" :show-icon="true">{{ chatError }}</NAlert>

          <NSpace :size="8" align="end" style="flex-wrap: nowrap">
            <NInput
              v-model:value="chatInput"
              type="textarea"
              :rows="2"
              placeholder="问点什么，或让我调整表单…（Enter 发送）"
              style="flex: 1"
              @keydown.enter.exact.prevent="doSend"
            />
            <NButton type="primary" :loading="sending" @click="doSend">
              <template #icon>
                <NIcon><SendOutline /></NIcon>
              </template>
              发送
            </NButton>
          </NSpace>
        </NSpace>
      </NTabPane>
    </NTabs>
  </NCard>
</template>

<style scoped lang="scss">
.ai-assistant-card {
  width: 100%;
}

.ai-title {
  font-size: 15px;
  font-weight: 600;
}

.ai-loading {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--text-secondary, #666);
  padding: 8px 0;
}

.hint {
  font-size: 12px;
  color: var(--n-text-color-disabled, #999);
}

.draft-result {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.draft-explain {
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
}

.draft-note {
  .note-title {
    font-size: 12px;
    font-weight: 600;
    margin-bottom: 4px;
  }
  .note-tags {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .note-line {
    font-size: 12px;
    line-height: 1.5;
  }
}

.chat-scroll {
  height: 340px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 4px 2px;
}

.chat-empty {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary, #888);
  padding: 8px;
  border: 1px dashed var(--border-color, #ddd);
  border-radius: 8px;
}

.chat-msg {
  display: flex;
  flex-direction: column;

  &.user {
    align-items: flex-end;
    .chat-body {
      background: rgba(99, 102, 241, 0.12);
    }
  }
  &.assistant {
    align-items: flex-start;
    .chat-body {
      background: rgba(128, 128, 128, 0.08);
    }
  }

  .chat-body {
    max-width: 92%;
    padding: 7px 10px;
    border-radius: 8px;
    font-size: 13px;
    line-height: 1.6;
    white-space: pre-wrap;
    word-break: break-word;

    &.thinking {
      color: var(--text-secondary, #888);
    }
  }
}

.patch-card {
  margin-top: 6px;
  max-width: 92%;
  border: 1px solid rgba(99, 102, 241, 0.35);
  border-radius: 8px;
  padding: 8px 10px;
  background: rgba(99, 102, 241, 0.05);

  .patch-title {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 12px;
    font-weight: 600;
    margin-bottom: 4px;
    color: #6366f1;
  }
  .patch-line {
    font-size: 12px;
    line-height: 1.6;
    word-break: break-all;
  }
  .patch-warn {
    font-size: 12px;
    line-height: 1.6;
    color: #d97706;
  }
}
</style>
