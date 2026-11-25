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
  NCheckbox,
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
  ClipboardOutline,
  ArchiveOutline,
  KeyOutline,
  ArrowForwardOutline,
  TrashOutline,
  EyeOutline,
  SaveOutline,
} from '@vicons/ionicons5'
import {
  fileList,
  fileStat,
  fileMkdir,
  fileDownloadUrl,
  fileUpload,
  fileMove,
  fileCopy,
  fileRemove,
  fileChmod,
  fileCompress,
  fileReadText,
  fileReadHead,
  fileWriteText,
  getHost,
} from '../../api/hosts'
import type { FileInfo } from '../../api/types'
import { useAuthStore } from '../../stores/auth'
import { useSettingsStore } from '../../stores/settings'
import { SETTING_KEYS } from '../../api/settings'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'
import { isTextContent, TEXT_SNIFF_BYTES } from '../../utils/fileText'
import CodeEditor from '../common/CodeEditor.vue'

const props = defineProps<{
  hostId: string
  os?: string
  /** 从应用挂载卷等入口直达某个目录：优先于设置里的默认路径。 */
  initialPath?: string
  /** 批量操作开关：仅独立文件管理页（/files/:id）开启；主机详情文件页签
      复用本面板但不在本批范围，保持原样（不传即关闭）。 */
  batchEnabled?: boolean
}>()

const message = useMessage()
const dialog = useDialog()
const auth = useAuthStore()
const settingsStore = useSettingsStore()
// 首轮加载完成前保持 loading：首帧若以 files=[] 渲染表格会闪出「无数据」假空状态
//（onMounted 须先 await detectOS 与 settings 加载，load() 尚未开始）。
const loading = ref(true)
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

// ---- 批量操作（删除 / 下载 / 移动）--------------------------------------
// 范式同 NetworkList.vue：勾选按 path 跨页保留、3 台并发、复用单机接口、
// 执行期间逐行显示进度、失败可重试、完成后给成功/失败汇总。
// 选择集只在当前目录内有效：目录切换或刷新后清理已不存在的勾选（见 load）。
const selectedFiles = ref(new Map<string, FileInfo>())
const BATCH_CONCURRENCY = 3
type BatchOpPhase = 'queued' | 'removing' | 'downloading' | 'moving' | 'done' | 'failed'
const batchOpStatus = ref<Record<string, { phase: BatchOpPhase; message?: string }>>({})
const batchRunning = ref(false)
const batchKind = ref<'remove' | 'download' | 'move' | null>(null)
const batchSummary = ref<{ done: number; failed: number; skipped: number } | null>(null)
// 批量删除确认弹窗（危险操作，弹窗内就地确认）与批量移动目标目录弹窗
const showBatchRemove = ref(false)
const showBatchMove = ref(false)
const batchMoveTarget = ref('')
const batchMoveTargetResolved = ref('')

const selectedList = computed(() => [...selectedFiles.value.values()])
const selectedFileCount = computed(() => selectedList.value.filter((f) => !f.is_dir).length)
const selectedDirCount = computed(() => selectedList.value.filter((f) => f.is_dir).length)
// NDataTable 全量数据已在内存：checked-row-keys 传全部已选 key，
// 翻页时其他页的勾选态由该数组维持；onCheckedRowKeys 按全量 files 合并。
const pageCheckedKeys = computed(() =>
  files.value.filter((f) => selectedFiles.value.has(f.path)).map((f) => f.path),
)

function onCheckedRowKeys(keys: Array<string | number>) {
  const keySet = new Set(keys.map((k) => String(k)))
  files.value.forEach((f) => {
    if (keySet.has(f.path)) selectedFiles.value.set(f.path, f)
    else selectedFiles.value.delete(f.path)
  })
}

function clearBatchSelection() {
  selectedFiles.value.clear()
  batchOpStatus.value = {}
  batchSummary.value = null
  batchKind.value = null
}


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
// 光标位置（行/列，1 起）：CodeEditor 的 cursor 事件回填，展示在编辑器状态栏。
const editorCursor = ref({ line: 1, col: 1 })
// 当前文件的语言展示名（CodeEditor 的 language 事件回填）与文件大小（字节）。
const editorLanguage = ref('')
const editorSize = ref(0)

// Text files above this size are refused rather than loaded into the browser.
const maxEditableSize = 2 * 1024 * 1024

const sep = computed(() => (isWindows.value ? '\\' : '/'))

const transferTitle = computed(() => {
  if (transferMode.value === 'rename') return '重命名'
  if (transferMode.value === 'move') return '移动'
  return '复制'
})

const editorDirty = computed(() => editorContent.value !== editorOriginal.value)

// 编辑器状态栏：行/列 · 语言 · 文件大小 · 未保存标记（语言未解析出时不占位）。
const editorStatus = computed(() => {
  const parts = [`行 ${editorCursor.value.line}，列 ${editorCursor.value.col}`]
  if (editorLanguage.value) parts.push(editorLanguage.value)
  parts.push(fmtSize(editorSize.value))
  parts.push(editorDirty.value ? '有未保存的修改' : '内容与主机一致')
  return parts.join(' · ')
})

function isPathCompatibleWithOS(path: string, isWin: boolean): boolean {
  if (!path) return false
  const isWinFormat = /^[a-zA-Z]:[\\/]/.test(path)
  const isPosixFormat = path.startsWith('/') || path.startsWith('~')
  return isWin ? isWinFormat : isPosixFormat
}

// 未配 initialPath / 初始路径打不开时的回落路径：设置默认路径优先于 OS 默认。
function defaultPath(): string {
  const custom = settingsStore.getUserKey<string>(SETTING_KEYS.filesDefaultPath, '').trim()
  if (custom && isPathCompatibleWithOS(custom, isWindows.value)) return custom
  return isWindows.value ? 'C:\\' : '/root'
}

// 取父目录（与 goBack 同规则，抽出来供初始路径解析复用）。
function parentDirOf(p: string): string {
  const norm = p.replace(/\\/g, '/')
  const isWin = isWindows.value || /^[a-zA-Z]:/.test(norm)
  if (isWin) {
    const driveMatch = norm.match(/^([a-zA-Z]:)/)
    const drive = driveMatch ? driveMatch[1] : 'C:'
    const sub = norm.slice(drive.length).split('/').filter(Boolean)
    if (sub.length > 0) {
      sub.pop()
      return sub.length === 0 ? drive + '\\' : drive + '\\' + sub.join('\\')
    }
    return drive + '\\'
  }
  const parts = norm.split('/').filter(Boolean)
  if (parts.length > 1) {
    parts.pop()
    return '/' + parts.join('/')
  }
  return '/'
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

async function load(silent = false): Promise<string | null> {
  loading.value = true
  try {
    const data = await fileList(props.hostId, currentPath.value)
    files.value = data || []
    resetPage()
    pathInput.value = currentPath.value
    // 清理已不存在的勾选：目录切换/刷新后，只保留仍在当前目录列表里的已选项
    const alive = new Set(files.value.map((f) => f.path))
    selectedFiles.value.forEach((_, p) => {
      if (!alive.has(p)) selectedFiles.value.delete(p)
    })
    // 非批量进行中、且无待重试的批量结果时，旧的行内进度不再对应当前列表
    if (!batchRunning.value && !batchSummary.value) {
      batchOpStatus.value = {}
    }
    return null
  } catch (e: any) {
    const msg = e.message || '获取文件列表失败'
    if (!silent) message.error(msg)
    return msg
  } finally {
    loading.value = false
  }
}

// 初始进入的路径解析：单文件挂载（如 custom.cnf）自动进父目录；
// 路径不存在或不可读时回落默认路径。只在进入时做一次，不影响手动导航。
async function loadInitial() {
  const firstError = await load(true)
  if (firstError === null) return
  try {
    const st = await fileStat(props.hostId, currentPath.value)
    if (st && st.is_dir === false) {
      const parent = parentDirOf(currentPath.value)
      if (parent && parent !== currentPath.value) {
        currentPath.value = parent
        pathInput.value = parent
        if ((await load(true)) === null) return
      }
    }
  } catch {
    // stat 失败则继续回落默认路径
  }
  const fallback = defaultPath()
  if (fallback !== currentPath.value) {
    currentPath.value = fallback
    pathInput.value = fallback
    await load()
  } else {
    message.error(firstError)
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
  currentPath.value = parentDirOf(currentPath.value)
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
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GB'
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB'
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + ' KB'
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

// ---- 批量删除 / 下载 / 移动 --------------------------------------------
// 前端并发编排（3 路并发），复用单文件接口，不新开后端批量接口；
// 写操作审计走既有单机接口的审计，无需新埋点。
async function runBatch(kind: 'remove' | 'download' | 'move', targets: FileInfo[]) {
  // 下载只针对文件：目录没有单文件下载接口，跳过并计入 skipped
  let eligible = targets
  let skipped = 0
  if (kind === 'download') {
    eligible = targets.filter((f) => !f.is_dir)
    skipped = targets.length - eligible.length
  }
  if (eligible.length === 0) {
    message.warning(
      kind === 'download' ? '所选项目中没有可下载的文件（目录不支持下载）' : '没有可执行的目标',
    )
    return
  }

  batchRunning.value = true
  batchKind.value = kind
  batchSummary.value = null
  batchOpStatus.value = {}
  eligible.forEach((f) => {
    batchOpStatus.value[f.path] = { phase: 'queued' }
  })
  let done = 0
  let failed = 0

  if (kind === 'download') {
    // 下载复用单文件下载链接逐个触发：同步连续触发、不加 await 延迟——
    // 离开点击手势的异步回调会被浏览器拦截为弹窗。浏览器是否真正落盘
    // 不可观测，行内只标记「已触发」；被拦截时浏览器地址栏有提示。
    eligible.forEach((f) => {
      batchOpStatus.value[f.path] = { phase: 'downloading' }
    })
    eligible.forEach((f) => downloadFile(f.path))
    eligible.forEach((f) => {
      batchOpStatus.value[f.path] = { phase: 'done' }
    })
    done = eligible.length
  } else {
    const queue = [...eligible]
    async function worker() {
      while (queue.length > 0) {
        const file = queue.shift()!
        try {
          if (kind === 'remove') {
            batchOpStatus.value[file.path] = { phase: 'removing' }
            await fileRemove(props.hostId, file.path)
          } else {
            batchOpStatus.value[file.path] = { phase: 'moving' }
            await fileMove(props.hostId, file.path, joinPath(batchMoveTargetResolved.value, file.name))
          }
          batchOpStatus.value[file.path] = { phase: 'done' }
          done += 1
        } catch (e: any) {
          batchOpStatus.value[file.path] = { phase: 'failed', message: e.message || '执行出错' }
          failed += 1
        }
      }
    }
    await Promise.all(
      Array.from({ length: Math.min(BATCH_CONCURRENCY, eligible.length) }, () => worker()),
    )
  }

  batchRunning.value = false
  batchSummary.value = { done, failed, skipped }
  const verb = kind === 'remove' ? '删除' : kind === 'download' ? '下载' : '移动'
  const skipText = skipped > 0 ? `，跳过 ${skipped} 项（目录不支持下载）` : ''
  if (failed === 0) {
    message.success(`批量${verb}完成：成功 ${done} 项${skipText}`)
  } else {
    message.warning(
      `批量${verb}完成：成功 ${done} 项，失败 ${failed} 项${skipText}，可点「重试失败」只重跑失败的项目`,
    )
  }
  // 刷新一次列表：已删/已移走的项随之消失，勾选由 load() 的清理逻辑同步剪掉；
  // 失败项仍在列表里、保持勾选，可直接重试。
  await load()
}

function retryBatchFailed() {
  if (!batchKind.value || !batchSummary.value || batchSummary.value.failed === 0) return
  const failedPaths = new Set(
    Object.entries(batchOpStatus.value)
      .filter(([, st]) => st.phase === 'failed')
      .map(([p]) => p),
  )
  const targets = files.value.filter((f) => failedPaths.has(f.path))
  runBatch(batchKind.value, targets)
}

function openBatchMove() {
  batchMoveTarget.value = currentPath.value
  showBatchMove.value = true
}

async function confirmBatchMove() {
  const dest = resolveTarget(batchMoveTarget.value)
  if (!dest) {
    message.warning('请填写目标目录')
    return
  }
  if (dest === currentPath.value) {
    message.warning('目标目录与当前目录相同，无需移动')
    return
  }
  batchMoveTargetResolved.value = dest
  showBatchMove.value = false
  await runBatch('move', selectedList.value)
}

async function confirmBatchRemove() {
  showBatchRemove.value = false
  await runBatch('remove', selectedList.value)
}

// ---- 编辑权限（chmod）-------------------------------------------------
// 权限值以位表示（0–07777），复选框与八进制输入双向同步；弹窗打开时按行
// 的 mode 字符串（如 "-rw-r--r--"）初始化，解析不出时按文件 0644 / 目录
// 0755 回落。
const showChmod = ref(false)
const chmodTarget = ref<FileInfo | null>(null)
const chmodPerm = ref(0o644)
const chmodOctal = ref('644')
const chmodBusy = ref(false)

const chmodClasses = [
  { label: '所有者', items: [{ bit: 0o400, label: '读取' }, { bit: 0o200, label: '写入' }, { bit: 0o100, label: '执行' }] },
  { label: '所属组', items: [{ bit: 0o040, label: '读取' }, { bit: 0o020, label: '写入' }, { bit: 0o010, label: '执行' }] },
  { label: '其他用户', items: [{ bit: 0o004, label: '读取' }, { bit: 0o002, label: '写入' }, { bit: 0o001, label: '执行' }] },
  { label: '特殊权限', items: [{ bit: 0o4000, label: 'SetUID' }, { bit: 0o2000, label: 'SetGID' }, { bit: 0o1000, label: '粘滞位' }] },
]

function parseModeString(mode: string, isDir: boolean): number {
  const fallback = isDir ? 0o755 : 0o644
  if (!mode || mode.length < 10) return fallback
  const s = mode.slice(1, 10)
  const bit = [0o400, 0o200, 0o100, 0o040, 0o020, 0o010, 0o004, 0o002, 0o001]
  let perm = 0
  for (let i = 0; i < 9; i++) {
    const ch = s[i]
    if (ch === 'r' || ch === 'w' || ch === 'x' || ch === 's' || ch === 't') perm |= bit[i]
  }
  if (s[2] === 's' || s[2] === 'S') perm |= 0o4000
  if (s[5] === 's' || s[5] === 'S') perm |= 0o2000
  if (s[8] === 't' || s[8] === 'T') perm |= 0o1000
  return perm
}

// permString renders bits the way ls does ("rwxr-xr-x", setuid/setgid as s/S,
// sticky as t/T); the modal preview only ever shows the applied value.
function permString(perm: number): string {
  const out: string[] = []
  for (let cls = 0; cls < 3; cls++) {
    const shift = 6 - cls * 3
    const r = (perm >> (shift + 2)) & 1
    const w = (perm >> (shift + 1)) & 1
    const x = (perm >> shift) & 1
    out.push(r ? 'r' : '-', w ? 'w' : '-')
    if (cls === 0 && (perm & 0o4000)) out.push(x ? 's' : 'S')
    else if (cls === 1 && (perm & 0o2000)) out.push(x ? 's' : 'S')
    else if (cls === 2 && (perm & 0o1000)) out.push(x ? 't' : 'T')
    else out.push(x ? 'x' : '-')
  }
  return out.join('')
}

function openChmod(file: FileInfo) {
  chmodTarget.value = file
  const perm = parseModeString(file.mode || '', file.is_dir)
  chmodPerm.value = perm
  chmodOctal.value = perm.toString(8)
  showChmod.value = true
}

function toggleChmodBit(bit: number, checked: boolean) {
  if (checked) chmodPerm.value |= bit
  else chmodPerm.value &= ~bit
  chmodOctal.value = chmodPerm.value.toString(8)
}

function onChmodOctalInput() {
  const v = chmodOctal.value.trim()
  if (!/^[0-7]{1,4}$/.test(v)) return
  const n = parseInt(v, 8)
  if (n >= 0 && n <= 0o7777) chmodPerm.value = n
}

async function submitChmod() {
  const target = chmodTarget.value
  if (!target) return
  const v = chmodOctal.value.trim()
  if (!/^[0-7]{1,4}$/.test(v)) {
    message.warning('请输入 0–7777 之间的八进制权限值')
    return
  }
  const mode = parseInt(v, 8)
  chmodBusy.value = true
  try {
    await fileChmod(props.hostId, target.path, mode)
    message.success(`权限已改为 ${v}`)
    showChmod.value = false
    await load()
  } catch (e: any) {
    message.error(e.message || '修改权限失败')
  } finally {
    chmodBusy.value = false
  }
}

// ---- 复制路径 / 压缩 --------------------------------------------------
// 复制路径是纯只读操作，所有角色可用；剪贴板 API 不可用时回落传统方式。
async function copyPath(path: string) {
  const done = () => message.success('已复制路径')
  try {
    await navigator.clipboard.writeText(path)
    done()
    return
  } catch {
    // 非安全上下文或无剪贴板权限时回落到传统方式
  }
  const ta = document.createElement('textarea')
  ta.value = path
  ta.style.position = 'fixed'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  try {
    document.execCommand('copy')
    done()
  } catch {
    message.error('复制失败')
  }
  document.body.removeChild(ta)
}

// 压缩是非破坏性操作（只产生一个新文件），确认后直接执行；大目录可能要
// 一两分钟，期间用常驻提示占位，服务端 120 秒未回则提示超时、后台继续压。
function confirmCompress(file: FileInfo) {
  dialog.info({
    title: '压缩',
    content: `将把 ${file.path} 压缩为同目录下的 ${file.name}.tar.gz（同名时自动加序号）。`,
    positiveText: '压缩',
    negativeText: '取消',
    onPositiveClick: async () => {
      const loadingMsg = message.loading('正在压缩…', { duration: 0 })
      try {
        const res = await fileCompress(props.hostId, file.path)
        message.success(`已压缩为 ${res.archive}（${fmtSize(res.size)}）`)
        await load()
      } catch (e: any) {
        message.error(e.message || '压缩失败')
      } finally {
        loadingMsg.destroy()
      }
    },
  })
}

async function openEditor(file: FileInfo, readOnly: boolean) {
  if (file.size > maxEditableSize) {
    message.warning(`文件超过 ${fmtSize(maxEditableSize)}，请下载后查看`)
    return
  }
  // Sniff the file's head before opening: the menu no longer filters by
  // extension, so a binary file would otherwise load as mojibake and risk
  // being mangled by a save round trip.
  let head: Uint8Array
  try {
    head = await fileReadHead(props.hostId, file.path, TEXT_SNIFF_BYTES)
  } catch (e: any) {
    message.error(e.message || '读取文件失败')
    return
  }
  if (!isTextContent(head)) {
    message.error('该文件是二进制文件，无法在线预览/编辑，请下载后查看')
    return
  }
  editorPath.value = file.path
  editorReadOnly.value = readOnly || !canWrite.value
  editorContent.value = ''
  editorOriginal.value = ''
  editorLanguage.value = ''
  editorSize.value = file.size
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
// the caller's role are offered. The preview/edit entry is offered for every
// regular file within the size cap — whether the content is actually text is
// decided by sniffing the file's head when it is opened, not by extension.
// Copy-path is read-only, so viewers get it too; chmod/compress are writes.
function rowMenuOptions(row: FileInfo) {
  const opts: any[] = []
  if (!row.is_dir) {
    opts.push({
      key: 'preview',
      label: canWrite.value ? '预览 / 编辑' : '预览',
      icon: () => h(NIcon, { component: canWrite.value ? CreateOutline : EyeOutline }),
    })
  }
  if (canWrite.value) {
    if (opts.length) opts.push({ key: 'd1', type: 'divider' })
    opts.push(
      { key: 'chmod', label: '编辑权限', icon: () => h(NIcon, { component: KeyOutline }) },
      { key: 'compress', label: '压缩', icon: () => h(NIcon, { component: ArchiveOutline }) },
      { key: 'd2', type: 'divider' },
      { key: 'rename', label: '重命名', icon: () => h(NIcon, { component: CreateOutline }) },
      { key: 'move', label: '移动到…', icon: () => h(NIcon, { component: ArrowForwardOutline }) },
      { key: 'copy', label: '复制到…', icon: () => h(NIcon, { component: CopyOutline }) },
      { key: 'copy-path', label: '复制路径', icon: () => h(NIcon, { component: ClipboardOutline }) },
      { key: 'd3', type: 'divider' },
      {
        key: 'remove',
        label: '删除',
        icon: () => h(NIcon, { component: TrashOutline }),
        props: { style: 'color: #e88080' },
      },
    )
  } else {
    if (opts.length) opts.push({ key: 'd1', type: 'divider' })
    opts.push({ key: 'copy-path', label: '复制路径', icon: () => h(NIcon, { component: ClipboardOutline }) })
  }
  return opts
}

function handleRowMenu(key: string, row: FileInfo) {
  switch (key) {
    case 'preview':
      openEditor(row, !canWrite.value)
      break
    case 'chmod':
      openChmod(row)
      break
    case 'compress':
      confirmCompress(row)
      break
    case 'copy-path':
      copyPath(row.path)
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

const baseColumns: DataTableColumns<FileInfo> = [
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
          // Directories navigate in; regular files open in the preview/editor
          // (same as the menu's 预览 / 编辑, including the size cap and the
          // text/binary sniff).
          onClick: () => {
            if (row.is_dir) {
              navigateInto(row)
            } else {
              openEditor(row, !canWrite.value)
            }
          },
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
      return fmtDateTime(row.mod_time)
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

// 批量列：勾选列（NDataTable 勾选列宽 40px，BatchExec.vue 同款；28px 是
// DESIGN.md §9.20 自定义行列表的口径）与批量状态列只在独立文件管理页
// 开启；执行期间本页勾选框禁用，防批量进行中手动改勾选。
const columns = computed<DataTableColumns<FileInfo>>(() => {
  const cols: DataTableColumns<FileInfo> = [...baseColumns]
  if (!props.batchEnabled) return cols
  cols.unshift({
    type: 'selection',
    width: 40,
    disabled: () => batchRunning.value,
  })
  cols.push({
    title: '批量状态',
    key: '__batch_status',
    width: 110,
    render(row) {
      const st = batchOpStatus.value[row.path]
      if (!st) return ''
      const text =
        st.phase === 'queued'
          ? '排队中…'
          : st.phase === 'removing'
            ? '删除中…'
            : st.phase === 'downloading'
              ? '下载中…'
              : st.phase === 'moving'
                ? '移动中…'
                : st.phase === 'done'
                  ? '已完成'
                  : `失败：${st.message || '执行出错'}`
      return h('span', { class: `batch-status is-${st.phase}`, title: st.message || '' }, text)
    },
  })
  return cols
})

// 初始路径优先级：显式 initialPath > 设置默认路径 > OS 默认。
async function initPathAndLoad() {
  await detectOS()
  await settingsStore.load()
  const explicit = (props.initialPath || '').trim()
  if (explicit && isPathCompatibleWithOS(explicit, isWindows.value)) {
    currentPath.value = explicit
    pathInput.value = explicit
  } else {
    currentPath.value = defaultPath()
    pathInput.value = currentPath.value
  }
  await loadInitial()
}

watch(
  () => props.hostId,
  async () => {
    hostOs.value = ''
    await initPathAndLoad()
  }
)

// 带 ?path= 再次进入已缓存的页面时，initialPath 变化即跳转（空值忽略，
// 只作进入指令，不接管面板内的手动导航）。
watch(
  () => props.initialPath,
  async (next, prev) => {
    if (!next || next === prev || next === currentPath.value) return
    if (!isPathCompatibleWithOS(next, isWindows.value)) return
    currentPath.value = next
    pathInput.value = next
    await loadInitial()
  }
)

onMounted(async () => {
  await initPathAndLoad()
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
          <NButton secondary @click="() => load()" title="刷新">
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

    <!-- 批量操作栏：有勾选时出现（仅独立文件管理页，主机详情文件页签不开启） -->
    <div v-if="batchEnabled && selectedFiles.size > 0" class="batch-bar">
      <span class="batch-info">
        已选 {{ selectedFiles.size }} 项
        <span class="batch-sub">（文件 {{ selectedFileCount }} 个 · 目录 {{ selectedDirCount }} 个）</span>
      </span>
      <NButton
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'download'"
        :disabled="batchRunning || selectedFileCount === 0"
        @click="runBatch('download', selectedList)"
      >
        批量下载
      </NButton>
      <NButton
        v-if="canWrite"
        size="small"
        type="primary"
        secondary
        :loading="batchRunning && batchKind === 'move'"
        :disabled="batchRunning"
        @click="openBatchMove"
      >
        批量移动
      </NButton>
      <NButton
        v-if="canWrite"
        size="small"
        type="error"
        secondary
        :loading="batchRunning && batchKind === 'remove'"
        :disabled="batchRunning"
        @click="showBatchRemove = true"
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

    <!-- 文件数据表格：表头固定，仅数据区滚动（AGENTS.md 8.2） -->
    <NSpin v-if="loading" class="spin-center" />
    <div v-else class="file-table-container">
      <NDataTable
        flex-height
        :columns="columns"
        :data="files"
        :pagination="pagination"
        :bordered="false"
        :scroll-x="750"
        size="small"
        :row-key="(row: FileInfo) => row.path"
        :checked-row-keys="pageCheckedKeys"
        @update:checked-row-keys="onCheckedRowKeys"
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

    <!-- 批量删除 Modal：危险操作，弹窗内就地确认 -->
    <NModal
      v-model:show="showBatchRemove"
      preset="card"
      title="批量删除"
      style="width: 520px; max-width: 90vw"
    >
      <div class="batch-confirm-body">
        <NAlert type="warning" :bordered="false">
          确认删除以下 {{ selectedFiles.size }} 项吗？目录会递归删除全部内容，此操作不可恢复。
        </NAlert>
        <div class="batch-confirm-list">
          <div v-for="f in selectedList.slice(0, 8)" :key="f.path" class="batch-confirm-item">
            {{ f.name }}<span class="batch-confirm-path">{{ f.path }}</span>
          </div>
          <div v-if="selectedFiles.size > 8" class="batch-confirm-more">
            等 {{ selectedFiles.size - 8 }} 项…
          </div>
        </div>
      </div>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBatchRemove = false">取消</NButton>
          <NButton type="error" @click="confirmBatchRemove">
            确认删除（{{ selectedFiles.size }} 项）
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 批量移动 Modal：目标目录选择，文件名保持不变 -->
    <NModal
      v-model:show="showBatchMove"
      preset="card"
      title="批量移动"
      style="width: 520px; max-width: 90vw"
    >
      <div class="batch-confirm-body">
        <NAlert type="default" :bordered="false">
          将 {{ selectedFiles.size }} 项移动到目标目录，文件名保持不变。
        </NAlert>
        <NForm label-placement="top">
          <NFormItem label="目标目录">
            <NInput
              v-model:value="batchMoveTarget"
              placeholder="输入完整目标目录路径"
              @keyup.enter="confirmBatchMove"
            />
          </NFormItem>
          <div class="transfer-hint">
            填写相对名称时，将解析为当前目录 <code>{{ currentPath }}</code> 下的路径。
          </div>
          <NSpace justify="end">
            <NButton @click="showBatchMove = false">取消</NButton>
            <NButton type="primary" @click="confirmBatchMove">
              确认移动
            </NButton>
          </NSpace>
        </NForm>
      </div>
    </NModal>

    <!-- 编辑权限 Modal -->
    <NModal
      v-model:show="showChmod"
      preset="card"
      title="编辑权限"
      style="width: 440px"
    >
      <div class="chmod-modal-body">
        <NAlert type="default" :bordered="false">
          <div class="transfer-src">
            路径：<code>{{ chmodTarget?.path }}</code>
          </div>
        </NAlert>
        <NForm label-placement="top">
          <NFormItem v-for="cls in chmodClasses" :key="cls.label" :label="cls.label">
            <NSpace>
              <NCheckbox
                v-for="item in cls.items"
                :key="item.bit"
                :checked="!!(chmodPerm & item.bit)"
                @update:checked="(v: boolean) => toggleChmodBit(item.bit, v)"
              >
                {{ item.label }}
              </NCheckbox>
            </NSpace>
          </NFormItem>
          <NFormItem label="八进制权限值">
            <NInput v-model:value="chmodOctal" placeholder="例如 755" @input="onChmodOctalInput" />
          </NFormItem>
          <div class="chmod-preview">
            当前权限：<code>{{ chmodPerm.toString(8) }}（{{ permString(chmodPerm) }}）</code>
          </div>
          <NSpace justify="end">
            <NButton @click="showChmod = false">取消</NButton>
            <NButton type="primary" :loading="chmodBusy" @click="submitChmod">
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
      style="width: 1100px; max-width: 94vw"
      :mask-closable="false"
      :on-close="closeEditor"
    >
      <div class="editor-modal-body">
        <NSpin v-if="editorLoading" class="spin-center" />
        <template v-else>
          <NAlert v-if="editorReadOnly" type="warning" :bordered="false">
            只读模式：当前角色无写入权限，或该文件仅供预览。
          </NAlert>
          <div class="editor-wrap">
            <CodeEditor
              v-model="editorContent"
              :filename="editorPath"
              :readonly="editorReadOnly"
              @save="saveEditor"
              @cursor="editorCursor = $event"
              @language="editorLanguage = $event"
            />
          </div>
          <NSpace justify="space-between" align="center">
            <span class="editor-status">
              {{ editorStatus }}
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

  /* 批量操作栏：与 NetworkList.vue 同款（有勾选时出现，贴在导航栏下方） */
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

    .batch-sub {
      font-size: 12px;
      font-weight: 400;
      color: var(--text-secondary);
    }

    .batch-summary {
      font-size: 12px;
      color: var(--text-secondary);
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

    /* 批量状态列：render 生成的节点，用 :deep 穿透（与 .file-name-cell 同款） */
    :deep(.batch-status) {
      font-size: 12px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;

      &.is-queued,
      &.is-removing,
      &.is-downloading,
      &.is-moving {
        color: #3b82f6;
      }

      &.is-done {
        color: #059669;
      }

      &.is-failed {
        color: #ef4444;
      }
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

/* 批量删除 / 移动确认弹窗（与 .transfer-modal-body 同款顶层样式；
   NModal 内容 teleport 到 body，仍由本组件渲染、作用域属性跟随节点） */
.batch-confirm-body {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .batch-confirm-list {
    max-height: 240px;
    overflow-y: auto;
    border: 1px solid var(--border-color);
    border-radius: 6px;
    padding: 4px 12px;
  }

  .batch-confirm-item {
    font-size: 13px;
    padding: 6px 0;
    border-bottom: 1px solid var(--border-color);

    &:last-child {
      border-bottom: none;
    }
  }

  .batch-confirm-path {
    display: block;
    font-size: 12px;
    color: var(--text-secondary);
    font-family: 'SFMono-Regular', Consolas, monospace;
    word-break: break-all;
  }

  .batch-confirm-more {
    font-size: 12px;
    color: var(--text-secondary);
    padding: 6px 0;
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

.chmod-modal-body {
  display: flex;
  flex-direction: column;
  gap: 12px;

  .transfer-src {
    font-size: 13px;
    color: var(--text-secondary);
    word-break: break-all;
  }

  .chmod-preview {
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

  .editor-wrap {
    height: min(68vh, 640px);
    min-height: 300px;
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
