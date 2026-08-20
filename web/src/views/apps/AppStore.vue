<script setup lang="ts">
import { ref, onMounted, computed, h } from 'vue'
import { useRoute } from 'vue-router'
import {
  NCard,
  NGrid,
  NGridItem,
  NSpace,
  NButton,
  NTag,
  NTabs,
  NTabPane,
  NIcon,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NDataTable,
  NPopconfirm,
  NModal,
  useMessage,
} from 'naive-ui'
import {
  CloudDownloadOutline,
  RefreshOutline,
  TrashOutline,
  PlayOutline,
  StopOutline,
  DocumentTextOutline,
  GlobeOutline,
  LayersOutline,
  ServerOutline,
  FileTrayStackedOutline,
  ShieldCheckmarkOutline,
} from '@vicons/ionicons5'
import {
  getAppCatalog,
  getInstalledApps,
  installApp,
  uninstallApp,
  dockerOp,
  type AppTemplate,
  type AppInstance,
} from '../../api/hosts'

// Map icon identifier strings from the server catalog to actual icon components.
const iconMap: Record<string, any> = {
  GlobeOutline,
  LayersOutline,
  ServerOutline,
  FileTrayStackedOutline,
  ShieldCheckmarkOutline,
}

function renderAppIcon(iconName: string) {
  const comp = iconMap[iconName] || GlobeOutline
  return h(NIcon, { component: comp, size: 22 })
}

const props = defineProps<{ hostId?: string }>()
const route = useRoute()
const message = useMessage()

const currentHostId = computed(() => props.hostId || (route.params.id as string) || '')

const activeTab = ref<'catalog' | 'installed'>('catalog')
const catalog = ref<AppTemplate[]>([])
const installedApps = ref<AppInstance[]>([])
const loadingCatalog = ref(false)
const loadingInstalled = ref(false)

// Drawer install form
const showInstallDrawer = ref(false)
const selectedApp = ref<AppTemplate | null>(null)
const installing = ref(false)
const formInstanceName = ref('')
const formPort = ref<number | null>(null)
const formVolume = ref('')
const formEnv = ref<Record<string, string>>({})

// Logs modal
const showLogsModal = ref(false)
const currentLogs = ref('')
const logsTitle = ref('')

async function loadCatalog() {
  loadingCatalog.value = true
  try {
    catalog.value = await getAppCatalog()
  } catch (e: any) {
    message.error(e.message || '获取应用目录失败')
  } finally {
    loadingCatalog.value = false
  }
}

async function loadInstalled() {
  if (!currentHostId.value) return
  loadingInstalled.value = true
  try {
    installedApps.value = await getInstalledApps(currentHostId.value)
  } catch (e: any) {
    message.error(e.message || '获取已安装应用失败')
  } finally {
    loadingInstalled.value = false
  }
}

function openInstallDrawer(app: AppTemplate) {
  selectedApp.value = app
  formInstanceName.value = `app-${app.id}`
  formPort.value = app.default_port
  formVolume.value = app.default_volume
  const env: Record<string, string> = {}
  if (app.env_fields) {
    for (const f of app.env_fields) {
      env[f.key] = f.default || ''
    }
  }
  formEnv.value = env
  showInstallDrawer.value = true
}

async function handleInstall() {
  if (!selectedApp.value || !currentHostId.value) return
  installing.value = true
  try {
    await installApp(currentHostId.value, {
      app_id: selectedApp.value.id,
      name: formInstanceName.value.trim() || `app-${selectedApp.value.id}`,
      port: formPort.value || selectedApp.value.default_port,
      volume: formVolume.value.trim() || selectedApp.value.default_volume,
      env: formEnv.value,
    })
    message.success(`应用 ${selectedApp.value.name} 部署指令已下发！`)
    showInstallDrawer.value = false
    activeTab.value = 'installed'
    setTimeout(() => {
      loadInstalled()
    }, 1500)
  } catch (e: any) {
    message.error(e.message || '安装应用失败')
  } finally {
    installing.value = false
  }
}

async function handleUninstall(appName: string) {
  if (!currentHostId.value) return
  try {
    await uninstallApp(currentHostId.value, appName)
    message.success(`应用 ${appName} 卸载成功`)
    await loadInstalled()
  } catch (e: any) {
    message.error(e.message || '卸载应用失败')
  }
}

async function handleContainerOp(op: string, container: string) {
  if (!currentHostId.value) return
  try {
    await dockerOp(currentHostId.value, op, container)
    message.success(`${op} 操作成功`)
    await loadInstalled()
  } catch (e: any) {
    message.error(e.message)
  }
}

async function viewLogs(container: string) {
  if (!currentHostId.value) return
  logsTitle.value = `应用日志: ${container}`
  showLogsModal.value = true
  currentLogs.value = '正在加载日志...'
  try {
    const res = await dockerOp(currentHostId.value, 'logs', container)
    currentLogs.value = res?.raw || res?.data || JSON.stringify(res, null, 2) || '暂无日志输出'
  } catch (e: any) {
    currentLogs.value = e.message
  }
}

const installedColumns = [
  { title: '应用名称', key: 'name', width: 160 },
  { title: '模板类型', key: 'app_id', width: 120 },
  { title: '镜像', key: 'image', ellipsis: { tooltip: true } },
  {
    title: '运行状态',
    key: 'running',
    width: 110,
    render: (row: AppInstance) =>
      h(
        NTag,
        { type: row.running ? 'success' : 'default', size: 'small', round: true },
        { default: () => (row.running ? '运行中' : '已停止') },
      ),
  },
  { title: '端口映射', key: 'ports', ellipsis: { tooltip: true } },
  {
    title: '操作',
    key: 'actions',
    width: 240,
    render: (row: AppInstance) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          row.running
            ? h(
                NButton,
                { size: 'tiny', quaternary: true, onClick: () => handleContainerOp('stop', row.name) },
                {
                  icon: () => h(NIcon, { component: StopOutline }),
                  default: () => '停止',
                },
              )
            : h(
                NButton,
                { size: 'tiny', quaternary: true, onClick: () => handleContainerOp('start', row.name) },
                {
                  icon: () => h(NIcon, { component: PlayOutline }),
                  default: () => '启动',
                },
              ),
          h(
            NButton,
            { size: 'tiny', quaternary: true, onClick: () => viewLogs(row.name) },
            {
              icon: () => h(NIcon, { component: DocumentTextOutline }),
              default: () => '日志',
            },
          ),
          h(
            NPopconfirm,
            { onPositiveClick: () => handleUninstall(row.name) },
            {
              trigger: () =>
                h(
                  NButton,
                  { size: 'tiny', quaternary: true, type: 'error' },
                  {
                    icon: () => h(NIcon, { component: TrashOutline }),
                    default: () => '卸载',
                  },
                ),
              default: () => `确认卸载并删除应用实例 ${row.name}？`,
            },
          ),
        ],
      }),
  },
]

onMounted(() => {
  loadCatalog()
  loadInstalled()
})
</script>

<template>
  <div class="app-store-container">
    <NCard :bordered="false" class="app-store-card">
      <NTabs v-model:value="activeTab" type="line" @update:value="(v: string) => { if (v === 'installed') loadInstalled() }">
        <!-- Tab 1: 应用市场模板库 -->
        <NTabPane name="catalog" tab="应用模板库">
          <div class="catalog-toolbar">
            <span class="sub-tip">精选开箱即用的优质服务模板，支持参数化一键容器化部署。</span>
            <NButton size="small" :loading="loadingCatalog" @click="loadCatalog">
              <template #icon><NIcon :component="RefreshOutline" /></template>
              刷新
            </NButton>
          </div>

          <NGrid :cols="3" :x-gap="14" :y-gap="14" responsive="screen">
            <NGridItem v-for="app in catalog" :key="app.id">
              <div class="app-card">
                <div class="app-card-header">
                  <div class="app-icon-wrap">
                    <component :is="renderAppIcon(app.icon)" />
                  </div>
                  <div class="app-title-area">
                    <div class="title-row">
                      <span class="app-name">{{ app.name }}</span>
                      <NTag size="tiny" type="info" round>{{ app.version }}</NTag>
                    </div>
                    <span class="app-category">{{ app.category }}</span>
                  </div>
                </div>

                <div class="app-desc">
                  {{ app.description }}
                </div>

                <div class="app-card-footer">
                  <span class="port-hint">默认端口: {{ app.default_port }}</span>
                  <NButton
                    type="primary"
                    size="small"
                    secondary
                    @click="openInstallDrawer(app)"
                  >
                    <template #icon><NIcon :component="CloudDownloadOutline" /></template>
                    一键安装
                  </NButton>
                </div>
              </div>
            </NGridItem>
          </NGrid>
        </NTabPane>

        <!-- Tab 2: 已部署应用实例 -->
        <NTabPane name="installed" tab="已部署实例">
          <NSpace vertical :size="10">
            <NSpace justify="space-between" align="center">
              <span class="sub-tip">当前主机通过应用市场或标准 Label 管理的容器应用。</span>
              <NButton size="small" :loading="loadingInstalled" @click="loadInstalled">
                <template #icon><NIcon :component="RefreshOutline" /></template>
                刷新
              </NButton>
            </NSpace>

            <NDataTable
              :columns="installedColumns"
              :data="installedApps"
              :loading="loadingInstalled"
              size="small"
              :bordered="false"
            >
              <template #empty>暂未安装任何应用</template>
            </NDataTable>
          </NSpace>
        </NTabPane>
      </NTabs>
    </NCard>

    <!-- 应用部署配置抽屉 -->
    <NDrawer v-model:show="showInstallDrawer" :width="440" placement="right">
      <NDrawerContent v-if="selectedApp" :title="`部署 ${selectedApp.name}`" closable>
        <NForm label-placement="top" size="small">
          <NFormItem label="实例名称" required>
            <NInput v-model:value="formInstanceName" placeholder="例如: my-redis" />
          </NFormItem>

          <NFormItem label="宿主机对外端口" required>
            <NInputNumber
              v-model:value="formPort"
              :min="1"
              :max="65535"
              style="width: 100%"
            />
          </NFormItem>

          <NFormItem label="数据持久化挂载目录">
            <NInput v-model:value="formVolume" placeholder="例如: /data/redis" />
          </NFormItem>

          <!-- 动态环境变量表单 -->
          <template v-if="selectedApp.env_fields && selectedApp.env_fields.length > 0">
            <div class="env-section-title">环境变量配置</div>
            <NFormItem
              v-for="f in selectedApp.env_fields"
              :key="f.key"
              :label="f.label"
              :required="f.required"
            >
              <NInput
                v-if="f.type === 'password'"
                v-model:value="formEnv[f.key]"
                type="password"
                show-password-on="click"
                :placeholder="f.description || f.key"
              />
              <NInput
                v-else
                v-model:value="formEnv[f.key]"
                :placeholder="f.description || f.key"
              />
            </NFormItem>
          </template>
        </NForm>

        <template #footer>
          <NSpace justify="end">
            <NButton @click="showInstallDrawer = false">取消</NButton>
            <NButton type="primary" :loading="installing" @click="handleInstall">
              立即部署
            </NButton>
          </NSpace>
        </template>
      </NDrawerContent>
    </NDrawer>

    <!-- 日志弹窗 -->
    <NModal
      v-model:show="showLogsModal"
      preset="card"
      :title="logsTitle"
      style="width: 700px"
    >
      <pre class="app-logs-pre">{{ currentLogs }}</pre>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.app-store-container {
  .app-store-card {
    background-color: transparent;
  }

  .catalog-toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 14px;

    .sub-tip {
      font-size: 13px;
      color: #9ca3af;
    }
  }

  .app-card {
    background-color: var(--bg-card-subtle);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    transition: all 0.2s ease;
    box-shadow: var(--shadow-sm);

    &:hover {
      border-color: rgba(99, 102, 241, 0.5);
      transform: translateY(-2px);
      box-shadow: var(--shadow-md);
    }

    .app-card-header {
      display: flex;
      align-items: center;
      gap: 12px;

      .app-icon-wrap {
        width: 44px;
        height: 44px;
        border-radius: 8px;
        background-color: rgba(99, 102, 241, 0.12);
        color: #6366f1;
        display: flex;
        align-items: center;
        justify-content: center;
        flex-shrink: 0;
      }

      .app-title-area {
        flex: 1;
        display: flex;
        flex-direction: column;

        .title-row {
          display: flex;
          align-items: center;
          gap: 6px;

          .app-name {
            font-weight: 700;
            font-size: 15px;
            color: var(--text-primary);
          }
        }

        .app-category {
          font-size: 12px;
          color: var(--text-secondary);
        }
      }
    }

    .app-desc {
      font-size: 13px;
      color: var(--text-secondary);
      line-height: 1.5;
      min-height: 38px;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
    }

    .app-card-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-top: 1px solid var(--border-color);
      padding-top: 10px;

      .port-hint {
        font-size: 12px;
        color: var(--text-muted);
      }
    }
  }

  .env-section-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 12px 0 8px;
    padding-bottom: 4px;
    border-bottom: 1px dashed var(--border-dashed);
  }

  .app-logs-pre {
    background: var(--code-box-bg);
    color: var(--text-primary);
    border: 1px solid var(--border-color);
    padding: 12px;
    border-radius: 4px;
    font-size: 13px;
    white-space: pre-wrap;
    max-height: 450px;
    overflow: auto;
  }
}
</style>
