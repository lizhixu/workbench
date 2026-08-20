<script setup lang="ts">
import { onMounted, ref } from 'vue'
import {
  NCard, NButton, NSpace, NTable, NTag, NModal, NForm, NFormItem,
  NInput, NSelect, NPopconfirm, useMessage, NIcon, NEmpty,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, KeyOutline, TrashOutline,
} from '@vicons/ionicons5'
import type { Role, UserRecord } from '../../api/types'
import { listUsers, createUser, updateUserRole, deleteUser, resetPassword } from '../../api/users'
import { useAuthStore } from '../../stores/auth'

const message = useMessage()
const auth = useAuthStore()

const users = ref<UserRecord[]>([])
const loading = ref(false)

const showCreate = ref(false)
const showReset = ref(false)
const form = ref({ username: '', password: '', role: 'operator' as Role })
const resetTarget = ref('')
const newPassword = ref('')

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

async function refresh() {
  loading.value = true
  try {
    const res = await listUsers()
    users.value = res.data || []
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

const isAdmin = () => auth.role === 'admin'

onMounted(() => {
  if (isAdmin()) refresh()
  else message.warning('需要管理员权限')
})
</script>

<template>
  <NSpace vertical :size="16">
    <NSpace align="center" justify="space-between">
      <h2 class="page-title">用户管理</h2>
      <NSpace>
        <NButton @click="refresh" :loading="loading">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
        <NButton type="primary" @click="showCreate = true" v-if="isAdmin()">
          <template #icon><NIcon :component="AddCircleOutline" /></template>
          新增用户
        </NButton>
      </NSpace>
    </NSpace>

    <NCard>
      <NEmpty v-if="users.length === 0 && !loading" description="暂无用户" />
      <NTable v-else :single-line="false">
        <thead>
          <tr>
            <th>用户名</th>
            <th>角色</th>
            <th>创建时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.id">
            <td class="username-cell">
              {{ u.username }}
              <NTag v-if="u.username === auth.user?.username" size="tiny" type="info" round>我</NTag>
            </td>
            <td>
              <NSelect
                v-if="isAdmin() && u.username !== auth.user?.username"
                :value="u.role"
                :options="roleOptions"
                size="small"
                style="width: 160px"
                @update:value="(v: Role) => doRoleChange(u, v)"
              />
              <NTag v-else :type="roleTagType(u.role)" size="small" round>{{ roleLabel(u.role) }}</NTag>
            </td>
            <td class="muted">{{ u.created_at?.slice(0, 19).replace('T', ' ') }}</td>
            <td>
              <NSpace :size="4">
                <NButton size="small" quaternary @click="openReset(u.username)" v-if="isAdmin()">
                  <template #icon><NIcon :component="KeyOutline" /></template>
                  重置密码
                </NButton>
                <NPopconfirm
                  v-if="isAdmin() && u.username !== auth.user?.username"
                  @positive-click="doDelete(u.username)"
                >
                  <template #trigger>
                    <NButton size="small" quaternary type="error">
                      <template #icon><NIcon :component="TrashOutline" /></template>
                      删除
                    </NButton>
                  </template>
                  确认删除用户 {{ u.username }}？
                </NPopconfirm>
              </NSpace>
            </td>
          </tr>
        </tbody>
      </NTable>
    </NCard>
  </NSpace>

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
.page-title {
  margin: 0;
  font-size: 18px;
}
.username-cell {
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 6px;
}
.muted {
  color: #9ca3af;
  font-size: 13px;
}
</style>