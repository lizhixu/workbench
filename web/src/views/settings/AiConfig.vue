<script setup lang="ts">
import { ref, onMounted } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NInput,
  NSwitch,
  NFormItem,
  NSelect,
  NAlert,
  NIcon,
  NTag,
  NDynamicInput,
  useMessage,
} from 'naive-ui'
import { CheckmarkCircleOutline, SparklesOutline } from '@vicons/ionicons5'
import { getAIConfig, setAIConfig, testAIConfig, type AIConfig } from '../../api/ai'

// 从旧 Settings.vue 抽取：AI 大模型配置与助手
defineOptions({ name: 'AiConfig' })

const message = useMessage()

const aiConfig = ref<AIConfig>({
  base_url: 'https://api.deepseek.com',
  model: 'deepseek-chat',
  api_key: '',
  enabled: false,
  provider: 'deepseek',
})
const selectedProvider = ref('deepseek')
const savingAI = ref(false)
const testingAI = ref(false)
const testResult = ref<{ ok: boolean; message: string } | null>(null)
// 自定义请求头（键值对编辑器用，保存/测试前同步回 aiConfig.headers）
const headerPairs = ref<{ key: string; value: string }[]>([])

function syncHeadersToConfig() {
  const headers: Record<string, string> = {}
  for (const p of headerPairs.value) {
    const k = (p.key || '').trim()
    const v = (p.value || '').trim()
    if (k && v) headers[k] = v
  }
  if (Object.keys(headers).length > 0) {
    aiConfig.value.headers = headers
  } else {
    delete aiConfig.value.headers
  }
}

const providerPresets: Record<string, { label: string; base_url: string; model: string; key_tip: string }> = {
  deepseek: {
    label: 'DeepSeek (推荐)',
    base_url: 'https://api.deepseek.com',
    model: 'deepseek-chat',
    key_tip: '填入 DeepSeek API Key (sk-...)',
  },
  openai: {
    label: 'OpenAI',
    base_url: 'https://api.openai.com/v1',
    model: 'gpt-4o-mini',
    key_tip: '填入 OpenAI API Key (sk-...)',
  },
  ollama: {
    label: 'Ollama (本地私有部署)',
    base_url: 'http://localhost:11434',
    model: 'qwen2.5:14b',
    key_tip: '本地 Ollama 通常无需填 API Key',
  },
  siliconflow: {
    label: '硅基流动 (SiliconFlow)',
    base_url: 'https://api.siliconflow.cn/v1',
    model: 'deepseek-ai/DeepSeek-V3',
    key_tip: '填入 SiliconFlow API Key',
  },
  dashscope: {
    label: '阿里云百炼 (通义千问)',
    base_url: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    model: 'qwen-plus',
    key_tip: '填入 DashScope API Key',
  },
  custom: {
    label: '自定义 OpenAI 兼容接口 / vLLM / OneAPI',
    base_url: 'http://localhost:8000/v1',
    model: 'qwen2.5:14b',
    key_tip: '根据服务提供方要求填入 Key',
  },
}

const providerOptions = Object.entries(providerPresets).map(([k, v]) => ({
  label: v.label,
  value: k,
}))

function handleProviderChange(val: string) {
  selectedProvider.value = val
  const preset = providerPresets[val]
  if (preset) {
    aiConfig.value.base_url = preset.base_url
    aiConfig.value.model = preset.model
    aiConfig.value.provider = val
  }
}

async function loadAIConfig() {
  try {
    const data = await getAIConfig()
    if (data) {
      aiConfig.value = data
      headerPairs.value = Object.entries(data.headers || {}).map(([key, value]) => ({ key, value }))
      if (data.provider) {
        selectedProvider.value = data.provider
      } else if (data.base_url?.includes('deepseek')) {
        selectedProvider.value = 'deepseek'
      } else if (data.base_url?.includes(':11434')) {
        selectedProvider.value = 'ollama'
      } else if (data.base_url?.includes('openai.com')) {
        selectedProvider.value = 'openai'
      } else {
        selectedProvider.value = 'custom'
      }
    }
  } catch {
    // AI not configured — ignore
  }
}

async function saveAIConfig() {
  savingAI.value = true
  try {
    syncHeadersToConfig()
    aiConfig.value.provider = selectedProvider.value
    await setAIConfig(aiConfig.value)
    message.success('AI 大模型配置已成功保存并持久化！')
    await loadAIConfig()
  } catch (e: any) {
    message.error(e.message || '保存 AI 配置失败')
  } finally {
    savingAI.value = false
  }
}

async function handleTestAI() {
  if (!aiConfig.value.base_url) {
    message.warning('请先填写 LLM 端点 URL')
    return
  }
  testingAI.value = true
  testResult.value = null
  try {
    syncHeadersToConfig()
    const res = await testAIConfig(aiConfig.value)
    testResult.value = { ok: true, message: res.message }
    message.success('AI 接口连通性测试通过！')
  } catch (e: any) {
    testResult.value = { ok: false, message: e.message || '连通性测试失败' }
    message.error(e.message || 'AI 接口连通性测试失败')
  } finally {
    testingAI.value = false
  }
}

onMounted(() => {
  loadAIConfig()
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><SparklesOutline /></NIcon>
        AI 大模型配置与助手
      </span>
    </template>
    <template #header-extra>
      <NTag :type="aiConfig.enabled ? 'success' : 'default'" size="small" round>
        {{ aiConfig.enabled ? '已启用 AI 功能' : '未启用' }}
      </NTag>
    </template>

    <NSpace vertical :size="14">
      <p class="muted tip-hint">
        Watchman 支持集成各类大语言模型（DeepSeek、OpenAI、Ollama、vLLM 等）。启用后将在在线终端、命令执行排错、主机资源诊断中提供智能辅助。
      </p>

      <NFormItem label="服务提供商 / 预设方案">
        <NSelect
          v-model:value="selectedProvider"
          :options="providerOptions"
          @update:value="handleProviderChange"
        />
      </NFormItem>

      <NFormItem label="LLM 接口端点 (Base URL)" required>
        <NInput
          v-model:value="aiConfig.base_url"
          placeholder="例如: https://api.deepseek.com 或 http://localhost:11434"
        />
      </NFormItem>

      <NFormItem label="模型名称 (Model)" required>
        <NInput
          v-model:value="aiConfig.model"
          placeholder="例如: deepseek-chat 或 qwen2.5:14b"
        />
      </NFormItem>

      <NFormItem label="API Key (密钥)">
        <NInput
          v-model:value="aiConfig.api_key"
          type="password"
          show-password-on="click"
          :placeholder="providerPresets[selectedProvider]?.key_tip || '输入 API Key'"
        />
      </NFormItem>

      <NFormItem label="自定义请求头 (Headers)">
        <NDynamicInput
          v-model:value="headerPairs"
          preset="pair"
          key-placeholder="Header 名称，例如 X-Custom-Key"
          value-placeholder="Header 值"
        />
        <template #feedback>
          <span class="muted tip-hint">
            随每次 LLM 请求发送的额外 HTTP 头（如网关/代理要求的鉴权头）。留空则不发送；
            如填写 Authorization 将覆盖默认的 Bearer 鉴权。
          </span>
        </template>
      </NFormItem>

      <NFormItem label="启用 AI 助手与诊断功能">
        <NSwitch v-model:value="aiConfig.enabled" />
      </NFormItem>

      <NAlert
        v-if="testResult"
        :type="testResult.ok ? 'success' : 'error'"
        :title="testResult.ok ? '连通性测试通过' : '测试失败'"
        closable
        @close="testResult = null"
      >
        {{ testResult.message }}
      </NAlert>

      <NSpace justify="space-between" align="center" style="margin-top: 8px">
        <NButton
          secondary
          type="info"
          :loading="testingAI"
          @click="handleTestAI"
        >
          <template #icon><NIcon :component="SparklesOutline" /></template>
          测试连接
        </NButton>

        <NButton
          type="primary"
          :loading="savingAI"
          @click="saveAIConfig"
        >
          <template #icon><NIcon :component="CheckmarkCircleOutline" /></template>
          保存配置
        </NButton>
      </NSpace>
    </NSpace>
  </NCard>
</template>

<style scoped lang="scss">
.muted { color: var(--text-secondary); font-size: 13px; }
</style>
