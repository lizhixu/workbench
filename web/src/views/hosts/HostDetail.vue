<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NSpin, NIcon, NSkeleton, useMessage, NTooltip, NTabs, NTabPane } from 'naive-ui'
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

// 系统状态子页签由本页持有（DESIGN.md §9.5：Tab 在卡片外渲染），
// SysInfoPane 只按传入的页签取数。
const sysinfoTab = ref('process')

// 系统状态子页签横向滚动：窄屏下页签放不下时，纵向滚轮转为横向滚动。
function onTabWheel(e: WheelEvent) {
  const el = e.currentTarget as HTMLElement
  if (!el) return
  if (Math.abs(e.deltaY) > Math.abs(e.deltaX)) {
    el.scrollLeft += e.deltaY * 0.8
    e.preventDefault()
  }
}

// 左侧图标导航 tooltip 受控显隐：Naive 默认 keep-alive-on-hover 会让弹层在
// 鼠标移入弹层本身后继续存活，叠加切页签时触发节点重渲染丢 mouseleave，弹层
// 会长时间盖住右侧内容（文件管理工具栏 / 系统状态 PID=1 首行 / Docker 首行
// 容器名）。显隐由本文件接管：切页签时强制全部隐藏，鼠标离开触发器即隐藏。
const navTooltipShown = ref<Record<string, boolean>>({})

function hideNavTooltips() {
  for (const item of subNavItems) navTooltipShown.value[item.key] = false
}

// Suspense fallback 的加载文案：按当前子页签给明确提示，避免只有转圈。
const activeTabLabel = computed(
  () => subNavItems.find((i) => i.key === activeSubTab.value)?.label || '',
)

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
  hideNavTooltips()
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

let pollTimer: any = null

function checkPolling() {
  if (host.value?.upgrading) {
    if (!pollTimer) {
      pollTimer = setInterval(async () => {
        try {
          const hostId = route.params.id as string
          if (!hostId || route.name !== 'host-detail') return
          const updated = await getHost(hostId)
          host.value = updated
          if (!updated.upgrading && pollTimer) {
            clearInterval(pollTimer)
            pollTimer = null
          }
        } catch {
          // ignore
        }
      }, 3000)
    }
  } else {
    if (pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  }
}

watch(() => host.value?.upgrading, () => {
  checkPolling()
})

onUnmounted(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
})

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
    checkPolling()
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
            :keep-alive-on-hover="false"
            :delay="200"
            :show="navTooltipShown[item.key]"
            @update:show="(v: boolean) => { navTooltipShown[item.key] = v }"
          >
            <template #trigger>
              <div
                class="sub-nav-item"
                :class="{ active: activeSubTab === item.key }"
                @click="selectTab(item.key)"
                @mouseleave="navTooltipShown[item.key] = false"
              >
                <NIcon size="20">
                  <component :is="item.icon" />
                </NIcon>
              </div>
            </template>
            {{ item.label }}
          </NTooltip>
        </div>

        <!-- 右侧子视图列：系统状态子页签在卡片外（DESIGN.md §9.5），其余页签无 Tabs -->
        <div class="sub-view-column">
          <div v-if="activeSubTab === 'sysinfo'" class="tabs-container">
            <div class="tabs-scroller" @wheel.prevent="onTabWheel">
              <NTabs v-model:value="sysinfoTab" type="line" size="small">
                <NTabPane name="process" tab="进程清单" />
                <NTabPane name="port" tab="网络端口" />
                <NTabPane name="user" tab="系统账号" />
                <NTabPane name="login" tab="登录历史" />
              </NTabs>
            </div>
          </div>

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
                  :probe-enabled="!!host.probe_enabled"
                />

                <!-- 系统状态与进程 -->
                <SysInfoPane
                  v-else-if="activeSubTab === 'sysinfo'"
                  :host-id="host.id"
                  :active-tab="sysinfoTab"
                  @update:active-tab="sysinfoTab = $event"
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
                <!-- 子面板异步 chunk 加载中：骨架 + 明确文案，不要只有转圈和空白 -->
                <div class="pane-loading">
                  <NSkeleton text :repeat="1" :width="'40%'" />
                  <NSkeleton text :repeat="3" />
                  <NSkeleton :height="'180px'" :sharp="false" />
                  <div class="pane-loading-text">
                    <NSpin size="small" />
                    <span>正在加载{{ activeTabLabel }}，请稍候…</span>
                  </div>
                </div>
              </template>
            </Suspense>
          </div>
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

    /* 右侧子视图列：Tab 在卡片外（DESIGN.md §9.5），tabs-container 与
       sub-view-pane 纵向排列，卡片吃掉剩余高度。 */
    .sub-view-column {
      flex: 1;
      min-width: 0;
      min-height: 0;
      display: flex;
      flex-direction: column;
      gap: 12px;

      .tabs-container {
        flex-shrink: 0;

        .tabs-scroller {
          overflow-x: auto;
          overflow-y: hidden;
          -webkit-overflow-scrolling: touch;
          scrollbar-width: none;

          &::-webkit-scrollbar {
            display: none;
          }

          :deep(.n-tabs) {
            min-width: max-content;
          }

          :deep(.n-tabs-tab) {
            white-space: nowrap;
          }
        }
      }
    }

    .sub-view-pane {
      flex: 1;
      min-width: 0;
      min-height: 0;
      background-color: var(--bg-card);
      /* DESIGN.md §9.6：卡片不画外层实线边框，靠底色与阴影分隔。 */
      border: none;
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
        flex-direction: column;
        align-items: stretch;
        justify-content: flex-start;
        gap: 12px;
        min-height: 240px;
        height: 100%;

        .pane-loading-text {
          display: flex;
          align-items: center;
          justify-content: center;
          gap: 8px;
          margin-top: 4px;
          color: var(--text-secondary);
          font-size: 13px;
        }
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
