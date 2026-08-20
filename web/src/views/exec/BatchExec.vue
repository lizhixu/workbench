<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard, NSpace, NButton, NInput, NSelect, NCheckbox, NDataTable,
  useMessage, useDialog, NSpin,
} from 'naive-ui'
import { listHosts, batchExec } from '../../api/hosts'
import type { Host, BatchExecResult } from '../../api/types'

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const hosts = ref<Host[]>([])
const loading = ref(true)
const selectedHostIds = ref<string[]>([])
const command = ref('')
const shell = ref('')
const isScript = ref(false)
const running = ref(false)
const results = ref<BatchExecResult[]>([])

const shellOptions = [
  { label: '自动', value: '' },
  { label: 'bash', value: 'bash' },
  { label: 'sh', value: 'sh' },
  { label: 'powershell', value: 'powershell' },
  { label: 'cmd', value: 'cmd' },
]

async function loadHosts() {
  loading.value = true
  try {
    const res = await listHosts()
    hosts.value = res.data || res
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

async function run() {
  if (!command.value) {
    message.warning('请输入命令')
    return
  }
  if (selectedHostIds.value.length === 0) {
    message.warning('请选择目标主机')
    return
  }
  running.value = true
  results.value = []
  try {
    results.value = await batchExec(selectedHostIds.value, command.value, shell.value, 120)
  } catch (e: any) {
    // 409 = high-risk command requires confirmation.
    if (e.status === 409 && e.needsConfirm) {
      running.value = false
      dialog.warning({
        title: '高危命令确认',
        content: `该命令被标记为高危操作：${e.message || '需要二次确认'}\n\n即将执行的命令：\n${command.value}`,
        positiveText: '确认执行',
        negativeText: '取消',
        onPositiveClick: async () => {
          running.value = true
          try {
            results.value = await batchExec(selectedHostIds.value, command.value, shell.value, 120, true)
          } catch (e2: any) {
            message.error(e2.message || '执行失败')
          } finally {
            running.value = false
          }
        },
      })
      return
    }
    // 403 = blocked by policy.
    if (e.status === 403) {
      message.error(`命令已被拦截：${e.message || '命中黑名单规则'}`)
    } else {
      message.error(e.message || '执行失败')
    }
  } finally {
    running.value = false
  }
}

const hostColumns = [
  { title: '主机名', key: 'hostname' },
  { title: '系统', key: 'os', width: 80 },
  {
    title: '状态', key: 'status', width: 80,
    render: (row: Host) => row.status === 'online'
      ? '<span style="color:#52c41a">在线</span>'
      : '<span style="color:#999">离线</span>',
  },
]

const resultColumns = [
  { title: '主机ID', key: 'host_id', width: 120, ellipsis: { tooltip: true } },
  { title: '退出码', key: 'exit_code', width: 80, render: (r: BatchExecResult) => r.exit_code },
  { title: 'stdout', key: 'stdout', ellipsis: { tooltip: true } },
  { title: 'stderr', key: 'stderr', ellipsis: { tooltip: true } },
  { title: '错误', key: 'error', ellipsis: { tooltip: true } },
]

onMounted(async () => {
  await loadHosts()
  // Support prefill from query params (e.g. "send to exec" from AI scan report).
  if (route.query.command) {
    command.value = String(route.query.command)
  }
  if (route.query.shell) {
    shell.value = String(route.query.shell)
  }
  if (route.query.host_id) {
    selectedHostIds.value = [String(route.query.host_id)]
  }
})
</script>

<template>
  <NSpace vertical :size="16">
    <NCard title="推送命令" :bordered="false">
      <NSpace vertical :size="12">
        <NSpace align="center">
          <span>Shell:</span>
          <NSelect v-model:value="shell" :options="shellOptions" style="width: 140px" />
          <NCheckbox v-model:checked="isScript">多行脚本</NCheckbox>
        </NSpace>
        <NInput
          v-model:value="command"
          :type="isScript ? 'textarea' : 'text'"
          :rows="isScript ? 6 : undefined"
          placeholder="输入要执行的命令，如: uptime; df -h; free -m"
        />
        <NSpace>
          <NButton type="primary" :loading="running" @click="run">执行</NButton>
          <span class="muted">已选 {{ selectedHostIds.length }} 台主机</span>
        </NSpace>
      </NSpace>
    </NCard>

    <NCard title="选择目标主机" :bordered="false">
      <NSpin v-if="loading" />
      <NDataTable
        v-else
        :columns="hostColumns"
        :data="hosts.filter(h => h.status === 'online')"
        :row-key="(r: Host) => r.id"
        v-model:checked-row-keys="selectedHostIds"
        :bordered="false"
        size="small"
        :max-height="200"
      />
    </NCard>

    <NCard v-if="results.length > 0" title="执行结果" :bordered="false">
      <NDataTable
        :columns="resultColumns"
        :data="results"
        :bordered="false"
        size="small"
        :max-height="300"
      />
    </NCard>
  </NSpace>
</template>

<style scoped lang="scss">
.muted { color: #9ca3af; font-size: 13px; }
</style>