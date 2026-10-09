<script setup lang="ts">
import { h, ref, watch, onMounted, computed } from 'vue'
import {
  NTabs,
  NTabPane,
  NDataTable,
  NSpin,
  NButton,
  NIcon,
  NEmpty,
  NTag,
  NPopconfirm,
  NSpace,
  NCheckbox,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import { RefreshOutline, StopCircleOutline } from '@vicons/ionicons5'
import { getSysInfo, killProcess } from '../../api/hosts'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'

const props = defineProps<{ hostId: string }>()
const message = useMessage()
const auth = useAuthStore()
const loading = ref(false)
const activeTab = ref('process')
const updateTime = ref('')
const rows = ref<Record<string, any>[]>([])
const rawText = ref('')

// 进程/端口清单动辄数百上千行（AGENTS.md 8.2），分页后表头与分页条固定，
// 只有数据区滚动。
const rowCount = computed(() => rows.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount })

// Only admins and operators may terminate processes; viewers see no action.
const canKill = computed(() => auth.role === 'admin' || auth.role === 'operator')
// force selects SIGKILL over SIGTERM, reset each time a confirm popover opens.
const forceKill = ref(false)
const killingPid = ref<number | null>(null)

async function doKill(row: Record<string, any>) {
  const pid = Number(row.pid)
  if (!Number.isFinite(pid) || pid <= 0) {
    message.error('该行没有可用的 PID')
    return
  }
  killingPid.value = pid
  try {
    const res = await killProcess(props.hostId, pid, forceKill.value, String(row.name || ''))
    message.success(`已结束进程 ${res.name || row.name || ''} (PID ${pid})`)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '结束进程失败')
  } finally {
    killingPid.value = null
    forceKill.value = false
  }
}

async function refresh() {
  loading.value = true
  rows.value = []
  rawText.value = ''
  try {
    const data = await getSysInfo(props.hostId, activeTab.value)
    // The agent may return either a structured { entries: [...] } object
    // (login/user) or a plain array (process/port). Normalise.
    let list: any[] = []
    if (Array.isArray(data)) {
      list = data
    } else if (data && typeof data === 'object') {
      const wrapped = (data as any).entries || (data as any).items || (data as any).data || (data as any).list
      if (Array.isArray(wrapped)) list = wrapped
      rawText.value = (data as any).raw || ''
    }
    rows.value = list
    resetPage()
    updateTime.value = fmtDateTime(new Date())
  } catch (e: any) {
    rows.value = []
  } finally {
    loading.value = false
  }
}

// ---- Login history columns ----------------------------------------------
const loginColumns = computed<DataTableColumns<Record<string, any>>>(() => {
  const detailTag = (_row: Record<string, any>, _idx: number) => {
    const d = (_row as any)?.detail
    if (!d) return h('span', { class: 'muted-cell' }, '—')
    const type: 'success' | 'warning' | 'default' =
      d === 'still logged in' ? 'success' : d === 'still running' ? 'info' as any : 'default'
    return h(NTag, { size: 'small', bordered: false, type: type as any }, { default: () => d })
  }
  const typeLabel = (row: Record<string, any>) => {
    const t = (row as any)?.type
    const map: Record<string, { text: string; color: string }> = {
      session: { text: '用户登录', color: 'info' },
      reboot: { text: '重启', color: 'warning' },
      shutdown: { text: '关机', color: 'error' },
      wtmp: { text: '日志起点', color: 'default' },
    }
    const e = map[t] || { text: t || '未知', color: 'default' }
    return h(NTag, { size: 'small', bordered: false, type: e.color as any }, { default: () => e.text })
  }
  return [
    { title: '类型', key: 'type', width: 100, render: typeLabel },
    { title: '用户', key: 'user', width: 120 },
    { title: '终端', key: 'tty', width: 90 },
    { title: '来源', key: 'from', minWidth: 140, ellipsis: { tooltip: true } },
    { title: '开始时间', key: 'started', minWidth: 180 },
    { title: '时长', key: 'duration', width: 100 },
    { title: '状态', key: 'detail', width: 130, render: detailTag },
    { title: '内核', key: 'kernel', width: 160 },
  ]
})

// ---- User account columns ------------------------------------------------
const userColumns = computed<DataTableColumns<Record<string, any>>>(() => {
  const canLoginTag = (row: Record<string, any>) => {
    const ok = (row as any)?.login_ok
    return h(NTag,
      { size: 'small', bordered: false, type: ok ? 'success' : 'default' },
      { default: () => (ok ? '可登录' : '禁止登录') })
  }
  const accountTypeTag = (row: Record<string, any>) => {
    const sys = (row as any)?.is_system
    return h(NTag,
      { size: 'small', bordered: false, type: sys ? 'warning' : 'info' },
      { default: () => (sys ? '系统账号' : '普通用户') })
  }
  return [
    { title: '用户名', key: 'name', width: 140 },
    { title: 'UID', key: 'uid', width: 80 },
    { title: 'GID', key: 'gid', width: 80 },
    { title: '账号类型', key: 'is_system', width: 110, render: accountTypeTag },
    { title: '登录权限', key: 'login_ok', width: 110, render: canLoginTag },
    { title: '家目录', key: 'home', minWidth: 180, ellipsis: { tooltip: true } },
    { title: 'Shell', key: 'shell', minWidth: 160, ellipsis: { tooltip: true } },
  ]
})

// ---- Generic columns for other kinds (process / port) --------------------
const genericColumns = computed<DataTableColumns<Record<string, any>>>(() => {
  const first = rows.value[0]
  if (!first) return [] as DataTableColumns<Record<string, any>>
  const titleMap: Record<string, Record<string, string>> = {
    process: { name: '进程名', pid: 'PID', user: '用户', uid: 'UID', start_time: '启动时间', cpu: 'CPU(%)', mem: '内存', cmd: '命令参数', command: '命令' },
    port: { proto: '协议', addr: '监听地址', laddr: '本地地址', process: '进程名', pid: 'PID', state: '状态' },
  }
  const titles = titleMap[activeTab.value] || {}
  const cols = Object.keys(first).map((k) => ({
    title: titles[k] || k,
    key: k,
    ellipsis: { tooltip: true },
  })) as DataTableColumns<Record<string, any>>
  if (activeTab.value === 'process' && canKill.value) {
    cols.push({
      title: '操作',
      key: '__kill',
      width: 110,
      fixed: 'right',
      render: (row) => h(
        NPopconfirm,
        {
          onPositiveClick: () => doKill(row),
          positiveText: '结束进程',
          negativeText: '取消',
          onUpdateShow: (show: boolean) => { if (show) forceKill.value = false },
        },
        {
          trigger: () => h(
            NButton,
            {
              size: 'tiny',
              quaternary: true,
              type: 'error',
              loading: killingPid.value === Number(row.pid),
            },
            {
              icon: () => h(NIcon, { component: StopCircleOutline }),
              default: () => '结束',
            },
          ),
          default: () => h(NSpace, { vertical: true, size: 6 }, {
            default: () => [
              h('div', null, `确认结束进程 ${row.name || ''} (PID ${row.pid})？`),
              h(
                NCheckbox,
                {
                  checked: forceKill.value,
                  'onUpdate:checked': (v: boolean) => { forceKill.value = v },
                },
                { default: () => '强制结束 (SIGKILL)' },
              ),
            ],
          }),
        },
      ),
    })
  }
  return cols
})

const columns = computed<DataTableColumns<Record<string, any>>>(() => {
  if (activeTab.value === 'login') return loginColumns.value
  if (activeTab.value === 'user') return userColumns.value
  return genericColumns.value
})

const isStructured = computed(() => activeTab.value === 'login' || activeTab.value === 'user')

function onTabWheel(e: WheelEvent) {
  const el = e.currentTarget as HTMLElement
  if (!el) return
  if (Math.abs(e.deltaY) > Math.abs(e.deltaX)) {
    el.scrollLeft += e.deltaY * 0.8
    e.preventDefault()
  }
}

watch(activeTab, refresh)
onMounted(refresh)
</script>

<template>
  <div class="sysinfo-pane">
    <div class="sysinfo-header">
      <div class="tabs-scroller" @wheel.prevent="onTabWheel">
        <NTabs v-model:value="activeTab" type="line" size="small">
          <NTabPane name="process" tab="进程清单" />
          <NTabPane name="port" tab="网络端口" />
          <NTabPane name="user" tab="系统账号" />
          <NTabPane name="login" tab="登录历史" />
        </NTabs>
      </div>

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
        v-else-if="rows.length > 0 || isStructured"
        flex-height
        :columns="columns"
        :data="rows"
        :pagination="pagination"
        :bordered="false"
        size="small"
        :scroll-x="800"
      >
        <template #empty>
          <NEmpty description="暂无数据，或主机离线 / Agent 未上报该类型信息" />
        </template>
      </NDataTable>
      <NEmpty
        v-else
        :description="rawText ? '当前主机返回了原始文本（Agent 版本过旧）' : '暂无数据，或主机离线 / Agent 未上报该类型信息'"
      >
        <pre v-if="rawText" class="raw-fallback">{{ rawText }}</pre>
      </NEmpty>
    </div>
  </div>
</template>

<style scoped lang="scss">
.sysinfo-pane {
  display: flex;
  flex-direction: column;
  /* 面板撑满 .sub-view-pane，让表格按剩余高度计算滚动区（AGENTS.md 8.2） */
  height: 100%;
  min-height: 0;

  .sysinfo-header {
    position: sticky;
    /* 抵消 .sub-view-pane 的内边距，使吸顶条能贴住面板上沿 */
    top: calc(-1 * var(--card-padding));
    z-index: 20;
    background: var(--bg-card);
    margin: calc(-1 * var(--card-padding)) calc(-1 * var(--card-padding)) 12px;
    padding: 10px var(--card-padding) 0;
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: 16px;
    border-bottom: 1px solid var(--border-color);

    .tabs-scroller {
      flex: 1;
      min-width: 0;
      overflow-x: auto;
      overflow-y: hidden;
      -webkit-overflow-scrolling: touch;
      scrollbar-width: none;

      &::-webkit-scrollbar {
        display: none;
      }

      :deep(.n-tabs) {
        min-width: max-content;
      }

      :deep(.n-tabs-nav-scroll-content) {
        border-bottom: none !important;
      }

      :deep(.n-tabs-rail) {
        border-bottom: none !important;
      }

      :deep(.n-tabs-tab) {
        padding: 6px 12px 10px 12px;
        white-space: nowrap;
        font-size: 13px;
      }

      :deep(.n-tabs-bar) {
        bottom: 0 !important;
      }
    }

    .update-time-box {
      font-size: 12px;
      color: var(--text-secondary);
      display: flex;
      align-items: center;
      gap: 6px;
      flex-shrink: 0;
      padding-bottom: 8px;
      user-select: none;
    }
  }

  .sysinfo-body {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;

    :deep(.n-data-table) {
      flex: 1;
      min-height: 0;
    }
  }

  .spin-box {
    display: flex;
    justify-content: center;
    padding: 60px 0;
  }

  .raw-fallback {
    max-height: 360px;
    overflow: auto;
    background: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 4px;
    padding: 10px 12px;
    font-family: 'SFMono-Regular', Consolas, monospace;
    font-size: 11px;
    line-height: 1.4;
    white-space: pre;
    text-align: left;
    margin: 0;
  }
}

/* 窄屏：tab 区域占满，更新时间换行 */
@media (max-width: 768px) {
  .sysinfo-header {
    flex-wrap: wrap;

    .tabs-scroller {
      order: 1;
      flex: 1 1 100%;
    }

    .update-time-box {
      order: 2;
      flex: 1 1 100%;
      justify-content: flex-end;
    }
  }
}
</style>