<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NInput,
  NSelect,
  NTag,
  NDataTable,
  NEmpty,
  NIcon,
  NModal,
  NForm,
  NFormItem,
  NSwitch,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import {
  BookmarksOutline,
  AddOutline,
  RefreshOutline,
  TrashOutline,
  CreateOutline,
  CopyOutline,
} from '@vicons/ionicons5'
import {
  listCommands,
  createCommand,
  updateCommand,
  deleteCommand,
  type CommandEntry,
  type CommandShell,
} from '../../api/commands'
import { copyToClipboard } from '../../utils/clipboard'
import { useAuthStore } from '../../stores/auth'

const message = useMessage()
const auth = useAuthStore()

const entries = ref<CommandEntry[]>([])
const loading = ref(false)
const saving = ref(false)
const keyword = ref('')
const shellFilter = ref('')

const showEditor = ref(false)
const editingId = ref<string | null>(null)
const form = ref<{
  name: string
  shell: CommandShell
  content: string
  description: string
  tags: string
  shared: boolean
}>({ name: '', shell: 'bash', content: '', description: '', tags: '', shared: false })

const shellOptions: { label: string; value: CommandShell }[] = [
  { label: 'Linux Bash', value: 'bash' },
  { label: 'Linux sh', value: 'sh' },
  { label: 'Windows PowerShell', value: 'powershell' },
  { label: 'Windows CMD', value: 'cmd' },
]

const filterOptions = [{ label: '全部 Shell', value: '' }, ...shellOptions]

const isAdmin = computed(() => auth.role === 'admin')

function shellLabel(s: string): string {
  return shellOptions.find((o) => o.value === s)?.label || s
}

function canMutate(row: CommandEntry): boolean {
  if (isAdmin.value) return true
  return !!row.owner && row.owner === auth.user?.username
}

async function refresh() {
  loading.value = true
  try {
    entries.value = await listCommands({
      shell: shellFilter.value || undefined,
      q: keyword.value.trim() || undefined,
    })
  } catch (e: any) {
    message.error(e.message || '加载常用命令失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = null
  form.value = { name: '', shell: 'bash', content: '', description: '', tags: '', shared: false }
  showEditor.value = true
}

function openEdit(row: CommandEntry) {
  editingId.value = row.id
  form.value = {
    name: row.name,
    shell: row.shell,
    content: row.content,
    description: row.description,
    tags: (row.tags || []).join(', '),
    shared: row.shared,
  }
  showEditor.value = true
}

async function submit() {
  if (!form.value.name.trim() || !form.value.content.trim()) {
    message.warning('请填写命令名称与命令内容')
    return
  }
  saving.value = true
  const payload = {
    name: form.value.name.trim(),
    shell: form.value.shell,
    content: form.value.content,
    description: form.value.description.trim(),
    tags: form.value.tags
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean),
    shared: form.value.shared,
  }
  try {
    if (editingId.value) {
      await updateCommand(editingId.value, payload)
      message.success('常用命令已更新')
    } else {
      await createCommand(payload)
      message.success('常用命令已保存')
    }
    showEditor.value = false
    await refresh()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function remove(row: CommandEntry) {
  try {
    await deleteCommand(row.id)
    message.success('已删除')
    await refresh()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

async function copyContent(row: CommandEntry) {
  const ok = await copyToClipboard(row.content)
  if (ok) {
    message.success('已复制命令内容')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

const columns = [
  { title: '名称', key: 'name', width: 170, ellipsis: { tooltip: true } },
  {
    title: 'Shell',
    key: 'shell',
    width: 150,
    render: (r: CommandEntry) =>
      h(NTag, { size: 'small', bordered: false }, { default: () => shellLabel(r.shell) }),
  },
  {
    title: '命令内容',
    key: 'content',
    ellipsis: { tooltip: true },
    render: (r: CommandEntry) => h('code', { style: 'font-size:12px' }, r.content),
  },
  { title: '说明', key: 'description', width: 190, ellipsis: { tooltip: true } },
  {
    title: '归属',
    key: 'owner',
    width: 100,
    render: (r: CommandEntry) =>
      r.owner
        ? h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => r.owner })
        : h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '内置/共享' }),
  },
  { title: '使用次数', key: 'use_count', width: 90 },
  {
    title: '操作',
    key: 'actions',
    width: 190,
    render: (r: CommandEntry) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => copyContent(r) },
            { icon: () => h(NIcon, { component: CopyOutline }), default: () => '复制' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, disabled: !canMutate(r), onClick: () => openEdit(r) },
            { icon: () => h(NIcon, { component: CreateOutline }), default: () => '编辑' },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => remove(r) },
            {
              trigger: () =>
                h(
                  NButton,
                  { size: 'tiny', quaternary: true, type: 'error', disabled: !canMutate(r) },
                  { icon: () => h(NIcon, { component: TrashOutline }) },
                ),
              default: () => `确认删除「${r.name}」？`,
            },
          ),
        ],
      }),
  },
]

onMounted(refresh)
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><BookmarksOutline /></NIcon>
        自定义常用命令
      </span>
    </template>
    <template #header-extra>
      <NSpace align="center" :size="8">
        <NInput
          v-model:value="keyword"
          size="small"
          clearable
          placeholder="搜索名称/内容/说明"
          style="width: 200px"
          @keydown.enter="refresh"
        />
        <NSelect
          v-model:value="shellFilter"
          size="small"
          :options="filterOptions"
          style="width: 160px"
          @update:value="refresh"
        />
        <NButton size="small" quaternary :loading="loading" @click="refresh">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          刷新
        </NButton>
        <NButton size="small" type="primary" @click="openCreate">
          <template #icon><NIcon><AddOutline /></NIcon></template>
          新增命令
        </NButton>
      </NSpace>
    </template>

    <NSpace vertical :size="12">
      <p class="muted">
        命令库在「推送命令」页面可直接选取套用。内置命令与管理员发布的共享命令对所有人可见，个人命令仅本人可见；使用次数高的排在前面。
      </p>

      <!-- 常用命令库：辅助性小表格，限高不分页（AGENTS.md 8.2 例外） -->
      <NDataTable
        :columns="columns"
        :data="entries"
        :loading="loading"
        size="small"
        :bordered="false"
        :row-key="(r: CommandEntry) => r.id"
        :max-height="360"
      >
        <template #empty>
          <NEmpty description="暂无常用命令，点击右上角新增" />
        </template>
      </NDataTable>
    </NSpace>
  </NCard>

  <NModal
    v-model:show="showEditor"
    preset="card"
    :title="editingId ? '编辑常用命令' : '新增常用命令'"
    style="width: 620px"
  >
    <NForm label-placement="top">
      <NFormItem label="名称" required>
        <NInput v-model:value="form.name" placeholder="例如：查看磁盘占用" />
      </NFormItem>
      <NFormItem label="Shell 类型" required>
        <NSelect v-model:value="form.shell" :options="shellOptions" />
      </NFormItem>
      <NFormItem label="命令内容" required>
        <NInput
          v-model:value="form.content"
          type="textarea"
          :rows="6"
          placeholder="支持单行命令或多行脚本"
        />
      </NFormItem>
      <NFormItem label="说明">
        <NInput v-model:value="form.description" placeholder="可选，说明该命令的用途" />
      </NFormItem>
      <NFormItem label="标签">
        <NInput v-model:value="form.tags" placeholder="可选，逗号分隔，如：磁盘, 巡检" />
      </NFormItem>
      <NFormItem v-if="isAdmin" label="发布为共享命令（所有用户可见）">
        <NSwitch v-model:value="form.shared" />
      </NFormItem>
    </NForm>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="showEditor = false">取消</NButton>
        <NButton type="primary" :loading="saving" @click="submit">
          {{ editingId ? '保存' : '创建' }}
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.muted {
  color: var(--text-secondary);
  font-size: 13px;
}
</style>
