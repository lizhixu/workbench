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
  NSpin,
  NUpload,
  NIcon,
  NButtonGroup,
  type DataTableColumns,
} from 'naive-ui'
import {
  ArrowUpOutline,
  RefreshOutline,
  FolderOutline,
  DocumentOutline,
  AddOutline,
  CloudUploadOutline,
} from '@vicons/ionicons5'
import {
  fileList,
  fileMkdir,
  fileDownloadUrl,
  fileUpload,
  getHost,
} from '../../api/hosts'
import type { FileInfo } from '../../api/types'

const props = defineProps<{
  hostId: string
  os?: string
}>()

const message = useMessage()
const loading = ref(false)
const hostOs = ref(props.os || '')

// Set default path based on OS: Windows -> "C:\", Linux -> "/root"
const isWindows = computed(() => {
  const osLower = (hostOs.value || props.os || '').toLowerCase()
  return osLower.includes('win')
})

const currentPath = ref(isWindows.value ? 'C:\\' : '/root')
const pathInput = ref(currentPath.value)
const isEditingPath = ref(false)
const files = ref<FileInfo[]>([])

const showMkdir = ref(false)
const mkdirPath = ref('')
const showUpload = ref(false)

async function detectOS() {
  if (!hostOs.value && props.hostId) {
    try {
      const h = await getHost(props.hostId)
      if (h && h.os) {
        hostOs.value = h.os
        if (isWindows.value && currentPath.value === '/root') {
          currentPath.value = 'C:\\'
          pathInput.value = 'C:\\'
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

const columns: DataTableColumns<FileInfo> = [
  {
    title: '名称',
    key: 'name',
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
    width: 120,
    render(row) {
      return row.is_dir ? '-' : fmtSize(row.size)
    },
  },
  {
    title: '属性',
    key: 'mode',
    width: 140,
    render(row) {
      return h('span', { class: 'mono-font' }, row.mode || '-')
    },
  },
  {
    title: '修改时间',
    key: 'mod_time',
    width: 180,
    render(row) {
      return row.mod_time?.slice(0, 19) || '-'
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 90,
    render(row) {
      if (row.is_dir) return null
      return h(
        NButton,
        {
          size: 'tiny',
          quaternary: true,
          type: 'primary',
          onClick: () => downloadFile(row.path),
        },
        { default: () => '下载' }
      )
    },
  },
]

watch(
  () => props.hostId,
  () => {
    detectOS().then(() => load())
  }
)

onMounted(async () => {
  await detectOS()
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
        <NButton type="primary" secondary size="small" @click="showMkdir = true">
          <template #icon><NIcon><AddOutline /></NIcon></template>
          新建目录
        </NButton>

        <NButton type="primary" size="small" @click="showUpload = true">
          <template #icon><NIcon><CloudUploadOutline /></NIcon></template>
          上传文件
        </NButton>
      </div>
    </div>

    <!-- 文件数据表格 -->
    <NSpin v-if="loading" class="spin-center" />
    <div v-else class="file-table-container">
      <NDataTable
        :columns="columns"
        :data="files"
        :bordered="false"
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
    overflow-y: auto;

    :deep(.file-name-cell) {
      display: flex;
      align-items: center;
      gap: 8px;
      cursor: pointer;

      .file-name-text {
        font-weight: 500;
        color: var(--text-primary);
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
</style>
