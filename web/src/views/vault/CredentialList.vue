<script setup lang="ts">
import { onMounted, ref, computed, h } from 'vue'
import {
  NCard, NButton, NSpace, NTag, NModal, NForm, NFormItem, NDataTable,
  NInput, NSelect, NPopconfirm, NEmpty, useMessage, NIcon, NTooltip,
  NDescriptions, NDescriptionsItem,
  type DataTableColumns,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, TrashOutline, LockClosedOutline, EyeOutline,
  CreateOutline, KeyOutline, CodeSlashOutline, ServerOutline, ShieldCheckmarkOutline,
  CopyOutline, SearchOutline,
} from '@vicons/ionicons5'
import type { Credential } from '../../api/vault'
import { listCredentials, createCredential, updateCredential, deleteCredential, getCredential } from '../../api/vault'
import { listHosts } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import { useTablePagination } from '../../composables/useTablePagination'

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
  { label: 'API Key', value: 'api_key' },
  { label: '数据库密码', value: 'database' },
  { label: '自定义', value: 'custom' },
]

const formTypeOptions = [
  { label: 'SSH 密码', value: 'ssh_password' },
  { label: 'API Key', value: 'api_key' },
  { label: '数据库密码', value: 'database' },
  { label: '自定义', value: 'custom' },
]

function typeMeta(t: string): { label: string; tagType: 'default' | 'info' | 'success' | 'warning' | 'error'; icon: any } {
  switch (t) {
    case 'ssh_password':
      return { label: 'SSH 密码', tagType: 'info', icon: KeyOutline }
    case 'api_key':
      return { label: 'API Key', tagType: 'warning', icon: CodeSlashOutline }
    case 'database':
      return { label: '数据库密码', tagType: 'success', icon: ServerOutline }
    case 'custom':
    default:
      return { label: t === 'password' ? '登录密码' : (t || '自定义'), tagType: 'default', icon: LockClosedOutline }
  }
}

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
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

const columns = computed<DataTableColumns<Credential>>(() => [
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
    render: (c) => h('span', { class: 'time-cell' }, fmtTime(c.updated_at)),
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

async function refresh() {
  loading.value = true
  try {
    const [cList, hList] = await Promise.all([
      listCredentials(),
      listHosts().catch(() => ({ data: [] })),
    ])
    creds.value = cList || []
    hostOptions.value = (hList.data || []).map((h: any) => ({
      label: `${h.hostname} (${h.internal_ip || h.public_ip || h.id.slice(0, 8)})`,
      value: h.hostname || h.id,
    }))
    resetPage()
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

async function viewCred(id: string) {
  try {
    viewingCred.value = await getCredential(id)
    showView.value = true
  } catch (e: any) {
    message.error(e.message || '获取凭据详情失败')
  }
}

async function doDelete(id: string) {
  try {
    await deleteCredential(id)
    message.success('凭据已彻底销毁')
    refresh()
  } catch (e: any) {
    message.error(e.message || '删除凭据失败')
  }
}

onMounted(refresh)
</script>

<template>
  <div class="cred-view page-flex-column">
    <!-- Header: 标题 + 刷新/新建 -->
    <div class="cred-header">
      <div class="header-left">
        <div class="title-wrap">
          <h2 class="page-title">凭据金库</h2>
          <NTag size="small" :bordered="false" round type="info">AES-256-GCM 硬件加密</NTag>
        </div>
      </div>
      <div class="header-right">
        <NSpace align="center" :size="10">
          <NButton :loading="loading" @click="refresh">
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
        :scroll-x="960"
      >
        <template #empty>
          <NEmpty description="暂无符合条件的凭据。点击右上角「新增凭据」添加首个加密凭据。" />
        </template>
      </NDataTable>
    </NCard>
  </div>

  <!-- Create / Edit Modal -->
  <NModal
    v-model:show="showCreate"
    preset="card"
    :title="editingId ? '编辑凭据' : '新增安全凭据'"
    style="width: 540px"
  >
    <NForm label-placement="top">
      <NFormItem label="凭据名称" required>
        <NInput v-model:value="form.name" placeholder="例如：生产数据库 Root 密码 / AWS 生产集群 AK" />
      </NFormItem>

      <NFormItem label="凭据类型" required>
        <NSelect v-model:value="form.type" :options="formTypeOptions" />
      </NFormItem>

      <NFormItem label="用户名 / 账号 (可选)">
        <NInput v-model:value="form.username" placeholder="例如：root / admin / ubuntu" />
      </NFormItem>

      <NFormItem :label="editingId ? '重置密钥 / 密码 (留空保持不变)' : '密钥 / 密码'" :required="!editingId">
        <NInput
          v-model:value="form.secret"
          type="password"
          show-password-on="click"
          :placeholder="editingId ? '留空保持原密钥不变' : '请输入密码、Private Key 或 API Token'"
        >
          <template #suffix>
            <NButton text size="tiny" type="primary" @click="generateRandomPassword">
              随机生成
            </NButton>
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
          <NSpace align="center" :size="8">
            <span class="secret-shielded">•••••••••••••••• (受安全策略保护，不回传浏览器)</span>
          </NSpace>
        </NDescriptionsItem>
        <NDescriptionsItem label="创建时间">
          <span class="time-cell">{{ fmtTime(viewingCred.created_at) }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="最后更新">
          <span class="time-cell">{{ fmtTime(viewingCred.updated_at) }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="备注说明" :span="2">
          <span v-if="viewingCred.description">{{ viewingCred.description }}</span>
          <span v-else class="muted">暂无备注说明</span>
        </NDescriptionsItem>
      </NDescriptions>

      <div class="view-notice">
        <NIcon size="14" color="#6366f1"><ShieldCheckmarkOutline /></NIcon>
        <span>此凭据已在主控端密钥环通过高强度派生密钥加密。当您在「在线终端」或「批量执行」中使用它时，系统将在服务端直接通过鉴权管道加载，保护凭据不暴露在网络流量与控制台前端中。</span>
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

    .title-wrap {
      display: flex;
      align-items: center;
      gap: 8px;

      .page-title {
        margin: 0;
        font-size: 18px;
        font-weight: 600;
        color: var(--text-primary);
      }
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
</style>
