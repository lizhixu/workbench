<script setup lang="ts">
import { defineAsyncComponent, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NSpin, NIcon, useMessage, NTooltip } from 'naive-ui'
import { getHost } from '../../api/hosts'
import type { Host } from '../../api/types'
import { useWorkspaceStore } from '../../stores/workspace'
import { useSettingsStore } from '../../stores/settings'
import { SETTING_KEYS } from '../../api/settings'
import HostHeaderBanner from '../../components/host/HostHeaderBanner.vue'
import { subNavItems, validHostTabs } from './hostTabs'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'HostDetail' })

// 七个面板按需加载：静态导入会把 echarts、xterm、Docker、漏洞管理全部打进
// 本路由的同一个 chunk（约 1MB），只想看进程列表也得先下完整包。
const FileManagerPane = defineAsyncComponent(() => import('../../components/host/FileManagerPane.vue'))
const MetricsPane = defineAsyncComponent(() => import('../../components/host/MetricsPane.vue'))
const SysInfoPane = defineAsyncComponent(() => import('../../components/host/SysInfoPane.vue'))
const TerminalPane = defineAsyncComponent(() => import('../../components/host/TerminalPane.vue'))
const DockerView = defineAsyncComponent(() => import('../docker/Docker.vue'))
const VulnerabilitiesView = defineAsyncComponent(() => import('./tabs/Vulnerabilities.vue'))
const HostNetworkPane = defineAsyncComponent(() => import('../../components/host/HostNetworkPane.vue'))

const route = useRoute()
const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()
const settings = useSettingsStore()

const host = ref<Host | null>(null)
const loading = ref(true)

// 无显式页签时的兜底：用户在通用设置里配的首选页签；非法值退回文件管理。
function defaultTab(): string {
  const pref = settings.getUserKey<string>(SETTING_KEYS.defaultHostTab, 'files')
  return validHostTabs.includes(pref) ? pref : 'files'
}

function resolveTab(raw: unknown): string {
  const key = typeof raw === 'string' ? raw : ''
  if (key) return validHostTabs.includes(key) ? key : 'files'
  return defaultTab()
}

const activeSubTab = ref(resolveTab(route.query.tab))

// 设置可能在挂载后才加载完（比如首屏直连主机详情）。只在用户没有显式
// 选过页签（URL 或点击）时应用首选项，避免覆盖用户操作。
settings.load().then(() => {
  if (route.name !== 'host-detail' || route.query.tab) return
  const pref = defaultTab()
  if (pref !== activeSubTab.value) selectTab(pref)
})

// 子页签既要能深链、刷新后还原，也要在顶部页签之间来回切换时记得住。所以
// 除了写 URL，还把带 query 的完整路径回写到工作区页签上——点页签回来时用的
// 就是这个 path，不带 query 的话会退回默认的「文件管理」，终端会话就断了。
function selectTab(key: string) {
  if (activeSubTab.value === key) return
  activeSubTab.value = key
  const query = { ...route.query, tab: key }
  router.replace({ query })
  rememberSubTab(key)
}

function rememberSubTab(key: string) {
  const hostId = route.params.id as string
  if (!hostId) return
  workspace.openTab({
    key: `/hosts/${hostId}`,
    title: host.value?.hostname || hostId,
    path: `/hosts/${hostId}?tab=${key}`,
    closable: true,
    viewName: 'HostDetail',
  })
}

watch(
  () => route.query.tab,
  (raw) => {
    // 组件被 KeepAlive 缓存后，切到别的页签时这里仍会收到通知，而那时
    // query.tab 是空的。只在仍处于本主机路由时才同步，否则会把已选的子页签
    // 重置成默认值，回来时终端已经被卸载。
    if (route.name !== 'host-detail') return
    activeSubTab.value = resolveTab(raw)
  },
)

async function load() {
  loading.value = true
  try {
    const hostId = route.params.id as string
    host.value = await getHost(hostId)
    workspace.openTab({
      key: `/hosts/${hostId}`,
      title: host.value.hostname,
      path: `/hosts/${hostId}?tab=${activeSubTab.value}`,
      closable: true,
      viewName: 'HostDetail',
    })
  } catch (e: any) {
    message.error(e.message || '加载主机信息失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="host-detail-layout">
    <NSpin v-if="loading" class="spin-center" />
    <template v-else-if="host">
      <!-- 顶部固定主机 Header 横幅 -->
      <HostHeaderBanner :host="host" @refresh="load" />

      <!-- 下部主内容区：左侧窄版二级 Icon 导航 + 右侧模块面板 -->
      <div class="detail-body-container">
        <!-- 左侧 Icon 二级导航 -->
        <div class="sub-nav-sidebar">
          <NTooltip
            v-for="item in subNavItems"
            :key="item.key"
            trigger="hover"
            placement="right"
          >
            <template #trigger>
              <div
                class="sub-nav-item"
                :class="{ active: activeSubTab === item.key }"
                @click="selectTab(item.key)"
              >
                <NIcon size="20">
                  <component :is="item.icon" />
                </NIcon>
              </div>
            </template>
            {{ item.label }}
          </NTooltip>
        </div>

        <!-- 右侧子视图面板 -->
        <div class="sub-view-pane" :class="{ 'is-terminal': activeSubTab === 'terminal' }">
          <!-- 面板按需加载，首次切换时显示等待态而不是空白 -->
          <Suspense>
            <template #default>
              <!-- 文件管理 -->
              <FileManagerPane
                v-if="activeSubTab === 'files'"
                :host-id="host.id"
                :os="host.os"
              />

              <!-- 资源监控 (Overview) -->
              <MetricsPane
                v-else-if="activeSubTab === 'metrics'"
                :host-id="host.id"
              />

              <!-- 系统状态与进程 -->
              <SysInfoPane
                v-else-if="activeSubTab === 'sysinfo'"
                :host-id="host.id"
              />

              <!-- 在线终端 -->
              <div v-else-if="activeSubTab === 'terminal'" class="terminal-pane-wrapper">
                <TerminalPane :host-id="host.id" :os="host.os" :distro="host.distro" />
              </div>

              <!-- Docker 管理（观测与运维入口，应用部署统一走全局应用中心） -->
              <DockerView v-else-if="activeSubTab === 'docker'" />

              <!-- 漏洞管理 (P3) -->
              <VulnerabilitiesView v-else-if="activeSubTab === 'vulnerabilities'" :host-id="host.id" />

              <!-- 异地组网 (Tailscale) -->
              <HostNetworkPane
                v-else-if="activeSubTab === 'network'"
                :host-id="host.id"
                :hostname="host.hostname"
                :os="host.os"
              />
            </template>
            <template #fallback>
              <div class="pane-loading">
                <NSpin size="medium" />
              </div>
            </template>
          </Suspense>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped lang="scss">
.host-detail-layout {
  display: flex;
  flex-direction: column;
  height: 100%;
  box-sizing: border-box;

  .spin-center {
    display: flex;
    justify-content: center;
    align-items: center;
    height: 100%;
  }

  .detail-body-container {
    flex: 1;
    min-height: 0;
    display: flex;
    gap: 12px;

    .sub-nav-sidebar {
      width: 48px;
      flex-shrink: 0;
      background-color: var(--bg-card);
      border: 1px solid var(--border-color);
      border-radius: 8px;
      box-shadow: var(--shadow-sm);
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 12px 0;
      gap: 12px;

      .sub-nav-item {
        width: 36px;
        height: 36px;
        border-radius: 6px;
        display: flex;
        align-items: center;
        justify-content: center;
        cursor: pointer;
        color: var(--text-secondary);
        transition: all 0.15s ease;
        line-height: 1;
        box-sizing: border-box;

        :deep(.n-icon) {
          display: flex;
          align-items: center;
          justify-content: center;
          margin: 0;
          padding: 0;
        }

        &:hover {
          color: #6366f1;
          background-color: var(--bg-hover);
        }

        &.active {
          color: #ffffff;
          background-color: #6366f1;
          box-shadow: var(--shadow-sm);
        }
      }
    }

    .sub-view-pane {
      flex: 1;
      min-width: 0;
      background-color: var(--bg-card);
      border: 1px solid var(--border-color);
      border-radius: 8px;
      box-shadow: var(--shadow-sm);
      overflow-y: auto;
      overflow-x: hidden;
      padding: var(--card-padding);
      -webkit-overflow-scrolling: touch;
      overscroll-behavior: contain;

      &.is-terminal {
        overflow: hidden;
        padding: var(--card-padding);
      }

      .terminal-pane-wrapper {
        height: 100%;
        min-height: 100%;
        background: var(--code-box-bg);
        border: 1px solid var(--border-color);
        border-radius: 6px;
        overflow: hidden;
      }

      .pane-loading {
        display: flex;
        align-items: center;
        justify-content: center;
        min-height: 240px;
        height: 100%;
      }
    }
  }
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .host-detail-layout {
    .detail-body-container {
      flex-direction: column;
      gap: 8px;

      /* 左侧竖版 Icon 导航 → 顶部横向滚动条 */
      .sub-nav-sidebar {
        width: 100%;
        flex-direction: row;
        justify-content: flex-start;
        padding: 6px 8px;
        gap: 8px;
        overflow-x: auto;

        &::-webkit-scrollbar {
          display: none;
        }

        .sub-nav-item {
          flex-shrink: 0;
        }
      }

      .sub-view-pane {
        overflow-y: auto;

        .terminal-pane-wrapper {
          min-height: 420px;
        }
      }
    }
  }
}
</style>
