<script setup lang="ts">
import { computed, onActivated, onMounted, ref, watch, h } from 'vue'
import {
  NCard, NButton, NSpace, NTable, NTag, NModal, NForm, NFormItem, NDataTable,
  NInput, NSelect, NPopconfirm, NEmpty, NIcon, NDrawer, NDrawerContent,
  NSpin, useMessage, type DataTableColumns,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, TrashOutline, CreateOutline,
  PeopleOutline, ServerOutline,
} from '@vicons/ionicons5'
import {
  listGroups, createGroup, updateGroup, deleteGroup,
  listGroupUsers, setGroupUser, revokeGroupUser,
  type GrantRole, type GroupGrant, type HostGroup,
} from '../../api/groups'
import { listUsers } from '../../api/users'
import { listHosts, setHostGroup } from '../../api/hosts'
import type { Host, UserRecord } from '../../api/types'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { useIsMobile } from '../../composables/useIsMobile'
import { fmtDateTime } from '../../utils/time'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'GroupList' })

const message = useMessage()
const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')

const groups = ref<HostGroup[]>([])
const loading = ref(false)

// Create / edit form state.
const showForm = ref(false)
const editingId = ref<string | null>(null)
const form = ref({ name: '', description: '', parent_id: '' })

// Per-group user authorization drawer state.
const showGrants = ref(false)
const grantGroup = ref<HostGroup | null>(null)
const grants = ref<GroupGrant[]>([])
const grantsLoading = ref(false)
const allUsers = ref<UserRecord[]>([])
const newGrantUser = ref<string | null>(null)
const newGrantRole = ref<GrantRole>('operate')

// Host membership drawer state.
const showHosts = ref(false)
const hostGroup = ref<HostGroup | null>(null)
const hosts = ref<Host[]>([])
const hostsLoading = ref(false)

const groupCount = computed(() => groups.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount: groupCount })

// ---- 批量删除（范式见 NetworkList.vue） ----
// 勾选按分组 id 存、跨页保留；执行期 3 路并发、逐行显示进度、失败可重试。
// 删除分组是 admin 专属操作（单机删除按钮同款准入），非 admin 的行不可选。
const selectedGroupIds = ref<Set<string>>(new Set())
const BATCH_CONCURRENCY = 3
type BatchPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
const showBatchDeleteModal = ref(false)

const isMobile = useIsMobile()
// 移动端隐藏次要列（上级分组/描述/创建时间），只留分组名称、计数与操作，
// 横向滚动范围随之收窄（scroll-x 1000 → 640）。
// 无 key 的列（如批量勾选列）一律保留，key 过滤只针对具名数据列。
const visibleColumns = computed(() =>
  isMobile.value
    ? columns.value.filter((c) => !('key' in c) || !['parent_id', 'description', 'created_at'].includes(c.key as string))
    : columns.value,
)
// 移动端分页切紧凑形态（仅上一页/页码输入/下一页），避免每页条数选择器撑出视口。
const tablePagination = computed(() => ({ ...pagination, simple: isMobile.value }))

const roleOptions = [
  { label: '可操作 (operate)', value: 'operate' },
  { label: '只读 (view)', value: 'view' },
]

const parentOptions = computed(() => [
  { label: '（无上级分组）', value: '' },
  ...groups.value
    .filter((g) => g.id !== editingId.value)
    .map((g) => ({ label: g.name, value: g.id })),
])

const userOptions = computed(() =>
  allUsers.value
    .filter((u) => !grants.value.some((g) => g.username === u.username))
    .map((u) => ({ label: `${u.username} (${u.role})`, value: u.username })),
)

function groupNameById(id?: string): string {
  if (!id) return '-'
  return groups.value.find((g) => g.id === id)?.name ?? id
}


const columns = computed<DataTableColumns<HostGroup>>(() => [
  {
    // 批量勾选列：NDataTable 复选框列取 40px（BatchExec.vue 同款口径；
    // DESIGN.md §9.20 的 28px 是 flex 行列表口径，复选框 + 单元格内边距放不下）。
    type: 'selection',
    width: 40,
    disabled: () => !isAdmin.value || batchRunning.value,
  },
  {
    title: '分组名称',
    key: 'name',
    minWidth: 150,
    render: (g) => h('div', null, [
      h('span', { class: 'group-name', title: g.name }, g.name),
      batchProgressNode(g),
    ]),
  },
  { title: '上级分组', key: 'parent_id', width: 130, render: (g) => h('span', { class: 'muted' }, groupNameById(g.parent_id)) },
  { title: '描述', key: 'description', minWidth: 180, ellipsis: { tooltip: true }, render: (g) => h('span', { class: 'muted' }, g.description || '-') },
  {
    title: '主机数',
    key: 'host_count',
    width: 90,
    render: (g) => h(NTag, { size: 'small', bordered: false, type: g.host_count > 0 ? 'info' : 'default' }, { default: () => String(g.host_count) }),
  },
  {
    title: '授权用户',
    key: 'user_count',
    width: 100,
    render: (g) => h(NTag, { size: 'small', bordered: false, type: g.user_count > 0 ? 'success' : 'default' }, { default: () => String(g.user_count) }),
  },
  { title: '创建时间', key: 'created_at', width: 170, render: (g) => h('span', { class: 'muted' }, fmtDateTime(g.created_at)) },
  {
    title: '操作',
    key: 'actions',
    width: 290,
    render: (g) =>
      h(NSpace, { size: 4, wrap: false, wrapItem: false }, {
        default: () => [
          h(NButton, { size: 'small', quaternary: true, onClick: () => openHosts(g) }, {
            icon: () => h(NIcon, { component: ServerOutline }),
            default: () => '主机',
          }),
          h(NButton, { size: 'small', quaternary: true, disabled: !isAdmin.value, onClick: () => openGrants(g) }, {
            icon: () => h(NIcon, { component: PeopleOutline }),
            default: () => '授权',
          }),
          h(NButton, { size: 'small', quaternary: true, disabled: !isAdmin.value, onClick: () => openEdit(g) }, {
            icon: () => h(NIcon, { component: CreateOutline }),
            default: () => '编辑',
          }),
          isAdmin.value
            ? h(NPopconfirm, { onPositiveClick: () => doDelete(g) }, {
                trigger: () => h(NButton, { size: 'small', quaternary: true, type: 'error' }, {
                  icon: () => h(NIcon, { component: TrashOutline }),
                }),
                default: () => `删除分组 ${g.name}？该分组下 ${g.host_count} 台主机将被移出分组，用户授权同时失效。`,
              })
            : null,
        ],
      }),
  },
])

async function refresh() {
  loading.value = true
  try {
    groups.value = await listGroups()
    // 刷新清掉上一批的旧进度；批量执行中不清除（runBatch 在执行完才刷新，
    // 此时 batchRunning 仍为 true，进度得以保留到汇总展示）。
    if (!batchRunning.value) {
      opStatus.value = {}
      batchSummary.value = null
    }
    resetPage()
  } catch (e: any) {
    message.error(e.message || '加载分组失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = null
  form.value = { name: '', description: '', parent_id: '' }
  showForm.value = true
}

function openEdit(g: HostGroup) {
  editingId.value = g.id
  form.value = { name: g.name, description: g.description, parent_id: g.parent_id || '' }
  showForm.value = true
}

async function submitForm() {
  const name = form.value.name.trim()
  if (!name) {
    message.warning('请填写分组名称')
    return
  }
  const payload = {
    name,
    description: form.value.description.trim(),
    parent_id: form.value.parent_id || undefined,
  }
  try {
    if (editingId.value) {
      const res = await updateGroup(editingId.value, payload)
      message.success(
        res.hosts_migrated > 0
          ? `分组已更新，${res.hosts_migrated} 台主机已迁移到新分组名`
          : '分组已更新',
      )
    } else {
      await createGroup(payload)
      message.success('分组已创建')
    }
    showForm.value = false
    await refresh()
  } catch (e: any) {
    message.error(e.message || '保存分组失败')
  }
}

async function doDelete(g: HostGroup) {
  try {
    const res = await deleteGroup(g.id)
    message.success(
      res.hosts_cleared > 0
        ? `分组已删除，${res.hosts_cleared} 台主机已移出该分组`
        : '分组已删除',
    )
    selectedGroupIds.value.delete(g.id)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '删除分组失败')
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

const selectedGroups = computed(() => groups.value.filter((g) => selectedGroupIds.value.has(g.id)))

// 当前页分组（前端分页切片）：勾选同步以当前页为基准合并，跨页已选保持不变。
const pagedGroups = computed(() => {
  const start = (pagination.page - 1) * pagination.pageSize
  return groups.value.slice(start, start + pagination.pageSize)
})

function onGroupChecked(keys: (string | number)[]) {
  const pageIds = new Set(pagedGroups.value.map((g) => g.id))
  const next = new Set([...selectedGroupIds.value].filter((id) => !pageIds.has(id)))
  keys.forEach((k) => {
    if (pageIds.has(String(k))) next.add(String(k))
  })
  selectedGroupIds.value = next
}

// 分组在列表里消失（被删除）后清理对应的勾选与进度。
watch(groups, (list) => {
  const alive = new Set(list.map((g) => g.id))
  selectedGroupIds.value = new Set([...selectedGroupIds.value].filter((id) => alive.has(id)))
  Object.keys(opStatus.value).forEach((id) => {
    if (!alive.has(id)) delete opStatus.value[id]
  })
})

async function runBatchDelete(targets: HostGroup[]) {
  if (targets.length === 0) {
    message.warning('所选分组中没有可删除的')
    return
  }
  batchRunning.value = true
  batchSummary.value = null
  opStatus.value = {}
  targets.forEach((g) => {
    opStatus.value[g.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...targets]
  async function worker() {
    while (queue.length > 0) {
      const group = queue.shift()!
      opStatus.value[group.id] = { phase: 'running' }
      try {
        // 复用单机删除接口：改名/删分组时批量改写各主机记录的分组列表
        // 是服务端已有行为（见 AGENTS.md B.3），审计走既有单机接口的审计。
        await deleteGroup(group.id)
        opStatus.value[group.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[group.id] = { phase: 'failed', message: e.message || '执行出错' }
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
    message.success(`批量删除完成：成功 ${done} 个分组`)
  } else {
    message.warning(`批量删除完成：成功 ${done} 个，失败 ${failed} 个，可点「重试失败」只重跑失败的分组`)
  }
}

function retryBatchFailed() {
  if (!batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  runBatchDelete(groups.value.filter((g) => failedIds.includes(g.id)))
}

function confirmBatchDelete() {
  showBatchDeleteModal.value = false
  runBatchDelete(selectedGroups.value)
}

function clearBatchSelection() {
  selectedGroupIds.value = new Set()
  opStatus.value = {}
  batchSummary.value = null
}

// ---------- 分组用户授权 ----------
async function openGrants(g: HostGroup) {
  grantGroup.value = g
  showGrants.value = true
  grantsLoading.value = true
  newGrantUser.value = null
  newGrantRole.value = 'operate'
  try {
    const [gr, us] = await Promise.all([listGroupUsers(g.id), listUsers()])
    grants.value = gr
    allUsers.value = us.data || []
  } catch (e: any) {
    message.error(e.message || '加载授权信息失败')
  } finally {
    grantsLoading.value = false
  }
}

async function addGrant() {
  const g = grantGroup.value
  if (!g) return
  if (!newGrantUser.value) {
    message.warning('请选择要授权的用户')
    return
  }
  try {
    await setGroupUser(g.id, newGrantUser.value, newGrantRole.value)
    message.success('授权已保存')
    newGrantUser.value = null
    grants.value = await listGroupUsers(g.id)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '授权失败')
  }
}

async function changeGrantRole(grant: GroupGrant, role: GrantRole) {
  const g = grantGroup.value
  if (!g) return
  try {
    await setGroupUser(g.id, grant.username, role)
    message.success('权限已更新')
    grants.value = await listGroupUsers(g.id)
  } catch (e: any) {
    message.error(e.message || '更新权限失败')
  }
}

async function removeGrant(grant: GroupGrant) {
  const g = grantGroup.value
  if (!g) return
  try {
    await revokeGroupUser(g.id, grant.username)
    message.success('授权已撤销')
    grants.value = await listGroupUsers(g.id)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '撤销授权失败')
  }
}

// ---------- 分组主机关联 ----------
async function openHosts(g: HostGroup) {
  hostGroup.value = g
  showHosts.value = true
  hostsLoading.value = true
  try {
    const res = await listHosts()
    hosts.value = res.data || []
  } catch (e: any) {
    message.error(e.message || '加载主机失败')
  } finally {
    hostsLoading.value = false
  }
}

const memberHosts = computed(() =>
  hostGroup.value ? hosts.value.filter((x) => (x.groups || []).includes(hostGroup.value!.name)) : [],
)
const candidateHosts = computed(() =>
  hostGroup.value ? hosts.value.filter((x) => !(x.groups || []).includes(hostGroup.value!.name)) : [],
)

// assignHost adds the host to the current group, or removes it from the
// current group when remove is set; the host's other group memberships
// are untouched (a host may belong to several groups).
async function assignHost(host: Host, remove = false) {
  const name = hostGroup.value?.name
  if (!name) return
  const current = host.groups || []
  const next = remove ? current.filter((g) => g !== name) : current.includes(name) ? current : [...current, name]
  try {
    await setHostGroup(host.id, next)
    host.groups = next
    message.success(remove ? `${host.hostname} 已移出 ${name}` : `${host.hostname} 已加入 ${name}`)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '设置主机分组失败')
  }
}

onMounted(() => {
  refresh()
})
onActivated(() => {
  refresh()
})
</script>

<template>
  <div class="group-view page-flex-column">
    <div class="group-toolbar">
      <h2 class="page-title">分组与权限</h2>
      <NSpace align="center" :size="12">
        <span class="muted group-count-text">共 {{ groupCount }} 个分组</span>
        <NButton :loading="loading" @click="refresh">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
        <NButton v-if="isAdmin" type="primary" @click="openCreate">
          <template #icon><NIcon :component="AddCircleOutline" /></template>
          新增分组
        </NButton>
      </NSpace>
    </div>

    <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐行显示进度 -->
    <div v-if="isAdmin && selectedGroupIds.size > 0" class="batch-bar">
      <span class="batch-info">已选 {{ selectedGroupIds.size }} 个分组</span>
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

    <NCard :bordered="false" class="table-flex-fill">
      <NDataTable
        flex-height
        :columns="visibleColumns"
        :data="groups"
        :pagination="tablePagination"
        :loading="loading"
        :bordered="false"
        size="small"
        :row-key="(g: HostGroup) => g.id"
        :checked-row-keys="[...selectedGroupIds]"
        :scroll-x="isMobile ? 640 : 1000"
        @update:checked-row-keys="onGroupChecked"
      >
        <template #empty>
          <NEmpty description="暂无分组。创建分组后可关联主机并给用户授权，实现主机访问隔离。" />
        </template>
      </NDataTable>
    </NCard>

    <!-- 新增/编辑分组 -->
    <NModal
      v-model:show="showForm"
      preset="card"
      :title="editingId ? '编辑分组' : '新增分组'"
      style="width: 480px"
    >
      <NForm label-placement="top">
        <NFormItem label="分组名称">
          <NInput v-model:value="form.name" placeholder="例如：生产环境 / 办公内网" />
        </NFormItem>
        <NFormItem label="上级分组">
          <NSelect v-model:value="form.parent_id" :options="parentOptions" />
        </NFormItem>
        <NFormItem label="描述">
          <NInput v-model:value="form.description" type="textarea" :rows="3" placeholder="可选" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showForm = false">取消</NButton>
          <NButton type="primary" @click="submitForm">{{ editingId ? '保存' : '创建' }}</NButton>
        </NSpace>
      </template>
    </NModal>
    <!-- 批量删除二次确认（DESIGN.md §9.17：破坏性确认在弹窗内就地展开） -->
    <NModal
      v-model:show="showBatchDeleteModal"
      preset="card"
      :title="`批量删除分组（${selectedGroups.length} 个）`"
      style="width: 520px"
    >
      <div class="batch-confirm-body">
        <div class="batch-target-list">
          <div
            v-for="g in selectedGroups.slice(0, 5)"
            :key="g.id"
            class="batch-target-row"
          >
            {{ g.name }}（{{ g.host_count }} 台主机）
          </div>
          <div v-if="selectedGroups.length > 5" class="batch-target-row muted">
            等 {{ selectedGroups.length - 5 }} 个…
          </div>
        </div>
        <p class="field-hint">删除分组后，该分组下主机将被移出分组，用户授权同时失效，请确认。</p>
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

    <!-- 分组用户授权 -->
    <NDrawer v-model:show="showGrants" :width="520" placement="right">
      <NDrawerContent :title="`授权用户 - ${grantGroup?.name ?? ''}`" closable>
        <NSpin :show="grantsLoading">
          <NSpace vertical :size="16">
            <NCard title="新增授权" size="small" :bordered="false">
              <NSpace vertical :size="10">
                <NSelect
                  v-model:value="newGrantUser"
                  :options="userOptions"
                  filterable
                  clearable
                  placeholder="选择用户"
                />
                <NSelect v-model:value="newGrantRole" :options="roleOptions" />
                <NButton type="primary" size="small" block @click="addGrant">添加授权</NButton>
              </NSpace>
            </NCard>

            <NEmpty
              v-if="!grants.length"
              description="该分组暂无授权用户。未被授权的用户默认可见全部主机。"
            />
            <!-- 单个分组的授权名单：条数天然有限，抽屉内不分页（AGENTS.md 8.2 例外） -->
            <!-- 授权抽屉 100vw 时表内 150px 角色下拉 + 时间列会撑出抽屉，包一层横向滚动 -->
            <div v-else class="grants-table-wrap">
            <NTable :single-line="false" size="small" style="min-width: 480px">
              <thead>
                <tr>
                  <th>用户</th>
                  <th>权限</th>
                  <th>授权时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="gr in grants" :key="gr.username">
                  <td class="group-name">{{ gr.username }}</td>
                  <td>
                    <NSelect
                      :value="gr.role"
                      :options="roleOptions"
                      size="small"
                      style="width: 150px"
                      @update:value="(v: GrantRole) => changeGrantRole(gr, v)"
                    />
                  </td>
                  <td class="muted">{{ fmtDateTime(gr.granted_at) }}</td>
                  <td>
                    <NPopconfirm @positive-click="removeGrant(gr)">
                      <template #trigger>
                        <NButton size="small" quaternary type="error">
                          <template #icon><NIcon :component="TrashOutline" /></template>
                        </NButton>
                      </template>
                      撤销 {{ gr.username }} 对该分组的访问权限？
                    </NPopconfirm>
                  </td>
                </tr>
              </tbody>
            </NTable>
            </div>
          </NSpace>
        </NSpin>
      </NDrawerContent>
    </NDrawer>

    <!-- 分组主机关联 -->
    <NDrawer v-model:show="showHosts" :width="560" placement="right">
      <NDrawerContent :title="`关联主机 - ${hostGroup?.name ?? ''}`" closable>
        <NSpin :show="hostsLoading">
          <NSpace vertical :size="16">
            <NCard :title="`已在本分组（${memberHosts.length}）`" size="small" :bordered="false">
              <NEmpty v-if="!memberHosts.length" description="该分组还没有主机" size="small" />
              <div v-else class="host-rows">
                <div v-for="hostItem in memberHosts" :key="hostItem.id" class="host-row">
                  <span class="host-name" :title="hostItem.hostname">{{ hostItem.hostname }}</span>
                  <NTag size="tiny" :bordered="false" :type="hostItem.status === 'online' ? 'success' : 'default'">
                    {{ hostItem.status === 'online' ? '在线' : '离线' }}
                  </NTag>
                  <NButton
                    size="tiny"
                    quaternary
                    type="error"
                    :disabled="!isAdmin"
                    @click="assignHost(hostItem, true)"
                  >
                    移出
                  </NButton>
                </div>
              </div>
            </NCard>

            <NCard title="可加入的主机" size="small" :bordered="false">
              <NEmpty v-if="!candidateHosts.length" description="没有可加入的主机" size="small" />
              <div v-else class="host-rows">
                <div v-for="hostItem in candidateHosts" :key="hostItem.id" class="host-row">
                  <span class="host-name" :title="hostItem.hostname">{{ hostItem.hostname }}</span>
                  <span v-if="hostItem.groups?.length" class="group-tags">
                    <NTag v-for="g in hostItem.groups" :key="g" size="tiny" :bordered="false">{{ g }}</NTag>
                  </span>
                  <NTag v-else size="tiny" :bordered="false" type="warning">未分组</NTag>
                  <NButton
                    size="tiny"
                    secondary
                    type="primary"
                    :disabled="!isAdmin || !hostGroup"
                    @click="assignHost(hostItem)"
                  >
                    加入
                  </NButton>
                </div>
              </div>
            </NCard>
          </NSpace>
        </NSpin>
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped lang="scss">
// 页面把滚动交给表格，自身必须 overflow: hidden（AGENTS.md 8.3），否则会出现
// 外层与数据区两层滚动条。
.group-view {
  gap: 16px;
  overflow: hidden;
}
.group-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: var(--text-primary);
}
.group-name,
:deep(.group-name) {
  font-weight: 600;
  // 长分组名单行省略不断字，全文走 title
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: middle;
}
.muted,
:deep(.muted) {
  color: var(--text-secondary);
  font-size: 13px;
}
.host-rows {
  display: flex;
  flex-direction: column;
  gap: 6px;
  max-height: 300px;
  overflow-y: auto;
}
// 抽屉内主机已有分组以多标签展示（同主机列表）。
.group-tags {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 4px;
  flex-shrink: 0;
}
.host-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  background: var(--bg-card-subtle);

  .host-name {
    flex: 1;
    min-width: 0;
    font-size: 13px;
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
}

/* ===================== 移动端适配（≤768px） ===================== */
@media (max-width: 768px) {
  .group-view {
    gap: 8px;
  }
  .group-toolbar {
    flex-wrap: wrap;
    gap: 8px;
  }
  // 窄屏只留标题与操作按钮，隐藏计数文字
  .group-count-text {
    display: none;
  }
  .grants-table-wrap {
    overflow-x: auto;
  }
}

// 批量操作栏（NetworkList.vue 同款）
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  background: var(--n-color-modal);
  border: 1px solid var(--n-border-color);
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
