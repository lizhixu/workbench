<script setup lang="ts">
// Control-plane backup / restore (AGENTS.md 5.1 数据自主可控). Admin-only:
// an archive carries every credential, recording and audit entry the server
// holds, and a restore rewrites all of them.
import { computed, onMounted, ref, h } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NIcon,
  NInput,
  NDataTable,
  NEmpty,
  NAlert,
  NTag,
  NModal,
  NPopconfirm,
  NUpload,
  NProgress,
  useMessage,
  type DataTableColumns,
  type UploadFileInfo,
} from 'naive-ui'
import {
  ArchiveOutline,
  RefreshOutline,
  DownloadOutline,
  TrashOutline,
  CloudUploadOutline,
  ArrowUndoOutline,
} from '@vicons/ionicons5'
import {
  listBackups,
  createBackup,
  deleteBackup,
  restoreBackup,
  uploadRestore,
  backupDownloadUrl,
  type BackupMeta,
  type RestoreResult,
} from '../../api/backup'
import { useAuthStore } from '../../stores/auth'

const message = useMessage()
const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')

const entries = ref<BackupMeta[]>([])
const loading = ref(false)
const creating = ref(false)
const restoring = ref(false)
const note = ref('')

const showUpload = ref(false)
const uploadFile = ref<File | null>(null)
const uploadPercent = ref(0)

// Set after a successful restore: the running process still holds the
// pre-restore data in memory, so the banner stays until the page is reloaded.
const lastRestore = ref<{ result: RestoreResult; message: string } | null>(null)

function formatBytes(n: number): string {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

function formatTime(s: string): string {
  if (!s) return '-'
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? s : d.toLocaleString('zh-CN')
}

async function refresh() {
  loading.value = true
  try {
    entries.value = await listBackups()
  } catch (e: any) {
    message.error(e.message || '备份列表加载失败')
  } finally {
    loading.value = false
  }
}

async function onCreate() {
  creating.value = true
  try {
    const meta = await createBackup(note.value.trim())
    note.value = ''
    message.success(`备份完成：${meta.name}（${meta.files} 个文件，${formatBytes(meta.size)}）`)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '备份失败')
  } finally {
    creating.value = false
  }
}

function onDownload(row: BackupMeta) {
  // A stream response is fetched by navigation, not XHR, so the browser's own
  // download manager handles a multi-gigabyte archive.
  window.open(backupDownloadUrl(row.name), '_blank')
}

async function onDelete(row: BackupMeta) {
  try {
    await deleteBackup(row.name)
    message.success('备份已删除')
    await refresh()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

async function onRestore(row: BackupMeta) {
  restoring.value = true
  try {
    const res = await restoreBackup(row.name)
    lastRestore.value = { result: res.data, message: res.message }
    message.success(res.message)
    await refresh()
  } catch (e: any) {
    message.error(e.message || '恢复失败')
  } finally {
    restoring.value = false
  }
}

function onFileChange(data: { fileList: UploadFileInfo[] }) {
  uploadFile.value = (data.fileList[0]?.file as File) || null
}

async function onUploadRestore() {
  if (!uploadFile.value) {
    message.warning('请先选择一个备份归档文件')
    return
  }
  restoring.value = true
  uploadPercent.value = 0
  try {
    const res = await uploadRestore(uploadFile.value, (p) => (uploadPercent.value = p))
    lastRestore.value = { result: res.data, message: res.message }
    message.success(res.message)
    showUpload.value = false
    uploadFile.value = null
    await refresh()
  } catch (e: any) {
    message.error(e.message || '上传恢复失败')
  } finally {
    restoring.value = false
  }
}

const columns = computed<DataTableColumns<BackupMeta>>(() => [
  {
    title: '归档文件',
    key: 'name',
    minWidth: 260,
    render: (row) =>
      h('div', {}, [
        h('div', { style: 'font-family: monospace; font-size: 12px' }, row.name),
        row.note
          ? h('div', { style: 'color: var(--text-secondary); font-size: 12px' }, row.note)
          : null,
      ]),
  },
  {
    title: '类型',
    key: 'auto',
    width: 100,
    render: (row) =>
      h(
        NTag,
        { size: 'small', type: row.auto ? 'warning' : 'success', bordered: false },
        { default: () => (row.auto ? '自动' : '手动') },
      ),
  },
  {
    title: '文件数',
    key: 'files',
    width: 90,
    render: (row) => (row.files ? String(row.files) : '-'),
  },
  {
    title: '大小',
    key: 'size',
    width: 100,
    render: (row) => formatBytes(row.size),
  },
  {
    title: '创建者',
    key: 'created_by',
    width: 110,
    render: (row) => row.created_by || '-',
  },
  {
    title: '创建时间',
    key: 'created_at',
    width: 170,
    render: (row) => formatTime(row.created_at),
  },
  {
    title: '操作',
    key: 'actions',
    width: 210,
    render: (row) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => onDownload(row) },
            {
              icon: () => h(NIcon, null, { default: () => h(DownloadOutline) }),
              default: () => '下载',
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => onRestore(row) },
            {
              trigger: () =>
                h(
                  NButton,
                  { size: 'tiny', quaternary: true, type: 'warning', disabled: restoring.value },
                  {
                    icon: () => h(NIcon, null, { default: () => h(ArrowUndoOutline) }),
                    default: () => '恢复',
                  },
                ),
              default: () =>
                '恢复会用该归档覆盖当前数据目录（用户、主机、录像、审计）。恢复前会自动备份当前状态，恢复后需要重启控制端进程。确认继续？',
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => onDelete(row) },
            {
              trigger: () =>
                h(
                  NButton,
                  { size: 'tiny', quaternary: true, type: 'error' },
                  {
                    icon: () => h(NIcon, null, { default: () => h(TrashOutline) }),
                    default: () => '删除',
                  },
                ),
              default: () => `确认删除归档 ${row.name}？该操作不可撤销。`,
            },
          ),
        ],
      }),
  },
])

onMounted(() => {
  if (isAdmin.value) refresh()
})
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><ArchiveOutline /></NIcon>
        备份与恢复
      </span>
    </template>
    <template #header-extra>
      <NSpace v-if="isAdmin" align="center" :size="8">
        <NButton size="small" quaternary :loading="loading" @click="refresh">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          刷新
        </NButton>
        <NButton size="small" :disabled="restoring" @click="showUpload = true">
          <template #icon><NIcon><CloudUploadOutline /></NIcon></template>
          上传归档恢复
        </NButton>
      </NSpace>
    </template>

    <NEmpty
      v-if="!isAdmin"
      description="备份与恢复仅管理员可用：归档中包含全部凭证、录像与审计记录。"
    />

    <NSpace v-else vertical :size="12">
      <p class="muted">
        备份会把整个数据目录（用户账号、主机注册信息、终端录像、监控历史、审计日志、设置）打包成一个
        tar.gz 归档，存放在服务端 <code>data/backups</code> 下，可下载后带到另一台机器恢复，实现跨机迁移。
        恢复采用覆盖写入：归档中有的文件会被写回，归档中没有的现有文件保持不动。
      </p>

      <NAlert v-if="lastRestore" type="warning" :bordered="false" title="恢复已完成，需要重启控制端">
        本次恢复写入 {{ lastRestore.result.files }} 个文件（{{ formatBytes(lastRestore.result.bytes) }}）。
        控制端进程仍持有恢复前的内存状态，并会在下次写入时覆盖磁盘，请尽快重启进程加载恢复后的数据。
        恢复前的状态已自动存档为
        <code>{{ lastRestore.result.safety_copy }}</code>，如需回退可直接恢复该归档。
      </NAlert>

      <NSpace align="center" :size="8">
        <NInput
          v-model:value="note"
          size="small"
          clearable
          placeholder="备注（可选），例如：升级前备份"
          style="width: 280px"
        />
        <NButton size="small" type="primary" :loading="creating" @click="onCreate">
          <template #icon><NIcon><ArchiveOutline /></NIcon></template>
          立即备份
        </NButton>
      </NSpace>

      <!-- 备份归档列表：辅助性小表格，限高不分页（AGENTS.md 8.2 例外） -->
      <NDataTable
        :columns="columns"
        :data="entries"
        :loading="loading"
        size="small"
        :bordered="false"
        :row-key="(r: BackupMeta) => r.name"
        :max-height="360"
      >
        <template #empty>
          <NEmpty description="暂无备份归档，点击「立即备份」创建第一个" />
        </template>
      </NDataTable>
    </NSpace>
  </NCard>

  <NModal
    v-model:show="showUpload"
    preset="card"
    title="上传归档并恢复"
    style="width: 560px"
  >
    <NSpace vertical :size="12">
      <NAlert type="warning" :bordered="false">
        恢复会用归档内容覆盖当前数据目录。恢复前系统会自动备份当前状态，恢复完成后需要重启控制端进程。
      </NAlert>
      <NUpload
        :max="1"
        :default-upload="false"
        accept=".gz,.tgz,application/gzip"
        @change="onFileChange"
      >
        <NButton>
          <template #icon><NIcon><CloudUploadOutline /></NIcon></template>
          选择备份归档（.tar.gz）
        </NButton>
      </NUpload>
      <NProgress
        v-if="restoring && uploadPercent > 0"
        type="line"
        :percentage="uploadPercent"
        :height="8"
      />
    </NSpace>
    <template #footer>
      <NSpace justify="end">
        <NButton :disabled="restoring" @click="showUpload = false">取消</NButton>
        <NButton
          type="warning"
          :loading="restoring"
          :disabled="!uploadFile"
          @click="onUploadRestore"
        >
          上传并恢复
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.muted {
  color: var(--text-secondary);
  font-size: 13px;
  line-height: 1.7;
  margin: 0;

  code {
    background-color: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 3px;
    padding: 1px 5px;
    font-size: 12px;
  }
}
</style>
