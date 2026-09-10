<script setup lang="ts">
import { onMounted, ref, computed, h } from 'vue'
import {
  NCard, NButton, NSpace, NTag, NModal, NForm, NFormItem, NDataTable,
  NInput, NSelect, NPopconfirm, useMessage, NIcon, NEmpty,
  type DataTableColumns,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, KeyOutline, TrashOutline,
} from '@vicons/ionicons5'
import type { Role, UserRecord } from '../../api/types'
import { listUsers, createUser, updateUserRole, deleteUser, resetPassword } from '../../api/users'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'UserList' })

const message = useMessage()
const auth = useAuthStore()

const users = ref<UserRecord[]>([])
const loading = ref(false)

const showCreate = ref(false)
const showReset = ref(false)
const form = ref({ username: '', password: '', role: 'operator' as Role })
const resetTarget = ref('')
const newPassword = ref('')

const userCount = computed(() => users.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount: userCount })

const roleOptions = [
  { label: '管理员 (admin)', value: 'admin' },
  { label: '操作员 (operator)', value: 'operator' },
  { label: '只读 (viewer)', value: 'viewer' },
]

function roleTagType(role: Role) {
  if (role === 'admin') return 'error' as const
  if (role === 'operator') return 'success' as const
  return 'default' as const
}

function roleLabel(role: Role) {
  if (role === 'admin') return '管理员'
  if (role === 'operator') return '操作员'
  return '只读'
}

const isAdmin = () => auth.role === 'admin'

const columns = computed<DataTableColumns<UserRecord>>(() => [
  {
    title: '用户名',
    key: 'username',
    minWidth: 180,
    render: (u) =>
      h(NSpace, { size: 6, align: 'center', wrapItem: false }, {
        default: () => [
          h('span', { class: 'username-text' }, u.username),
          u.username === auth.user?.username
            ? h(NTag, { size: 'tiny', type: 'info', round: true }, { default: () => '我' })
            : null,
        ],
      }),
  },
  {
    title: '角色',
    key: 'role',
    width: 190,
    render: (u) =>
      isAdmin() && u.username !== auth.user?.username
        ? h(NSelect, {
            value: u.role,
            options: roleOptions,
            size: 'small',
            style: 'width: 160px',
            'onUpdate:value': (v: Role) => doRoleChange(u, v),
          })
        : h(NTag, { type: roleTagType(u.role), size: 'small', round: true }, { default: () => roleLabel(u.role) }),
  },
  {
    title: '创建时间',
    key: 'created_at',
    width: 180,
    render: (u) => h('span', { class: 'muted' }, u.created_at?.slice(0, 19).replace('T', ' ') || '-'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    render: (u) =>
      h(NSpace, { size: 4, wrapItem: false }, {
        default: () => [
          isAdmin()
            ? h(NButton, { size: 'small', quaternary: true, onClick: () => openReset(u.username) }, {
                icon: () => h(NIcon, { component: KeyOutline }),
                default: () => '重置密码',
              })
            : null,
          isAdmin() && u.username !== auth.user?.username
            ? h(NPopconfirm, { onPositiveClick: () => doDelete(u.username) }, {
                trigger: () => h(NButton, { size: 'small', quaternary: true, type: 'error' }, {
                  icon: () => h(NIcon, { component: TrashOutline }),
                  default: () => '删除',
                }),
                default: () => `确认删除用户 ${u.username}？`,
              })
            : null,
        ],
      }),
  },
])

async function refresh() {
  loading.value = true
  try {
    const res = await listUsers()
    users.value = res.data || []
    resetPage()
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

async function doCreate() {
  if (!form.value.username || !form.value.password) {
    message.warning('请填写用户名和密码')
    return
  }
  try {
    await createUser(form.value.username, form.value.password, form.value.role)
    message.success('用户创建成功')
    showCreate.value = false
    form.value = { username: '', password: '', role: 'operator' }
    refresh()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doRoleChange(u: UserRecord, role: Role) {
  try {
    await updateUserRole(u.username, role)
    message.success('角色已更新')
    refresh()
  } catch (e: any) {
    message.error(e.message)
  }
}

function openReset(username: string) {
  resetTarget.value = username
  newPassword.value = ''
  showReset.value = true
}

async function doReset() {
  if (!newPassword.value) {
    message.warning('请输入新密码')
    return
  }
  try {
    await resetPassword(resetTarget.value, newPassword.value)
    message.success('密码已重置')
    showReset.value = false
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doDelete(username: string) {
  try {
    await deleteUser(username)
    message.success('用户已删除')
    refresh()
  } catch (e: any) {
    message.error(e.message)
  }
}

onMounted(() => {
  if (isAdmin()) refresh()
  else message.warning('需要管理员权限')
})
</script>

<template>
  <div class="user-view page-flex-column">
    <div class="user-toolbar">
      <h2 class="page-title">用户管理</h2>
      <NSpace align="center" :size="12">
        <span class="muted">共 {{ userCount }} 个用户</span>
        <NButton @click="refresh" :loading="loading">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
        <NButton type="primary" @click="showCreate = true" v-if="isAdmin()">
          <template #icon><NIcon :component="AddCircleOutline" /></template>
          新增用户
        </NButton>
      </NSpace>
    </div>

    <NCard class="table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="users"
        :pagination="pagination"
        :loading="loading"
        :bordered="false"
        size="small"
        :row-key="(u: UserRecord) => u.id"
        :scroll-x="760"
      >
        <template #empty>
          <NEmpty description="暂无用户" />
        </template>
      </NDataTable>
    </NCard>
  </div>

  <!-- Create user modal -->
  <NModal v-model:show="showCreate" preset="card" title="新增用户" style="width: 460px">
    <NForm label-placement="top">
      <NFormItem label="用户名">
        <NInput v-model:value="form.username" placeholder="登录用户名" />
      </NFormItem>
      <NFormItem label="密码">
        <NInput v-model:value="form.password" type="password" placeholder="初始密码" show-password-on="click" />
      </NFormItem>
      <NFormItem label="角色">
        <NSelect v-model:value="form.role" :options="roleOptions" />
      </NFormItem>
    </NForm>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showCreate = false">取消</NButton>
        <NButton type="primary" @click="doCreate">创建</NButton>
      </NSpace>
    </template>
  </NModal>

  <!-- Reset password modal -->
  <NModal v-model:show="showReset" preset="card" title="重置密码" style="width: 420px">
    <p>为用户 <code>{{ resetTarget }}</code> 设置新密码：</p>
    <NInput v-model:value="newPassword" type="password" placeholder="新密码" show-password-on="click" />
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showReset = false">取消</NButton>
        <NButton type="primary" @click="doReset">重置</NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.user-view {
  gap: 16px;
  overflow: hidden;
}
.user-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.page-title {
  margin: 0;
  font-size: 18px;
}
:deep(.username-text) {
  font-weight: 600;
}
.muted,
:deep(.muted) {
  color: var(--text-secondary);
  font-size: 13px;
}
:deep(.n-data-table__pagination) {
  padding: 12px 16px;
  margin: 0 !important;
  border-top: 1px solid var(--border-color);
  box-sizing: border-box;
}
</style>
