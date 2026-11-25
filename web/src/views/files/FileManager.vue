<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { NAlert, NButton, NSpin, useMessage } from 'naive-ui'
import { getHost } from '../../api/hosts'
import type { Host } from '../../api/types'
import { useWorkspaceStore } from '../../stores/workspace'
import { useSettingsStore } from '../../stores/settings'
import HostHeaderBanner from '../../components/host/HostHeaderBanner.vue'
import FileManagerPane from '../../components/host/FileManagerPane.vue'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'FileManager' })

const route = useRoute()
const initialPath = computed(() => {
  const p = route.query.path
  return typeof p === 'string' && p.trim() ? p.trim() : undefined
})
const message = useMessage()
const workspace = useWorkspaceStore()
const settingsStore = useSettingsStore()

const host = ref<Host | null>(null)
const loading = ref(true)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const hostId = route.params.id as string
    host.value = await getHost(hostId)
    workspace.openTab({
      key: `/files/${hostId}`,
      title: host.value.hostname,
      path: `/files/${hostId}`,
      closable: true,
      viewName: (route.meta.viewName as string) || 'FileManager',
    })
  } catch (e: any) {
    loadError.value = e.message || '加载主机信息失败'
    message.error(loadError.value)
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  // 先把通用设置预加载完再取主机、挂载文件面板：面板挂载时会读
  // filesDefaultPath，设置未就绪时它在进入加载态前已按 files=[] 渲染出
  // 「无数据」假空状态。预加载后面板挂载即进入加载态，不再闪现假空状态。
  await settingsStore.load()
  await load()
})
</script>

<template>
  <div class="file-manager-view page-flex-column">
    <h2 class="page-title">文件管理</h2>
    <NSpin v-if="loading" class="spin-center" />
    <template v-else-if="host">
      <!-- 主机头部信息栏（与主机详情页同款）：文件管理默认收起规格区 -->
      <HostHeaderBanner :host="host" :default-expanded="false" @refresh="load" />
      <!-- 主机已取到才挂载面板，并把 os 传进去：面板不再为判 OS 另调一次
           getHost，挂载后直接进入文件列表加载态。独立页开启批量操作
           （删除/下载/移动），主机详情文件页签复用面板但不开启。 -->
      <div class="file-pane-wrap">
        <FileManagerPane :host-id="host.id" :os="host.os" :initial-path="initialPath" :batch-enabled="true" />
      </div>
    </template>
    <template v-else>
      <NAlert type="error" title="加载主机信息失败">{{ loadError }}</NAlert>
      <div class="load-error-action">
        <NButton @click="load">重新加载</NButton>
      </div>
    </template>
  </div>
</template>

<style scoped lang="scss">
.file-manager-view {
  .page-title {
    margin: 0 0 12px;
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
    flex-shrink: 0;
  }

  .spin-center {
    display: flex;
    justify-content: center;
    align-items: center;
    flex: 1;
    min-height: 0;
  }

  .file-pane-wrap {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .load-error-action {
    margin-top: 12px;
  }
}
</style>
