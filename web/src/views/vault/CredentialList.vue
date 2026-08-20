<script setup lang="ts">
import { onMounted, ref } from 'vue'
import {
  NCard, NButton, NSpace, NTable, NTag, NModal, NForm, NFormItem,
  NInput, NSelect, NPopconfirm, NEmpty, useMessage, NIcon,
} from 'naive-ui'
import {
  AddCircleOutline, RefreshOutline, TrashOutline, LockClosedOutline, EyeOutline,
  CreateOutline,
} from '@vicons/ionicons5'
import type { Credential } from '../../api/vault'
import { listCredentials, createCredential, updateCredential, deleteCredential, getCredential } from '../../api/vault'

const message = useMessage()
const creds = ref<Credential[]>([])
const loading = ref(false)

const showCreate = ref(false)
const showView = ref(false)
const viewingCred = ref<Credential | null>(null)
const editingId = ref<string | null>(null)
const form = ref({
  name: '', type: 'ssh_password', username: '', secret: '', host: '', description: '',
})

const typeOptions = [
  { label: 'SSH 密码', value: 'ssh_password' },
  { label: 'API Key', value: 'api_key' },
  { label: '数据库密码', value: 'database' },
  { label: '自定义', value: 'custom' },
]

function typeLabel(t: string) {
  return typeOptions.find((x) => x.value === t)?.label || t
}

function fmtTime(s: string): string {
  if (!s) return '-'
  return s.slice(0, 19).replace('T', ' ')
}

async function refresh() {
  loading.value = true
  try {
    creds.value = await listCredentials()
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

async function doCreate() {
  if (!form.value.name || !form.value.secret) {
    message.warning('请填写名称和密钥')
    return
  }
  try {
    if (editingId.value) {
      await updateCredential(editingId.value, form.value)
      message.success('凭据已更新')
    } else {
      await createCredential(form.value)
      message.success('凭据已创建')
    }
    showCreate.value = false
    editingId.value = null
    form.value = { name: '', type: 'ssh_password', username: '', secret: '', host: '', description: '' }
    refresh()
  } catch (e: any) {
    message.error(e.message)
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
    name: c.name, type: c.type, username: c.username,
    secret: '', host: c.host, description: c.description,
  }
  showCreate.value = true
}

async function viewCred(id: string) {
  try {
    viewingCred.value = await getCredential(id)
    showView.value = true
  } catch (e: any) {
    message.error(e.message)
  }
}

async function doDelete(id: string) {
  try {
    await deleteCredential(id)
    message.success('凭据已删除')
    refresh()
  } catch (e: any) {
    message.error(e.message)
  }
}

onMounted(refresh)
</script>

<template>
  <NSpace vertical :size="16">
    <NSpace align="center" justify="space-between">
      <h2 class="page-title">凭据管理</h2>
      <NSpace>
        <NButton @click="refresh" :loading="loading">
          <template #icon><NIcon :component="RefreshOutline" /></template>
          刷新
        </NButton>
        <NButton type="primary" @click="openCreate">
          <template #icon><NIcon :component="AddCircleOutline" /></template>
          新增凭据
        </NButton>
      </NSpace>
    </NSpace>

    <NCard>
      <NEmpty v-if="creds.length === 0 && !loading" description="暂无凭据。存储 SSH 密码、API Key 等敏感信息，AES-256-GCM 加密存储。" />
      <NTable v-else :single-line="false" size="small">
        <thead>
          <tr>
            <th>名称</th>
            <th>类型</th>
            <th>用户名</th>
            <th>关联主机</th>
            <th>更新时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="c in creds" :key="c.id">
            <td class="cred-name">
              <NIcon :component="LockClosedOutline" class="lock-icon" />
              {{ c.name }}
            </td>
            <td><NTag size="small" round>{{ typeLabel(c.type) }}</NTag></td>
            <td>{{ c.username || '-' }}</td>
            <td>{{ c.host || '-' }}</td>
            <td class="muted">{{ fmtTime(c.updated_at) }}</td>
            <td>
              <NSpace :size="4">
                <NButton size="small" quaternary @click="viewCred(c.id)">
                  <template #icon><NIcon :component="EyeOutline" /></template>
                  查看
                </NButton>
                <NButton size="small" quaternary @click="openEdit(c)">
                  <template #icon><NIcon :component="CreateOutline" /></template>
                  编辑
                </NButton>
                <NPopconfirm @positive-click="doDelete(c.id)">
                  <template #trigger>
                    <NButton size="small" quaternary type="error">
                      <template #icon><NIcon :component="TrashOutline" /></template>
                    </NButton>
                  </template>
                  确认删除凭据 {{ c.name }}？
                </NPopconfirm>
              </NSpace>
            </td>
          </tr>
        </tbody>
      </NTable>
    </NCard>
  </NSpace>

  <!-- Create/Edit modal -->
  <NModal v-model:show="showCreate" preset="card" :title="editingId ? '编辑凭据' : '新增凭据'" style="width: 520px">
    <NForm label-placement="top">
      <NFormItem label="名称">
        <NInput v-model:value="form.name" placeholder="例如：生产数据库root密码" />
      </NFormItem>
      <NFormItem label="类型">
        <NSelect v-model:value="form.type" :options="typeOptions" />
      </NFormItem>
      <NFormItem label="用户名">
        <NInput v-model:value="form.username" placeholder="可选" />
      </NFormItem>
      <NFormItem :label="editingId ? '新密钥/密码（留空则不修改）' : '密钥/密码'">
        <NInput v-model:value="form.secret" type="password" show-password-on="click" :placeholder="editingId ? '留空保持原密钥不变' : '敏感信息，加密存储'" />
      </NFormItem>
      <NFormItem label="关联主机">
        <NInput v-model:value="form.host" placeholder="可选，主机名或IP" />
      </NFormItem>
      <NFormItem label="描述">
        <NInput v-model:value="form.description" type="textarea" placeholder="可选" />
      </NFormItem>
    </NForm>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showCreate = false">取消</NButton>
        <NButton type="primary" @click="doCreate">{{ editingId ? '保存' : '创建' }}</NButton>
      </NSpace>
    </template>
  </NModal>

  <!-- View modal -->
  <NModal v-model:show="showView" preset="card" title="凭据详情" style="width: 520px">
    <template v-if="viewingCred">
      <div class="cred-detail">
        <div class="detail-row"><span class="label">名称:</span> {{ viewingCred.name }}</div>
        <div class="detail-row"><span class="label">类型:</span> {{ typeLabel(viewingCred.type) }}</div>
        <div class="detail-row"><span class="label">用户名:</span> {{ viewingCred.username || '-' }}</div>
        <div class="detail-row"><span class="label">密钥:</span> <code class="secret-value">{{ viewingCred.secret }}</code></div>
        <div class="detail-row"><span class="label">关联主机:</span> {{ viewingCred.host || '-' }}</div>
        <div class="detail-row"><span class="label">描述:</span> {{ viewingCred.description || '-' }}</div>
      </div>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.page-title {
  margin: 0;
  font-size: 18px;
}
.cred-name {
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 6px;
}
.lock-icon {
  color: #f59e0b;
}
.muted {
  color: #9ca3af;
  font-size: 13px;
}
.cred-detail {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.detail-row {
  display: flex;
  gap: 8px;
  font-size: 14px;
  .label {
    color: #9ca3af;
    min-width: 70px;
    flex-shrink: 0;
  }
}
.secret-value {
  background: #f6f6f6;
  padding: 2px 8px;
  border-radius: 3px;
  font-family: monospace;
  word-break: break-all;
}
</style>