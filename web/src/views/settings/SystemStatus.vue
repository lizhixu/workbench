<script setup lang="ts">
import { ref, onMounted } from 'vue'
import {
  NCard,
  NDescriptions,
  NDescriptionsItem,
  NTag,
  useMessage,
} from 'naive-ui'
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
  <NCard title="系统状态" :bordered="false">
    <NDescriptions :column="3" label-placement="left" bordered v-if="healthData">
      <NDescriptionsItem label="状态">
        <NTag type="success" size="small">运行中</NTag>
      </NDescriptionsItem>
      <NDescriptionsItem label="在线主机">{{ healthData.agents }}</NDescriptionsItem>
      <NDescriptionsItem label="版本">{{ healthData.version }}</NDescriptionsItem>
    </NDescriptions>
  </NCard>
</template>
