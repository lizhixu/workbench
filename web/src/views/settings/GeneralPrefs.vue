<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
  NCard,
  NButton,
  NSelect,
  NSwitch,
  NInput,
  NForm,
  NFormItem,
  NIcon,
  NSpin,
  useMessage,
} from 'naive-ui'
import { SettingsOutline } from '@vicons/ionicons5'
import { useSettingsStore } from '../../stores/settings'
import { SETTING_KEYS, type SettingsData } from '../../api/settings'
// Single source of truth for host tabs: the same list HostDetail renders.
import { subNavItems } from '../hosts/hostTabs'

const message = useMessage()
const settings = useSettingsStore()

const form = ref({
  defaultTab: 'files',
  showTips: true,
  defaultPath: '',
})
const saving = ref(false)
const loading = ref(false)

const tabOptions = computed(() =>
  subNavItems.map((i) => ({ label: i.label, value: i.key })),
)

function readForm(data: SettingsData) {
  const get = <T>(key: string, fallback: T): T => {
    const v = data[key]
    return v === undefined || v === null ? fallback : (v as T)
  }
  form.value = {
    defaultTab: get(SETTING_KEYS.defaultHostTab, 'files'),
    showTips: get(SETTING_KEYS.showTips, true),
    defaultPath: get(SETTING_KEYS.filesDefaultPath, ''),
  }
}

async function submit() {
  saving.value = true
  try {
    const data = await settings.saveUserKeys({
      [SETTING_KEYS.defaultHostTab]: form.value.defaultTab,
      [SETTING_KEYS.showTips]: form.value.showTips,
      [SETTING_KEYS.filesDefaultPath]: form.value.defaultPath.trim(),
    })
    readForm(data)
    message.success('通用设置已保存')
  } catch (e: any) {
    message.error(e.message || '保存通用设置失败')
  } finally {
    saving.value = false
  }
}

onMounted(async () => {
  loading.value = true
  try {
    await settings.load()
    readForm(settings.userData)
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><SettingsOutline /></NIcon>
        通用设置
      </span>
    </template>
    <NSpin :show="loading">
      <NForm label-placement="left" label-width="140px">
        <NFormItem label="主机默认页签">
          <NSelect
            v-model:value="form.defaultTab"
            :options="tabOptions"
            style="max-width: 280px"
          />
        </NFormItem>
        <NFormItem label="显示功能提示语">
          <NSwitch v-model:value="form.showTips" />
          <span class="muted tip-hint" style="margin-left: 8px">关闭后隐藏各页面的功能说明文字</span>
        </NFormItem>
        <NFormItem label="文件管理默认路径">
          <NInput
            v-model:value="form.defaultPath"
            placeholder="留空使用系统默认（Linux /root，Windows C:\）"
            style="max-width: 380px"
            clearable
          />
        </NFormItem>
        <NFormItem label=" ">
          <NButton type="primary" :loading="saving" @click="submit">保存</NButton>
        </NFormItem>
      </NForm>
      <p class="muted tip-hint">
        首选页面决定进入主机详情时默认展示的模块；文件默认路径支持 Linux 绝对路径与 Windows 盘符路径。
      </p>
    </NSpin>
  </NCard>
</template>
