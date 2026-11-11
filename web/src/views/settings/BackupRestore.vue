<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NEmpty, NForm, NFormItem, NInput,
  NInputNumber, NModal, NPopconfirm, NProgress, NSelect, NSpace, NSwitch,
  NTag, NUpload, useMessage, type DataTableColumns, type UploadFileInfo,
} from 'naive-ui'
import {
  backupDownloadUrl, backupStatus, cancelPendingRestore, createBackup, deleteBackup,
  getBackupPolicy, listBackups, restoreBackup, saveBackupPolicy, uploadRestore,
  type BackupMeta, type BackupPolicy, type BackupTask,
} from '../../api/backup'
import { listS3Targets, type S3Target } from '../../api/snapshots'
import { restartServer } from '../../api/upgrade'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDateTime } from '../../utils/time'

defineOptions({ name: 'BackupRestore' })
const message = useMessage()
const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')
const entries = ref<BackupMeta[]>([])
const targets = ref<S3Target[]>([])
const task = ref<BackupTask | null>(null)
const pending = ref(false)
const loading = ref(false)
const submitting = ref(false)
const savingPolicy = ref(false)
const restarting = ref(false)
const loadError = ref('')
const maxUploadBytes = ref(65 * 1024 ** 3)
const note = ref('')
const targetID = ref<string | null>('local')
const policy = reactive<BackupPolicy>({ enabled: false, time: '03:00', retention: 7, target_id: 'local' })
const busy = computed(() => submitting.value || task.value?.status === 'running' || pending.value || restarting.value)
const targetOptions = computed(() => [
  { label: '仅控制端本地存储', value: 'local' },
  ...targets.value.map(t => ({ label: `本地 + S3 副本：${t.name}`, value: t.id })),
])
const { pagination } = useTablePagination({ pageSize: 10, rowCount: computed(() => entries.value.length) })
const phaseLabels: Record<string, string> = {
  queued: '任务已受理', archiving: '正在打包数据', uploading_s3: '正在上传 S3 副本',
  validating: '正在校验归档', awaiting_restart: '校验完成，等待重启应用', applying: '正在应用恢复', complete: '任务结束',
}
const taskTitle = computed(() => {
  if (task.value?.status === 'failed') return '任务失败'
  if (task.value?.status === 'success') return task.value.kind === 'restore' ? '恢复已应用' : '备份已完成'
  if (task.value?.status === 'cancelled') return '待恢复任务已取消'
  return phaseLabels[task.value?.phase || ''] || '任务处理中'
})
function bytes(n: number) {
  if (!n) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), 3)
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${units[i]}`
}

let active = false
let inFlight = false
let sequence = 0
let timer: ReturnType<typeof setInterval> | null = null
let statusKey = ''
async function poll() {
  if (!active || inFlight || !isAdmin.value) return
  inFlight = true
  const seq = sequence
  try {
    const res = await backupStatus()
    if (seq !== sequence) return
    task.value = res.data
    pending.value = res.pending
    maxUploadBytes.value = res.max_upload_bytes
    loadError.value = ''
    const key = `${res.data.id}:${res.data.status}`
    if (statusKey !== key) {
      statusKey = key
      const rows = await listBackups()
      if (seq === sequence) entries.value = rows
    }
  } catch (e: any) {
    if (seq === sequence && !restarting.value) loadError.value = e.message || '任务状态加载失败'
  } finally { inFlight = false }
}
async function refresh() {
  loading.value = true
  try { entries.value = await listBackups(); await poll() }
  catch (e: any) { message.error(e.message || '备份列表加载失败') }
  finally { loading.value = false }
}
function activate() {
  if (active || !isAdmin.value) return
  active = true
  poll()
  timer = setInterval(poll, 2000)
}
function deactivate() {
  active = false
  sequence++
  if (timer) clearInterval(timer)
  timer = null
}
onMounted(async () => {
  if (!isAdmin.value) return
  try {
    const [p, t] = await Promise.all([getBackupPolicy(), listS3Targets()])
    Object.assign(policy, p, { target_id: p.target_id || 'local' })
    targets.value = t
  } catch (e: any) { message.error(e.message || '备份配置加载失败') }
  activate()
})
onActivated(activate)
onDeactivated(deactivate)
onUnmounted(deactivate)

async function savePolicy() {
  savingPolicy.value = true
  try { Object.assign(policy, await saveBackupPolicy(policy)); message.success('自动备份策略已保存') }
  catch (e: any) { message.error(e.message || '保存失败') }
  finally { savingPolicy.value = false }
}
async function create() {
  if (busy.value) { message.warning('请等待当前任务结束'); return }
  submitting.value = true
  try { task.value = await createBackup(note.value.trim(), targetID.value || 'local'); note.value = ''; message.success('备份任务已受理') }
  catch (e: any) { message.error(e.message || '备份失败') }
  finally { submitting.value = false }
}
async function remove(row: BackupMeta) {
  submitting.value = true
  try {
    const res = await deleteBackup(row.name)
    if (res.warning) message.warning(`本地归档已删除；${res.warning}`)
    else message.success('本地归档及已记录的 S3 副本已删除')
    await refresh()
  }
  catch (e: any) { message.error(e.message || '删除失败') }
  finally { submitting.value = false }
}
async function cancelRestore() {
  try { await cancelPendingRestore(); await poll(); message.success('待恢复任务已取消') }
  catch (e: any) { message.error(e.message || '取消失败') }
}
async function applyRestore() {
  restarting.value = true
  try { await restartServer(); message.info('控制端正在重启，请稍后刷新并使用恢复后的账号登录', { duration: 12000 }) }
  catch (e: any) { restarting.value = false; message.error(e.message || '重启失败') }
}

const showRestore = ref(false)
const restoreName = ref('')
const fileList = ref<UploadFileInfo[]>([])
const vaultPass = ref('')
const confirmed = ref(false)
const uploadPercent = ref(0)
const selectedFile = computed(() => fileList.value[0]?.file || null)
const canSubmitRestore = computed(() => !busy.value && (!!restoreName.value || !!selectedFile.value))
function openRestore(row?: BackupMeta) {
  restoreName.value = row?.name || ''
  fileList.value = []
  vaultPass.value = ''
  confirmed.value = false
  uploadPercent.value = 0
  showRestore.value = true
}
watch([fileList, vaultPass, restoreName], () => { confirmed.value = false }, { deep: true })
watch(showRestore, value => { if (!value) { confirmed.value = false; vaultPass.value = ''; fileList.value = [] } })
async function submitRestore() {
  if (!confirmed.value || !canSubmitRestore.value) { message.warning('请核对归档并确认恢复'); return }
  if (selectedFile.value && selectedFile.value.size > maxUploadBytes.value) { message.warning(`归档不能超过 ${bytes(maxUploadBytes.value)}`); return }
  submitting.value = true
  try {
    task.value = restoreName.value
      ? await restoreBackup(restoreName.value, vaultPass.value)
      : await uploadRestore(selectedFile.value!, vaultPass.value, p => { uploadPercent.value = p })
    showRestore.value = false
    message.success('归档已提交校验，校验通过后可重启应用恢复')
  } catch (e: any) { confirmed.value = false; message.error(e.message || '提交恢复失败') }
  finally { submitting.value = false }
}
const columns = computed<DataTableColumns<BackupMeta>>(() => [
  { title: '归档', key: 'name', minWidth: 240, ellipsis: { tooltip: true }, render: r => r.note ? `${r.name} · ${r.note}` : r.name },
  { title: '类型', key: 'kind', width: 100, render: r => h(NTag, { size: 'small', bordered: false }, { default: () => r.kind === 'scheduled' ? '定时' : r.auto ? '安全副本' : '手动' }) },
  { title: '大小', key: 'size', width: 100, render: r => bytes(r.size) },
  { title: 'S3 副本', key: 'remote_name', width: 150, ellipsis: { tooltip: true }, render: r => r.remote_error || r.remote_name || '仅本地' },
  { title: '创建时间', key: 'created_at', width: 170, render: r => fmtDateTime(r.created_at) },
  { title: '操作', key: 'actions', width: 210, fixed: 'right', render: r => h(NSpace, { size: 4 }, { default: () => [
    h(NButton, { size: 'tiny', secondary: true, onClick: () => window.open(backupDownloadUrl(r.name), '_blank') }, { default: () => '下载' }),
    h(NButton, { size: 'tiny', secondary: true, type: 'warning', disabled: busy.value, onClick: () => openRestore(r) }, { default: () => '恢复' }),
    h(NPopconfirm, { onPositiveClick: () => remove(r) }, {
      trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error', disabled: busy.value }, { default: () => '删除' }),
      default: () => '删除本地归档及已记录的 S3 副本？此操作不可撤销。',
    }),
  ] }) },
])
</script>

<template>
  <NEmpty v-if="!isAdmin" description="备份与恢复仅管理员可用" />
  <NSpace v-else vertical :size="16">
    <NAlert type="warning" :bordered="false">
      备份包含控制端配置、凭据和解密密钥，请妥善保存。恢复会采用归档中的账号、面板域名与安全入口；被管主机上的应用数据需单独备份。
    </NAlert>
    <NAlert v-if="loadError" type="error" :bordered="false">{{ loadError }}</NAlert>
    <NCard v-if="task?.id || pending" size="small" :title="taskTitle">
      <NAlert v-if="task?.error" type="error" :bordered="false">{{ task.error }}</NAlert>
      <p v-if="task?.status === 'running'" class="task-detail">{{ phaseLabels[task.phase] || task.phase }} · 已处理 {{ task.files }} 个文件 / {{ bytes(task.bytes) }}</p>
      <p v-if="task?.result?.safety_copy" class="task-detail">恢复前安全副本：{{ task.result.safety_copy }}</p>
      <NAlert v-if="pending" type="warning" :bordered="false">
        归档已校验并暂存，尚未修改当前数据。重启时会在服务启动前应用恢复，失败自动回滚。恢复后可能需要通过原面板域名和安全入口重新登录。
      </NAlert>
      <NSpace v-if="pending" class="pending-actions">
        <NPopconfirm @positive-click="applyRestore">
          <template #trigger><NButton type="warning" :loading="restarting">重启并应用恢复</NButton></template>
          确认重启控制端并应用这份归档？重启期间面板和 Agent 连接会短暂中断。
        </NPopconfirm>
        <NPopconfirm @positive-click="cancelRestore">
          <template #trigger><NButton :disabled="restarting">取消待恢复</NButton></template>
          取消已暂存的恢复任务？当前业务数据不会改变。
        </NPopconfirm>
      </NSpace>
    </NCard>

    <NCard title="自动备份" size="small">
      <NForm label-placement="top">
        <div class="policy-grid">
          <NFormItem label="启用每日备份"><NSwitch v-model:value="policy.enabled" /></NFormItem>
          <NFormItem label="每日时间（控制端时区）"><NInput v-model:value="policy.time" placeholder="03:00" /></NFormItem>
          <NFormItem label="自动备份保留份数"><NInputNumber v-model:value="policy.retention" :min="1" :max="100" /></NFormItem>
          <NFormItem label="存储位置"><NSelect v-model:value="policy.target_id" :options="targetOptions" /></NFormItem>
        </div>
      </NForm>
      <NSpace justify="space-between" align="center">
        <span class="field-hint tip-hint">保留策略仅清理定时备份，手动归档和恢复前安全副本保留。S3 目标在快照备份中配置。</span>
        <NButton type="primary" :loading="savingPolicy" @click="savePolicy">保存策略</NButton>
      </NSpace>
      <p v-if="policy.last_run" class="field-hint">最近调度：{{ fmtDateTime(policy.last_run) }}</p>
      <NAlert v-if="policy.last_error" type="error" :bordered="false">{{ policy.last_error }}</NAlert>
    </NCard>

    <NCard title="备份归档" size="small">
      <template #header-extra><NSpace><NButton size="small" :loading="loading" @click="refresh">刷新</NButton><NButton size="small" :disabled="busy" @click="openRestore()">上传归档恢复</NButton></NSpace></template>
      <div class="backup-toolbar">
        <NInput v-model:value="note" :maxlength="200" clearable placeholder="备份备注（可选）" />
        <NSelect v-model:value="targetID" :options="targetOptions" />
        <NButton type="primary" :disabled="busy" @click="create">立即备份</NButton>
      </div>
      <!-- 设置分区中的辅助归档表：固定限高，并对持续累积的记录分页。 -->
      <NDataTable :columns="columns" :data="entries" :pagination="pagination" :loading="loading" :bordered="false" :max-height="360" :scroll-x="980" :row-key="(r: BackupMeta) => r.name" size="small" />
    </NCard>
  </NSpace>

  <NModal v-model:show="showRestore" preset="card" title="恢复控制端备份" style="width: 600px; max-width: calc(100vw - 24px); max-height: calc(100vh - 40px); overflow-y: auto" :closable="!submitting" :mask-closable="!submitting" :close-on-esc="!submitting">
    <NForm label-placement="top" :disabled="confirmed || submitting">
      <NFormItem v-if="restoreName" label="归档文件"><span class="archive-name">{{ restoreName }}</span></NFormItem>
      <NFormItem v-else label="备份归档" required>
        <NUpload v-model:file-list="fileList" :disabled="confirmed || submitting" :max="1" :default-upload="false" accept=".gz,.tgz,application/gzip"><NButton :disabled="confirmed || submitting">选择 .tar.gz 归档</NButton></NUpload>
        <template #feedback><span class="field-hint tip-hint">上传上限 {{ bytes(maxUploadBytes) }}；恢复数据上限 64 GiB。先完整校验，重启后再应用。</span></template>
      </NFormItem>
      <NFormItem label="原服务器 Vault 密钥（仅旧归档需要）">
        <NInput v-model:value="vaultPass" type="password" show-password-on="click" :maxlength="4096" placeholder="新归档已包含密钥，通常留空" />
        <template #feedback><span class="field-hint tip-hint">旧归档跨机器恢复时，填写原 watchman.env 中的 WATCHMAN_VAULT_PASS；密钥不匹配会拒绝恢复。</span></template>
      </NFormItem>
    </NForm>
    <NProgress v-if="submitting && selectedFile" type="line" :percentage="uploadPercent" />
    <template #footer>
      <div class="restore-footer">
        <NAlert v-if="confirmed" type="warning" :bordered="false">确认使用这份备份覆盖控制端数据。校验通过后需重启应用恢复，账号、域名与安全入口将采用归档中的配置；失败会自动回滚。</NAlert>
        <NSpace justify="end">
          <NButton :disabled="submitting" @click="confirmed ? confirmed = false : showRestore = false">{{ confirmed ? '返回核对' : '取消' }}</NButton>
          <NButton v-if="!confirmed" type="warning" :disabled="!canSubmitRestore" @click="confirmed = true">核对并继续</NButton>
          <NButton v-else type="error" :loading="submitting" :disabled="!canSubmitRestore" @click="submitRestore">确认提交恢复</NButton>
        </NSpace>
      </div>
    </template>
  </NModal>
</template>

<style scoped lang="scss">
.policy-grid { display: grid; grid-template-columns: 120px 1fr 1fr 2fr; gap: 16px; }
.backup-toolbar { display: flex; gap: 12px; margin-bottom: 16px; align-items: center; }
.backup-toolbar > .n-input, .backup-toolbar > .n-select { flex: 1; min-width: 0; }
.field-hint { color: var(--text-secondary); font-size: 12px; line-height: 1.6; }
.task-detail { margin: 0 0 12px; overflow-wrap: anywhere; }
.pending-actions { margin-top: 12px; }
.archive-name { overflow-wrap: anywhere; }
.restore-footer { display: flex; flex-direction: column; gap: 12px; }
@media (max-width: 900px) {
  .policy-grid { grid-template-columns: 1fr 1fr; }
  .backup-toolbar { flex-wrap: wrap; }
  .backup-toolbar > .n-input, .backup-toolbar > .n-select { flex-basis: 100%; }
}
</style>
