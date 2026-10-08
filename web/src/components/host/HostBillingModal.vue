<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  NModal, NForm, NFormItem, NInput, NInputNumber, NSelect, NSwitch,
  NButton, NSpace, NGrid, NGridItem, NDatePicker, useMessage,
} from 'naive-ui'
import { updateHostBilling } from '../../api/hosts'
import type { Host, HostBillingConfig } from '../../api/types'

const props = defineProps<{
  show: boolean
  host: Host | null
}>()

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void
  (e: 'saved'): void
}>()

const message = useMessage()
const saving = ref(false)

const currencyOptions = [
  { label: 'CNY (人民币 ¥)', value: 'CNY' },
  { label: 'USD (美元 $)', value: 'USD' },
  { label: 'EUR (欧元 €)', value: 'EUR' },
  { label: 'HKD (港币 HK$)', value: 'HKD' },
  { label: 'JPY (日元 ¥)', value: 'JPY' },
  { label: 'GBP (英镑 £)', value: 'GBP' },
  { label: 'USDT (泰达币)', value: 'USDT' },
]

const cycleOptions = [
  { label: '月付 (Monthly)', value: '月付' },
  { label: '季付 (Quarterly)', value: '季付' },
  { label: '半年付 (Semi-Annually)', value: '半年付' },
  { label: '年付 (Annually)', value: '年付' },
  { label: '两年付 (Biennially)', value: '两年付' },
  { label: '三年付 (Triennially)', value: '三年付' },
  { label: '一次性 (One-time)', value: '一次性' },
  { label: '按量计费 (Pay As You Go)', value: '按量计费' },
]

const calcTypeOptions = [
  { label: '上行 + 下行 (双向)', value: 'both' },
  { label: '仅上行 (出向流量)', value: 'out' },
  { label: '仅下行 (入向流量)', value: 'in' },
]

const form = ref<HostBillingConfig>({
  price: undefined,
  currency: 'CNY',
  billing_cycle: '年付',
  expires_at: '',
  auto_renewal: false,
  traffic_limit_gb: undefined,
  traffic_calc_type: 'both',
  traffic_reset_day: 1,
  renewal_url: '',
  notes: '',
})

const expiresTimestamp = ref<number | null>(null)

watch(
  () => props.show,
  (val) => {
    if (val && props.host) {
      form.value = {
        price: props.host.price,
        currency: props.host.currency || 'CNY',
        billing_cycle: props.host.billing_cycle || '年付',
        expires_at: props.host.expires_at || '',
        auto_renewal: !!props.host.auto_renewal,
        traffic_limit_gb: props.host.traffic_limit_gb,
        traffic_calc_type: props.host.traffic_calc_type || 'both',
        traffic_reset_day: props.host.traffic_reset_day || 1,
        renewal_url: props.host.renewal_url || '',
        notes: props.host.notes || '',
      }
      if (props.host.expires_at) {
        const d = new Date(props.host.expires_at)
        expiresTimestamp.value = isNaN(d.getTime()) ? null : d.getTime()
      } else {
        expiresTimestamp.value = null
      }
    }
  },
  { immediate: true },
)

function handleDateChange(ts: number | null) {
  if (!ts) {
    form.value.expires_at = ''
    return
  }
  const d = new Date(ts)
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  form.value.expires_at = `${y}/${m}/${day}`
}

async function handleSave() {
  if (!props.host) return
  saving.value = true
  try {
    await updateHostBilling(props.host.id, form.value)
    message.success('主机财务与流量配置已保存')
    emit('update:show', false)
    emit('saved')
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :title="`主机财务与规格配置 · ${host?.hostname || ''}`"
    style="width: 660px; max-width: 95vw"
    @update:show="(val) => emit('update:show', val)"
  >
    <NForm label-placement="top" size="small">
      <!-- 第一行：价格、货币、计费周期 -->
      <NGrid :cols="3" :x-gap="12">
        <NGridItem>
          <NFormItem label="价格">
            <NInputNumber
              v-model:value="form.price"
              :min="0"
              :precision="2"
              placeholder="例如: 111.11"
              style="width: 100%"
            />
          </NFormItem>
        </NGridItem>
        <NGridItem>
          <NFormItem label="货币">
            <NSelect
              v-model:value="form.currency"
              :options="currencyOptions"
              filterable
              tag
              placeholder="选择或输入"
            />
          </NFormItem>
        </NGridItem>
        <NGridItem>
          <NFormItem label="计费周期">
            <NSelect
              v-model:value="form.billing_cycle"
              :options="cycleOptions"
              filterable
              tag
              placeholder="选择周期"
            />
          </NFormItem>
        </NGridItem>
      </NGrid>

      <!-- 第二行：到期时间、自动续费 -->
      <NGrid :cols="3" :x-gap="12">
        <NGridItem :span="2">
          <NFormItem label="到期时间">
            <NDatePicker
              v-model:value="expiresTimestamp"
              type="date"
              clearable
              format="yyyy/MM/dd"
              placeholder="请选择到期日期 (例如: 2028/11/11)"
              style="width: 100%"
              @update:value="handleDateChange"
            />
          </NFormItem>
        </NGridItem>
        <NGridItem>
          <NFormItem label="自动续费">
            <div style="display: flex; align-items: center; height: 34px; gap: 8px">
              <NSwitch v-model:value="form.auto_renewal" />
              <span style="font-size: 13px">{{ form.auto_renewal ? '已开启' : '未开启' }}</span>
            </div>
          </NFormItem>
        </NGridItem>
      </NGrid>

      <!-- 第三行：流量限制 (GB)、月流量计算类型、流量重置日 -->
      <NGrid :cols="3" :x-gap="12">
        <NGridItem>
          <NFormItem label="流量限制 (GB)">
            <NInputNumber
              v-model:value="form.traffic_limit_gb"
              :min="0"
              placeholder="e.g. 1000"
              style="width: 100%"
            />
          </NFormItem>
        </NGridItem>
        <NGridItem>
          <NFormItem label="月流量计算类型">
            <NSelect
              v-model:value="form.traffic_calc_type"
              :options="calcTypeOptions"
            />
          </NFormItem>
        </NGridItem>
        <NGridItem>
          <NFormItem label="流量重置日">
            <NInputNumber
              v-model:value="form.traffic_reset_day"
              :min="1"
              :max="28"
              placeholder="每月 1 日"
              style="width: 100%"
            />
          </NFormItem>
        </NGridItem>
      </NGrid>

      <!-- 第四行：续费地址 -->
      <NFormItem label="续费地址 (控制台或服务商链接)">
        <NInput
          v-model:value="form.renewal_url"
          placeholder="例如: https://console.example.com/vps/billing"
        />
      </NFormItem>

      <!-- 第五行：备注说明 -->
      <NFormItem label="备注信息">
        <NInput
          v-model:value="form.notes"
          type="textarea"
          :rows="2"
          placeholder="可记录主机优惠码、套餐配置、独立 IP 数、用途等任意备注说明"
        />
      </NFormItem>
    </NForm>

    <template #footer>
      <NSpace justify="end">
        <NButton @click="emit('update:show', false)">取消</NButton>
        <NButton type="primary" :loading="saving" @click="handleSave">
          保存配置
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
:deep(.n-form-item) {
  margin-bottom: 8px;
}
</style>
