<script setup lang="ts">
import { computed, onActivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NForm, NFormItem, NIcon, NInput, NInputGroup,
  NPopconfirm, NSpace, NSwitch, useMessage,
} from 'naive-ui'
import { KeyOutline, RefreshOutline, CopyOutline, DiceOutline } from '@vicons/ionicons5'
import {
  SETTING_KEYS, getSystemSettings, saveSystemSettings,
} from '../../api/settings'

// 系统设置 → 安全入口（仅管理员可见）。
// 开启后只能通过秘密入口地址访问/登录面板：访问入口 URL 签发 HttpOnly
// cookie 后才能看到登录页并发起登录；直接访问 / 或调登录 API 一律 404。
// 后端见 server/internal/secentry。
defineOptions({ name: 'SecureEntry' })

const message = useMessage()
const loading = ref(false)
const saving = ref(false)

const form = reactive({
  enabled: false,
  path: '',
})

const entryUrl = computed(() =>
  form.path ? `${window.location.origin}/api/v1/secure-entry/${form.path}` : '',
)

function randomPath(): string {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
  const buf = new Uint32Array(16)
  crypto.getRandomValues(buf)
  return Array.from(buf, (n) => chars[n % chars.length]).join('')
}

function genPath() {
  form.path = randomPath()
}

async function load() {
  loading.value = true
  try {
    const res = await getSystemSettings()
    form.enabled = res.data[SETTING_KEYS.secureEntryEnabled] === true
    const p = res.data[SETTING_KEYS.secureEntryPath]
    form.path = typeof p === 'string' ? p : ''
  } catch (e: any) {
    message.error(e?.message || '加载安全入口配置失败')
  } finally {
    loading.value = false
  }
}

function validate(): string | null {
  if (form.enabled && !form.path) return '启用安全入口必须先设置入口路径'
  if (form.path && !/^[A-Za-z0-9][A-Za-z0-9_-]{5,63}$/.test(form.path)) {
    return '入口路径必须为 6~64 位字母、数字、_、-，且首字符为字母或数字'
  }
  return null
}

async function doSave() {
  const err = validate()
  if (err) {
    message.warning(err)
    return
  }
  saving.value = true
  try {
    await saveSystemSettings({
      [SETTING_KEYS.secureEntryEnabled]: form.enabled,
      [SETTING_KEYS.secureEntryPath]: form.path,
    })
    message.success('安全入口配置已保存')
  } catch (e: any) {
    message.error(e?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function copyUrl() {
  try {
    await navigator.clipboard.writeText(entryUrl.value)
    message.success('入口地址已复制')
  } catch {
    message.warning('复制失败，请手动复制')
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="secure-entry page-flex-column">
    <NAlert type="info" :show-icon="true" class="tip-hint" style="flex-shrink: 0; margin-bottom: 12px">
      开启安全入口后，面板只能通过下方唯一的秘密入口地址访问：访问该地址会签发通行凭证，之后才能看到登录页并发起登录。
      直接访问面板首页或直接调用登录接口将一律返回 404，扫描器无法发现面板存在。Agent 安装/注册、应用 webhook 等基础设施接口不受影响。
    </NAlert>

    <NAlert
      v-if="form.enabled"
      type="warning" :show-icon="true" style="flex-shrink: 0; margin-bottom: 12px"
    >
      安全入口已启用。请务必收藏好入口地址——<b>忘记入口路径将无法登录面板</b>。
      恢复方法：在控制机上执行
      <code>watchman-server -reset-secure-entry -data &lt;数据目录&gt;</code>
     （默认数据目录 /opt/watchman/data），或直接编辑数据目录下的 settings.json
      将 security.secure_entry_enabled 改为 false 后重启。
      注意：启用后你当前浏览器也需要访问一次入口地址签发凭证，否则刷新页面将看到 404（你的登录会话本身不受影响）。
    </NAlert>

    <NForm :disabled="loading" label-placement="left" label-width="120px" style="max-width: 640px; flex-shrink: 0">
      <NFormItem label="启用安全入口">
        <NSwitch v-model:value="form.enabled" />
      </NFormItem>
      <NFormItem label="入口路径" :feedback="validate() || undefined" :validation-status="validate() ? 'error' : undefined">
        <NInputGroup>
          <NInput
            v-model:value="form.path"
            placeholder="6~64 位字母/数字/_/-，如 k9xQ2mZ7aB4cD8eF"
            :maxlength="64"
            clearable
          />
          <NButton @click="genPath">
            <template #icon><NIcon><DiceOutline /></NIcon></template>
            随机生成
          </NButton>
        </NInputGroup>
      </NFormItem>
      <NFormItem label=" ">
        <NSpace>
          <NPopconfirm @positive-click="doSave">
            <template #trigger>
              <NButton type="primary" :loading="saving || loading">
                <template #icon><NIcon><KeyOutline /></NIcon></template>
                保存
              </NButton>
            </template>
            <template v-if="form.enabled">
              开启后只能通过秘密入口地址登录面板，直接访问首页将返回 404。确认已记下入口路径吗？
            </template>
            <template v-else>确认保存安全入口配置？</template>
          </NPopconfirm>
          <NButton :loading="loading" @click="load">
            <template #icon><NIcon><RefreshOutline /></NIcon></template>
            重新加载
          </NButton>
        </NSpace>
      </NFormItem>
    </NForm>

    <NAlert
      v-if="form.enabled && entryUrl"
      type="success" :show-icon="true" style="flex-shrink: 0; margin-top: 4px; max-width: 640px"
    >
      <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap">
        <span>秘密入口地址：</span>
        <code style="word-break: break-all">{{ entryUrl }}</code>
        <NButton size="small" secondary @click="copyUrl" :disabled="!entryUrl">
          <template #icon><NIcon><CopyOutline /></NIcon></template>
          复制
        </NButton>
      </div>
    </NAlert>
  </div>
</template>

<style scoped lang="scss">
.secure-entry {
  height: 100%;
  overflow-y: auto;
}
</style>
