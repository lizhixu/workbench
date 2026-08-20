<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  NModal,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NRadioGroup,
  NRadio,
  NButton,
  useMessage,
  NSpace,
  NTag,
} from 'naive-ui'
import { shareTerminal } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'

const props = defineProps<{
  show: boolean
  hostId?: string
  sessionId?: string
}>()

const emit = defineEmits<{(e: 'update:show', val: boolean): void }>()

const message = useMessage()

const code = ref('')
const token = ref('')
const expireMinutes = ref(15)
const mode = ref<'view' | 'control'>('view')
const generating = ref(false)
const shareUrl = ref('')

const expireOptions = [
  { label: '5 分钟', value: 5 },
  { label: '15 分钟', value: 15 },
  { label: '1 小时', value: 60 },
  { label: '24 小时', value: 1440 },
]

async function handleGenerateLink() {
  if (!props.hostId || !props.sessionId) {
    message.error('终端会话尚未就绪')
    return
  }
  generating.value = true
  try {
    const res = await shareTerminal(props.hostId, props.sessionId, {
      expire_minutes: expireMinutes.value,
      mode: mode.value,
    })
    token.value = res.share_token
    code.value = res.share_code
    shareUrl.value = `${window.location.origin}/share/terminal?token=${res.share_token}&code=${res.share_code}`
    const ok = await copyToClipboard(shareUrl.value)
    if (ok) {
      message.success('已生成授权链接并复制到剪贴板！')
    } else {
      message.success('已生成授权链接，请手动复制！')
    }
  } catch (e: any) {
    message.error(e.message || '生成协同分享链接失败')
  } finally {
    generating.value = false
  }
}

watch(
  () => props.show,
  (val) => {
    if (val && !code.value && props.hostId && props.sessionId) {
      handleGenerateLink()
    }
  },
)
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    title="远程协助 / 终端协同分享"
    style="width: 500px"
    @update:show="val => emit('update:show', val)"
  >
    <div class="remote-assist-form">
      <NForm label-placement="top" size="small">
        <NFormItem label="协同口令与链接">
          <div class="code-input-row">
            <NInput
              :value="shareUrl || (code ? `口令: ${code}` : '正在生成...')"
              readonly
              placeholder="生成后自动填充分享链接"
            />
            <NButton type="primary" secondary :loading="generating" @click="handleGenerateLink">
              重新生成
            </NButton>
          </div>
        </NFormItem>

        <div v-if="code" class="code-badge-row">
          <span class="label">6位快捷口令:</span>
          <NTag type="info" size="large" round class="code-tag">
            {{ code }}
          </NTag>
          <span class="tip">（访客可在协同页面直接输入口令连接）</span>
        </div>

        <NFormItem label="链接有效期">
          <NSelect v-model:value="expireMinutes" :options="expireOptions" />
        </NFormItem>

        <NFormItem label="访客权限模式">
          <NRadioGroup v-model:value="mode" name="assist-mode">
            <NSpace vertical>
              <NRadio value="view">
                <strong>只读观看 (View Only)</strong> - 仅允许对方观看你的终端操作，无法输入或调整窗口
              </NRadio>
              <NRadio value="control">
                <strong>协同控制 (Full Control)</strong> - 允许对方直接在终端内输入命令并与你实时协同
              </NRadio>
            </NSpace>
          </NRadioGroup>
        </NFormItem>
      </NForm>

      <NSpace justify="center" style="margin-top: 16px">
        <NButton
          type="primary"
          block
          style="width: 240px"
          :loading="generating"
          @click="handleGenerateLink"
        >
          复制分享链接
        </NButton>
      </NSpace>
    </div>
  </NModal>
</template>

<style scoped lang="scss">
.remote-assist-form {
  .code-input-row {
    display: flex;
    gap: 8px;
    width: 100%;
  }

  .code-badge-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 12px;
    padding: 8px 12px;
    background-color: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 4px;

    .label {
      font-size: 13px;
      color: var(--text-secondary);
    }

    .code-tag {
      font-family: monospace;
      font-size: 16px;
      font-weight: 700;
      letter-spacing: 2px;
    }

    .tip {
      font-size: 12px;
      color: var(--text-muted);
    }
  }
}
</style>
