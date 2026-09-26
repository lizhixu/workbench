<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NSelect,
  NInputNumber,
  NSwitch,
  NForm,
  NFormItem,
  NGrid,
  NGridItem,
  NIcon,
  NSpin,
  useMessage,
} from 'naive-ui'
import {
  TerminalOutline,
  CheckmarkCircleOutline,
  RefreshOutline,
} from '@vicons/ionicons5'
import { useTermPrefsStore } from '../../stores/termPrefs'
import { resolveTermTheme } from '../../utils/termThemes'
import type { TermPrefs, TermShell } from '../../api/settings'

const message = useMessage()
const termPrefs = useTermPrefsStore()

const form = ref<TermPrefs>({ ...termPrefs.prefs })
const saving = ref(false)
const loading = ref(false)

// Mirrors the server-side bounds in server/internal/settings so an
// out-of-range value is rejected in the form rather than by the API.
const FONT_MIN = 10
const FONT_MAX = 24
const SCROLLBACK_MIN = 500
const SCROLLBACK_MAX = 100000

const themeOptions = computed(() =>
  termPrefs.themes.map((t) => ({ label: t, value: t })),
)

const shellOptions: { label: string; value: TermShell }[] = [
  { label: '跟随主机默认 (Linux bash / Windows PowerShell)', value: '' },
  { label: 'Linux Bash', value: 'bash' },
  { label: 'Linux sh', value: 'sh' },
  { label: 'Windows PowerShell', value: 'powershell' },
  { label: 'Windows CMD', value: 'cmd' },
]

const fontOptions = [
  {
    label: 'Consolas / Cascadia Code (默认)',
    value: 'Consolas, "Cascadia Code", "Courier New", monospace',
  },
  { label: 'JetBrains Mono', value: '"JetBrains Mono", Consolas, monospace' },
  { label: 'Fira Code', value: '"Fira Code", Consolas, monospace' },
  { label: 'Menlo / Monaco', value: 'Menlo, Monaco, Consolas, monospace' },
  {
    label: 'Sarasa Mono SC (中文等宽)',
    value: '"Sarasa Mono SC", "Microsoft YaHei Mono", Consolas, monospace',
  },
]

// The preview reuses the same palette the terminal will render, so the choice
// is visible before saving.
const previewTheme = computed(() => resolveTermTheme(form.value.theme))
const previewStyle = computed(() => ({
  background: previewTheme.value.background || '#0d1117',
  color: previewTheme.value.foreground || '#c9d1d9',
  fontFamily: form.value.font_family,
  fontSize: `${form.value.font_size}px`,
}))

async function refresh() {
  loading.value = true
  try {
    const p = await termPrefs.load(true)
    form.value = { ...p }
  } finally {
    loading.value = false
  }
}

async function submit() {
  saving.value = true
  try {
    const saved = await termPrefs.save({ ...form.value })
    form.value = { ...saved }
    message.success('终端偏好已保存，新开的终端会话将立即生效')
  } catch (e: any) {
    message.error(e.message || '保存终端偏好失败')
  } finally {
    saving.value = false
  }
}

function resetToDefault() {
  termPrefs.reset()
  form.value = { ...termPrefs.prefs }
  message.info('已恢复默认值，点击保存后生效')
}

onMounted(refresh)
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><TerminalOutline /></NIcon>
        在线终端偏好
      </span>
    </template>
    <template #header-extra>
      <NSpace align="center" :size="8">
        <NButton size="small" quaternary :loading="loading" @click="refresh">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          重新加载
        </NButton>
        <NButton size="small" quaternary @click="resetToDefault">恢复默认</NButton>
      </NSpace>
    </template>

    <NSpin :show="loading">
      <NSpace vertical :size="14">
        <p class="muted tip-hint">
          偏好保存在控制端而非浏览器，从任意设备登录同一账号都会套用相同的终端外观。修改保存后对新开的终端会话生效，主题切换对当前会话立即生效。
        </p>

        <NGrid :cols="2" :x-gap="16">
          <NGridItem>
            <NForm label-placement="top">
              <NFormItem label="配色主题">
                <NSelect v-model:value="form.theme" :options="themeOptions" />
              </NFormItem>
              <NFormItem label="默认 Shell">
                <NSelect v-model:value="form.default_shell" :options="shellOptions" />
              </NFormItem>
              <NFormItem label="字体">
                <NSelect v-model:value="form.font_family" :options="fontOptions" tag filterable />
              </NFormItem>
            </NForm>
          </NGridItem>

          <NGridItem>
            <NForm label-placement="top">
              <NFormItem :label="`字号 (${FONT_MIN}-${FONT_MAX}px)`">
                <NInputNumber
                  v-model:value="form.font_size"
                  :min="FONT_MIN"
                  :max="FONT_MAX"
                  style="width: 100%"
                />
              </NFormItem>
              <NFormItem :label="`回滚缓冲行数 (${SCROLLBACK_MIN}-${SCROLLBACK_MAX})`">
                <NInputNumber
                  v-model:value="form.scrollback"
                  :min="SCROLLBACK_MIN"
                  :max="SCROLLBACK_MAX"
                  :step="500"
                  style="width: 100%"
                />
              </NFormItem>
              <NFormItem label="光标闪烁">
                <NSwitch v-model:value="form.cursor_blink" />
              </NFormItem>
            </NForm>
          </NGridItem>
        </NGrid>

        <div>
          <div class="preview-label">预览</div>
          <div class="term-preview" :style="previewStyle">
            <div>
              <span :style="{ color: previewTheme.green }">operator@web-01</span>:<span
                :style="{ color: previewTheme.blue }"
                >~</span
              >$ systemctl status nginx
            </div>
            <div><span :style="{ color: previewTheme.green }">●</span> nginx.service - A high performance web server</div>
            <div>
              &nbsp;&nbsp;&nbsp;Active: <span :style="{ color: previewTheme.green }">active (running)</span>
              since Mon 2026-09-02 09:14:03 CST
            </div>
            <div>
              &nbsp;&nbsp;&nbsp;&nbsp;Warn: <span :style="{ color: previewTheme.yellow }">worker process 1024 exited</span>
            </div>
            <div>
              &nbsp;&nbsp;&nbsp;&nbsp;Fail: <span :style="{ color: previewTheme.red }">connect() failed (111: Connection refused)</span>
            </div>
          </div>
        </div>

        <NSpace justify="end">
          <NButton type="primary" :loading="saving" @click="submit">
            <template #icon><NIcon :component="CheckmarkCircleOutline" /></template>
            保存偏好
          </NButton>
        </NSpace>
      </NSpace>
    </NSpin>
  </NCard>
</template>

<style scoped lang="scss">
.muted {
  color: var(--text-secondary);
  font-size: 13px;
}

.preview-label {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary);
  margin-bottom: 6px;
}

.term-preview {
  border-radius: 6px;
  border: 1px solid var(--border-color);
  padding: 12px 14px;
  line-height: 1.7;
  white-space: nowrap;
  overflow-x: auto;
}
</style>
