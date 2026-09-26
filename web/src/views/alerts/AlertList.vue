<script setup lang="ts">
import { onMounted, ref, computed, watch, h } from 'vue'
import { useRouter } from 'vue-router'
import {
  NCard,
  NButton,
  NSpace,
  NTable,
  NTag,
  NModal,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NSwitch,
  NInputNumber,
  NPopconfirm,
  NTabs,
  NTabPane,
  NEmpty,
  useMessage,
  NIcon,
  NBadge,
  NRadioGroup,
  NRadioButton,
  NTooltip,
  NDataTable,
  type DataTableColumns,
} from 'naive-ui'
import {
  AddCircleOutline,
  RefreshOutline,
  TrashOutline,
  NotificationsOutline,
  WarningOutline,
  AlertCircleOutline,
  InformationCircleOutline,
  CheckmarkDoneOutline,
  ServerOutline,
  SparklesOutline,
  PaperPlaneOutline,
  CheckmarkCircleOutline,
  CloseCircleOutline,
} from '@vicons/ionicons5'
import type { AlertRule, RuleType, Severity, WebhookConfig, AlertEvent, WebhookTestResult } from '../../api/alerts'
import {
  listRules,
  createRule,
  updateRule,
  deleteRule,
  getWebhook,
  setWebhook,
  testWebhook,
} from '../../api/alerts'
import { useWorkspaceStore } from '../../stores/workspace'
import { useNotificationStore } from '../../stores/notifications'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'AlertList' })

const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()
const notifStore = useNotificationStore()

const rules = ref<AlertRule[]>([])
const loading = ref(false)
const activeTab = ref('messages')

// Filter state for messages tab
const statusFilter = ref<'all' | 'unread' | 'resolved'>('all')
const severityFilter = ref<string>('all')
const searchKeyword = ref('')

// Client-side paging for the message list. Events are held in the notification
// store (also used by the header bell), so paging slices that list rather than
// refetching — the store already caps history server-side.
const page = ref(1)
const pageSize = ref(20)

// Rule modal
const showRuleModal = ref(false)
const editingRule = ref<AlertRule | null>(null)
const ruleForm = ref({
  name: '',
  type: 'offline' as RuleType,
  severity: 'warning' as Severity,
  threshold: 90,
  duration: 0,
  metric: 'cpu',
  host_filter: '',
  group_filter: '',
  enabled: true,
})

// Webhook
const webhook = ref<WebhookConfig>({ url: '', secret: '', enabled: false })

const ruleTypeOptions = [
  { label: '主机上线', value: 'online' },
  { label: '主机离线', value: 'offline' },
  { label: 'CPU 使用率高', value: 'cpu_high' },
  { label: '内存使用率高', value: 'mem_high' },
  { label: '磁盘使用率高', value: 'disk_high' },
  { label: 'AI 异常检测', value: 'anomaly' },
]

const anomalyMetricOptions = [
  { label: 'CPU 使用率', value: 'cpu' },
  { label: '内存使用率', value: 'mem' },
  { label: '网络接收速率', value: 'net_rx' },
  { label: '网络发送速率', value: 'net_tx' },
  { label: '磁盘读速率', value: 'disk_read' },
  { label: '磁盘写速率', value: 'disk_write' },
]

const severityOptions = [
  { label: '全部级别', value: 'all' },
  { label: '严重 (Critical)', value: 'critical' },
  { label: '警告 (Warning)', value: 'warning' },
  { label: '信息 (Info)', value: 'info' },
]

const filteredEvents = computed(() => {
  return notifStore.events.filter((e) => {
    // Status filter
    if (statusFilter.value === 'unread' && e.resolved) return false
    if (statusFilter.value === 'resolved' && !e.resolved) return false

    // Severity filter
    if (severityFilter.value !== 'all' && e.severity !== severityFilter.value) return false

    // Keyword filter
    if (searchKeyword.value.trim()) {
      const q = searchKeyword.value.trim().toLowerCase()
      const hostMatch = (e.hostname || '').toLowerCase().includes(q)
      const msgMatch = (e.message || '').toLowerCase().includes(q)
      const ruleMatch = (e.rule_name || '').toLowerCase().includes(q)
      if (!hostMatch && !msgMatch && !ruleMatch) return false
    }

    return true
  })
})

// Changing a filter shrinks the result set; staying on a now-empty page would
// look like "no messages", so snap back to the first page.
watch([statusFilter, severityFilter, searchKeyword], () => {
  page.value = 1
})

// Acking or deleting can empty the current page (e.g. the last row on page 3).
// Clamp so the table never renders blank while records still exist.
watch(filteredEvents, (list) => {
  const maxPage = Math.max(1, Math.ceil(list.length / pageSize.value))
  if (page.value > maxPage) page.value = maxPage
})

function handlePageSizeChange(ps: number) {
  pageSize.value = ps
  page.value = 1
}

const eventColumns = computed<DataTableColumns<AlertEvent>>(() => [
  {
    title: '级别',
    key: 'severity',
    width: 80,
    render: (e) =>
      h(NTag, { type: sevTagType(e.severity), size: 'small', round: true }, {
        icon: () =>
          h(NIcon, null, {
            default: () =>
              h(
                e.severity === 'critical'
                  ? AlertCircleOutline
                  : e.severity === 'warning'
                    ? WarningOutline
                    : InformationCircleOutline,
              ),
          }),
        default: () => sevLabel(e.severity),
      }),
  },
  {
    title: '触发规则',
    key: 'rule_name',
    width: 110,
    ellipsis: { tooltip: true },
    render: (e) => h('span', { class: 'rule-name-tag' }, cleanRuleName(e.rule_name)),
  },
  {
    title: '关联主机',
    key: 'hostname',
    width: 130,
    ellipsis: { tooltip: true },
    render: (e) =>
      h('div', {
        class: ['host-cell', e.host_id ? 'clickable' : ''],
        onClick: () => goHostDetail(e.host_id, e.hostname),
      }, [
        h(NIcon, { size: 14 }, { default: () => h(ServerOutline) }),
        h('span', { class: 'host-name-text' }, e.hostname || e.host_id || '-'),
      ]),
  },
  {
    title: '消息详情',
    key: 'message',
    minWidth: 160,
    ellipsis: { tooltip: true },
    render: (e) => e.message,
  },
  {
    title: 'AI 解读',
    key: 'ai_interpretation',
    width: 150,
    render: (e) =>
      e.ai_interpretation
        ? h(NTooltip, { placement: 'top', style: { maxWidth: '400px' } }, {
            trigger: () =>
              h('span', { class: 'ai-interpret-summary' }, [
                h(NIcon, { size: 12, color: '#6366f1' }, { default: () => h(SparklesOutline) }),
                h('span', { class: 'ai-interpret-text' }, e.ai_interpretation),
              ]),
            default: () => e.ai_interpretation,
          })
        : h('span', { class: 'muted-text' }, '-'),
  },
  {
    title: '发生时间',
    key: 'fired_at',
    width: 150,
    render: (e) => h('span', { class: 'time-cell' }, fmtTime(e.fired_at)),
  },
  {
    title: '状态',
    key: 'resolved',
    width: 80,
    render: (e) =>
      h(NTag, { type: e.resolved ? 'default' : 'error', size: 'small', round: true }, {
        default: () => (e.resolved ? '已恢复' : '告警中'),
      }),
  },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    render: (e) =>
      h(NSpace, { size: 6, wrapItem: false }, {
        default: () => [
          !e.resolved
            ? h(NButton, { size: 'tiny', type: 'success', secondary: true, onClick: () => doAckEvent(e.id) }, { default: () => '解决' })
            : null,
          e.host_id
            ? h(NButton, { size: 'tiny', quaternary: true, type: 'primary', onClick: () => goHostDetail(e.host_id, e.hostname) }, { default: () => '查看' })
            : null,
          h(NPopconfirm, { onPositiveClick: () => doDeleteEvent(e.id) }, {
            trigger: () => h(NButton, { size: 'tiny', quaternary: true, type: 'error' }, {
              icon: () => h(NIcon, { component: TrashOutline }),
            }),
            default: () => '删除此条消息？',
          }),
        ],
      }),
  },
])

// naive-ui 内置分页负责切片，页码状态仍由本组件持有（筛选变化要能重置）。
const eventPagination = computed(() => ({
  page: page.value,
  pageSize: pageSize.value,
  itemCount: filteredEvents.value.length,
  showSizePicker: true,
  pageSizes: [20, 50, 100],
  onChange: (p: number) => {
    page.value = p
  },
  onUpdatePageSize: handlePageSizeChange,
}))

function sevTagType(s: Severity) {
  if (s === 'critical') return 'error' as const
  if (s === 'warning') return 'warning' as const
  return 'info' as const
}

function sevLabel(s: Severity) {
  if (s === 'critical') return '严重'
  if (s === 'warning') return '警告'
  return '信息'
}

function ruleTypeLabel(t: RuleType) {
  const o = ruleTypeOptions.find((x) => x.value === t)
  return o?.label || t
}

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
}

function goHostDetail(hostId: string, hostname: string) {
  if (!hostId) return
  workspace.openTab({
    key: `/hosts/${hostId}`,
    title: hostname || hostId,
    path: `/hosts/${hostId}`,
    closable: true,
  })
  router.push(`/hosts/${hostId}`)
}

async function loadRules() {
  try {
    rules.value = await listRules()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function loadWebhook() {
  try {
    const loaded = await getWebhook()
    webhook.value = {
      url: loaded.url || '',
      secret: loaded.secret || '',
      enabled: !!loaded.enabled,
    }
  } catch (e: any) {
    message.error(e.message)
  }
}

async function refreshAll() {
  loading.value = true
  await Promise.all([notifStore.fetchEvents(), loadRules(), loadWebhook()])
  loading.value = false
}

function openCreateRule() {
  editingRule.value = null
  ruleForm.value = {
    name: '',
    type: 'offline',
    severity: 'warning',
    threshold: 90,
    duration: 0,
    metric: 'cpu',
    host_filter: '',
    group_filter: '',
    enabled: true,
  }
  showRuleModal.value = true
}

function openEditRule(r: AlertRule) {
  editingRule.value = r
  ruleForm.value = {
    name: r.name,
    type: r.type,
    severity: r.severity,
    threshold: r.threshold,
    duration: r.duration,
    metric: r.metric || 'cpu',
    host_filter: r.host_filter,
    group_filter: r.group_filter,
    enabled: r.enabled,
  }
  showRuleModal.value = true
}

async function saveRule() {
  if (!ruleForm.value.name) {
    message.warning('请输入规则名称')
    return
  }
  try {
    if (editingRule.value) {
      await updateRule(editingRule.value.id, ruleForm.value)
      message.success('规则已更新')
    } else {
      await createRule(ruleForm.value)
      message.success('规则已创建')
    }
    showRuleModal.value = false
    loadRules()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doDeleteRule(id: string) {
  try {
    await deleteRule(id)
    message.success('规则已删除')
    loadRules()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function toggleRule(r: AlertRule, enabled: boolean) {
  try {
    await updateRule(r.id, { ...r, enabled })
    loadRules()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function saveWebhook() {
  try {
    await setWebhook(webhook.value)
    message.success('Webhook 配置已保存')
    await loadWebhook()
  } catch (e: any) {
    message.error(e.message)
  }
}

// Webhook 测试推送状态
const testingWebhook = ref(false)
const webhookTestResult = ref<WebhookTestResult | null>(null)
const showWebhookPreview = ref(false)

const detectedWebhookPlatform = computed(() => {
  const u = (webhook.value.url || '').toLowerCase().trim()
  if (!u) return null
  if (u.includes('dingtalk.com')) {
    return { name: '钉钉自定义机器人', type: 'info' as const, note: '自动按 HmacSHA256 毫秒加签，支持 Markdown 卡片推送' }
  }
  if (u.includes('weixin.qq.com') || u.includes('work.weixin.qq.com')) {
    return { name: '企业微信群机器人', type: 'success' as const, note: '自动封装企业微信标准 Markdown 告警消息' }
  }
  if (u.includes('feishu.cn') || u.includes('larksuite.com')) {
    return { name: '飞书群机器人', type: 'warning' as const, note: '自动适配飞书消息结构体与签名' }
  }
  return { name: '自建系统 / 通用 Webhook', type: 'default' as const, note: '发送原始 JSON 结构，Header 附带 X-Webhook-Secret' }
})

async function doTestWebhook() {
  if (!webhook.value.url?.trim()) {
    message.warning('请先输入 Webhook URL')
    return
  }
  testingWebhook.value = true
  webhookTestResult.value = null
  try {
    const res = await testWebhook(webhook.value)
    webhookTestResult.value = res
    if (res.ok) {
      message.success(`Webhook 推送成功 (耗时 ${res.duration_ms}ms)`)
    } else {
      message.error(res.error || '推送失败')
    }
  } catch (e: any) {
    message.error(e.message || '请求失败')
  } finally {
    testingWebhook.value = false
  }
}

const webhookPreview = computed(() => {
  const platform = detectedWebhookPlatform.value
  const url = webhook.value.url?.trim() || ''
  const secret = webhook.value.secret?.trim() || ''
  const maskedURL = url
    ? url.replace(/(access_token=|key=|token=)[^&]+/gi, '$1••••••••')
    : '等待输入 Webhook URL'
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const payload: Record<string, unknown> = {
    event: {
      id: 'preview-event',
      rule_name: 'Webhook 告警通知',
      severity: 'info',
      hostname: 'watchman-console',
      message: '这是一条告警消息预览，真实发送时会替换为实际事件内容。',
      fired_at: '发送时动态生成',
    },
    timestamp: '发送时动态生成',
  }
  let body: Record<string, unknown> = payload
  let signing = '不使用签名'
  if (platform?.name.includes('钉钉')) {
    signing = secret
      ? '已配置 Secret；发送时生成 timestamp + HmacSHA256 sign，并追加到 URL'
      : '未配置 Secret，不会生成签名'
    body = {
      msgtype: 'markdown',
      markdown: {
        title: '[告警通知] Webhook 告警通知: watchman-console',
        text: '### [告警通知] Webhook 告警通知\\n\\n- **告警级别**: INFO 信息\\n- **关联主机**: `watchman-console`\\n- **详情说明**: 这是一条告警消息预览。',
      },
    }
  } else if (platform?.name.includes('企业微信')) {
    signing = '按企业微信机器人协议发送 Markdown'
    body = {
      msgtype: 'markdown',
      markdown: { content: '### Watchman 告警通知\\n> 告警级别: <font color=\\"comment\\">信息</font>' },
    }
  } else if (platform?.name.includes('飞书')) {
    signing = secret
      ? (secret === '********'
        ? '已配置 Secret；发送时生成 timestamp + HmacSHA256 sign'
        : '发送时生成 timestamp + HmacSHA256 sign')
      : '未配置 Secret，不会生成签名'
    body = {
      msg_type: 'text',
      content: { text: '[告警通知] Watchman 告警通知\\n【规则名称】Webhook 告警通知' },
    }
  } else if (secret && secret !== '********') {
    headers['X-Webhook-Secret'] = '••••••••'
  }
  return {
    platform: platform?.name || '未识别平台',
    url: maskedURL,
    method: 'POST',
    headers,
    signing,
    body: JSON.stringify(body, null, 2),
  }
})

async function doAckEvent(id: string) {
  try {
    await notifStore.markAsRead(id)
    message.success('已标记为已读')
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doAckAllEvents() {
  try {
    await notifStore.markAllAsRead()
    message.success('全部消息已标记为已读')
  } catch (e: any) {
    message.error(e.message)
  }
}

function cleanRuleName(name?: string): string {
  if (!name) return '系统监控'
  if (name.includes('\uFFFD') || name.includes('?')) {
    return '流量监控规则'
  }
  return name
}

async function doDeleteEvent(id: string) {
  try {
    await notifStore.removeEvent(id)
    message.success('消息已删除')
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

async function doClearAllEvents() {
  try {
    await notifStore.clearAll(false)
    message.success('已清空全部消息')
  } catch (e: any) {
    message.error(e.message || '清空失败')
  }
}

onMounted(() => {
  workspace.openTab({
    key: '/alerts',
    title: '消息中心',
    path: '/alerts',
    closable: true,
  })
  refreshAll()
})
</script>

<template>
  <div class="message-center-view">
    <!-- Header -->
    <div class="page-header">
      <div class="header-left">
        <h2 class="page-title">消息与告警中心</h2>
      </div>
      <div class="header-right">
        <NSpace :size="10">
          <NButton
            v-if="activeTab === 'messages' && notifStore.unreadCount > 0"
            secondary
            type="primary"
            size="small"
            @click="doAckAllEvents"
          >
            <template #icon><NIcon :component="CheckmarkDoneOutline" /></template>
            全部标记解决
          </NButton>
          <NPopconfirm
            v-if="activeTab === 'messages' && notifStore.events.length > 0"
            @positive-click="doClearAllEvents"
          >
            <template #trigger>
              <NButton quaternary type="error" size="small">
                <template #icon><NIcon :component="TrashOutline" /></template>
                清空消息
              </NButton>
            </template>
            确认清空全部消息通知记录？
          </NPopconfirm>
          <NButton size="small" @click="refreshAll" :loading="loading || notifStore.loading">
            <template #icon><NIcon :component="RefreshOutline" /></template>
            刷新
          </NButton>
        </NSpace>
      </div>
    </div>

    <!-- Tabs Container -->
    <div class="tabs-container">
      <NTabs v-model:value="activeTab" type="line">
        <!-- 消息列表 / 事件通知 Tab -->
        <NTabPane name="messages">
          <template #tab>
            <NSpace align="center" :size="6">
              <span>消息列表</span>
              <NBadge
                v-if="notifStore.unreadCount > 0"
                :value="notifStore.unreadCount"
                :max="99"
                type="error"
              />
            </NSpace>
          </template>

          <div class="messages-tab-content">
            <!-- Filter Toolbar -->
            <div class="filter-toolbar">
              <div class="filter-left">
                <NRadioGroup v-model:value="statusFilter" size="small">
                  <NRadioButton value="all">全部 ({{ notifStore.events.length }})</NRadioButton>
                  <NRadioButton value="unread">告警中 ({{ notifStore.unreadCount }})</NRadioButton>
                  <NRadioButton value="resolved">已恢复</NRadioButton>
                </NRadioGroup>

                <NSelect
                  v-model:value="severityFilter"
                  :options="severityOptions"
                  size="small"
                  style="width: 140px"
                />

                <NInput
                  v-model:value="searchKeyword"
                  placeholder="搜索主机、消息内容或规则"
                  size="small"
                  clearable
                  style="width: 220px"
                />
              </div>

              <div class="filter-right">
                <span class="result-count">
                  共 {{ filteredEvents.length }} 条记录
                  <template v-if="filteredEvents.length > pageSize">
                    · 第 {{ page }}/{{ Math.ceil(filteredEvents.length / pageSize) }} 页
                  </template>
                </span>
              </div>
            </div>

            <!-- Events List Table：表头与分页固定，仅数据区滚动（AGENTS.md 8.2） -->
            <div class="table-card table-flex-fill">
              <NDataTable
                flex-height
                :columns="eventColumns"
                :data="filteredEvents"
                :pagination="eventPagination"
                :bordered="false"
                size="small"
                :row-key="(e: AlertEvent) => e.id"
                :row-class-name="(e: AlertEvent) => (e.resolved ? '' : 'unread-row')"
                :scroll-x="1000"
              >
                <template #empty>
                  <NEmpty description="暂无符合条件的消息通知" />
                </template>
              </NDataTable>
            </div>
          </div>
        </NTabPane>

        <!-- 告警规则 Tab -->
        <NTabPane name="rules" tab="告警规则配置">
          <div class="rules-tab-content">
            <div class="toolbar-bar">
              <NButton type="primary" size="small" @click="openCreateRule">
                <template #icon><NIcon :component="AddCircleOutline" /></template>
                新增规则
              </NButton>
            </div>

            <div class="table-card">
              <NEmpty v-if="rules.length === 0" description="暂无告警规则，点击上方按钮新增" />
              <!-- 告警规则清单：条数天然有限，不分页（AGENTS.md 8.2 例外） -->
              <NTable v-else :single-line="false" size="small" class="custom-table">
                <thead>
                  <tr>
                    <th>规则名称</th>
                    <th>监控类型</th>
                    <th>告警级别</th>
                    <th>触发阈值</th>
                    <th>主机过滤</th>
                    <th>状态</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="r in rules" :key="r.id">
                    <td class="rule-title-cell" @click="openEditRule(r)">
                      {{ r.name }}
                    </td>
                    <td>{{ ruleTypeLabel(r.type) }}</td>
                    <td>
                      <NTag :type="sevTagType(r.severity)" size="small" round>
                        {{ sevLabel(r.severity) }}
                      </NTag>
                    </td>
                    <td>{{
                      r.type === 'offline' ? '心跳中断'
                      : r.type === 'anomaly' ? `> ${r.threshold}σ`
                      : `> ${r.threshold}%`
                    }}</td>
                    <td>{{ r.host_filter || '全部主机' }}</td>
                    <td>
                      <NSwitch
                        :value="r.enabled"
                        size="small"
                        @update:value="(v: boolean) => toggleRule(r, v)"
                      />
                    </td>
                    <td>
                      <NSpace :size="4">
                        <NButton size="small" quaternary @click="openEditRule(r)">编辑</NButton>
                        <NPopconfirm @positive-click="doDeleteRule(r.id)">
                          <template #trigger>
                            <NButton size="small" quaternary type="error">
                              <template #icon><NIcon :component="TrashOutline" /></template>
                            </NButton>
                          </template>
                          确认删除规则 {{ r.name }}？
                        </NPopconfirm>
                      </NSpace>
                    </td>
                  </tr>
                </tbody>
              </NTable>
            </div>
          </div>
        </NTabPane>

        <!-- Webhook 通知渠道 Tab -->
        <NTabPane name="webhook" tab="通知渠道 (Webhook)">
          <div class="webhook-tab-content">
            <NCard title="Webhook 告警推送" class="webhook-card">
              <p class="webhook-intro">
                配置企业微信、钉钉、飞书或自建系统 Webhook 地址，当告警触发时系统会自动格式化推送消息并发送 HTTP POST 请求。
              </p>
              <NForm label-placement="top" style="margin-top: 16px">
                <NFormItem label="Webhook URL" required>
                  <NInput
                    v-model:value="webhook.url"
                    placeholder="例如：https://oapi.dingtalk.com/robot/send?access_token=..."
                  />
                  <template #feedback v-if="detectedWebhookPlatform">
                    <NSpace align="center" :size="6" style="margin-top: 4px">
                      <NTag size="tiny" :type="detectedWebhookPlatform.type" round>
                        {{ detectedWebhookPlatform.name }}
                      </NTag>
                      <span class="muted-text">{{ detectedWebhookPlatform.note }}</span>
                    </NSpace>
                  </template>
                </NFormItem>
                <NFormItem label="签名密钥 Secret (可选，钉钉自动加签，企微/飞书按需签名，自建系统带入 X-Webhook-Secret 头部)">
                  <NInput
                    v-model:value="webhook.secret"
                    placeholder="请输入密钥，例如钉钉机器人的 SEC 开头加签密钥"
                    type="password"
                    show-password-on="click"
                  />
                </NFormItem>
                <NFormItem label="启用 Webhook 告警自动推送">
                  <NSwitch v-model:value="webhook.enabled" />
                </NFormItem>
              </NForm>

              <NSpace justify="space-between" align="center" style="margin-top: 16px">
                <NSpace align="center" :size="8">
                  <NButton
                    secondary
                    type="info"
                    :loading="testingWebhook"
                    :disabled="!webhook.url"
                    @click="doTestWebhook"
                  >
                    <template #icon><NIcon :component="PaperPlaneOutline" /></template>
                    发送测试消息
                  </NButton>
                  <NButton
                    quaternary
                    size="small"
                    :disabled="!webhook.url"
                    @click="showWebhookPreview = !showWebhookPreview"
                  >
                    <template #icon><NIcon :component="InformationCircleOutline" /></template>
                    {{ showWebhookPreview ? '收起请求预览' : '查看请求预览' }}
                  </NButton>
                </NSpace>
                <NButton type="primary" @click="saveWebhook">
                  <template #icon><NIcon :component="NotificationsOutline" /></template>
                  保存配置
                </NButton>
              </NSpace>

              <!-- 实际请求预览：仅展示将发送的结构，签名在发送瞬间动态生成，不展示失效静态签名 -->
              <div v-if="showWebhookPreview" class="webhook-preview-box">
                <div class="preview-head">
                  <div class="preview-title">
                    <NIcon size="15" color="#6366f1"><InformationCircleOutline /></NIcon>
                    请求预览
                  </div>
                  <span class="preview-note">预览不发送请求</span>
                </div>
                <div class="preview-grid">
                  <div class="preview-label">目标平台</div>
                  <div class="preview-value"><NTag size="tiny" type="info" round>{{ webhookPreview.platform }}</NTag></div>
                  <div class="preview-label">请求</div>
                  <div class="preview-value mono">{{ webhookPreview.method }} {{ webhookPreview.url }}</div>
                  <div class="preview-label">签名处理</div>
                  <div class="preview-value">{{ webhookPreview.signing }}</div>
                  <div class="preview-label">请求头</div>
                  <pre class="preview-code">{{ JSON.stringify(webhookPreview.headers, null, 2) }}</pre>
                  <div class="preview-label">消息体</div>
                  <pre class="preview-code">{{ webhookPreview.body }}</pre>
                </div>
              </div>

              <!-- 测试推送结果反馈卡片 -->
              <div v-if="webhookTestResult" class="test-result-box" :class="{ success: webhookTestResult.ok, error: !webhookTestResult.ok }">
                <div class="result-header">
                  <div class="result-title">
                    <NIcon size="16" :color="webhookTestResult.ok ? '#18a058' : '#d03050'">
                      <CheckmarkCircleOutline v-if="webhookTestResult.ok" />
                      <CloseCircleOutline v-else />
                    </NIcon>
                    <span>{{ webhookTestResult.ok ? '测试消息已成功推送' : '测试消息推送失败' }}</span>
                  </div>
                  <div class="result-meta">
                    <NTag size="tiny" :bordered="false">{{ webhookTestResult.platform }}</NTag>
                    <span>耗时 {{ webhookTestResult.duration_ms }}ms</span>
                    <span>HTTP {{ webhookTestResult.status_code }}</span>
                  </div>
                </div>
                <div v-if="webhookTestResult.error" class="result-error">
                  {{ webhookTestResult.error }}
                </div>
                <div v-if="webhookTestResult.response" class="result-response">
                  <span class="resp-label">对端响应内容:</span>
                  <code>{{ webhookTestResult.response }}</code>
                </div>
              </div>
            </NCard>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <!-- Rule modal -->
    <NModal
      v-model:show="showRuleModal"
      preset="card"
      :title="editingRule ? '编辑告警规则' : '新增告警规则'"
      style="width: 520px"
    >
      <NForm label-placement="top">
        <NFormItem label="规则名称" required>
          <NInput v-model:value="ruleForm.name" placeholder="例如：生产服务器 CPU 高负载预警" />
        </NFormItem>
        <NFormItem label="监控类型" required>
          <NSelect v-model:value="ruleForm.type" :options="ruleTypeOptions" />
        </NFormItem>
        <NFormItem label="告警级别" required>
          <NSelect
            v-model:value="ruleForm.severity"
            :options="severityOptions.filter((o) => o.value !== 'all')"
          />
        </NFormItem>
        <NFormItem label="阈值 (%)" v-if="ruleForm.type !== 'offline' && ruleForm.type !== 'online' && ruleForm.type !== 'anomaly'">
          <NInputNumber v-model:value="ruleForm.threshold" :min="1" :max="100" style="width: 100%" />
        </NFormItem>
        <NFormItem label="监控指标" v-if="ruleForm.type === 'anomaly'">
          <NSelect v-model:value="ruleForm.metric" :options="anomalyMetricOptions" />
        </NFormItem>
        <NFormItem label="偏离倍数 (σ)" v-if="ruleForm.type === 'anomaly'">
          <NInputNumber v-model:value="ruleForm.threshold" :min="1" :max="10" :step="0.5" style="width: 100%" />
          <span class="muted-text" style="margin-left: 8px">当前值偏离均值超过 {{ ruleForm.threshold }}σ 时触发</span>
        </NFormItem>
        <NFormItem label="主机过滤 (主机名包含，留空则匹配全部)">
          <NInput v-model:value="ruleForm.host_filter" placeholder="例如：prod 或 web" />
        </NFormItem>
        <NFormItem label="分组过滤 (留空则匹配全部)">
          <NInput v-model:value="ruleForm.group_filter" placeholder="例如：production" />
        </NFormItem>
        <NFormItem label="是否启用">
          <NSwitch v-model:value="ruleForm.enabled" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showRuleModal = false">取消</NButton>
          <NButton type="primary" @click="saveRule">保存</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.message-center-view {
  height: 100%;
  min-height: 0;
  box-sizing: border-box;
  display: flex;
  flex-direction: column;
  gap: 14px;
  // 滚动交给表格数据区（AGENTS.md 8.3）：外层留 auto 会出现双层滚动条，
  // 并把分页条推到首屏之外。
  overflow: hidden;

  .page-header {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;

    .header-left {
      display: flex;
      align-items: center;

      .page-title {
        margin: 0;
        font-size: 18px;
        font-weight: 600;
        color: var(--text-primary);
      }
    }
  }

  // NTabs 与其 pane 都要具备 flex 能力，否则消息表格无法按视口高度收缩，
  // 分页条会被推到首屏之外（AGENTS.md 8.2）。
  .tabs-container {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;

    :deep(.n-tabs) {
      flex: 1;
      min-height: 0;
    }

    :deep(.n-tabs-pane-wrapper) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    :deep(.n-tab-pane) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }
  }

  // 消息列表页签：筛选栏固定，表格吃掉剩余高度。规则/Webhook 页签内容较短，
  // 允许自身滚动。
  .messages-tab-content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .rules-tab-content,
  .webhook-tab-content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    overscroll-behavior: contain;
    -webkit-overflow-scrolling: touch;
  }

  .filter-toolbar {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;
    flex-wrap: wrap;
    gap: 10px;

    .filter-left {
      display: flex;
      align-items: center;
      gap: 12px;
      flex-wrap: wrap;
    }

    .filter-right {
      .result-count {
        font-size: 12px;
        color: var(--text-secondary);
      }
    }
  }

  .table-card {
    background-color: var(--bg-card);
    border-radius: 8px;
    padding: 12px 16px;
    box-shadow: var(--shadow-sm);

    &.table-flex-fill {
      padding: 0;
    }

    // 消息表格改用 NDataTable，单元格由 render 函数生成，样式需穿透 scoped。
    :deep(.unread-row td) {
      background-color: rgba(99, 102, 241, 0.04);
    }

    :deep(.rule-name-tag) {
      font-weight: 500;
      color: var(--text-primary);
    }

    :deep(.host-cell) {
      display: flex;
      align-items: center;
      gap: 6px;
      color: var(--text-secondary);

      &.clickable {
        cursor: pointer;
        color: #3b82f6;
        &:hover {
          text-decoration: underline;
        }
      }

      .host-name-text {
        font-weight: 500;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
    }

    :deep(.ai-interpret-summary) {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      color: #6366f1;
      cursor: help;
      line-height: 1.4;
      font-size: 12px;
      max-width: 100%;

      // AI 解读是长句，列宽有限：单行省略号，完整内容走 tooltip。
      .ai-interpret-text {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
    }

    :deep(.muted-text) {
      color: var(--text-secondary);
      font-size: 12px;
    }

    :deep(.time-cell) {
      font-size: 12px;
      color: var(--text-secondary);
    }

    // 告警规则页签仍是原生 NTable（规则数量天然有限，属 8.2 例外）。
    .custom-table {
      width: 100%;

      th {
        background-color: var(--bg-card-subtle);
        color: var(--text-secondary);
        font-weight: 600;
        font-size: 12px;
      }

      td {
        color: var(--text-primary);
        font-size: 13px;
        vertical-align: middle;
      }

      .rule-title-cell {
        font-weight: 600;
        cursor: pointer;
        color: #3b82f6;
        &:hover {
          text-decoration: underline;
        }
      }
    }
  }

  .muted-text {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .toolbar-bar {
    margin-bottom: 12px;
  }

  .webhook-card {
    max-width: 650px;
    background-color: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;

    .webhook-intro {
      margin: 0;
      font-size: 13px;
      color: var(--text-secondary);
      line-height: 1.5;
    }

    .webhook-preview-box {
      margin-top: 14px;
      padding: 12px 14px;
      border-radius: 6px;
      background: rgba(99, 102, 241, 0.045);
      border: 1px solid rgba(99, 102, 241, 0.2);

      .preview-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding-bottom: 8px;
        margin-bottom: 8px;
        border-bottom: 1px solid var(--border-color);

        .preview-title {
          display: flex;
          align-items: center;
          gap: 6px;
          font-size: 13px;
          font-weight: 600;
          color: var(--text-primary);
        }

        .preview-note {
          font-size: 11px;
          color: var(--text-secondary);
        }
      }

      .preview-grid {
        display: grid;
        grid-template-columns: 72px minmax(0, 1fr);
        align-items: start;
        gap: 8px 10px;
        font-size: 12px;

        .preview-label {
          color: var(--text-secondary);
          line-height: 1.5;
        }

        .preview-value {
          min-width: 0;
          color: var(--text-primary);
          line-height: 1.5;

          &.mono {
            font-family: var(--font-mono, monospace);
            color: #10b981;
            word-break: break-all;
          }
        }

        .preview-code {
          min-width: 0;
          max-height: 150px;
          margin: 0;
          padding: 7px 9px;
          border-radius: 4px;
          overflow: auto;
          background: var(--code-box-bg);
          color: var(--text-primary);
          font-family: var(--font-mono, monospace);
          font-size: 11px;
          line-height: 1.45;
          white-space: pre-wrap;
          word-break: break-all;
        }
      }
    }

    .test-result-box {
      margin-top: 16px;
      padding: 12px 14px;
      border-radius: 6px;
      border: 1px solid var(--border-color);
      font-size: 12.5px;
      line-height: 1.5;

      &.success {
        background-color: rgba(24, 160, 88, 0.08);
        border-color: rgba(24, 160, 88, 0.3);
      }

      &.error {
        background-color: rgba(208, 48, 80, 0.08);
        border-color: rgba(208, 48, 80, 0.3);
      }

      .result-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 12px;
        margin-bottom: 6px;

        .result-title {
          display: flex;
          align-items: center;
          gap: 6px;
          font-weight: 600;
          color: var(--text-primary);
        }

        .result-meta {
          display: flex;
          align-items: center;
          gap: 8px;
          color: var(--text-secondary);
          font-size: 11.5px;
        }
      }

      .result-error {
        color: #d03050;
        margin-bottom: 6px;
        font-weight: 500;
      }

      .result-response {
        display: flex;
        flex-direction: column;
        gap: 4px;
        margin-top: 6px;

        .resp-label {
          font-size: 11px;
          color: var(--text-secondary);
        }

        code {
          padding: 4px 8px;
          border-radius: 4px;
          background: var(--code-box-bg);
          font-family: var(--font-mono, monospace);
          font-size: 11.5px;
          color: var(--text-primary);
          word-break: break-all;
          max-height: 120px;
          overflow-y: auto;
        }
      }
    }
  }
}
</style>
