<script setup lang="ts">
import { ref, onMounted } from 'vue'
import {
  NCard,
  NDescriptions,
  NDescriptionsItem,
  NIcon,
  NTag,
  useMessage,
} from 'naive-ui'
import { PulseOutline } from '@vicons/ionicons5'
import { health } from '../../api/hosts'

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

onMounted(() => {
  loadHealth()
})
</script>

<template>
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
</template>
