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
// 初始即为加载态：首屏渲染时不先闪现空状态，等首轮请求落定再决定是否展示空态。
const loading = ref(true)
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

// ---- 批量删除（范式见 NetworkList.vue） ----
// 勾选按命令 id 存；执行期 3 路并发、逐行显示进度、失败可重试。
// 本表是 AGENTS.md §8.2 例外的小表（限高不分页），只加勾选与批量栏，
// 不改分页口径。删除准入沿用单机 canMutate（admin 或本人创建）。
const selectedCommandIds = ref<Set<string>>(new Set())
const BATCH_CONCURRENCY = 3
type BatchPhase = 'queued' | 'running' | 'done' | 'failed'
const opStatus = ref<Record<string, { phase: BatchPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
const showBatchDeleteModal = ref(false)

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
    // 刷新清掉上一批的旧进度；批量执行中不清除（runBatchDelete 在执行完才刷新，
    // 此时 batchRunning 仍为 true，进度得以保留到汇总展示）。
    if (!batchRunning.value) {
      opStatus.value = {}
      batchSummary.value = null
    }
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
    selectedCommandIds.value.delete(row.id)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '删除失败')
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

const selectedCommands = computed(() => entries.value.filter((e) => selectedCommandIds.value.has(e.id)))
// 只有本人创建的命令或 admin 能删（单机删除按钮同款 canMutate 准入），
// 他人的命令不参与执行、计入跳过。
const batchDeletable = computed(() => selectedCommands.value.filter((e) => canMutate(e)))

function onCommandChecked(keys: (string | number)[]) {
  // 本表不分页：naive-ui 回传的即全量勾选集合，直接替换。
  selectedCommandIds.value = new Set(keys.map((k) => String(k)))
}

async function runBatchDelete(targets: CommandEntry[]) {
  const eligible = targets.filter((e) => canMutate(e))
  const skipped = targets.length - eligible.length
  if (eligible.length === 0) {
    message.warning('所选命令中没有可删除的（仅本人创建的命令或管理员可删除）')
    return
  }
  batchRunning.value = true
  batchSummary.value = null
  opStatus.value = {}
  eligible.forEach((e) => {
    opStatus.value[e.id] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0
  const queue = [...eligible]
  async function worker() {
    while (queue.length > 0) {
      const entry = queue.shift()!
      opStatus.value[entry.id] = { phase: 'running' }
      try {
        // 复用单机删除接口，审计走既有单机接口的审计，无需新埋点。
        await deleteCommand(entry.id)
        opStatus.value[entry.id] = { phase: 'done' }
        done += 1
      } catch (e: any) {
        opStatus.value[entry.id] = { phase: 'failed', message: e.message || '执行出错' }
        failed += 1
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(BATCH_CONCURRENCY, eligible.length) }, () => worker()),
  )
  // 本表 entries 是服务端按关键词过滤的，刷新无法核验勾选行是否还存在，
  // 故不在刷新时清理勾选；删除成功即已知不存在，直接从勾选中移除。
  Object.entries(opStatus.value).forEach(([id, st]) => {
    if (st.phase === 'done') selectedCommandIds.value.delete(id)
  })
  // 批量执行完再刷新列表（batchRunning 仍为 true，refresh 的清进度守卫
  // 不触发）：成功删除的行随之消失，失败行保留进度供重试。
  await refresh()
  batchRunning.value = false
  batchSummary.value = { done, failed, skipped }
  const skipText = skipped > 0 ? `，跳过 ${skipped} 条（无删除权限）` : ''
  if (failed === 0) {
    message.success(`批量删除完成：成功 ${done} 条${skipText}`)
  } else {
    message.warning(`批量删除完成：成功 ${done} 条，失败 ${failed} 条${skipText}，可点「重试失败」只重跑失败的命令`)
  }
}

function retryBatchFailed() {
  if (!batchSummary.value || batchSummary.value.failed === 0) return
  const failedIds = Object.entries(opStatus.value)
    .filter(([, st]) => st.phase === 'failed')
    .map(([id]) => id)
  runBatchDelete(entries.value.filter((e) => failedIds.includes(e.id)))
}

function confirmBatchDelete() {
  showBatchDeleteModal.value = false
  runBatchDelete(selectedCommands.value)
}

function clearBatchSelection() {
  selectedCommandIds.value = new Set()
  opStatus.value = {}
  batchSummary.value = null
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
  {
    // 批量勾选列：NDataTable 复选框列取 40px（BatchExec.vue 同款口径；
    // DESIGN.md §9.20 的 28px 是 flex 行列表口径，复选框 + 单元格内边距放不下）。
    type: 'selection' as const,
    width: 40,
    disabled: () => batchRunning.value,
  },
  {
    title: '名称',
    key: 'name',
    width: 170,
    ellipsis: { tooltip: true },
    render: (r: CommandEntry) =>
      h('div', null, [
        h('span', null, r.name),
        batchProgressNode(r),
      ]),
  },
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
      <p class="muted tip-hint">
        命令库在「推送命令」页面可直接选取套用。内置命令与管理员发布的共享命令对所有人可见，个人命令仅本人可见；使用次数高的排在前面。
      </p>

      <!-- 批量操作栏：有勾选时出现，执行期间按钮禁用、行内逐行显示进度 -->
      <div v-if="selectedCommandIds.size > 0" class="batch-bar">
        <span class="batch-info">
          已选 {{ selectedCommandIds.size }} 条
          <span class="text-muted">（可删除 {{ batchDeletable.length }} 条）</span>
        </span>
        <NButton
          size="small"
          type="error"
          secondary
          :loading="batchRunning"
          :disabled="batchRunning || batchDeletable.length === 0"
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
          成功 {{ batchSummary.done }} · 失败 {{ batchSummary.failed }}<span v-if="batchSummary.skipped > 0"> · 跳过 {{ batchSummary.skipped }}</span>
        </span>
        <NButton size="small" quaternary :disabled="batchRunning" @click="clearBatchSelection">
          取消选择
        </NButton>
      </div>

      <!-- 常用命令库：辅助性小表格，限高不分页（AGENTS.md 8.2 例外） -->
      <NDataTable
        :columns="columns"
        :data="entries"
        :loading="loading"
        size="small"
        :bordered="false"
        :row-key="(r: CommandEntry) => r.id"
        :checked-row-keys="[...selectedCommandIds]"
        :max-height="360"
        @update:checked-row-keys="onCommandChecked"
      >
        <template #empty>
          <NEmpty v-if="!loading" description="暂无常用命令，点击右上角新增" />
        </template>
      </NDataTable>
    </NSpace>
  </NCard>

  <!-- 批量删除二次确认（DESIGN.md §9.17：破坏性确认在弹窗内就地展开） -->
  <NModal
    v-model:show="showBatchDeleteModal"
    preset="card"
    :title="`批量删除常用命令（${batchDeletable.length} 条）`"
    style="width: 520px"
  >
    <div class="batch-confirm-body">
      <div class="batch-target-list">
        <div
          v-for="e in batchDeletable.slice(0, 5)"
          :key="e.id"
          class="batch-target-row"
        >
          {{ e.name }}
        </div>
        <div v-if="batchDeletable.length > 5" class="batch-target-row muted">
          等 {{ batchDeletable.length - 5 }} 条…
        </div>
      </div>
      <p v-if="selectedCommands.length > batchDeletable.length" class="field-hint">
        其中 {{ selectedCommands.length - batchDeletable.length }} 条为他人创建的命令，无删除权限，将跳过不执行。
      </p>
      <p v-else class="field-hint">删除后命令无法恢复，请确认。</p>
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

  .text-muted {
    color: var(--text-secondary);
    font-weight: 400;
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
