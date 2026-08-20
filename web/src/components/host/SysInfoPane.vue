<script setup lang="ts">
import { ref, watch, onMounted, computed } from 'vue'
import {
  NTabs,
  NTabPane,
  NDataTable,
  NSpin,
  NButton,
  NIcon,
  NEmpty,
  type DataTableColumns,
} from 'naive-ui'
import { RefreshOutline } from '@vicons/ionicons5'
import { getSysInfo } from '../../api/hosts'

const props = defineProps<{ hostId: string }>()
const loading = ref(false)
const activeTab = ref('process')
const updateTime = ref('')
const rows = ref<Record<string, any>[]>([])

async function refresh() {
  loading.value = true
  rows.value = []
  try {
    const data = await getSysInfo(props.hostId, activeTab.value)
    // getSysInfo returns the raw JSON payload from the agent. It may be an
    // array of records or an object wrapping a list — normalise to an array.
    let list: any[] = []
    if (Array.isArray(data)) {
      list = data
    } else if (data && typeof data === 'object') {
      // Some agents wrap results in { items: [...] } or { data: [...] }.
      const wrapped = (data as any).items || (data as any).data || (data as any).list
      if (Array.isArray(wrapped)) list = wrapped
      else list = [data]
    }
    rows.value = list
    const now = new Date()
    updateTime.value = `${now.getFullYear()}.${String(now.getMonth() + 1).padStart(2, '0')}.${String(now.getDate()).padStart(2, '0')} ${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}:${String(now.getSeconds()).padStart(2, '0')}`
  } catch (e: any) {
    // Keep the error message visible via the empty-state; do not inject mock data.
    rows.value = []
  } finally {
    loading.value = false
  }
}

// Column definitions vary per kind. The agent returns arbitrary JSON keys, so
// we derive columns from the first row of data when available, with sensible
// per-kind overrides.
const columns = computed<DataTableColumns<Record<string, any>>>(() => {
  const first = rows.value[0]
  if (!first) return []
  const keys = Object.keys(first)

  // Provide nicer titles for well-known keys per kind.
  const titleMap: Record<string, Record<string, string>> = {
    process: { name: '进程名', pid: 'PID', user: '用户', uid: 'UID', start_time: '启动时间', cpu: 'CPU(%)', mem: '内存', cmd: '命令参数', command: '命令' },
    port: { proto: '协议', addr: '监听地址', laddr: '本地地址', process: '进程名', pid: 'PID', state: '状态' },
    user: { name: '用户名', user: '用户名', uid: 'UID', gid: 'GID', home: '家目录', shell: 'Shell', status: '状态' },
    login: { user: '用户名', type: '类型', host: '来源', time: '登录时间', status: '状态', tty: '终端' },
  }
  const titles = titleMap[activeTab.value] || {}

  return keys.map((k) => ({
    title: titles[k] || k,
    key: k,
    ellipsis: { tooltip: true },
  }))
})

watch(activeTab, refresh)
onMounted(refresh)
</script>

<template>
  <div class="sysinfo-pane">
    <div class="sysinfo-header">
      <NTabs v-model:value="activeTab" type="line" size="small">
        <NTabPane name="process" tab="进程清单" />
        <NTabPane name="port" tab="网络端口" />
        <NTabPane name="user" tab="系统账号" />
        <NTabPane name="login" tab="登录历史" />
      </NTabs>

      <div class="update-time-box">
        <span v-if="updateTime">数据更新于 {{ updateTime }}</span>
        <NButton quaternary size="tiny" :loading="loading" @click="refresh">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
        </NButton>
      </div>
    </div>

    <div class="sysinfo-body">
      <NSpin v-if="loading" class="spin-box" />
      <NDataTable
        v-else
        :columns="columns"
        :data="rows"
        :bordered="false"
        size="small"
        :max-height="520"
      >
        <template #empty>
          <NEmpty description="暂无数据，或主机离线 / Agent 未上报该类型信息" />
        </template>
      </NDataTable>
    </div>
  </div>
</template>

<style scoped lang="scss">
.sysinfo-pane {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .sysinfo-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    border-bottom: 1px solid var(--border-color);
    padding-bottom: 4px;

    .update-time-box {
      font-size: 12px;
      color: var(--text-secondary);
      display: flex;
      align-items: center;
      gap: 6px;
    }
  }

  .spin-box {
    display: flex;
    justify-content: center;
    padding: 60px 0;
  }
}
</style>