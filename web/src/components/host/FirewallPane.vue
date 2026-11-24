<script setup lang="ts">
import { h, ref, computed, onMounted } from 'vue'
import {
  NAlert,
  NButton,
  NDataTable,
  NEmpty,
  NIcon,
  NInputNumber,
  NModal,
  NPopconfirm,
  NSelect,
  NSpace,
  NSpin,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import { RefreshOutline } from '@vicons/ionicons5'
import {
  getFirewall,
  firewallAllow,
  firewallDeny,
  firewallDelete,
} from '../../api/hosts'
import type { FirewallRule, FirewallStatus } from './sysinfo-types'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'

const props = defineProps<{ hostId: string }>()
const message = useMessage()
const auth = useAuthStore()

// 防火墙改主机状态，与服务端 hostWrite 路由的角色门禁一致。
const canOperate = computed(() => auth.role === 'admin' || auth.role === 'operator')

const loading = ref(false)
const status = ref<FirewallStatus | null>(null)
const loadError = ref('')
const needsUpgrade = ref(false)
const updateTime = ref('')

const port = ref<number | null>(null)
const protocol = ref<'tcp' | 'udp'>('tcp')
const busy = ref(false)
// 正在删除的行（端口/协议），用于删除按钮的 loading；直接读 busy 不行，
// columns 是 computed，render 里读到的 busy 不会触发它重新计算。
const deletingKey = ref<string | null>(null)
// 22 是 SSH 端口：点放行/拒绝时先弹二次确认，确认后才执行。
const pending22Action = ref<'allow' | 'deny' | null>(null)

const rules = computed(() => status.value?.rules || [])
const rowCount = computed(() => rules.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount })

const backendLabel = computed(() => {
  const map: Record<string, string> = {
    ufw: 'UFW',
    firewalld: 'Firewalld',
    iptables: 'iptables',
    none: '未检测到',
  }
  return map[status.value?.backend || 'none'] || status.value?.backend
})

async function load() {
  loading.value = true
  loadError.value = ''
  needsUpgrade.value = false
  try {
    status.value = await getFirewall(props.hostId)
    resetPage()
    updateTime.value = fmtDateTime(new Date())
  } catch (e: any) {
    const text = e.message || '加载防火墙状态失败'
    // 老 Agent 不认 firewall kind：服务端透传 agent 的 unknown sysinfo kind。
    needsUpgrade.value = text.includes('unknown sysinfo kind')
    loadError.value = text
    status.value = null
  } finally {
    loading.value = false
  }
}

function checkPort(): number | null {
  const p = port.value
  if (p === null || !Number.isFinite(p) || p < 1 || p > 65535) {
    message.error('请输入 1~65535 之间的端口')
    return null
  }
  // 9090 是 Agent 回连端口：前后端双重硬拒，前端直接拦截不发请求。
  if (p === 9090) {
    message.error('9090 为 Agent 回连端口，禁止操作')
    return null
  }
  return p
}

async function runChange(action: 'allow' | 'deny') {
  const p = checkPort()
  if (p === null) return
  if (p === 22) {
    pending22Action.value = action
    return
  }
  await doChange(action, p)
}

async function doChange(action: 'allow' | 'deny', p: number) {
  busy.value = true
  try {
    const res = await (action === 'allow'
      ? firewallAllow(props.hostId, p, protocol.value)
      : firewallDeny(props.hostId, p, protocol.value))
    message.success(
      res.already
        ? `端口 ${p}/${protocol.value} 已是${action === 'allow' ? '放行' : '拒绝'}状态`
        : `已${action === 'allow' ? '放行' : '拒绝'}端口 ${p}/${protocol.value}`,
    )
    await load()
  } catch (e: any) {
    message.error(e.message || '操作失败')
  } finally {
    busy.value = false
    pending22Action.value = null
  }
}

async function doDelete(row: FirewallRule) {
  busy.value = true
  deletingKey.value = `${row.port}/${row.protocol}`
  try {
    await firewallDelete(props.hostId, row.port, row.protocol)
    message.success(`已删除端口 ${row.port}/${row.protocol} 的规则`)
    await load()
  } catch (e: any) {
    message.error(e.message || '操作失败')
  } finally {
    busy.value = false
    deletingKey.value = null
  }
}

// 规则列表是不定长数据（AGENTS.md 8.2）：分页后表头与分页条固定。
const columns = computed<DataTableColumns<FirewallRule>>(() => {
  // 在 computed 体内先读 deletingKey，让它成为依赖，删除时表格才会重渲染。
  const dk = deletingKey.value
  const cols: DataTableColumns<FirewallRule> = [
    { title: '端口', key: 'port', width: 110 },
    {
      title: '协议',
      key: 'protocol',
      width: 90,
      render: (row) => (row.protocol || '').toUpperCase(),
    },
    {
      title: '动作',
      key: 'action',
      width: 110,
      render: (row) =>
        h(
          NTag,
          { size: 'small', bordered: false, type: row.action === 'allow' ? 'success' : 'error' },
          { default: () => (row.action === 'allow' ? '放行' : '拒绝') },
        ),
    },
  ]
  if (canOperate.value) {
    cols.push({
      title: '操作',
      key: '__delete',
      width: 110,
      fixed: 'right',
      render: (row) =>
        h(
          NPopconfirm,
          {
            onPositiveClick: () => doDelete(row),
            positiveText: '删除规则',
            negativeText: '取消',
          },
          {
            trigger: () =>
              h(
                NButton,
                { size: 'tiny', quaternary: true, type: 'error', loading: dk === `${row.port}/${row.protocol}` },
                { default: () => '删除' },
              ),
            default: () => `确认删除端口 ${row.port}/${row.protocol} 的${row.action === 'allow' ? '放行' : '拒绝'}规则？`,
          },
        ),
    })
  }
  return cols
})

onMounted(load)
</script>

<template>
  <div class="firewall-pane">
    <div class="firewall-header">
      <div class="update-time-box">
        <span v-if="updateTime">数据更新于 {{ updateTime }}</span>
        <NButton quaternary size="tiny" :loading="loading" @click="load">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
        </NButton>
      </div>
    </div>

    <NSpin v-if="loading" class="spin-box" />
    <template v-else-if="needsUpgrade">
      <NAlert type="warning" title="需升级 Agent">
        当前 Agent 版本过旧，不支持防火墙端口控制，请先升级 Agent 再使用。
      </NAlert>
    </template>
    <template v-else-if="loadError">
      <NAlert type="error" title="加载失败">{{ loadError }}</NAlert>
    </template>
    <template v-else-if="status">
      <div class="firewall-body">
        <NSpace align="center" :size="8">
          <span class="backend-label">后端：{{ backendLabel }}</span>
          <NTag size="small" :bordered="false" :type="status.enabled ? 'success' : 'warning'">
            {{ status.enabled ? '已启用' : '未启用' }}
          </NTag>
          <span v-if="!status.supported" class="muted-cell">仅支持 Linux 主机</span>
        </NSpace>

        <NAlert v-if="status.hint" type="warning" :show-icon="false" style="margin-top: 12px">
          {{ status.hint }}
        </NAlert>
        <NAlert v-if="!status.supported" type="info" :show-icon="false" style="margin-top: 12px">
          v1 仅支持 Linux 主机（按 ufw → firewalld → iptables 顺序探测后端）。
        </NAlert>

        <template v-if="status.supported">
          <NSpace v-if="canOperate" align="center" :size="8" class="add-row">
            <NInputNumber
              v-model:value="port"
              :min="1"
              :max="65535"
              placeholder="端口，如 8080"
              :disabled="busy || !status.enabled"
              style="width: 180px"
            />
            <NSelect
              v-model:value="protocol"
              :options="[
                { label: 'TCP', value: 'tcp' },
                { label: 'UDP', value: 'udp' },
              ]"
              :disabled="busy || !status.enabled"
              style="width: 110px"
            />
            <NButton type="primary" :loading="busy" :disabled="!status.enabled" @click="runChange('allow')">
              放行
            </NButton>
            <NButton :loading="busy" :disabled="!status.enabled" @click="runChange('deny')">
              拒绝
            </NButton>
            <span class="muted-cell hint-text">
              9090 为 Agent 回连端口，禁止操作；22 为 SSH 端口，操作前需二次确认。
            </span>
          </NSpace>
          <NAlert v-else type="info" :show-icon="false" style="margin-top: 12px">
            只读角色不可操作防火墙。
          </NAlert>

          <NDataTable
            v-if="rules.length > 0"
            flex-height
            :columns="columns"
            :data="rules"
            :pagination="pagination"
            :bordered="false"
            size="small"
            :scroll-x="600"
            style="margin-top: 12px"
          />
          <NEmpty v-else description="暂无防火墙规则" style="margin-top: 24px" />
        </template>
      </div>
    </template>

    <NModal
      :show="pending22Action !== null"
      preset="dialog"
      title="确认操作 SSH 端口"
      positive-text="确认执行"
      negative-text="取消"
      @positive-click="pending22Action !== null && port !== null && doChange(pending22Action, port)"
      @update:show="(v: boolean) => { if (!v) pending22Action = null }"
    >
      端口 22 是 SSH 端口，{{ pending22Action === 'allow' ? '放行' : '拒绝' }}它可能影响远程登录，确认继续吗？
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.firewall-pane {
  display: flex;
  flex-direction: column;
  /* 面板撑满 .sub-view-pane，让表格按剩余高度计算滚动区（AGENTS.md 8.2） */
  height: 100%;
  min-height: 0;

  .firewall-header {
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

  .firewall-body {
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

  .backend-label {
    font-size: 13px;
    font-weight: 600;
  }

  .muted-cell {
    font-size: 12px;
    color: var(--text-secondary);
  }

  .hint-text {
    max-width: 420px;
  }

  .add-row {
    flex-wrap: wrap;
    margin-top: 12px;
  }

  .spin-box {
    display: flex;
    justify-content: center;
    padding: 60px 0;
  }
}
</style>
