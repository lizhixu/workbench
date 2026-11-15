<script setup lang="ts">
import { ref, computed, h, onMounted } from 'vue'
import {
  NCard,
  NButton,
  NDataTable,
  NModal,
  NAlert,
  NSpin,
  NTag,
  NSpace,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import {
  systemCleanupPreview,
  systemCleanup,
  type CleanupCategory,
  type CleanupPreview,
  type CleanupStep,
} from '../../../api/hosts'
import { useAuthStore } from '../../../stores/auth'

const props = defineProps<{ hostId: string }>()

const message = useMessage()
const auth = useAuthStore()

// 清理改主机状态，与服务端 hostWrite 路由的角色门禁一致。
const canCleanup = computed(() => auth.role === 'admin' || auth.role === 'operator')

const loading = ref(false)
const preview = ref<CleanupPreview | null>(null)
const loadError = ref('')
const checkedKeys = ref<string[]>([])
const running = ref(false)
const showConfirm = ref(false)
const results = ref<CleanupStep[] | null>(null)

function formatBytes(bytes?: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let b = bytes
  while (b >= 1024 && i < units.length - 1) {
    b /= 1024
    i++
  }
  return `${b.toFixed(1)} ${units[i]}`
}

const titleByName = computed(() => {
  const m = new Map<string, string>()
  for (const c of preview.value?.categories || []) m.set(c.name, c.title)
  return m
})

const selectedCategories = computed(() =>
  (preview.value?.categories || []).filter((c) => checkedKeys.value.includes(c.name)),
)

const totalReclaimable = computed(() =>
  selectedCategories.value.reduce((s, c) => s + (c.reclaimable || 0), 0),
)

const totalReclaimed = computed(() =>
  (results.value || []).reduce((s, r) => s + (r.reclaimed || 0), 0),
)

async function loadPreview() {
  loading.value = true
  loadError.value = ''
  try {
    const res = await systemCleanupPreview(props.hostId)
    preview.value = res
    // 默认全选可清理项：取数失败的行不可选。
    checkedKeys.value = (res.categories || []).filter((c) => !c.error).map((c) => c.name)
    results.value = null
  } catch (e: any) {
    loadError.value = e.message || '加载清理预览失败'
    preview.value = null
  } finally {
    loading.value = false
  }
}

const previewColumns: DataTableColumns<CleanupCategory> = [
  { type: 'selection', disabled: (row) => !!row.error },
  {
    title: '类别',
    key: 'title',
    render: (row: CleanupCategory) =>
      h('div', [
        h('div', row.title),
        h('div', { style: 'color: var(--text-color-3); font-size: 12px' }, row.desc),
      ]),
  },
  {
    title: '可回收',
    key: 'reclaimable',
    width: 120,
    render: (row: CleanupCategory) =>
      row.error
        ? h('span', { style: 'color: var(--text-color-3)' }, `取数失败：${row.error}`)
        : formatBytes(row.reclaimable),
  },
]

const resultColumns: DataTableColumns<CleanupStep> = [
  {
    title: '类别',
    key: 'name',
    render: (row) => titleByName.value.get(row.name) || row.name,
  },
  {
    title: '回收量',
    key: 'reclaimed',
    width: 120,
    render: (row) => formatBytes(row.reclaimed),
  },
  {
    title: '状态',
    key: 'ok',
    width: 100,
    render: (row) =>
      row.ok
        ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '成功' })
        : h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => '失败' }),
  },
  {
    title: '说明',
    key: 'error',
    render: (row) => row.error || '—',
  },
]

async function runCleanup() {
  running.value = true
  try {
    results.value = await systemCleanup(props.hostId, checkedKeys.value)
    const failed = results.value.filter((r) => !r.ok).length
    if (failed === 0) {
      message.success(`系统清理完成，共回收 ${formatBytes(totalReclaimed.value)}`)
    } else {
      message.warning(`系统清理完成，${failed} 类失败，共回收 ${formatBytes(totalReclaimed.value)}`)
    }
    await loadPreview()
  } catch (e: any) {
    message.error(e.message || '系统清理失败')
  } finally {
    running.value = false
    showConfirm.value = false
  }
}

onMounted(loadPreview)
</script>

<template>
  <div class="tab-pane-content">
    <NCard :bordered="false" class="table-flex-fill">
      <div class="tab-body">
        <div class="cleanup-toolbar">
          <h3 class="card-title">系统清理</h3>
          <NSpace>
            <NButton size="small" :loading="loading" @click="loadPreview">刷新</NButton>
            <NButton
              size="small"
              type="error"
              :disabled="!canCleanup || running || checkedKeys.length === 0 || !preview?.supported"
              :loading="running"
              @click="showConfirm = true"
            >
              开始清理
            </NButton>
          </NSpace>
        </div>

        <p class="muted">
          清理主机系统级垃圾：APT 缓存、journal 日志、临时文件与旧日志。Docker
          相关清理请走 Docker 页的「深度清理」。清理前先预览每类可回收量，勾选后确认执行。
        </p>
        <p v-if="!canCleanup" class="muted">当前角色仅可查看，执行清理需要管理员或运维角色。</p>

        <div v-if="loading && !preview" class="pane-loading">
          <NSpin size="medium" />
        </div>

        <NAlert v-else-if="loadError" type="error" :title="loadError" class="cleanup-alert">
          <NButton size="small" @click="loadPreview">重试</NButton>
        </NAlert>

        <NAlert
          v-else-if="preview && !preview.supported"
          type="warning"
          title="当前系统不支持系统清理"
          class="cleanup-alert"
        >
          该主机系统为 {{ preview.os || '未知' }}，系统清理 v1 仅支持 Debian/Ubuntu。
        </NAlert>

        <template v-else-if="preview">
          <NDataTable
            :columns="previewColumns"
            :data="preview.categories"
            :row-key="(row: CleanupCategory) => row.name"
            v-model:checked-row-keys="checkedKeys"
            :bordered="false"
            size="small"
            :loading="loading"
          />
          <p class="muted footnote">
            注：journal 日志的可回收量为估算下界（vacuum 同时受 7 天与 500MB 约束），实际清理量只多不少。
            已选 {{ checkedKeys.length }} 类，预计回收 {{ formatBytes(totalReclaimable) }}。
          </p>

          <template v-if="results">
            <h3 class="card-title result-title">清理结果</h3>
            <NDataTable
              :columns="resultColumns"
              :data="results"
              :row-key="(row: CleanupStep) => row.name"
              :bordered="false"
              size="small"
            />
            <p class="muted footnote">共回收 {{ formatBytes(totalReclaimed) }}。</p>
          </template>
        </template>
      </div>
    </NCard>

    <!-- 执行前确认：列出将清理的类别与预计回收量 -->
    <NModal v-model:show="showConfirm" preset="card" title="确认清理" style="width: 480px">
      <NSpace vertical>
        <span>将清理以下类别：</span>
        <ul class="confirm-list">
          <li v-for="c in selectedCategories" :key="c.name">
            {{ c.title }}（预计回收 {{ formatBytes(c.reclaimable) }}）
          </li>
        </ul>
        <span>预计共回收 {{ formatBytes(totalReclaimable) }}。清理会删除主机上的对应文件，执行后不可恢复。</span>
      </NSpace>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showConfirm = false">取消</NButton>
          <NButton type="error" :loading="running" @click="runCleanup">开始清理</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.cleanup-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 8px;
}

.cleanup-alert {
  margin-top: 12px;
}

.footnote {
  margin-top: 8px;
}

.result-title {
  margin-top: 16px;
}

.confirm-list {
  margin: 0;
  padding-left: 20px;
}
</style>
