<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard,
  NSpace,
  NButton,
  NDataTable,
  NTabs,
  NTabPane,
  NTag,
  NPopconfirm,
  NInput,
  NIcon,
  NModal,
  NAlert,
  useMessage,
} from 'naive-ui'
import {
  RefreshOutline,
  TrashOutline,
  DownloadOutline,
  TerminalOutline,
  ConstructOutline,
} from '@vicons/ionicons5'
import { dockerAll, dockerOp, installDocker } from '../../api/hosts'
import TerminalPane from '../../components/host/TerminalPane.vue'
import { useAuthStore } from '../../stores/auth'
import { useTablePagination } from '../../composables/useTablePagination'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'Docker' })

const route = useRoute()
const message = useMessage()
const hostId = route.params.id as string
const auth = useAuthStore()

// Installing Docker changes host state, matching the server-side role gate on
// POST /hosts/:id/docker/install-script.
const canInstall = computed(() => auth.role === 'admin' || auth.role === 'operator')

const loading = ref(false)
const tab = ref('containers')
const containers = ref<any[]>([])
const images = ref<any[]>([])
const logsContainer = ref('')
const logsText = ref('')
const pullImageName = ref('')
const dockerNotInstalled = ref(false)

// 容器与镜像列表都可能很长（AGENTS.md 8.2），各自独立分页。
const containerCount = computed(() => containers.value.length)
const imageCount = computed(() => images.value.length)
const { pagination: containerPagination } = useTablePagination({ pageSize: 20, rowCount: containerCount })
const { pagination: imagePagination } = useTablePagination({ pageSize: 20, rowCount: imageCount })

// Container Terminal Modal state
const showContainerTerm = ref(false)
const targetContainer = ref('')

// Docker Installer state
const showInstallModal = ref(false)
const installing = ref(false)
const installOutput = ref('')

// loadAll fetches containers + images in a single round trip via the
// /docker/all endpoint, so the dashboard doesn't pay two sequential calls.
async function loadAll() {
  loading.value = true
  try {
    const res = await dockerAll(hostId)
    containers.value = res.containers || []
    images.value = res.images || []
    dockerNotInstalled.value = false
  } catch (e: any) {
    if (
      e.message?.includes('not found') ||
      e.message?.includes('executable file not found') ||
      e.message?.includes('Docker 未安装') ||
      e.message?.includes('not in PATH')
    ) {
      dockerNotInstalled.value = true
      containers.value = []
      images.value = []
    } else {
      message.error(e.message || '加载 Docker 数据失败')
    }
  } finally {
    loading.value = false
  }
}

async function loadContainers() {
  await loadAll()
}

async function loadImages() {
  // Images are already populated by loadAll; only re-fetch if missing.
  if (images.value.length === 0) {
    await loadAll()
  }
}

async function doOp(op: string, container: string) {
  // Optimistic update: tweak the local container state immediately so the UI
  // reacts without waiting for a full re-fetch. For start/stop/restart we
  // rewrite the Status field; for rm we drop the row. A background re-fetch
  // reconciles any drift.
  const name = container.replace(/^\//, '')
  const prev = containers.value.map((c) => ({ ...c }))
  try {
    if (op === 'rm') {
      containers.value = containers.value.filter(
        (c) => (c.Names || c.ID || '').replace(/^\//, '') !== name,
      )
    } else if (op === 'start' || op === 'stop' || op === 'restart') {
      containers.value = containers.value.map((c) => {
        const cn = (c.Names || c.ID || '').replace(/^\//, '')
        if (cn === name) {
          return {
            ...c,
            Status: op === 'stop' ? 'Exited' : `Up (optimistic)`,
            State: op === 'stop' ? 'exited' : 'running',
          }
        }
        return c
      })
    }
    await dockerOp(hostId, op, container)
    message.success(`${op} 成功`)
    // Reconcile with a background refresh (no await, no loading spinner).
    loadAll()
  } catch (e: any) {
    // Roll back on failure.
    containers.value = prev
    message.error(e.message)
  }
}

function openContainerTerminal(container: string) {
  targetContainer.value = container.replace(/^\//, '')
  showContainerTerm.value = true
}

async function viewLogs(container: string) {
  logsContainer.value = container
  tab.value = 'logs'
  try {
    const res = await dockerOp(hostId, 'logs', container)
    logsText.value = res?.raw || res?.data || JSON.stringify(res, null, 2) || ''
  } catch (e: any) {
    logsText.value = e.message
  }
}

async function removeImage(imageId: string) {
  try {
    await dockerOp(hostId, 'remove_image', '', imageId)
    message.success('镜像已删除')
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function pruneImages() {
  try {
    await dockerOp(hostId, 'prune_images')
    message.success('已清除未使用镜像')
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function pullImage() {
  if (!pullImageName.value) {
    message.warning('请输入镜像名称')
    return
  }
  try {
    await dockerOp(hostId, 'pull', '', pullImageName.value)
    message.success(`镜像 ${pullImageName.value} 拉取成功`)
    pullImageName.value = ''
    await loadImages()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function startInstallDocker() {
  installing.value = true
  installOutput.value = '>>> 正在下发服务端内置的 Docker 一键安装脚本 (curl -fsSL https://get.docker.com | sh)...\n'
  try {
    const res = await installDocker(hostId)
    if (res.ok) {
      installOutput.value += '\n>>> 安装执行完成！\n' + (res.stdout || '')
      message.success('Docker 安装完成，正在重新加载...')
      dockerNotInstalled.value = false
      setTimeout(() => {
        loadAll()
      }, 2000)
    } else {
      installOutput.value += '\n>>> 执行异常 (Exit code ' + res.exit_code + '):\n' + (res.stderr || res.stdout || '')
      message.error('Docker 安装未成功，请检查输出日志')
    }
  } catch (e: any) {
    installOutput.value += '\n>>> 安装请求失败: ' + (e.message || e)
    message.error('安装请求失败: ' + e.message)
  } finally {
    installing.value = false
  }
}

const containerColumns = [
  { title: '名称', key: 'Names', width: 140, ellipsis: { tooltip: true } },
  { title: '镜像', key: 'Image', ellipsis: { tooltip: true } },
  {
    title: '状态',
    key: 'Status',
    width: 120,
    render: (row: any) => {
      const running = row.Status?.includes('Up') || row.State === 'running'
      return h(
        NTag,
        { type: running ? 'success' : 'default', size: 'small', round: true },
        { default: () => row.Status || row.State || '-' },
      )
    },
  },
  { title: '端口', key: 'Ports', ellipsis: { tooltip: true } },
  {
    title: '操作',
    key: 'actions',
    width: 280,
    render: (row: any) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(
            NButton,
            {
              size: 'tiny',
              type: 'primary',
              quaternary: true,
              onClick: () => openContainerTerminal(row.Names || row.ID),
            },
            {
              icon: () => h(NIcon, { component: TerminalOutline }),
              default: () => '终端',
            },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('start', row.Names) },
            { default: () => '启动' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('stop', row.Names) },
            { default: () => '停止' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => doOp('restart', row.Names) },
            { default: () => '重启' },
          ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => viewLogs(row.Names) },
            { default: () => '日志' },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => doOp('rm', row.Names) },
            {
              trigger: () =>
                h(NButton, { size: 'tiny', quaternary: true, type: 'error' }, { default: () => '删除' }),
              default: () => `确认删除容器 ${row.Names}?`,
            },
          ),
        ],
      }),
  },
]

const imageColumns = [
  { title: '仓库', key: 'Repository', ellipsis: { tooltip: true } },
  { title: '标签', key: 'Tag', width: 100 },
  { title: 'ID', key: 'ID', width: 80, ellipsis: { tooltip: true } },
  { title: '大小', key: 'Size', width: 80 },
  {
    title: '操作',
    key: 'actions',
    width: 100,
    render: (row: any) =>
      h(
        NPopconfirm,
        { onPositiveClick: () => removeImage(row.ID || row.Id) },
        {
          trigger: () =>
            h(
              NButton,
              { size: 'tiny', quaternary: true, type: 'error' },
              {
                icon: () => h(NIcon, { component: TrashOutline }),
                default: () => '删除',
              },
            ),
          default: () => `确认删除镜像 ${row.Repository}:${row.Tag}?`,
        },
      ),
  },
]

onMounted(loadContainers)
</script>

<template>
  <div class="docker-view page-flex-column">
    <div class="docker-toolbar">
      <h2 class="page-title">Docker 管理</h2>
    </div>

    <!-- 未安装 Docker 时的引导卡片 -->
    <NAlert
      v-if="dockerNotInstalled"
      type="warning"
      title="未检测到 Docker 环境"
      closable
      class="docker-alert"
    >
      <div style="display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 4px">
        <span v-if="canInstall">当前主机可能尚未安装 Docker 引擎或守护进程未启动。点击右侧按钮可通过自动化脚本一键安装配置官方 Docker 环境。</span>
        <span v-else>当前主机可能尚未安装 Docker 引擎或守护进程未启动。安装需要管理员或运维角色，请联系管理员处理。</span>
        <NButton
          v-if="canInstall"
          type="warning"
          size="small"
          @click="showInstallModal = true; startInstallDocker()"
        >
          <template #icon><NIcon :component="ConstructOutline" /></template>
          一键安装 Docker
        </NButton>
      </div>
    </NAlert>

    <NCard :bordered="false" class="docker-card">
      <NTabs
        v-model:value="tab"
        type="line"
        class="docker-tabs"
        @update:value="(v: string) => { if (v === 'images' && images.length === 0) loadImages() }"
      >
        <NTabPane name="containers" tab="容器" display-directive="show:lazy">
          <div class="tab-body">
            <NSpace align="center" justify="space-between" class="tab-toolbar">
              <NButton size="small" :loading="loading" @click="loadContainers">
                <template #icon><NIcon :component="RefreshOutline" /></template>
                刷新
              </NButton>
              <NButton
                v-if="dockerNotInstalled && canInstall"
                size="small"
                type="warning"
                @click="showInstallModal = true; startInstallDocker()"
              >
                一键安装 Docker
              </NButton>
            </NSpace>
            <NDataTable
              flex-height
              :columns="containerColumns"
              :data="containers"
              :pagination="containerPagination"
              :bordered="false"
              size="small"
              :loading="loading"
              :scroll-x="700"
            >
              <template #empty>无容器或 Docker 未运行</template>
            </NDataTable>
          </div>
        </NTabPane>

        <NTabPane name="images" tab="镜像" display-directive="show:lazy">
          <div class="tab-body">
            <NSpace align="center" class="tab-toolbar">
              <NButton size="small" :loading="loading" @click="loadImages">
                <template #icon><NIcon :component="RefreshOutline" /></template>
                刷新
              </NButton>
              <NInput
                v-model:value="pullImageName"
                placeholder="nginx:latest"
                size="small"
                style="width: 200px"
              />
              <NButton size="small" type="primary" @click="pullImage">
                <template #icon><NIcon :component="DownloadOutline" /></template>
                拉取
              </NButton>
              <NPopconfirm @positive-click="pruneImages">
                <template #trigger>
                  <NButton size="small" type="warning">清除未使用</NButton>
                </template>
                确认清除所有未使用镜像？
              </NPopconfirm>
            </NSpace>
            <NDataTable
              flex-height
              :columns="imageColumns"
              :data="images"
              :pagination="imagePagination"
              :bordered="false"
              size="small"
              :loading="loading"
            >
              <template #empty>无镜像或 Docker 未运行</template>
            </NDataTable>
          </div>
        </NTabPane>

        <NTabPane name="logs" tab="容器日志" display-directive="show:lazy">
          <div class="tab-body">
            <span class="muted">容器: {{ logsContainer || '请从容器列表点击"日志"' }}</span>
            <pre class="log-output log-output-fill">{{ logsText || '暂无日志' }}</pre>
          </div>
        </NTabPane>
      </NTabs>
    </NCard>

    <!-- 容器终端直连弹窗 -->
    <NModal
      v-model:show="showContainerTerm"
      preset="card"
      :title="`容器终端直连: ${targetContainer}`"
      style="width: 80vw; max-width: 1000px; height: 600px"
    >
      <div style="height: 500px">
        <TerminalPane
          v-if="showContainerTerm"
          :host-id="hostId"
          :shell="`docker exec -it ${targetContainer} /bin/sh`"
        />
      </div>
    </NModal>

    <!-- Docker 一键安装实时日志弹窗 -->
    <NModal
      v-model:show="showInstallModal"
      preset="card"
      title="一键安装 Docker"
      style="width: 650px"
    >
      <NSpace vertical size="medium">
        <NAlert v-if="installing" type="info">
          正在远程执行安装脚本，由于依赖网络下载，可能需要 1~3 分钟，请耐心等待...
        </NAlert>
        <pre class="log-output install-log">{{ installOutput }}</pre>
        <NSpace justify="end">
          <NButton :disabled="installing" @click="showInstallModal = false">
            关闭
          </NButton>
        </NSpace>
      </NSpace>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.docker-view {
  gap: 12px;
}

.docker-toolbar {
  flex-shrink: 0;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 700;
  color: var(--text-primary);
}

.docker-alert {
  flex-shrink: 0;
}

/* 卡片吃掉剩余高度，tabs 内容区再向下传递，使表格能按视口计算滚动区 */
.docker-card {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;

  :deep(.n-card-content) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
}

.docker-tabs {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;

  :deep(.n-tabs-pane-wrapper),
  :deep(.n-tab-pane) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
}

.tab-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;

  .tab-toolbar {
    flex-shrink: 0;
  }

  :deep(.n-data-table) {
    flex: 1;
    min-height: 0;
  }
}

/* 日志页签：填满剩余高度而不是固定 400px */
.log-output-fill {
  flex: 1;
  min-height: 0;
  max-height: none;
  margin: 0;
}

.muted {
  color: var(--text-secondary);
  font-size: 12px;
}
.log-output {
  background: var(--code-box-bg);
  color: var(--text-primary);
  border: 1px solid var(--border-color);
  padding: 12px;
  border-radius: 4px;
  font-size: 13px;
  white-space: pre-wrap;
  max-height: 400px;
  overflow: auto;

  &.install-log {
    max-height: 320px;
    background: var(--cmd-box-bg);
    color: var(--cmd-text-color);
    border: 1px solid var(--border-color);
  }
}
</style>
