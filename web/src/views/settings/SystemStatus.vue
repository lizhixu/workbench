<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted } from 'vue'
import {
  NButton,
  NCard,
  NDescriptions,
  NDescriptionsItem,
  NForm,
  NFormItem,
  NIcon,
  NSelect,
  NSpace,
  NTag,
  useMessage,
} from 'naive-ui'
import { PulseOutline, TimeOutline } from '@vicons/ionicons5'
import { health } from '../../api/hosts'
import {
  SETTING_KEYS,
  getSystemSettings,
  saveSystemSettings,
} from '../../api/settings'
import { fmtDateTime, setPanelTimezone } from '../../utils/time'

// 从旧 Settings.vue 抽取：系统状态卡片
defineOptions({ name: 'SystemStatus' })

const message = useMessage()
const healthData = ref<any>(null)

async function loadHealth() {
  try {
    healthData.value = await health()
  } catch (e: any) {
    message.error(e.message)
  }
}

// ---- 面板时区（system 作用域，仅管理员可改）-------------------------------
// 控制端进程本地时间与面板全部时间展示的基准。纯下拉选择，不接受自由输入
// （服务端仍以 LoadLocation 校验兜底）；留空 = 跟随服务器系统时区。
const TIMEZONE_GROUPS: { label: string; zones: string[] }[] = [
  { label: '常用', zones: ['Asia/Shanghai', 'UTC'] },
  { label: '亚洲', zones: ['Asia/Hong_Kong', 'Asia/Taipei', 'Asia/Singapore', 'Asia/Tokyo', 'Asia/Seoul', 'Asia/Bangkok', 'Asia/Jakarta', 'Asia/Manila', 'Asia/Kolkata', 'Asia/Dubai'] },
  { label: '欧洲', zones: ['Europe/London', 'Europe/Berlin', 'Europe/Moscow'] },
  { label: '美洲', zones: ['America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles', 'America/Toronto', 'America/Vancouver', 'America/Mexico_City', 'America/Sao_Paulo'] },
  { label: '大洋洲 / 非洲', zones: ['Australia/Perth', 'Australia/Sydney', 'Pacific/Auckland', 'Africa/Cairo', 'Africa/Johannesburg'] },
]
const ALL_TIMEZONES = TIMEZONE_GROUPS.flatMap((g) => g.zones)

const tzVisible = ref(false) // 非管理员读不到 system 设置，整卡隐藏
const tzValue = ref('')
const tzSaving = ref(false)
const nowText = ref('')

const tzOptions = computed(() => {
  const opts: any[] = [
    { label: '跟随服务器系统时区', value: '' },
    ...TIMEZONE_GROUPS.map((g) => ({
      type: 'group',
      label: g.label,
      key: g.label,
      children: g.zones.map((z) => ({ label: z, value: z })),
    })),
  ]
  // 历史值或手改 settings.json 的时区不在列表内时单独置顶，保证可见、可改回。
  if (tzValue.value && !ALL_TIMEZONES.includes(tzValue.value)) {
    opts.splice(1, 0, {
      type: 'group',
      label: '当前值',
      key: '__current__',
      children: [{ label: tzValue.value, value: tzValue.value }],
    })
  }
  return opts
})

async function loadTimezone() {
  try {
    const res = await getSystemSettings()
    tzValue.value = (res.data[SETTING_KEYS.timezone] as string) || ''
    tzVisible.value = true
  } catch {
    tzVisible.value = false
  }
}

async function saveTimezone() {
  tzSaving.value = true
  try {
    await saveSystemSettings({ [SETTING_KEYS.timezone]: tzValue.value })
    // 立即在本机界面生效，无需等下一次设置加载。
    setPanelTimezone(tzValue.value)
    tickClock()
    message.success('面板时区已保存，即时生效')
  } catch (e: any) {
    message.error(e?.message || '保存失败')
    await loadTimezone()
  } finally {
    tzSaving.value = false
  }
}

let clockTimer: ReturnType<typeof setInterval> | null = null
function tickClock() {
  nowText.value = fmtDateTime(new Date())
}

onMounted(() => {
  loadHealth()
  loadTimezone()
  tickClock()
  clockTimer = setInterval(tickClock, 1000)
})

onUnmounted(() => {
  if (clockTimer) clearInterval(clockTimer)
})
</script>

<template>
  <NSpace vertical :size="16">
    <NCard :bordered="false" size="small">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">
          <NIcon style="vertical-align: middle; margin-right: 6px"><PulseOutline /></NIcon>
          系统状态
        </span>
      </template>
      <NDescriptions :column="3" label-placement="left" bordered v-if="healthData">
        <NDescriptionsItem label="状态">
          <NTag type="success" size="small">运行中</NTag>
        </NDescriptionsItem>
        <NDescriptionsItem label="在线主机">{{ healthData.agents }}</NDescriptionsItem>
        <NDescriptionsItem label="版本">{{ healthData.version }}</NDescriptionsItem>
      </NDescriptions>
    </NCard>

    <NCard v-if="tzVisible" :bordered="false" size="small">
      <template #header>
        <span style="font-size: 16px; font-weight: 700">
          <NIcon style="vertical-align: middle; margin-right: 6px"><TimeOutline /></NIcon>
          面板时区
        </span>
      </template>
      <NForm label-placement="left" label-width="120px">
        <NFormItem label="时区">
          <NSpace vertical :size="4">
            <NSelect
              v-model:value="tzValue"
              :options="tzOptions"
              clearable
              placeholder="跟随服务器系统时区"
              style="width: 320px"
            />
            <p class="muted tip-hint">
              面板全部时间（告警、审计、会话、监控图表与服务端日志）按此时区显示；所有登录用户一致。
              保存即时生效，无需重启；Agent 的月流量计费周期仍按各主机本地时间。
            </p>
          </NSpace>
        </NFormItem>
        <NFormItem label="当前面板时间">
          <span class="mono-font">{{ nowText }}</span>
        </NFormItem>
        <NFormItem>
          <NButton type="primary" :loading="tzSaving" @click="saveTimezone">保存</NButton>
        </NFormItem>
      </NForm>
    </NCard>
  </NSpace>
</template>
