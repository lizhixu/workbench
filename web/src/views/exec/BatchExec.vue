<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard, NSpace, NButton, NInput, NSelect, NCheckbox, NDataTable,
  useMessage, useDialog, NSpin, NIcon, NTag, NModal, NEmpty,
} from 'naive-ui'
import { CopyOutline, BookmarksOutline } from '@vicons/ionicons5'
import { listHosts, batchExec } from '../../api/hosts'
import { listCommands, markCommandUsed, type CommandEntry } from '../../api/commands'
import type { Host, BatchExecResult } from '../../api/types'
import { copyToClipboard } from '../../utils/clipboard'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'BatchExec' })

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const hosts = ref<Host[]>([])
const loading = ref(true)
const selectedHostIds = ref<string[]>([])
const command = ref('')
const shell = ref('')
const isScript = ref(false)
const running = ref(false)
const results = ref<BatchExecResult[]>([])

// Saved-command library picker.
const showLibrary = ref(false)
const libraryEntries = ref<CommandEntry[]>([])
const libraryLoading = ref(false)
const libraryKeyword = ref('')

const shellOptions = [
  { label: '自动', value: '' },
  { label: 'bash', value: 'bash' },
  { label: 'sh', value: 'sh' },
  { label: 'powershell', value: 'powershell' },
  { label: 'cmd', value: 'cmd' },
]

const filteredLibrary = computed(() => {
  const kw = libraryKeyword.value.trim().toLowerCase()
  if (!kw) return libraryEntries.value
  return libraryEntries.value.filter(
    (e) =>
      e.name.toLowerCase().includes(kw) ||
      e.content.toLowerCase().includes(kw) ||
      (e.description || '').toLowerCase().includes(kw),
  )
})

async function openLibrary() {
  showLibrary.value = true
  libraryLoading.value = true
  try {
    libraryEntries.value = await listCommands()
  } catch (e: any) {
    message.error(e.message || '加载常用命令失败')
  } finally {
    libraryLoading.value = false
  }
}

// applyLibraryEntry drops the saved command into the editable draft rather than
// executing it, matching the "生成的命令默认进入可编辑草稿" requirement.
async function applyLibraryEntry(entry: CommandEntry) {
  command.value = entry.content
  shell.value = entry.shell
  isScript.value = entry.content.includes('\n')
  showLibrary.value = false
  message.success(`已套用「${entry.name}」，确认后再执行`)
  try {
    await markCommandUsed(entry.id)
  } catch {
    // Usage counting is best-effort; never block the operator on it.
  }
}

async function loadHosts() {
  loading.value = true
  try {
    const res = await listHosts()
    hosts.value = res.data || res
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}

async function run() {
  if (!command.value) {
    message.warning('请输入命令')
    return
  }
  if (selectedHostIds.value.length === 0) {
    message.warning('请选择目标主机')
    return
  }
  running.value = true
  results.value = []
  try {
    results.value = await batchExec(selectedHostIds.value, command.value, shell.value, 120)
  } catch (e: any) {
    // 409 = high-risk command requires confirmation.
    if (e.status === 409 && e.needsConfirm) {
      running.value = false
      dialog.warning({
        title: '高危命令确认',
        content: `该命令被标记为高危操作：${e.message || '需要二次确认'}\n\n即将执行的命令：\n${command.value}`,
        positiveText: '确认执行',
        negativeText: '取消',
        onPositiveClick: async () => {
          running.value = true
          try {
            results.value = await batchExec(selectedHostIds.value, command.value, shell.value, 120, true)
          } catch (e2: any) {
            message.error(e2.message || '执行失败')
          } finally {
            running.value = false
          }
        },
      })
      return
    }
    // 403 = blocked by policy.
    if (e.status === 403) {
      message.error(`命令已被拦截：${e.message || '命中黑名单规则'}`)
    } else {
      message.error(e.message || '执行失败')
    }
  } finally {
    running.value = false
  }
}

async function copyCommand() {
  if (!command.value.trim()) {
    message.warning('请先输入需要复制的命令')
    return
  }
  const ok = await copyToClipboard(command.value)
  if (ok) {
    message.success('已复制命令到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

async function copyResult(r: BatchExecResult) {
  const content = r.stdout || r.stderr || r.error || (r.exit_code !== undefined ? `退出码: ${r.exit_code}` : '')
  if (!content) {
    message.warning('该执行结果无输出内容')
    return
  }
  const ok = await copyToClipboard(content)
  if (ok) {
    message.success('已复制执行输出到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

const hostColumns = [
  { title: '主机名', key: 'hostname' },
  { title: '系统', key: 'os', width: 80 },
  {
    title: '状态', key: 'status', width: 80,
    render: (row: Host) => row.status === 'online'
      ? h(NTag, { type: 'success', size: 'small', bordered: false }, { default: () => '在线' })
      : h(NTag, { type: 'default', size: 'small', bordered: false }, { default: () => '离线' }),
  },
]

const resultColumns = [
  { title: '主机ID', key: 'host_id', width: 120, ellipsis: { tooltip: true } },
  {
    title: '退出码',
    key: 'exit_code',
    width: 90,
    render: (r: BatchExecResult) => h(
      NTag,
      {
        size: 'small',
        type: r.exit_code === 0 ? 'success' : 'error',
        bordered: false
      },
      { default: () => String(r.exit_code ?? '-') }
    )
  },
  { title: 'stdout', key: 'stdout', ellipsis: { tooltip: true } },
  { title: 'stderr', key: 'stderr', ellipsis: { tooltip: true } },
  { title: '错误', key: 'error', ellipsis: { tooltip: true } },
  {
    title: '操作',
    key: 'actions',
    width: 100,
    render: (r: BatchExecResult) => h(
      NButton,
      {
        size: 'tiny',
        secondary: true,
        type: 'primary',
        onClick: () => copyResult(r),
      },
      {
        icon: () => h(NIcon, { component: CopyOutline }),
        default: () => '复制输出',
      }
    ),
  },
]

onMounted(async () => {
  await loadHosts()
  // Support prefill from query params (e.g. "send to exec" from AI scan report).
  if (route.query.command) {
    command.value = String(route.query.command)
  }
  if (route.query.shell) {
    shell.value = String(route.query.shell)
  }
  if (route.query.host_id) {
    selectedHostIds.value = [String(route.query.host_id)]
  }
})
</script>

<template>
  <div class="batch-exec-view">
    <NSpace vertical :size="16">
      <NCard title="推送命令" :bordered="false">
        <NSpace vertical :size="12">
          <NSpace align="center">
            <span>Shell:</span>
            <NSelect v-model:value="shell" :options="shellOptions" style="width: 140px" />
            <NCheckbox v-model:checked="isScript">多行脚本</NCheckbox>
            <NButton size="small" secondary @click="openLibrary">
              <template #icon><NIcon :component="BookmarksOutline" /></template>
              从常用命令库选取
            </NButton>
          </NSpace>
          <NInput
            v-model:value="command"
            :type="isScript ? 'textarea' : 'text'"
            :rows="isScript ? 6 : undefined"
            placeholder="输入要执行的命令，如: uptime; df -h; free -m"
          />
          <NSpace align="center">
            <NButton type="primary" :loading="running" @click="run">执行</NButton>
            <NButton secondary :disabled="!command.trim()" @click="copyCommand">
              <template #icon><NIcon :component="CopyOutline" /></template>
              复制命令
            </NButton>
            <span class="muted" style="margin-left: 8px">已选 {{ selectedHostIds.length }} 台主机</span>
          </NSpace>
        </NSpace>
      </NCard>

      <NCard title="选择目标主机" :bordered="false">
        <NSpin v-if="loading" />
        <!-- 主机勾选框：辅助性小表格，限高不分页（AGENTS.md 8.2 例外） -->
        <NDataTable
          v-else
          :columns="hostColumns"
          :data="hosts.filter(h => h.status === 'online')"
          :row-key="(r: Host) => r.id"
          v-model:checked-row-keys="selectedHostIds"
          :bordered="false"
          size="small"
          :max-height="240"
        />
      </NCard>

      <NCard v-if="results.length > 0" title="执行结果" :bordered="false">
        <!-- 结果条数 = 所选主机数，天然有限（AGENTS.md 8.2 例外） -->
        <NDataTable
          :columns="resultColumns"
          :data="results"
          :bordered="false"
          size="small"
          :max-height="420"
        />
      </NCard>
    </NSpace>

    <!-- Saved-command library picker -->
    <NModal v-model:show="showLibrary" preset="card" title="常用命令库" style="width: 720px">
      <NSpace vertical :size="12">
        <NInput v-model:value="libraryKeyword" clearable placeholder="搜索名称 / 内容 / 说明" />
        <NSpin :show="libraryLoading">
          <div class="library-list">
            <div
              v-for="entry in filteredLibrary"
              :key="entry.id"
              class="library-item"
              @click="applyLibraryEntry(entry)"
            >
              <div class="library-head">
                <span class="library-name">{{ entry.name }}</span>
                <NTag size="tiny" :bordered="false">{{ entry.shell }}</NTag>
                <NTag v-if="!entry.owner" size="tiny" type="success" :bordered="false">共享</NTag>
                <span class="library-count">使用 {{ entry.use_count }} 次</span>
              </div>
              <code class="library-content">{{ entry.content }}</code>
              <div v-if="entry.description" class="library-desc">{{ entry.description }}</div>
            </div>
            <NEmpty v-if="!libraryLoading && !filteredLibrary.length" description="暂无匹配的常用命令，可在「系统设置 - 自定义常用命令」中维护" />
          </div>
        </NSpin>
      </NSpace>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.batch-exec-view {
  height: 100%;
  overflow-y: auto;
  box-sizing: border-box;
  -webkit-overflow-scrolling: touch;
}
.muted { color: #9ca3af; font-size: 13px; }

.library-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 420px;
  overflow-y: auto;
}
.library-item {
  padding: 10px 12px;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  background: var(--bg-card-subtle);
  cursor: pointer;
  transition: border-color 0.15s, background 0.15s;

  &:hover {
    border-color: #6366f1;
    background: var(--bg-hover);
  }
}
.library-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.library-name {
  font-weight: 600;
  font-size: 14px;
  color: var(--text-primary);
}
.library-count {
  margin-left: auto;
  font-size: 12px;
  color: var(--text-secondary);
}
.library-content {
  display: block;
  padding: 6px 8px;
  border-radius: 4px;
  background: var(--cmd-box-bg);
  color: var(--cmd-text-color);
  font-family: monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.library-desc {
  margin-top: 6px;
  font-size: 12px;
  color: var(--text-secondary);
}
</style>