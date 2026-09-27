<script setup lang="ts">
import { onMounted, ref, computed, h, watch } from 'vue'
import {
  NSpace,
  NButton,
  NInput,
  NDataTable,
  NBreadcrumb,
  NBreadcrumbItem,
  NModal,
  NForm,
  NFormItem,
  useMessage,
  useDialog,
  NSpin,
  NUpload,
  NIcon,
  NButtonGroup,
  NDropdown,
  NTooltip,
  NAlert,
  type DataTableColumns,
} from 'naive-ui'
import {
  ArrowUpOutline,
  RefreshOutline,
  FolderOutline,
  DocumentOutline,
  AddOutline,
  CloudUploadOutline,
  DownloadOutline,
  EllipsisHorizontalOutline,
  CreateOutline,
  CopyOutline,
  ArrowForwardOutline,
  TrashOutline,
  EyeOutline,
  SaveOutline,
} from '@vicons/ionicons5'
import {
  fileList,
  fileMkdir,
  fileDownloadUrl,
  fileUpload,
  fileMove,
  fileCopy,
  fileRemove,
  fileReadText,
  fileWriteText,
  getHost,
} from '../../api/hosts'
import type { FileInfo } from '../../api/types'
import { useAuthStore } from '../../stores/auth'
import { useSettingsStore } from '../../stores/settings'
import { SETTING_KEYS } from '../../api/settings'
import { useTablePagination } from '../../composables/useTablePagination'

const props = defineProps<{
  hostId: string
  os?: string
}>()

const message = useMessage()
const dialog = useDialog()
const auth = useAuthStore()
const settingsStore = useSettingsStore()
const loading = ref(false)
const hostOs = ref(props.os || '')

// Viewers get a read-only file tree: download and preview only.
const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')

// Set default path based on OS: Windows -> "C:\", Linux -> "/root"
const isWindows = computed(() => {
  const osLower = (hostOs.value || props.os || '').toLowerCase()
  return osLower.includes('win')
})

const currentPath = ref(isWindows.value ? 'C:\\' : '/root')
const pathInput = ref(currentPath.value)
const isEditingPath = ref(false)
const files = ref<FileInfo[]>([])

// 目录里可能有上千个条目（AGENTS.md 8.2）：分页后表头与分页条固定，只有
// 数据区滚动，切换目录回到第一页。
const fileCount = computed(() => files.value.length)
const { pagination, resetPage } = useTablePagination({ pageSize: 50, rowCount: fileCount })

const showMkdir = ref(false)
const mkdirPath = ref('')
const showUpload = ref(false)

// Rename / move / copy share one modal, distinguished by `transferMode`.
type TransferMode = 'rename' | 'move' | 'copy'
const showTransfer = ref(false)
const transferMode = ref<TransferMode>('rename')
const transferSource = ref<FileInfo | null>(null)
const transferTarget = ref('')
const transferBusy = ref(false)

// Text preview / editor.
const showEditor = ref(false)
const editorPath = ref('')
const editorContent = ref('')
const editorOriginal = ref('')
const editorLoading = ref(false)
const editorSaving = ref(false)
const editorReadOnly = ref(false)

// Text files above this size are refused rather than loaded into the browser.
const maxEditableSize = 2 * 1024 * 1024

const sep = computed(() => (isWindows.value ? '\\' : '/'))

const transferTitle = computed(() => {
  if (transferMode.value === 'rename') return '重命名'
  if (transferMode.value === 'move') return '移动'
  return '复制'
})

const editorDirty = computed(() => editorContent.value !== editorOriginal.value)

function isPathCompatibleWithOS(path: string, isWin: boolean): boolean {
  if (!path) return false
  const isWinFormat = /^[a-zA-Z]:[\\/]/.test(path)
  const isPosixFormat = path.startsWith('/') || path.startsWith('~')
  return isWin ? isWinFormat : isPosixFormat
}

function applyDefaultPath(custom: string) {
  if (custom && isPathCompatibleWithOS(custom, isWindows.value)) {
    currentPath.value = custom
    pathInput.value = custom
  } else if (isWindows.value && !isPathCompatibleWithOS(currentPath.value, true)) {
    currentPath.value = 'C:\\'
    pathInput.value = 'C:\\'
  } else if (!isWindows.value && !isPathCompatibleWithOS(currentPath.value, false)) {
    currentPath.value = '/root'
    pathInput.value = '/root'
  }
}

async function detectOS() {
  if (!hostOs.value && props.hostId) {
    try {
      const h = await getHost(props.hostId)
      if (h && h.os) {
        hostOs.value = h.os
        if (isWindows.value && !isPathCompatibleWithOS(currentPath.value, true)) {
          currentPath.value = 'C:\\'
          pathInput.value = 'C:\\'
        } else if (!isWindows.value && !isPathCompatibleWithOS(currentPath.value, false)) {
          currentPath.value = '/root'
          pathInput.value = '/root'
        }
      }
    } catch {
      // ignore
    }
  }
}

async function load() {
  loading.value = true
  try {
    const data = await fileList(props.hostId, currentPath.value)
    files.value = data || []
    resetPage()
    pathInput.value = currentPath.value
  } catch (e: any) {
    message.error(e.message || '获取文件列表失败')
  } finally {
    loading.value = false
  }
}

function navigateTo(path: string) {
  currentPath.value = path
  pathInput.value = path
  load()
}

function submitPathInput() {
  let target = pathInput.value.trim()
  if (!target) {
    target = isWindows.value ? 'C:\\' : '/'
  }
  isEditingPath.value = false
  navigateTo(target)
}

function goBack() {
  const p = currentPath.value.replace(/\\/g, '/')
  const isWin = isWindows.value || /^[a-zA-Z]:/.test(p)

  if (isWin) {
    // Windows drive path like C:/Users/Administrator
    const driveMatch = p.match(/^([a-zA-Z]:)/)
    const drive = driveMatch ? driveMatch[1] : 'C:'
    const sub = p.slice(drive.length).split('/').filter(Boolean)
    if (sub.length > 0) {
      sub.pop()
      if (sub.length === 0) {
        currentPath.value = drive + '\\'
      } else {
        currentPath.value = drive + '\\' + sub.join('\\')
      }
    } else {
      currentPath.value = drive + '\\'
    }
  } else {
    const parts = p.split('/').filter(Boolean)
    if (parts.length > 1) {
      parts.pop()
      currentPath.value = '/' + parts.join('/')
    } else {
      currentPath.value = '/'
    }
  }
  load()
}

function goUp() {
  goBack()
}

function navigateInto(file: FileInfo) {
  if (file.is_dir) {
    navigateTo(file.path)
  }
}

interface Crumb {
  name: string
  path: string
}

const breadcrumbs = computed<Crumb[]>(() => {
  const raw = currentPath.value
  const normalized = raw.replace(/\\/g, '/')
  const isWin = isWindows.value || /^[a-zA-Z]:/.test(normalized)

  if (isWin) {
    const match = normalized.match(/^([a-zA-Z]:)(.*)/)
    if (match) {
      const drive = match[1].toUpperCase()
      const rest = match[2]
      const parts = rest.split('/').filter(Boolean)
      const list: Crumb[] = [{ name: drive, path: drive + '\\' }]
      let acc = drive + '\\'
      for (const p of parts) {
        acc += p + '\\'
        list.push({ name: p, path: acc.replace(/\\$/, '') })
      }
      return list
    }
  }

  // Unix breadcrumb
  const parts = normalized.split('/').filter(Boolean)
  const list: Crumb[] = [{ name: '根目录 (/)', path: '/' }]
  let acc = ''
  for (const p of parts) {
    acc += '/' + p
    list.push({ name: p, path: acc })
  }
  return list
})

async function doMkdir() {
  if (!mkdirPath.value.trim()) return
  try {
    const name = mkdirPath.value.trim()
    let target = ''
    const sep = isWindows.value ? '\\' : '/'
    if (name.startsWith('/') || (isWindows.value && /^[a-zA-Z]:/.test(name))) {
      target = name
    } else {
      const base = currentPath.value.endsWith(sep)
        ? currentPath.value
        : currentPath.value + sep
      target = base + name
    }
    await fileMkdir(props.hostId, target)
    message.success('目录创建成功')
    showMkdir.value = false
    mkdirPath.value = ''
    load()
  } catch (e: any) {
    message.error(e.message || '创建目录失败')
  }
}

function downloadFile(path: string) {
  window.open(fileDownloadUrl(props.hostId, path), '_blank')
}

async function handleUpload({ file }: { file: any }) {
  if (file.file) {
    try {
      await fileUpload(props.hostId, currentPath.value, file.file as File)
      message.success('文件上传成功')
      showUpload.value = false
      load()
    } catch (e: any) {
      message.error(e.message || '文件上传失败')
    }
  }
}

function fmtSize(n: number): string {
  if (!n) return '-'
  if (n >= 1e9) return (n / 1e9).toFixed(1) + ' GB'
  if (n >= 1e6) return (n / 1e6).toFixed(1) + ' MB'
  if (n >= 1e3) return (n / 1e3).toFixed(1) + ' KB'
  return n + ' B'
}

// joinPath appends a name to the current directory using the host's separator.
function joinPath(dir: string, name: string): string {
  const s = sep.value
  const base = dir.endsWith(s) ? dir : dir + s
  return base + name
}

// resolveTarget turns whatever the user typed into an absolute path. A bare name
// is treated as a sibling of the source; an absolute path is used verbatim.
function resolveTarget(input: string): string {
  const raw = input.trim()
  if (!raw) return ''
  if (raw.startsWith('/') || /^[a-zA-Z]:/.test(raw) || raw.startsWith('\\')) return raw
  return joinPath(currentPath.value, raw)
}

function openTransfer(file: FileInfo, mode: TransferMode) {
  transferSource.value = file
  transferMode.value = mode
  transferTarget.value = mode === 'rename' ? file.name : joinPath(currentPath.value, file.name)
  showTransfer.value = true
}

async function submitTransfer() {
  const src = transferSource.value
  if (!src) return
  const dest = resolveTarget(transferTarget.value)
  if (!dest) {
    message.warning('请填写目标名称或路径')
    return
  }
  if (dest === src.path) {
    message.warning('目标与源路径相同')
    return
  }
  transferBusy.value = true
  try {
    if (transferMode.value === 'copy') {
      await fileCopy(props.hostId, src.path, dest)
      message.success('复制完成')
    } else {
      await fileMove(props.hostId, src.path, dest)
      message.success(transferMode.value === 'rename' ? '重命名完成' : '移动完成')
    }
    showTransfer.value = false
    await load()
  } catch (e: any) {
    message.error(e.message || `${transferTitle.value}失败`)
  } finally {
    transferBusy.value = false
  }
}

// confirmRemove requires an explicit dialog because deletion is irreversible and
// the agent performs a recursive remove on directories.
function confirmRemove(file: FileInfo) {
  dialog.warning({
    title: file.is_dir ? '删除目录' : '删除文件',
    content: file.is_dir
      ? `确认删除目录 ${file.path} 及其全部内容？此操作不可恢复。`
      : `确认删除文件 ${file.path}？此操作不可恢复。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await fileRemove(props.hostId, file.path)
        message.success('已删除')
        await load()
      } catch (e: any) {
        message.error(e.message || '删除失败')
      }
    },
  })
}

// Extensions we are willing to open in the built-in text editor. Anything else
// falls back to download so binaries never get mangled by a round trip.
const textExtensions = [
  'txt', 'log', 'conf', 'cfg', 'ini', 'env', 'json', 'yaml', 'yml', 'toml',
  'xml', 'html', 'htm', 'css', 'scss', 'js', 'ts', 'jsx', 'tsx', 'vue',
  'go', 'py', 'rb', 'php', 'java', 'kt', 'rs', 'c', 'h', 'cpp', 'hpp',
  'sh', 'bash', 'zsh', 'ps1', 'bat', 'cmd', 'sql', 'md', 'csv', 'properties',
  'service', 'gitignore', 'dockerfile', 'makefile',
]

function isTextFile(name: string): boolean {
  const lower = name.toLowerCase()
  if (!lower.includes('.')) {
    // Extension-less files that are conventionally text.
    return ['dockerfile', 'makefile', 'readme', 'license', 'changelog'].includes(lower)
  }
  const ext = lower.slice(lower.lastIndexOf('.') + 1)
  return textExtensions.includes(ext)
}

async function openEditor(file: FileInfo, readOnly: boolean) {
  if (file.size > maxEditableSize) {
    message.warning(`文件超过 ${fmtSize(maxEditableSize)}，请下载后查看`)
    return
  }
  editorPath.value = file.path
  editorReadOnly.value = readOnly || !canWrite.value
  editorContent.value = ''
  editorOriginal.value = ''
  showEditor.value = true
  editorLoading.value = true
  try {
    const text = await fileReadText(props.hostId, file.path)
    editorContent.value = text
    editorOriginal.value = text
  } catch (e: any) {
    message.error(e.message || '读取文件失败')
    showEditor.value = false
  } finally {
    editorLoading.value = false
  }
}

async function saveEditor() {
  if (editorReadOnly.value) return
  editorSaving.value = true
  try {
    await fileWriteText(props.hostId, editorPath.value, editorContent.value)
    editorOriginal.value = editorContent.value
    message.success('已保存')
    await load()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    editorSaving.value = false
  }
}

function closeEditor() {
  if (editorDirty.value) {
    dialog.warning({
      title: '放弃修改',
      content: '当前内容尚未保存，关闭后修改将丢失。',
      positiveText: '放弃并关闭',
      negativeText: '继续编辑',
      onPositiveClick: () => { showEditor.value = false },
    })
    return
  }
  showEditor.value = false
}

// Row action menu: only the operations that make sense for the row's type and
// the caller's role are offered.
function rowMenuOptions(row: FileInfo) {
  const opts: any[] = []
  if (!row.is_dir && isTextFile(row.name)) {
    opts.push({
      key: 'preview',
      label: canWrite.value ? '预览 / 编辑' : '预览',
      icon: () => h(NIcon, { component: canWrite.value ? CreateOutline : EyeOutline }),
    })
  }
  if (canWrite.value) {
    if (opts.length) opts.push({ key: 'd1', type: 'divider' })
    opts.push(
      { key: 'rename', label: '重命名', icon: () => h(NIcon, { component: CreateOutline }) },
      { key: 'move', label: '移动到…', icon: () => h(NIcon, { component: ArrowForwardOutline }) },
      { key: 'copy', label: '复制到…', icon: () => h(NIcon, { component: CopyOutline }) },
      { key: 'd2', type: 'divider' },
      {
        key: 'remove',
        label: '删除',
        icon: () => h(NIcon, { component: TrashOutline }),
        props: { style: 'color: #e88080' },
      },
    )
  }
  return opts
}

function handleRowMenu(key: string, row: FileInfo) {
  switch (key) {
    case 'preview':
      openEditor(row, !canWrite.value)
      break
    case 'rename':
      openTransfer(row, 'rename')
      break
    case 'move':
      openTransfer(row, 'move')
      break
    case 'copy':
      openTransfer(row, 'copy')
      break
    case 'remove':
      confirmRemove(row)
      break
  }
}

const columns: DataTableColumns<FileInfo> = [
  {
    title: '名称',
    key: 'name',
    minWidth: 180,
    ellipsis: {
      tooltip: true,
    },
    render(row) {
      return h(
        'div',
        {
          class: 'file-name-cell',
          onClick: () => navigateInto(row),
        },
        [
          h(
            NIcon,
            {
              size: 18,
              color: row.is_dir ? '#f59e0b' : '#3b82f6',
              class: 'file-icon',
            },
            { default: () => h(row.is_dir ? FolderOutline : DocumentOutline) }
          ),
          h('span', { class: 'file-name-text' }, row.name),
        ]
      )
    },
  },
  {
    title: '大小',
    key: 'size',
    width: 90,
    render(row) {
      return row.is_dir ? '-' : fmtSize(row.size)
    },
  },
  {
    title: '属性',
    key: 'mode',
    width: 110,
    render(row) {
      return h('span', { class: 'mono-font' }, row.mode || '-')
    },
  },
  {
    title: '修改时间',
    key: 'mod_time',
    width: 150,
    render(row) {
      return row.mod_time?.slice(0, 19) || '-'
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 110,
    fixed: 'right',
    render(row) {
      const items = []
      if (!row.is_dir) {
        items.push(
          h(NTooltip, { trigger: 'hover' }, {
            trigger: () => h(
              NButton,
              {
                size: 'tiny',
                quaternary: true,
                type: 'primary',
                onClick: () => downloadFile(row.path),
              },
              { icon: () => h(NIcon, { component: DownloadOutline }) },
            ),
            default: () => '下载',
          }),
        )
      }
      const opts = rowMenuOptions(row)
      if (opts.length) {
        items.push(
          h(
            NDropdown,
            {
              trigger: 'click',
              options: opts,
              placement: 'bottom-end',
              onSelect: (key: string) => handleRowMenu(key, row),
            },
            {
              default: () => h(
                NButton,
                { size: 'tiny', quaternary: true },
                { icon: () => h(NIcon, { component: EllipsisHorizontalOutline }) },
              ),
            },
          ),
        )
      }
      if (!items.length) return null
      return h('div', { class: 'file-actions-cell' }, items)
    },
  },
]

watch(
  () => props.hostId,
  async () => {
    hostOs.value = ''
    await detectOS()
    const custom = settingsStore.getUserKey<string>(SETTING_KEYS.filesDefaultPath, '').trim()
    applyDefaultPath(custom)
    load()
  }
)

onMounted(async () => {
  await detectOS()
  // 通用设置里配了文件默认路径就优先用（但须与目标主机 OS 路径格式匹配）；留空或格式不兼容则沿用按 OS 的默认值。
  await settingsStore.load()
  const custom = settingsStore.getUserKey<string>(SETTING_KEYS.filesDefaultPath, '').trim()
  applyDefaultPath(custom)
  load()
})
</script>

<template>
  <div class="file-manager-pane">
    <!-- 顶部导航栏 -->
    <div class="file-nav-bar">
      <div class="nav-left">
        <NButtonGroup size="small">
          <NButton secondary @click="goUp" title="返回上一级">
            <template #icon><NIcon><ArrowUpOutline /></NIcon></template>
          </NButton>
          <NButton secondary @click="load" title="刷新">
            <template #icon><NIcon><RefreshOutline /></NIcon></template>
          </NButton>
        </NButtonGroup>

        <!-- Windows 快捷盘符切换 -->
        <div v-if="isWindows" class="drives-quick-box">
          <NButton
            v-for="d in ['C:', 'D:', 'E:']"
            :key="d"
            size="tiny"
            secondary
            :type="currentPath.toUpperCase().startsWith(d) ? 'primary' : 'default'"
            @click="navigateTo(d + '\\')"
          >
            {{ d }}
          </NButton>
        </div>

        <!-- 路径输入/面包屑 -->
        <div class="path-breadcrumb-box">
          <div v-if="!isEditingPath" class="crumbs-view" @click="isEditingPath = true">
            <NBreadcrumb separator="/">
              <NBreadcrumbItem
                v-for="(crumb, idx) in breadcrumbs"
                :key="idx"
                @click.stop="navigateTo(crumb.path)"
              >
                <span class="crumb-link">{{ crumb.name }}</span>
              </NBreadcrumbItem>
            </NBreadcrumb>
          </div>
          <div v-else class="input-view">
            <NInput
              v-model:value="pathInput"
              size="small"
              placeholder="输入完整路径后按回车跳转"
              autofocus
              @keyup.enter="submitPathInput"
              @blur="isEditingPath = false"
            />
          </div>
        </div>
      </div>

      <div class="nav-right">
        <NButton v-if="canWrite" type="primary" secondary size="small" @click="showMkdir = true">
          <template #icon><NIcon><AddOutline /></NIcon></template>
          新建目录
        </NButton>

        <NButton v-if="canWrite" type="primary" size="small" @click="showUpload = true">
          <template #icon><NIcon><CloudUploadOutline /></NIcon></template>
          上传文件
        </NButton>

        <NTooltip v-else trigger="hover">
          <template #trigger>
            <NButton size="small" secondary disabled>只读</NButton>
          </template>
          当前角色仅可浏览与下载文件
        </NTooltip>
      </div>
    </div>

    <!-- 文件数据表格：表头固定，仅数据区滚动（AGENTS.md 8.2） -->
    <NSpin v-if="loading" class="spin-center" />
    <div v-else class="file-table-container">
      <NDataTable
        flex-height
        :columns="columns"
        :data="files"
        :pagination="pagination"
        :bordered="false"
        :scroll-x="600"
        size="small"
      />
    </div>

    <!-- 新建目录 Modal -->
    <NModal
      v-model:show="showMkdir"
      preset="card"
      title="新建目录"
      style="width: 400px"
    >
      <NForm label-placement="top">
        <NFormItem label="目录名称">
          <NInput v-model:value="mkdirPath" placeholder="请输入新建目录名" />
        </NFormItem>
        <NSpace justify="end">
          <NButton @click="showMkdir = false">取消</NButton>
          <NButton type="primary" @click="doMkdir">创建</NButton>
        </NSpace>
      </NForm>
    </NModal>

    <!-- 上传文件 Modal -->
    <NModal
      v-model:show="showUpload"
      preset="card"
      title="上传文件"
      style="width: 440px"
    >
      <div class="upload-modal-body">
        <div class="target-path-info">上传至当前路径: <code>{{ currentPath }}</code></div>
        <NUpload :default-upload="false" @change="handleUpload">
          <NButton type="primary" secondary block>选择文件上传</NButton>
        </NUpload>
      </div>
    </NModal>

    <!-- 重命名 / 移动 / 复制 Modal -->
    <NModal
      v-model:show="showTransfer"
      preset="card"
      :title="transferTitle"
      style="width: 520px"
    >
      <div class="transfer-modal-body">
        <NAlert type="default" :bordered="false">
          <div class="transfer-src">
            源路径：<code>{{ transferSource?.path }}</code>
          </div>
        </NAlert>
        <NForm label-placement="top">
          <NFormItem :label="transferMode === 'rename' ? '新名称' : '目标路径'">
            <NInput
              v-model:value="transferTarget"
              :placeholder="transferMode === 'rename' ? '输入新的文件或目录名' : '输入完整目标路径'"
              @keyup.enter="submitTransfer"
            />
          </NFormItem>
          <div class="transfer-hint">
            填写相对名称时，将解析为当前目录 <code>{{ currentPath }}</code> 下的路径。
          </div>
          <NSpace justify="end">
            <NButton @click="showTransfer = false">取消</NButton>
            <NButton type="primary" :loading="transferBusy" @click="submitTransfer">
              确定
            </NButton>
          </NSpace>
        </NForm>
      </div>
    </NModal>

    <!-- 文本预览 / 编辑 Modal -->
    <NModal
      v-model:show="showEditor"
      preset="card"
      :title="editorPath"
      style="width: 900px; max-width: 94vw"
      :mask-closable="false"
      :on-close="closeEditor"
    >
      <div class="editor-modal-body">
        <NSpin v-if="editorLoading" class="spin-center" />
        <template v-else>
          <NAlert v-if="editorReadOnly" type="warning" :bordered="false">
            只读模式：当前角色无写入权限，或该文件仅供预览。
          </NAlert>
          <NInput
            v-model:value="editorContent"
            type="textarea"
            class="editor-textarea"
            :readonly="editorReadOnly"
            :autosize="{ minRows: 18, maxRows: 26 }"
            spellcheck="false"
          />
          <NSpace justify="space-between" align="center">
            <span class="editor-status">
              {{ editorDirty ? '有未保存的修改' : '内容与主机一致' }}
            </span>
            <NSpace>
              <NButton @click="closeEditor">关闭</NButton>
              <NButton
                type="primary"
                :disabled="!editorDirty || editorReadOnly"
                :loading="editorSaving"
                @click="saveEditor"
              >
                <template #icon><NIcon><SaveOutline /></NIcon></template>
                保存
              </NButton>
            </NSpace>
          </NSpace>
        </template>
      </div>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.file-manager-pane {
  display: flex;
  flex-direction: column;
  gap: 12px;
  height: 100%;

  .file-nav-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-bottom: 8px;
    border-bottom: 1px solid var(--border-color);

    .nav-left,
    .nav-right {
      display: flex;
      align-items: center;
      gap: 10px;
    }

    .drives-quick-box {
      display: flex;
      gap: 4px;
    }

    .path-breadcrumb-box {
      background-color: var(--bg-card-subtle);
      border: 1px solid var(--border-color);
      padding: 4px 12px;
      border-radius: 4px;
      margin-left: 6px;
      min-width: 260px;
      cursor: text;

      .crumbs-view {
        display: flex;
        align-items: center;
      }

      .crumb-link {
        cursor: pointer;
        font-family: 'SFMono-Regular', Consolas, monospace;
        font-size: 13px;
        color: var(--text-primary);
        &:hover {
          color: #3b82f6;
          text-decoration: underline;
        }
      }

      .input-view {
        min-width: 320px;
      }
    }
  }

  .file-table-container {
    flex: 1;
    /* min-height:0 让容器可收缩，滚动交给表格自身的 flex-height */
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;

    :deep(.n-data-table) {
      flex: 1;
      min-height: 0;
    }

    :deep(.n-data-table-td) {
      white-space: nowrap;
    }

    :deep(.file-name-cell) {
      display: flex;
      align-items: center;
      gap: 8px;
      cursor: pointer;
      min-width: 0;
      white-space: nowrap;

      .file-icon {
        flex-shrink: 0;
      }

      .file-name-text {
        font-weight: 500;
        color: var(--text-primary);
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
        &:hover {
          color: #3b82f6;
        }
      }
    }

    .mono-font {
      font-family: 'SFMono-Regular', Consolas, monospace;
      font-size: 12px;
      color: var(--text-secondary);
    }

    :deep(.file-actions-cell) {
      display: flex;
      align-items: center;
      gap: 2px;
    }
  }

  .spin-center {
    display: flex;
    justify-content: center;
    align-items: center;
    height: 200px;
  }

  .upload-modal-body {
    display: flex;
    flex-direction: column;
    gap: 12px;

    .target-path-info {
      font-size: 13px;
      color: var(--text-secondary);
      code {
        color: #3b82f6;
        background: var(--bg-card-subtle);
        padding: 2px 6px;
        border-radius: 3px;
      }
    }
  }
}

.transfer-modal-body {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .transfer-src {
    font-size: 13px;
    color: var(--text-secondary);
    word-break: break-all;
  }

  .transfer-hint {
    font-size: 12px;
    color: var(--text-tertiary);
    margin-bottom: 12px;
  }

  code {
    font-family: 'SFMono-Regular', Consolas, monospace;
    color: #6366f1;
    background: var(--code-box-bg);
    padding: 1px 5px;
    border-radius: 3px;
  }
}

.editor-modal-body {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .editor-textarea :deep(textarea) {
    font-family: 'SFMono-Regular', Consolas, monospace;
    font-size: 13px;
    line-height: 1.6;
  }

  .editor-status {
    font-size: 12px;
    color: var(--text-tertiary);
  }
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .file-manager-pane {
    gap: 8px;

    .file-nav-bar {
      flex-wrap: wrap;
      gap: 8px;

      .nav-left,
      .nav-right {
        flex-wrap: wrap;
        gap: 6px;
      }

      .path-breadcrumb-box {
        /* 路径栏占满一整行并可横向滚动 */
        order: 3;
        flex: 1 1 100%;
        min-width: 0;
        margin-left: 0;
        overflow-x: auto;

        .crumbs-view {
          overflow-x: auto;
          white-space: nowrap;
        }

        .input-view {
          min-width: 0;
          width: 100%;

          :deep(.n-input) {
            width: 100%;
          }
        }
      }
    }
  }
}
</style>
