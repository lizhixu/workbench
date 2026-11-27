<script setup lang="ts">
import { onMounted, ref, computed, watch, h } from 'vue'
import {
  NCard, NButton, NSpace, NTag, NModal, NForm, NFormItem, NDataTable,
  NInput, NSelect, NPopconfirm, NEmpty, useMessage, NIcon, NTooltip,
  NDescriptions, NDescriptionsItem,
  type DataTableColumns,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, TrashOutline, LockClosedOutline, EyeOutline,
  CreateOutline, KeyOutline, CodeSlashOutline, ServerOutline, ShieldCheckmarkOutline,
  CopyOutline, SearchOutline, GitBranchOutline, CubeOutline,
} from '@vicons/ionicons5'
import type { Credential } from '../../api/vault'
import { listCredentials, createCredential, updateCredential, deleteCredential, getCredential, revealCredentialSecret } from '../../api/vault'
import { listHosts } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'CredentialList' })

const message = useMessage()
const creds = ref<Credential[]>([])
const loading = ref(false)
const hostOptions = ref<{ label: string; value: string }[]>([])

// 搜索与类型过滤
const searchKeyword = ref('')
const selectedType = ref<string | null>(null)

const showCreate = ref(false)
const showView = ref(false)
const viewingCred = ref<Credential | null>(null)
const revealedSecret = ref('')
const revealingSecret = ref(false)
const editingId = ref<string | null>(null)
const form = ref({
  name: '',
  type: 'ssh_password',
  username: '',
  secret: '',
  host: '',
  description: '',
})

const typeOptions = [
  { label: '全部类型', value: '__all__' },
  { label: 'SSH 密码', value: 'ssh_password' },
  { label: 'Git 访问令牌', value: 'git_token' },
  { label: '镜像仓库凭据', value: 'container_registry' },
  { label: 'API Key', value: 'api_key' },
  { label: '数据库密码', value: 'database' },
  { label: '自定义', value: 'custom' },
]

const formTypeOptions = [
  { label: 'SSH 密码', value: 'ssh_password' },
  { label: 'Git 访问令牌', value: 'git_token' },
  { label: '镜像仓库凭据', value: 'container_registry' },
  { label: 'API Key', value: 'api_key' },
  { label: '数据库密码', value: 'database' },
  { label: '自定义', value: 'custom' },
]

function typeMeta(t: string): { label: string; tagType: 'default' | 'info' | 'success' | 'warning' | 'error'; icon: any } {
  switch (t) {
    case 'ssh_password':
      return { label: 'SSH 密码', tagType: 'info', icon: KeyOutline }
    case 'git_token':
      return { label: 'Git 访问令牌', tagType: 'info', icon: GitBranchOutline }
    case 'container_registry':
      return { label: '镜像仓库凭据', tagType: 'warning', icon: CubeOutline }
    case 'api_key':
      return { label: 'API Key', tagType: 'warning', icon: CodeSlashOutline }
    case 'database':
      return { label: '数据库密码', tagType: 'success', icon: ServerOutline }
    case 'custom':
    default:
      return { label: t === 'password' ? '登录密码' : (t || '自定义'), tagType: 'default', icon: LockClosedOutline }
  }
}


// 快速生成 20 位高强度随机密码
function generateRandomPassword() {
  const chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*()_+-='
  let res = ''
  const arr = new Uint8Array(20)
  crypto.getRandomValues(arr)
  for (let i = 0; i < 20; i++) {
    res += chars[arr[i] % chars.length]
  }
  form.value.secret = res
  message.success('已生成 20 位随机高强密码')
}

// 快捷复制
async function copyText(text: string, label: string) {
  if (!text) return
  const ok = await copyToClipboard(text)
  if (ok) {
    message.success(`已复制${label}`)
  } else {
    message.warning('复制失败，请手动复制')
  }
}

// 本地筛选
const filteredCreds = computed(() => {
  let list = creds.value
  if (selectedType.value && selectedType.value !== '__all__') {
    list = list.filter((c) => c.type === selectedType.value || (selectedType.value === 'custom' && c.type === 'password'))
  }
  if (searchKeyword.value.trim()) {
    const q = searchKeyword.value.trim().toLowerCase()
    list = list.filter((c) =>
      (c.name || '').toLowerCase().includes(q) ||
      (c.username || '').toLowerCase().includes(q) ||
      (c.host || '').toLowerCase().includes(q) ||
      (c.description || '').toLowerCase().includes(q),
    )
  }
  return list
})

const credCount = computed(() => filteredCreds.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount: credCount })

// ---- 批量删除（范式见 NetworkList.vue） ----
// 勾选按凭据 id 存、跨页保留；执行期 3 路并发、逐行显示进度、失败可重试。
const selectedCredIds = ref<Set<string>>(new Set())
const BATCH_CONCURRENCY = 3
type BatchPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
const showBatchDeleteModal = ref(false)

const columns = computed<DataTableColumns<Credential>>(() => [
  {
    // 批量勾选列：NDataTable 复选框列取 40px（BatchExec.vue 同款口径；
    // DESIGN.md §9.20 的 28px 是 flex 行列表口径，复选框 + 单元格内边距放不下）。
    type: 'selection',
    width: 40,
    disabled: () => batchRunning.value,
  },
  {
    title: '名称 / 描述',
    key: 'name',
    minWidth: 220,
    render: (c) =>
      h('div', { class: 'cred-title-cell' }, [
        h('div', { class: 'cred-name-row' }, [
          h(NIcon, { component: LockClosedOutline, class: 'lock-icon', size: 15 }),
          h('span', { class: 'cred-name-text' }, c.name),
        ]),
        c.description
          ? h('div', { class: 'cred-desc-text' }, c.description)
          : null,
        batchProgressNode(c),
      ]),
  },
  {
    title: '凭据类型',
    key: 'type',
    width: 130,
    render: (c) => {
      const meta = typeMeta(c.type)
      return h(
        NTag,
        { size: 'small', round: true, type: meta.tagType, bordered: false },
        {
          icon: () => h(NIcon, { component: meta.icon }),
          default: () => meta.label,
        },
      )
    },
  },
  {
    title: '用户名',
    key: 'username',
    width: 160,
    render: (c) =>
      c.username
        ? h('div', { class: 'copyable-cell' }, [
            h('span', { class: 'mono-text' }, c.username),
            h(
              NTooltip,
              {},
              {
                trigger: () =>
                  h(
                    NButton,
                    {
                      size: 'tiny',
                      quaternary: true,
                      circle: true,
                      class: 'cell-copy-btn',
                      onClick: (e: MouseEvent) => {
                        e.stopPropagation()
                        copyText(c.username, '用户名')
                      },
                    },
                    { icon: () => h(NIcon, { component: CopyOutline, size: 12 }) },
                  ),
                default: () => '复制用户名',
              },
            ),
          ])
        : h('span', { class: 'muted' }, '-'),
  },
  {
    title: '关联主机',
    key: 'host',
    width: 170,
    render: (c) =>
      c.host
        ? h('div', { class: 'host-pill' }, [
            h(NIcon, { component: ServerOutline, size: 13, class: 'host-icon' }),
            h('span', { class: 'host-name' }, c.host),
          ])
        : h('span', { class: 'muted' }, '-'),
  },
  {
    title: '更新时间',
    key: 'updated_at',
    width: 170,
    render: (c) => h('span', { class: 'time-cell' }, fmtDateTime(c.updated_at)),
  },
  {
    title: '操作',
    key: 'actions',
    width: 250,
    render: (c) =>
      h(NSpace, { size: 6, wrap: false, wrapItem: false }, {
        default: () => [
          h(
            NButton,
            { size: 'small', quaternary: true, type: 'primary', onClick: () => viewCred(c.id) },
            {
              icon: () => h(NIcon, { component: EyeOutline }),
              default: () => '查看',
            },
          ),
          h(
            NButton,
            { size: 'small', quaternary: true, onClick: () => openEdit(c) },
            {
              icon: () => h(NIcon, { component: CreateOutline }),
              default: () => '编辑',
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDelete(c.id) },
            {
              trigger: () =>
                h(
                  NButton,
                  { size: 'small', quaternary: true, type: 'error' },
                  {
                    icon: () => h(NIcon, { component: TrashOutline }),
                    default: () => '删除',
                  },
                ),
              default: () => `确认永久删除凭据「${c.name}」？关联服务可能会失去鉴权能力。`,
            },
          ),
        ],
      }),
  },
])

function isOpBusy(id: string) {
  const st = opStatus.value[id]
  return !!st && (st.phase === 'queued' || st.phase === 'running')
}

function batchRowClassName(c: Credential) {
  return isOpBusy(c.id) ? 'batch-row-busy' : ''
}

async function refresh(manual = false) {
  loading.value = true
  try {
    const [cList, hList] = await Promise.all([
      listCredentials(),
      listHosts().catch(() => ({ data: [] })),
    ])
    creds.value = cList || []
    // 关联主机的存储值沿用主机名（历史数据即如此），但主机名可能重复。
// 重复的 option value 会让 NSelect 渲染出重复条目，故按 value 去重，
// 保留首个（label 仍带 IP，便于识别同名主机）。
    const seenHosts = new Set<string>()
    const hostOpts: { label: string; value: string }[] = []
    for (const h of (hList.data || []) as any[]) {
      const value = h.hostname || h.id
      if (!value || seenHosts.has(value)) continue
      seenHosts.add(value)
      hostOpts.push({
        label: `${h.hostname || h.id.slice(0, 8)} (${h.internal_ip || h.public_ip || h.id.slice(0, 8)})`,
        value,
      })
    }
    hostOptions.value = hostOpts
    // 刷新清掉上一批的旧进度；批量执行中不清除（runBatchDelete 在执行完才刷新，
    // 此时 batchRunning 仍为 true，进度得以保留到汇总展示）。
    if (!batchRunning.value) {
      opStatus.value = {}
      batchSummary.value = null
    }
    if (manual) {
      resetPage()
    }
  } catch (e: any) {
    message.error(e.message || '加载凭据失败')
  } finally {
    loading.value = false
  }
}

async function doCreate() {
  if (!form.value.name.trim()) {
    message.warning('请填写凭据名称')
    return
  }
  if (!editingId.value && !form.value.secret.trim()) {
    message.warning('新建凭据时必须提供密钥或密码')
    return
  }
  try {
    if (editingId.value) {
      await updateCredential(editingId.value, form.value)
      message.success('凭据已成功更新')
    } else {
      await createCredential(form.value)
      message.success('凭据已加密保存')
    }
    showCreate.value = false
    editingId.value = null
    form.value = { name: '', type: 'ssh_password', username: '', secret: '', host: '', description: '' }
    refresh()
  } catch (e: any) {
    message.error(e.message || '保存凭据失败')
  }
}

function openCreate() {
  editingId.value = null
  form.value = { name: '', type: 'ssh_password', username: '', secret: '', host: '', description: '' }
  showCreate.value = true
}

function openEdit(c: Credential) {
  editingId.value = c.id
  form.value = {
    name: c.name,
    type: c.type === 'password' ? 'custom' : c.type,
    username: c.username,
    secret: '',
    host: c.host,
    description: c.description,
  }
  showCreate.value = true
}

let revealSeq = 0

async function viewCred(id: string) {
  revealSeq++
  revealedSecret.value = ''
  revealingSecret.value = false
  try {
    viewingCred.value = await getCredential(id)
    showView.value = true
  } catch (e: any) {
    message.error(e.message || '获取凭据详情失败')
  }
}

async function revealSecret() {
  const id = viewingCred.value?.id
  if (!id) return
  const seq = ++revealSeq
  revealingSecret.value = true
  try {
    const data = await revealCredentialSecret(id)
    // 在途时关闭弹窗或切到另一条凭据：丢弃结果，避免明文写进错误上下文。
    if (seq !== revealSeq || !showView.value || viewingCred.value?.id !== id) return
    revealedSecret.value = data.secret || ''
    if (!revealedSecret.value) message.info('该凭据未保存密钥')
  } catch (e: any) {
    if (seq !== revealSeq || !showView.value) return
    message.error(e.message || '查看密钥失败')
  } finally {
    if (seq === revealSeq) revealingSecret.value = false
  }
}

function hideSecret() {
  revealedSecret.value = ''
}

// 关闭详情弹窗立即清空明文，并让在途查看请求失效。
watch(showView, (visible) => {
  if (!visible) {
    revealedSecret.value = ''
    revealSeq++
  }
})

async function doDelete(id: string) {
  try {
    await deleteCredential(id)
    message.success('凭据已彻底销毁')
    selectedCredIds.value.delete(id)
    refresh()
  } catch (e: any) {
    message.error(e.message || '删除凭据失败')
  }
}

// ---- 批量删除（NetworkList.vue 范式：前端并发编排、复用单机接口） ----

// 批量执行行内进度（NetworkList .op-progress 同款文案与色板）：render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透（见文末样式）。
function batchProgressNode(row: { id: string }) {
  const st = opStatus.value[row.id]
  if (!st) return null
  const text =
    st.phase === 'queued' ? '排队中…'
    : st.phase === 'running' ? '执行中…'
    : st.phase === 'done' ? '已完成'
    : `失败：${st.message || '执行出错'}`
  return h('div', { class: ['batch-op-progress', `is-${st.phase}`], title: st.message || '' }, text)
}

const selectedCreds = computed(() => creds.value.filter((c) => selectedCredIds.value.has(c.id)))

// 当前页凭据（前端分页切片）：勾选同步以当前页为基准合并，跨页已选保持不变。
// 注意：筛选后的当前页按 filteredCreds 切片，勾选清理则以全量 creds 为准
// （被筛掉的行依然存在，勾选保留）。
const pagedCreds = computed(() => {
  const start = (pagination.page - 1) * pagination.pageSize
  return filteredCreds.value.slice(start, start + pagination.pageSize)
})

function onCredChecked(keys: (string | number)[]) {
  const pageIds = new Set(pagedCreds.value.map((c) => c.id))
  const next = new Set([...selectedCredIds.value].filter((id) => !pageIds.has(id)))
  keys.forEach((k) => {
    if (pageIds.has(String(k))) next.add(String(k))
  })
  selectedCredIds.value = next
}

// 凭据在全量列表里消失（被删除）后清理对应的勾选与进度。
watch(creds, (list) => {
  const alive = new Set(list.map((c) => c.id))
  selectedCredIds.value = new Set([...selectedCredIds.value].filter((id) => alive.has(id)))
  Object.keys(opStatus.value).forEach((id) => {
    if (!alive.has(id)) delete opStatus.value[id]
  })
})

async function runBatchDelete(targets: Credential[]) {
  if (targets.length === 0) {
    message.warning('所选凭据中没有可删除的')
    return
  }
  batchRunning.value = true
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((c) => {
    opStatus.value[c.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...targets]
  async function worker() {
    while (queue.length > 0) {
      const cred = queue.shift()!
      opStatus.value[cred.id] = { phase: 'running' }
      try {
        // 复用单机删除接口，审计走既有单机接口的审计，无需新埋点。
        await deleteCredential(cred.id)
        opStatus.value[cred.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[cred.id] = { phase: 'failed', message: e.message || '执行出错' }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, targets.length) }, () => worker()),
  )
  // 批量执行完再刷新列表（batchRunning 仍为 true，refresh 的清进度守卫
  // 不触发）：成功删除的行随之消失，失败行保留进度供重试。
  await refresh()
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped: 0 }
  if (failed === 0) {
    message.success(`批量删除完成：成功 ${done} 条凭据`)
  } else {
    message.warning(`批量删除完成：成功 ${done} 条，失败 ${failed} 条，可点「重试失败」只重跑失败的凭据`)
  }
}

function retryBatchFailed() {
  if (!batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  runBatchDelete(creds.value.filter((c) => failedIds.includes(c.id)))
}

function confirmBatchDelete() {
  showBatchDeleteModal.value = false
  runBatchDelete(selectedCreds.value)
}

function clearBatchSelection() {
  selectedCredIds.value = new Set()
  opStatus.value = {}
  batchSummary.value = null
}

onMounted(refresh)
</script>

<template>
  <div class="cred-view page-flex-column">
    <!-- Header: 标题 + 刷新/新建 -->
    <div class="cred-header">
      <div class="header-left">
        <h2 class="page-title">凭据金库</h2>
      </div>
      <div class="header-right">
        <NSpace align="center" :size="10">
          <NTag size="small" :bordered="false" round type="info">AES-256-GCM 加密</NTag>
          <NButton :loading="loading" @click="() => refresh(true)">
            <template #icon><NIcon :component="RefreshOutline" /></template>
            刷新
          </NButton>
          <NButton type="primary" @click="openCreate">
            <template #icon><NIcon :component="AddCircleOutline" /></template>
            新增凭据
          </NButton>
        </NSpace>
      </div>
    </div>

    <!-- Filter Toolbar -->
    <div class="filter-toolbar">
      <div class="filter-left">
        <NInput
          v-model:value="searchKeyword"
          placeholder="搜索凭据名称、用户名、关联主机..."
          size="small"
          clearable
          style="width: 260px"
        >
          <template #prefix>
            <NIcon><SearchOutline /></NIcon>
          </template>
        </NInput>
        <NSelect
          v-model:value="selectedType"
          :options="typeOptions"
          placeholder="凭据类型"
          size="small"
          clearable
          style="width: 140px"
        />
      </div>
      <div class="filter-right">
        <span class="muted">共 {{ credCount }} 条符合条件的凭据</span>
      </div>
    </div>

    <!-- Table 卡片 -->
    <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐行显示进度 -->
    <div v-if="selectedCredIds.size > 0" class="batch-bar">
      <span class="batch-info">已选 {{ selectedCredIds.size }} 条凭据</span>
      <NButton
        size="small"
        type="error"
        secondary
        :loading="batchRunning"
        :disabled="batchRunning"
        @click="showBatchDeleteModal = true"
      >
        批量删除
      </NButton>
      <NButton
        v-if="batchSummary && batchSummary.failed > 0"
        size="small"
        type="warning"
        secondary
        :disabled="batchRunning"
        @click="retryBatchFailed"
      >
        重试失败（{{ batchSummary.failed }}）
      </NButton>
      <span v-if="batchSummary" class="batch-summary">
        成功 {{ batchSummary.done }} · 失败 {{ batchSummary.failed }}
      </span>
      <NButton size="small" quaternary :disabled="batchRunning" @click="clearBatchSelection">
        取消选择
      </NButton>
    </div>

    <NCard class="table-card table-flex-fill" size="small">
      <NDataTable
        flex-height
        :columns="columns"
        :data="filteredCreds"
        :pagination="pagination"
        :loading="loading"
        :bordered="false"
        size="small"
        :row-key="(c: Credential) => c.id"
        :checked-row-keys="[...selectedCredIds]"
        :row-class-name="batchRowClassName"
        :scroll-x="960"
        @update:checked-row-keys="onCredChecked"
      >
        <template #empty>
          <NEmpty description="暂无符合条件的凭据。点击右上角「新增凭据」添加首个加密凭据。" />
        </template>
      </NDataTable>
    </NCard>
  </div>

  <!-- 批量删除二次确认（DESIGN.md §9.17：破坏性确认在弹窗内就地展开） -->
  <NModal
    v-model:show="showBatchDeleteModal"
    preset="card"
    :title="`批量删除凭据（${selectedCreds.length} 条）`"
    style="width: 520px"
  >
    <div class="batch-confirm-body">
      <div class="batch-target-list">
        <div
          v-for="c in selectedCreds.slice(0, 5)"
          :key="c.id"
          class="batch-target-row"
        >
          {{ c.name }}<span v-if="c.username"> · {{ c.username }}</span>
        </div>
        <div v-if="selectedCreds.length > 5" class="batch-target-row muted">
          等 {{ selectedCreds.length - 5 }} 条…
        </div>
      </div>
      <p class="field-hint">凭据删除后无法恢复，关联服务可能会失去鉴权能力，请确认。</p>
    </div>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showBatchDeleteModal = false">取消</NButton>
        <NButton type="error" :loading="batchRunning" @click="confirmBatchDelete">
          确认删除
        </NButton>
      </NSpace>
    </template>
  </NModal>

  <!-- Create / Edit Modal -->
  <NModal
    v-model:show="showCreate"
    preset="card"
    :title="editingId ? '编辑凭据' : '新增安全凭据'"
    style="width: 540px"
  >
    <NForm label-placement="top">
      <NFormItem label="凭据名称" required>
        <NInput
          v-model:value="form.name"
          :input-props="{ autocomplete: 'off' }"
          placeholder="例如：生产数据库 Root 密码 / AWS 生产集群 AK"
        />
      </NFormItem>

      <NFormItem label="凭据类型" required>
        <NSelect v-model:value="form.type" :options="formTypeOptions" />
      </NFormItem>

      <NFormItem label="用户名 / 账号 (可选)">
        <NInput
          v-model:value="form.username"
          :input-props="{ autocomplete: 'off' }"
          placeholder="例如：root / admin / ubuntu"
        />
      </NFormItem>

      <NFormItem :label="editingId ? '重置密钥 / 密码 (留空保持不变)' : '密钥 / 密码'" :required="!editingId">
        <NInput
          v-model:value="form.secret"
          type="password"
          show-password-on="click"
          :input-props="{ autocomplete: 'new-password' }"
          :placeholder="editingId ? '留空保持原密钥不变' : '请输入密码、Private Key 或 API Token'"
        >
          <template #suffix>
            <span class="secret-suffix">
              <NButton text size="tiny" type="primary" @click="generateRandomPassword">
                随机生成
              </NButton>
            </span>
          </template>
        </NInput>
      </NFormItem>

      <NFormItem label="关联主机 (可选)">
        <NSelect
          v-if="hostOptions.length > 0"
          v-model:value="form.host"
          filterable
          tag
          clearable
          :options="hostOptions"
          placeholder="请选择或直接输入主机名 / IP"
        />
        <NInput
          v-else
          v-model:value="form.host"
          :input-props="{ autocomplete: 'off' }"
          placeholder="例如：prod-app-01 或 192.168.1.100"
        />
      </NFormItem>

      <NFormItem label="用途说明 / 备注 (可选)">
        <NInput
          v-model:value="form.description"
          type="textarea"
          :rows="2"
          placeholder="记录该凭据的权限范围、过期周期或使用指引"
        />
      </NFormItem>
    </NForm>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showCreate = false">取消</NButton>
        <NButton type="primary" @click="doCreate">
          {{ editingId ? '保存变更' : '安全加密存储' }}
        </NButton>
      </NSpace>
    </template>
  </NModal>

  <!-- View Modal -->
  <NModal v-model:show="showView" preset="card" title="凭据详情" style="width: 560px">
    <template v-if="viewingCred">
      <NDescriptions :column="2" label-placement="left" bordered size="small">
        <NDescriptionsItem label="凭据名称" :span="2">
          <strong>{{ viewingCred.name }}</strong>
        </NDescriptionsItem>
        <NDescriptionsItem label="凭据类型">
          <NTag size="small" round :type="typeMeta(viewingCred.type).tagType">
            {{ typeMeta(viewingCred.type).label }}
          </NTag>
        </NDescriptionsItem>
        <NDescriptionsItem label="存储安全状态">
          <NTag size="small" round type="success">
            <template #icon><NIcon :component="ShieldCheckmarkOutline" /></template>
            AES-256 密文封存
          </NTag>
        </NDescriptionsItem>
        <NDescriptionsItem label="用户名">
          <span v-if="viewingCred.username" class="mono-text">{{ viewingCred.username }}</span>
          <span v-else class="muted">-</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="关联主机">
          <span v-if="viewingCred.host">{{ viewingCred.host }}</span>
          <span v-else class="muted">未关联指定主机</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="密钥保护" :span="2">
          <div v-if="revealedSecret" class="secret-reveal">
            <pre class="secret-revealed">{{ revealedSecret }}</pre>
            <NSpace :size="6" style="margin-top: 6px">
              <NButton size="tiny" secondary @click="() => copyText(revealedSecret, '密钥')">
                <template #icon><NIcon :component="CopyOutline" /></template>
                复制
              </NButton>
              <NButton size="tiny" quaternary @click="hideSecret">隐藏</NButton>
            </NSpace>
          </div>
          <NSpace v-else align="center" :size="8">
            <span class="secret-shielded">••••••••••••••••</span>
            <NButton size="tiny" secondary :loading="revealingSecret" @click="revealSecret">
              查看密钥
            </NButton>
          </NSpace>
        </NDescriptionsItem>
        <NDescriptionsItem label="创建时间">
          <span class="time-cell">{{ fmtDateTime(viewingCred.created_at) }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="最后更新">
          <span class="time-cell">{{ fmtDateTime(viewingCred.updated_at) }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="备注说明" :span="2">
          <span v-if="viewingCred.description">{{ viewingCred.description }}</span>
          <span v-else class="muted">暂无备注说明</span>
        </NDescriptionsItem>
      </NDescriptions>

      <div class="view-notice">
        <NIcon size="14" color="#6366f1"><ShieldCheckmarkOutline /></NIcon>
        <span>密钥静态加密存储，常规接口仅返回掩码。点击「查看密钥」会按需拉取明文，每次查看都会记录一条高危操作审计；在「在线终端」「批量执行」等场景中，系统仍在服务端内部加载凭据。</span>
      </div>
    </template>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showView = false">关闭</NButton>
        <NButton
          v-if="viewingCred"
          type="primary"
          @click="() => { const c = viewingCred; showView = false; if (c) openEdit(c); }"
        >
          <template #icon><NIcon :component="CreateOutline" /></template>
          编辑凭据
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.cred-view {
  gap: 14px;
  overflow: hidden;
}

.cred-header {
  flex-shrink: 0;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;

  .header-left {
    display: flex;
    flex-direction: column;
    gap: 4px;

    .page-title {
      margin: 0;
      font-size: 18px;
      font-weight: 600;
      color: var(--text-primary);
    }
  }

  .header-right {
    flex-shrink: 0;
  }
}

.filter-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;

  .filter-left {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }

  .filter-right {
    font-size: 12px;
  }
}

.table-card {
  :deep(.n-card-content) {
    padding: 0;
  }
}

:deep(.cred-title-cell) {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 2px 0;

  .cred-name-row {
    display: flex;
    align-items: center;
    gap: 6px;

    .lock-icon {
      color: #f59e0b;
      flex-shrink: 0;
    }

    .cred-name-text {
      font-weight: 600;
      color: var(--text-primary);
    }
  }

  .cred-desc-text {
    font-size: 11.5px;
    color: var(--text-secondary);
    padding-left: 21px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 320px;
  }
}

:deep(.copyable-cell) {
  display: flex;
  align-items: center;
  gap: 4px;

  .cell-copy-btn {
    opacity: 0;
    transition: opacity 0.15s ease;
  }

  &:hover .cell-copy-btn {
    opacity: 1;
  }
}

:deep(.host-pill) {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 8px;
  border-radius: 4px;
  background-color: var(--code-box-bg);
  color: var(--text-primary);
  font-size: 12px;

  .host-icon {
    color: var(--text-secondary);
  }

  .host-name {
    font-family: var(--font-mono, monospace);
  }
}

:deep(.mono-text) {
  font-family: var(--font-mono, monospace);
  font-size: 12.5px;
}

:deep(.time-cell) {
  font-size: 12px;
  color: var(--text-secondary);
}

.muted,
:deep(.muted) {
  color: var(--text-secondary);
  font-size: 12.5px;
}

.secret-shielded {
  font-family: var(--font-mono, monospace);
  color: #10b981;
  letter-spacing: 1px;
  font-size: 12px;
}

.secret-suffix {
  display: inline-flex;
  margin-right: 5px;
}

.secret-reveal {
  width: 100%;
}

.secret-revealed {
  margin: 0;
  padding: 8px 10px;
  max-height: 220px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-primary);
  background-color: rgba(127, 127, 127, 0.08);
  border: 1px solid rgba(127, 127, 127, 0.16);
  border-radius: 6px;
}

.view-notice {
  margin-top: 14px;
  padding: 10px 12px;
  border-radius: 6px;
  background-color: rgba(99, 102, 241, 0.05);
  border: 1px solid rgba(99, 102, 241, 0.15);
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
}

// 批量操作栏（NetworkList.vue 同款）
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 8px 12px;
  flex-shrink: 0;

  .batch-info {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .batch-summary {
    font-size: 12px;
    color: var(--text-secondary);
  }
}

/* 批量执行中的行：操作列整体置灰禁用，防单机操作与批量并发冲突（DESIGN.md §9.22） */
:deep(.batch-row-busy [data-col-key="actions"]) {
  pointer-events: none;
  opacity: 0.5;
}

// 批量删除确认弹窗：目标清单内部滚动，行数多也不撑出视口
.batch-confirm-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.batch-target-list {
  max-height: 180px;
  overflow-y: auto;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 6px 10px;

  .batch-target-row {
    padding: 3px 0;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;

    &.muted {
      color: var(--text-secondary);
    }
  }
}

.field-hint {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
  margin: 0;
}

// 批量执行行内进度（NetworkList .op-progress 同款色板；render 函数由
// NDataTable 在子组件渲染上下文里调用，样式须走 :deep 穿透）。
:deep(.batch-op-progress) {
  font-size: 12px;
  max-width: 100%;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;

  &.is-queued,
  &.is-running {
    color: #3b82f6;
  }

  &.is-done {
    color: #059669;
  }

  &.is-failed {
    color: #dc2626;
  }
}
</style>
