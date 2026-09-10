<script setup lang="ts">
import { computed, onMounted, ref, h } from 'vue'
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

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
}

const columns = computed<DataTableColumns<HostGroup>>(() => [
  { title: '分组名称', key: 'name', minWidth: 150, render: (g) => h('span', { class: 'group-name' }, g.name) },
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
  { title: '创建时间', key: 'created_at', width: 170, render: (g) => h('span', { class: 'muted' }, fmtTime(g.created_at)) },
  {
    title: '操作',
    key: 'actions',
    width: 260,
    render: (g) =>
      h(NSpace, { size: 4, wrapItem: false }, {
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
    await refresh()
  } catch (e: any) {
    message.error(e.message || '删除分组失败')
  }
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
  hostGroup.value ? hosts.value.filter((x) => x.group === hostGroup.value!.name) : [],
)
const candidateHosts = computed(() =>
  hostGroup.value ? hosts.value.filter((x) => x.group !== hostGroup.value!.name) : [],
)

// assignHost moves a host into (group name) or out of ('') the current group.
async function assignHost(host: Host, group: string) {
  try {
    await setHostGroup(host.id, group)
    host.group = group
    message.success(group ? `${host.hostname} 已加入 ${group}` : `${host.hostname} 已移出分组`)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '设置主机分组失败')
  }
}

onMounted(() => {
  refresh()
})
</script>

<template>
  <div class="group-view page-flex-column">
    <div class="group-toolbar">
      <h2 class="page-title">分组与权限</h2>
      <NSpace align="center" :size="12">
        <span class="muted">共 {{ groupCount }} 个分组</span>
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

    <NCard :bordered="false" class="table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="groups"
        :pagination="pagination"
        :loading="loading"
        :bordered="false"
        size="small"
        :row-key="(g: HostGroup) => g.id"
        :scroll-x="1000"
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
            <NTable v-else :single-line="false" size="small">
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
                  <td class="muted">{{ fmtTime(gr.granted_at) }}</td>
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
                  <span class="host-name">{{ hostItem.hostname }}</span>
                  <NTag size="tiny" :bordered="false" :type="hostItem.status === 'online' ? 'success' : 'default'">
                    {{ hostItem.status === 'online' ? '在线' : '离线' }}
                  </NTag>
                  <NButton
                    size="tiny"
                    quaternary
                    type="error"
                    :disabled="!isAdmin"
                    @click="assignHost(hostItem, '')"
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
                  <span class="host-name">{{ hostItem.hostname }}</span>
                  <NTag v-if="hostItem.group" size="tiny" :bordered="false">{{ hostItem.group }}</NTag>
                  <NTag v-else size="tiny" :bordered="false" type="warning">未分组</NTag>
                  <NButton
                    size="tiny"
                    secondary
                    type="primary"
                    :disabled="!isAdmin || !hostGroup"
                    @click="assignHost(hostItem, hostGroup!.name)"
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
}
.group-name,
:deep(.group-name) {
  font-weight: 600;
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
</style>
