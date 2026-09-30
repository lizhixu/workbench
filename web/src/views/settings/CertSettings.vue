<script setup lang="ts">
import { ref } from 'vue'
import {
  NAlert, NButton, NCard, NForm, NFormItem, NIcon, NInputNumber,
  NSpace, NSpin, useMessage,
} from 'naive-ui'
import { TimeOutline, RefreshOutline } from '@vicons/ionicons5'
import {
  SETTING_KEYS, getSystemSettings, saveSystemSettings,
} from '../../api/settings'

// 系统设置 → 证书时间设置（仅管理员可见）。
// 自动续期提前天数：ACME 自动续期在到期前 N 天触发；
// 手动证书到期提醒提前天数：手动签发 / 手动上传的证书不会自动续期，
// 到期前 N 天经告警中心（builtin-cert-expiry）提醒一次。
// 两者对短期证书都按有效期 1/3 自适应收紧（取较小者），修改即时生效，无需重启。
// 后端见 server/internal/settings（certs.auto_renew_days / certs.expiry_reminder_days）。
defineOptions({ name: 'CertSettings' })

const message = useMessage()
const loading = ref(false)
const saving = ref(false)

const form = ref({
  autoRenewDays: 30,
  expiryReminderDays: 30,
})

function clampDays(v: unknown): number {
  const n = Number(v)
  if (!Number.isFinite(n)) return 30
  return Math.min(90, Math.max(1, Math.round(n)))
}

async function load() {
  loading.value = true
  try {
    const res = await getSystemSettings()
    form.value.autoRenewDays = clampDays(res.data[SETTING_KEYS.certAutoRenewDays] ?? 30)
    form.value.expiryReminderDays = clampDays(res.data[SETTING_KEYS.certExpiryReminderDays] ?? 30)
  } catch (e: any) {
    message.error(e?.message || '加载证书时间设置失败')
  } finally {
    loading.value = false
  }
}

async function save() {
  const payload = {
    [SETTING_KEYS.certAutoRenewDays]: clampDays(form.value.autoRenewDays),
    [SETTING_KEYS.certExpiryReminderDays]: clampDays(form.value.expiryReminderDays),
  }
  form.value.autoRenewDays = payload[SETTING_KEYS.certAutoRenewDays]
  form.value.expiryReminderDays = payload[SETTING_KEYS.certExpiryReminderDays]
  saving.value = true
  try {
    await saveSystemSettings(payload)
    message.success('证书时间设置已保存，即时生效')
  } catch (e: any) {
    message.error(e?.message || '保存失败')
    await load()
  } finally {
    saving.value = false
  }
}

load()
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><TimeOutline /></NIcon>
        证书时间设置
      </span>
    </template>
    <template #header-extra>
      <NButton size="small" quaternary :loading="loading" @click="load">
        <template #icon><NIcon><RefreshOutline /></NIcon></template>
        重新加载
      </NButton>
    </template>

    <NSpin :show="loading">
      <NAlert type="info" :bordered="false" style="margin-bottom: 16px">
        控制自动续期与手动证书到期提醒的提前天数，保存后即时生效，无需重启服务。
      </NAlert>
      <NForm label-placement="left" label-width="220px">
        <NFormItem label="自动续期提前天数">
          <NSpace vertical :size="4">
            <NInputNumber
              v-model:value="form.autoRenewDays"
              :min="1" :max="90" :step="1"
              style="width: 200px"
            />
            <p class="muted tip-hint">ACME 证书在到期前 N 天自动续期（默认 30 天）。短期证书按有效期 1/3 自适应收紧，取较小者。</p>
          </NSpace>
        </NFormItem>
        <NFormItem label="手动证书到期提醒提前天数">
          <NSpace vertical :size="4">
            <NInputNumber
              v-model:value="form.expiryReminderDays"
              :min="1" :max="90" :step="1"
              style="width: 200px"
            />
            <p class="muted tip-hint">手动签发 / 手动上传的证书不会自动续期，到期前 N 天经告警中心提醒一次（默认 30 天）。短期证书同样按 1/3 收紧；续期、替换或删除后提醒自动解除。</p>
          </NSpace>
        </NFormItem>
        <NFormItem>
          <NButton type="primary" :loading="saving" @click="save">保存</NButton>
        </NFormItem>
      </NForm>
    </NSpin>
  </NCard>
</template>
