<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { NSpin, NIcon, useMessage, NTooltip } from 'naive-ui'
import {
  StatsChartOutline,
  ListOutline,
  CubeOutline,
  FolderOpenOutline,
  ShieldCheckmarkOutline,
  TerminalOutline,
  StorefrontOutline,
} from '@vicons/ionicons5'
import { getHost } from '../../api/hosts'
import type { Host } from '../../api/types'
import { useWorkspaceStore } from '../../stores/workspace'
import HostHeaderBanner from '../../components/host/HostHeaderBanner.vue'
import TerminalPane from '../../components/host/TerminalPane.vue'
import MetricsPane from '../../components/host/MetricsPane.vue'
import SysInfoPane from '../../components/host/SysInfoPane.vue'
import FileManagerPane from '../../components/host/FileManagerPane.vue'
import DockerView from '../docker/Docker.vue'
import AppStoreView from '../apps/AppStore.vue'
import VulnerabilitiesView from './tabs/Vulnerabilities.vue'

const route = useRoute()
const message = useMessage()
const workspace = useWorkspaceStore()

const host = ref<Host | null>(null)
const loading = ref(true)
const activeSubTab = ref('files') // Default to files tab as shown in screenshots

const subNavItems = [
  { key: 'files', label: '文件管理', icon: FolderOpenOutline },
  { key: 'metrics', label: '资源监控', icon: StatsChartOutline },
  { key: 'sysinfo', label: '系统状态', icon: ListOutline },
  { key: 'terminal', label: '在线终端', icon: TerminalOutline },
  { key: 'docker', label: 'Docker', icon: CubeOutline },
  { key: 'apps', label: '应用市场', icon: StorefrontOutline },
  { key: 'vulnerabilities', label: '漏洞管理', icon: ShieldCheckmarkOutline },
]

async function load() {
  loading.value = true
  try {
    const hostId = route.params.id as string
    host.value = await getHost(hostId)
    workspace.openTab({
      key: `/hosts/${hostId}`,
      title: host.value.hostname,
      path: `/hosts/${hostId}`,
      closable: true,
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
      <HostHeaderBanner :host="host" />

      <!-- 下部主内容区：左侧窄版二级 Icon 导航 + 右侧模块面板 -->
      <div class="detail-body-container">
        <!-- 左侧 Icon 二级导航 -->
        <div class="sub-nav-sidebar">
          <div
            v-for="item in subNavItems"
            :key="item.key"
            class="sub-nav-item"
            :class="{ active: activeSubTab === item.key }"
            @click="activeSubTab = item.key"
          >
            <NTooltip trigger="hover" placement="right">
              <template #trigger>
                <div class="icon-wrap">
                  <NIcon size="20"><component :is="item.icon" /></NIcon>
                </div>
              </template>
              {{ item.label }}
            </NTooltip>
          </div>
        </div>

        <!-- 右侧子视图面板 -->
        <div class="sub-view-pane">
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
            <TerminalPane :host-id="host.id" />
          </div>

          <!-- Docker 管理 (P2) -->
          <DockerView v-else-if="activeSubTab === 'docker'" />

          <!-- 应用市场 (P2) -->
          <AppStoreView v-else-if="activeSubTab === 'apps'" :host-id="host.id" />

          <!-- 漏洞管理 (P3) -->
          <VulnerabilitiesView v-else-if="activeSubTab === 'vulnerabilities'" :host-id="host.id" />
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
  padding: 14px;
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
      padding: 14px;

      .terminal-pane-wrapper {
        height: 100%;
        min-height: 550px;
        background: var(--code-box-bg);
        border-radius: 6px;
        overflow: hidden;
      }
    }
  }
}
</style>
