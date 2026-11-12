<script setup lang="ts">
import { h, ref, watch, onMounted, computed } from 'vue'
import {
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

// 子页签状态由 HostDetail 持有（DESIGN.md §9.5：Tab 在卡片外，由父级渲染），
// 本组件只按传入的页签取数。
const props = defineProps<{ hostId: string; activeTab: string }>()
const emit = defineEmits<{ (e: 'update:activeTab', value: string): void }>()
const message = useMessage()
const auth = useAuthStore()
const loading = ref(false)
const activeTab = computed({
  get: () => props.activeTab,
  set: (v: string) => emit('update:activeTab', v),
})
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
    const map: Record<string, { text: string; type: 'success' | 'info' | 'warning' | 'error' | 'default' }> = {
      'still logged in': { text: '仍在线', type: 'success' },
      'still running': { text: '运行中', type: 'info' },
      'gone - no logout': { text: '未正常退出', type: 'warning' },
      crash: { text: '崩溃', type: 'error' },
      down: { text: '已停止', type: 'default' },
    }
    const e = map[d] || { text: d, type: 'default' as const }
    return h(NTag, { size: 'small', bordered: false, type: e.type }, { default: () => e.text })
  }
  // `last -F` 把开始时间、结束时间与时长拼进同一字段
  // （如 "Aug 20 12:21 - 14:57 (02:35)"）：只取 " - " 之前的开始时间，
  // 再用 fmtDateTime 按面板时区格式化；时长另有 duration 列，不重复展示。
  const startedLabel = (row: Record<string, any>) => {
    const raw = String((row as any)?.started || '').trim()
    if (!raw) return '—'
    const start = raw.split(' - ')[0].replace(/\s*\(\d{2}:\d{2}\)\s*$/, '').trim()
    // 无 -F 的 `last` 输出不带年份（如 "Aug 20 12:21"，指当年）；V8 解析
    // 缺少年份的英文时间会默认成 2001 年，必须先补当年年份。
    const withYear = /\b\d{4}\b/.test(start) ? start : `${start} ${new Date().getFullYear()}`
    const d = new Date(withYear)
    if (Number.isNaN(d.getTime())) return start
    return fmtDateTime(d)
  }
  const typeLabel = (row: Record<string, any>) => {
    const t = (row as any)?.type
    const map: Record<string, { text: string; color: string }> = {
      session: { text: '用户登录', color: 'info' },
      reboot: { text: '重启', color: 'warning' },
      shutdown: { text: '关机', color: 'error' },
      crash: { text: '崩溃', color: 'error' },
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
    { title: '开始时间', key: 'started', minWidth: 180, ellipsis: { tooltip: true }, render: startedLabel },
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
    port: { proto: '协议', address: '地址', port: '端口', process: '进程名', pid: 'PID', state: '状态' },
  }
  const titles = titleMap[activeTab.value] || {}
  const cols = Object.keys(first).map((k) => {
    const col: Record<string, any> = {
      title: titles[k] || k,
      key: k,
      ellipsis: { tooltip: true },
    }
    // gopsutil 返回的 CPU/内存是未格式化的百分比浮点
    // （如 0.008562769145701074），保留 2 位小数并补 % 单位。
    if (activeTab.value === 'process' && (k === 'cpu' || k === 'mem')) {
      col.width = 100
      col.render = (row: Record<string, any>) => {
        const v = Number(row[k])
        return Number.isFinite(v) ? `${v.toFixed(2)}%` : '—'
      }
    }
    return col
  }) as DataTableColumns<Record<string, any>>
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

watch(activeTab, refresh)
onMounted(refresh)
</script>

<template>
  <div class="sysinfo-pane">
    <div class="sysinfo-header">
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

  /* Tab 已上提到 HostDetail 的卡片外（DESIGN.md §9.5），这里只剩右对齐的
     更新时间与刷新按钮。 */
  .sysinfo-header {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    flex-shrink: 0;
    margin-bottom: 12px;

    .update-time-box {
      font-size: 12px;
      color: var(--text-secondary);
      display: flex;
      align-items: center;
      gap: 6px;
      flex-shrink: 0;
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
</style>