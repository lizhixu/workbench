<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NSwitch,
  NInput,
  NTag,
  NDataTable,
  NEmpty,
  NIcon,
  useMessage,
} from 'naive-ui'
import { ShieldCheckmarkOutline, TrashOutline, AddOutline, RefreshOutline } from '@vicons/ionicons5'
import { h } from 'vue'
import {
  getCommandPolicy,
  setCommandPolicy,
  listCommandAudit,
  type CommandPolicy,
  type CommandAuditEntry,
} from '../../api/policy'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'

const message = useMessage()

const policy = ref<CommandPolicy>({
  enabled: true,
  blacklist: [],
  whitelist: [],
  high_risk_patterns: [],
})
const loading = ref(false)
const saving = ref(false)

// New pattern inputs
const newBlacklist = ref('')
const newWhitelist = ref('')
const newHighRisk = ref('')

// Audit
const auditEntries = ref<CommandAuditEntry[]>([])
const auditLoading = ref(false)
// 拦截审计会持续累积（AGENTS.md 8.2）：分页取代原来的 max-height 限高，
// 否则超出 limit 的记录在界面上根本触达不到。
const auditCount = computed(() => auditEntries.value.length)
const { pagination: auditPagination, resetPage: resetAuditPage } = useTablePagination({
  // 这张表是设置页某张卡片里的一节，每页 10 条既能翻到全部记录，
  // 又不会让整个设置页被它撑长。
  pageSize: 10,
  pageSizes: [10, 20, 50],
  rowCount: auditCount,
})

// The server omits empty pattern lists (Go marshals a nil slice as null), and
// the template reads `.length` on each one — without this normalisation the
// whole card throws during render and shows up blank.
function normalizePolicy(p: CommandPolicy): CommandPolicy {
  return {
    enabled: p.enabled,
    blacklist: p.blacklist ?? [],
    whitelist: p.whitelist ?? [],
    high_risk_patterns: p.high_risk_patterns ?? [],
  }
}

async function loadPolicy() {
  loading.value = true
  try {
    policy.value = normalizePolicy(await getCommandPolicy())
  } catch (e: any) {
    message.error(e.message || '加载命令策略失败')
  } finally {
    loading.value = false
  }
}

async function savePolicy() {
  saving.value = true
  try {
    const updated = await setCommandPolicy(policy.value)
    policy.value = normalizePolicy(updated)
    message.success('高危命令管控策略已保存')
  } catch (e: any) {
    message.error(e.message || '保存策略失败')
  } finally {
    saving.value = false
  }
}

function addPattern(list: 'blacklist' | 'whitelist' | 'high_risk_patterns', value: string) {
  const v = value.trim()
  if (!v) return
  if (policy.value[list].includes(v)) {
    message.warning('该规则已存在')
    return
  }
  policy.value[list].push(v)
}

function removePattern(list: 'blacklist' | 'whitelist' | 'high_risk_patterns', index: number) {
  policy.value[list].splice(index, 1)
}

async function loadAudit() {
  auditLoading.value = true
  try {
    // 服务端最多保留 1000 条；一次取全量交给前端分页，避免旧的 limit=100
    // 把更早的记录永久挡在界面之外。
    auditEntries.value = await listCommandAudit({ limit: 1000 })
    resetAuditPage()
  } catch (e: any) {
    // silent
  } finally {
    auditLoading.value = false
  }
}

function resultTagType(result: string): 'error' | 'warning' | 'success' | 'info' | 'default' {
  if (result === 'blocked') return 'error'
  if (result === 'denied') return 'warning'
  if (result === 'confirmed') return 'info'
  if (result === 'allowed') return 'success'
  return 'default'
}

function resultLabel(result: string): string {
  const m: Record<string, string> = { blocked: '已拦截', denied: '未确认', confirmed: '已确认执行', allowed: '已放行' }
  return m[result] || result
}

function riskLabel(risk: string): string {
  const m: Record<string, string> = { blocked: '阻断', high: '高危', medium: '中危', low: '低危' }
  return m[risk] || risk
}

function riskTagType(risk: string): 'error' | 'warning' | 'info' | 'default' {
  if (risk === 'blocked' || risk === 'high') return 'error'
  if (risk === 'medium') return 'warning'
  if (risk === 'low') return 'info'
  return 'default'
}

function formatTime(t: string): string {
  if (!t) return '-'
  return fmtDateTime(t)
}

const auditColumns = [
  { title: '时间', key: 'timestamp', width: 160, render: (r: CommandAuditEntry) => formatTime(r.timestamp) },
  { title: '用户', key: 'username', width: 90 },
  { title: '主机', key: 'host_id', width: 110, ellipsis: { tooltip: true } },
  {
    title: '命令',
    key: 'command',
    ellipsis: { tooltip: true },
    render: (r: CommandAuditEntry) => h('code', { style: 'font-size:12px' }, r.command),
  },
  {
    title: '风险',
    key: 'risk_level',
    width: 80,
    render: (r: CommandAuditEntry) => h(NTag, { size: 'small', type: riskTagType(r.risk_level), bordered: false }, { default: () => riskLabel(r.risk_level) }),
  },
  {
    title: '处理',
    key: 'result',
    width: 100,
    render: (r: CommandAuditEntry) => h(NTag, { size: 'small', type: resultTagType(r.result), bordered: false }, { default: () => resultLabel(r.result) }),
  },
  { title: '原因', key: 'reason', ellipsis: { tooltip: true } },
]

onMounted(() => {
  loadPolicy()
  loadAudit()
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><ShieldCheckmarkOutline /></NIcon>
        高危命令管控
      </span>
    </template>
    <template #header-extra>
      <NSpace align="center">
        <span class="muted">启用策略</span>
        <NSwitch v-model:value="policy.enabled" />
      </NSpace>
    </template>

    <NSpace vertical :size="16">
      <p class="muted tip-hint">
        控制端在下发命令前按以下规则检查：黑名单命中即拦截，高危模式命中需二次确认，白名单命中可跳过确认。Agent 侧另有内置硬编码高危拦截作为最后防线。
      </p>

      <!-- Blacklist -->
      <div class="pattern-section">
        <div class="pattern-title">
          <NTag type="error" size="small" :bordered="false">黑名单</NTag>
          <span class="muted">命中即拦截，不下发到 Agent</span>
        </div>
        <div class="pattern-list">
          <div v-for="(p, i) in policy.blacklist" :key="'b' + i" class="pattern-item">
            <code class="pattern-code">{{ p }}</code>
            <NButton size="tiny" quaternary circle @click="removePattern('blacklist', i)">
              <template #icon><NIcon><TrashOutline /></NIcon></template>
            </NButton>
          </div>
          <NEmpty v-if="!policy.blacklist.length" size="small" description="暂无黑名单规则" />
        </div>
        <NSpace align="center">
          <NInput v-model:value="newBlacklist" placeholder="正则表达式，如 rm\s+-rf\s+/" size="small" style="width: 360px" @keydown.enter="addPattern('blacklist', newBlacklist); newBlacklist = ''" />
          <NButton size="small" @click="addPattern('blacklist', newBlacklist); newBlacklist = ''">
            <template #icon><NIcon><AddOutline /></NIcon></template>
            添加
          </NButton>
        </NSpace>
      </div>

      <!-- Whitelist -->
      <div class="pattern-section">
        <div class="pattern-title">
          <NTag type="success" size="small" :bordered="false">白名单</NTag>
          <span class="muted">命中跳过二次确认，直接放行</span>
        </div>
        <div class="pattern-list">
          <div v-for="(p, i) in policy.whitelist" :key="'w' + i" class="pattern-item">
            <code class="pattern-code">{{ p }}</code>
            <NButton size="tiny" quaternary circle @click="removePattern('whitelist', i)">
              <template #icon><NIcon><TrashOutline /></NIcon></template>
            </NButton>
          </div>
          <NEmpty v-if="!policy.whitelist.length" size="small" description="暂无白名单规则" />
        </div>
        <NSpace align="center">
          <NInput v-model:value="newWhitelist" placeholder="正则表达式" size="small" style="width: 360px" @keydown.enter="addPattern('whitelist', newWhitelist); newWhitelist = ''" />
          <NButton size="small" @click="addPattern('whitelist', newWhitelist); newWhitelist = ''">
            <template #icon><NIcon><AddOutline /></NIcon></template>
            添加
          </NButton>
        </NSpace>
      </div>

      <!-- High-risk patterns -->
      <div class="pattern-section">
        <div class="pattern-title">
          <NTag type="warning" size="small" :bordered="false">高危模式</NTag>
          <span class="muted">命中需前端二次确认后方可下发</span>
        </div>
        <div class="pattern-list">
          <div v-for="(p, i) in policy.high_risk_patterns" :key="'h' + i" class="pattern-item">
            <code class="pattern-code">{{ p }}</code>
            <NButton size="tiny" quaternary circle @click="removePattern('high_risk_patterns', i)">
              <template #icon><NIcon><TrashOutline /></NIcon></template>
            </NButton>
          </div>
          <NEmpty v-if="!policy.high_risk_patterns.length" size="small" description="暂无高危模式" />
        </div>
        <NSpace align="center">
          <NInput v-model:value="newHighRisk" placeholder="正则表达式" size="small" style="width: 360px" @keydown.enter="addPattern('high_risk_patterns', newHighRisk); newHighRisk = ''" />
          <NButton size="small" @click="addPattern('high_risk_patterns', newHighRisk); newHighRisk = ''">
            <template #icon><NIcon><AddOutline /></NIcon></template>
            添加
          </NButton>
        </NSpace>
      </div>

      <NSpace justify="end">
        <NButton :loading="saving" type="primary" @click="savePolicy">保存策略</NButton>
      </NSpace>

      <!-- Audit log -->
      <div class="audit-section">
        <div class="audit-header">
          <span class="section-title">命令拦截审计</span>
          <NSpace align="center" :size="10">
            <span class="muted">共 {{ auditEntries.length }} 条</span>
            <NButton size="small" quaternary :loading="auditLoading" @click="loadAudit">
              <template #icon><NIcon><RefreshOutline /></NIcon></template>
              刷新
            </NButton>
          </NSpace>
        </div>
        <NDataTable
          :columns="auditColumns"
          :data="auditEntries"
          :pagination="auditPagination"
          size="small"
          :bordered="false"
          :row-key="(r: CommandAuditEntry) => r.id"
        >
          <template #empty>
            <NEmpty description="暂无命令拦截记录" />
          </template>
        </NDataTable>
      </div>
    </NSpace>
  </NCard>
</template>

<style scoped lang="scss">
.muted { color: var(--text-secondary); font-size: 13px; }

.pattern-section {
  .pattern-title {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
  }

  .pattern-list {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-bottom: 8px;
    min-height: 32px;
  }

  .pattern-item {
    display: flex;
    align-items: center;
    gap: 4px;
    background: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 4px;
    padding: 2px 6px;
  }

  .pattern-code {
    font-family: 'JetBrains Mono', Consolas, monospace;
    font-size: 12px;
    color: var(--text-primary);
  }
}

.audit-section {
  margin-top: 8px;

  .audit-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .section-title {
    font-size: 14px;
    font-weight: 600;
  }
}
</style>