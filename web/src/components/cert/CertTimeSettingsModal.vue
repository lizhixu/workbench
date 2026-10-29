<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  NAlert, NButton, NForm, NFormItem, NInputNumber, NModal, NSpace, NSpin, useMessage,
} from 'naive-ui'
import { SETTING_KEYS, getSystemSettings, saveSystemSettings } from '../../api/settings'

// 证书时间策略：自动续期提前天数 + 手动证书到期提醒提前天数。
// 这两个值只影响证书生命周期，入口从「系统设置」收进证书中心，与列表、
// 续期操作同屏；底层仍是系统级设置（certs.auto_renew_days /
// certs.expiry_reminder_days），读写接口为管理员专用，入口仅对管理员展示。
const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>()

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
    emit('update:show', false)
  } catch (e: any) {
    message.error(e?.message || '保存失败')
    await load()
  } finally {
    saving.value = false
  }
}

// 每次打开都重新拉一次，避免显示上一次停留时的旧值。
watch(
  () => props.show,
  (visible) => {
    if (visible) load()
  },
)
</script>

<template>
  <NModal
    :show="props.show"
    preset="card"
    title="证书时间设置"
    style="width: 620px; max-width: 94vw"
    @update:show="(v: boolean) => emit('update:show', v)"
  >
    <NAlert type="info" :bordered="false" style="margin-bottom: 16px">
      控制自动续期与手动证书到期提醒的提前天数，保存后即时生效，无需重启服务。
    </NAlert>
    <NSpin :show="loading">
      <NForm label-placement="top">
        <NFormItem label="自动续期提前天数">
          <NSpace vertical :size="4" style="width: 100%">
            <NInputNumber
              v-model:value="form.autoRenewDays"
              :min="1" :max="90" :step="1"
              style="width: 200px"
            />
            <p class="muted tip-hint">ACME 证书在到期前 N 天自动续期（默认 30 天）。短期证书按有效期 1/3 自适应收紧，取较小者。</p>
          </NSpace>
        </NFormItem>
        <NFormItem label="手动证书到期提醒提前天数">
          <NSpace vertical :size="4" style="width: 100%">
            <NInputNumber
              v-model:value="form.expiryReminderDays"
              :min="1" :max="90" :step="1"
              style="width: 200px"
            />
            <p class="muted tip-hint">手动签发 / 手动上传的证书不会自动续期，到期前 N 天经告警中心提醒一次（默认 30 天）。短期证书同样按 1/3 收紧；续期、替换或删除后提醒自动解除。</p>
          </NSpace>
        </NFormItem>
      </NForm>
    </NSpin>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="emit('update:show', false)">取消</NButton>
        <NButton type="primary" :loading="saving" :disabled="loading" @click="save">保存</NButton>
      </NSpace>
    </template>
  </NModal>
</template>
