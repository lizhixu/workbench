<script setup lang="ts">
import { computed, nextTick, ref, watch, onMounted, onUnmounted, type Component } from 'vue'
import { useRouter, useRoute, RouterView, type RouteLocationNormalizedLoaded } from 'vue-router'
import {
  NLayout,
  NLayoutHeader,
  NLayoutContent,
  NButton,
  NDropdown,
  NSpace,
  NIcon,
  NBadge,
  NTooltip,
  NModal,
  NForm,
  NFormItem,
  NInput,
  useMessage,
} from 'naive-ui'
import { useAuthStore } from '../../stores/auth'
import { useWorkspaceStore, type WorkspaceTab } from '../../stores/workspace'
import { useSettingsStore } from '../../stores/settings'
import { useNotificationStore } from '../../stores/notifications'
import { updatePassword } from '../../api/users'
import {
  PersonOutline,
  SunnyOutline,
  MoonOutline,
  SettingsOutline,
  NotificationsOutline,
  CloseOutline,
  ServerOutline,
  TerminalOutline,
  TimeOutline,
  DocumentTextOutline,
  KeyOutline,
  PeopleOutline,
  LayersOutline,
  SparklesOutline,
  BarChartOutline,
  ChevronBackOutline,
  ChevronForwardOutline,
} from '@vicons/ionicons5'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
const workspace = useWorkspaceStore()
const settings = useSettingsStore()
const notifications = useNotificationStore()
const message = useMessage()

onMounted(() => {
  notifications.startPolling()
})

onUnmounted(() => {
  notifications.stopPolling()
})

// ===================== 全局侧边导航 =====================
interface NavEntry {
  key: string
  title: string
  path: string
  icon: Component
  adminOnly?: boolean
  closable?: boolean
  /** 视图组件名，KeepAlive 的 include 用它决定缓存哪些页面。 */
  viewName: string
}

const navEntries: NavEntry[] = [
  { key: '/hosts', title: '主机列表', path: '/hosts', icon: ServerOutline, closable: false, viewName: 'HostList' },
  { key: '/batch-exec', title: '推送命令', path: '/batch-exec', icon: TerminalOutline, viewName: 'BatchExec' },
  { key: '/sessions', title: '会话审计', path: '/sessions', icon: TimeOutline, viewName: 'SessionList' },
  { key: '/audit', title: '操作审计', path: '/audit', icon: DocumentTextOutline, adminOnly: true, viewName: 'AuditList' },
  { key: '/alerts', title: '消息中心', path: '/alerts', icon: NotificationsOutline, viewName: 'AlertList' },
  { key: '/vault', title: '凭据金库', path: '/vault', icon: KeyOutline, adminOnly: true, viewName: 'CredentialList' },
  { key: '/groups', title: '分组权限', path: '/groups', icon: LayersOutline, viewName: 'GroupList' },
  { key: '/users', title: '用户管理', path: '/users', icon: PeopleOutline, adminOnly: true, viewName: 'UserList' },
  { key: '/ai/chat', title: 'AI 助手', path: '/ai/chat', icon: SparklesOutline, viewName: 'AiChat' },
  { key: '/ai/report', title: '运维报告', path: '/ai/report', icon: BarChartOutline, viewName: 'OpsReport' },
  { key: '/settings', title: '系统设置', path: '/settings', icon: SettingsOutline, viewName: 'Settings' },
]

const visibleNavEntries = computed(() =>
  navEntries.filter((e) => !e.adminOnly || auth.role === 'admin')
)

const navCollapsed = ref(localStorage.getItem('watchman_nav_collapsed') === '1')

function toggleNav() {
  navCollapsed.value = !navCollapsed.value
  localStorage.setItem('watchman_nav_collapsed', navCollapsed.value ? '1' : '0')
}

// 主机详情/文件/Docker 等子路由高亮到「主机列表」这一入口
const activeNavKey = computed(() => {
  const p = route.path
  const exact = visibleNavEntries.value.find((e) => e.path === p)
  if (exact) return exact.key
  if (p.startsWith('/hosts') || p.startsWith('/files/') || p.startsWith('/docker/')) return '/hosts'
  const prefixed = visibleNavEntries.value.find((e) => e.path !== '/' && p.startsWith(e.path + '/'))
  return prefixed?.key ?? ''
})

function goNav(entry: NavEntry) {
  workspace.openTab({
    key: entry.key,
    title: entry.title,
    path: entry.path,
    closable: entry.closable !== false,
    viewName: entry.viewName,
  })
  router.push(entry.path)
}

// 监听路由同步 Workspace 页签：直接输入 URL 进来的一级页面也补一个页签
watch(
  () => route.path,
  (newPath) => {
    const entry = navEntries.find((e) => e.path === newPath)
    if (!entry) return
    if (entry.closable === false) {
      workspace.setActiveKey(entry.key)
      return
    }
    workspace.openTab({
      key: entry.key,
      title: entry.title,
      path: entry.path,
      closable: true,
      viewName: entry.viewName,
    })
  },
  { immediate: true }
)

// 页签也可能由别处打开（主机列表点进详情、告警跳主机、AI 页自注册），那些
// 调用未必带 viewName，而且往往在路由变化之后才注册页签。所以这里同时盯着
// 页签数量：新页签一出现就按当前路由 meta 回填组件名。漏一处该页签就进不了
// KeepAlive 的 include，状态照旧会丢。
watch(
  () => [route.path, route.meta.viewName, workspace.tabs.length] as const,
  ([path, viewName]) => {
    if (typeof viewName === 'string' && viewName) {
      workspace.ensureViewName(path, viewName)
    }
  },
  { immediate: true, flush: 'post' }
)

function openSettings() {
  workspace.openTab({
    key: '/settings',
    title: '系统设置',
    path: '/settings',
    closable: true,
    viewName: 'Settings',
  })
  router.push('/settings')
}

function openMessages() {
  workspace.openTab({
    key: '/alerts',
    title: '消息中心',
    path: '/alerts',
    closable: true,
    viewName: 'AlertList',
  })
  router.push('/alerts')
}

// 修改密码弹窗
const showPwdModal = ref(false)
const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const pwdLoading = ref(false)

function openPwdModal() {
  oldPwd.value = ''
  newPwd.value = ''
  confirmPwd.value = ''
  showPwdModal.value = true
}

async function submitPassword() {
  if (!oldPwd.value || !newPwd.value || !confirmPwd.value) {
    message.warning('请填写所有字段')
    return
  }
  if (newPwd.value !== confirmPwd.value) {
    message.warning('两次输入的新密码不一致')
    return
  }
  if (newPwd.value.length < 6) {
    message.warning('新密码至少 6 位')
    return
  }
  pwdLoading.value = true
  try {
    await updatePassword(oldPwd.value, newPwd.value)
    message.success('密码修改成功')
    showPwdModal.value = false
  } catch (e: any) {
    message.error(e.message)
  } finally {
    pwdLoading.value = false
  }
}

function handleSelectTab(tab: WorkspaceTab) {
  workspace.setActiveKey(tab.key)
  router.push(tab.path)
}

function handleCloseTab(e: MouseEvent, tabKey: string) {
  e.stopPropagation()
  workspace.closeTab(tabKey)
  syncRouteToActiveTab()
}

// ---- 页签右键菜单 ----
const tabMenuVisible = ref(false)
const tabMenuX = ref(0)
const tabMenuY = ref(0)
const tabMenuKey = ref('')

const tabMenuOptions = computed(() => {
  const key = tabMenuKey.value
  const tab = workspace.tabs.find((t) => t.key === key)
  const pinned = tab?.closable === false
  return [
    { label: '关闭', key: 'close', disabled: pinned },
    { label: '关闭其他', key: 'close-others', disabled: !workspace.hasClosableOthers(key) },
    { label: '关闭右侧', key: 'close-right', disabled: !workspace.hasClosableRight(key) },
    { type: 'divider', key: 'd1' },
    { label: '关闭全部', key: 'close-all', disabled: !workspace.tabs.some((t) => t.closable !== false) },
  ]
})

function openTabMenu(e: MouseEvent, tabKey: string) {
  e.preventDefault()
  tabMenuKey.value = tabKey
  tabMenuX.value = e.clientX
  tabMenuY.value = e.clientY
  // 卸载再挂载，让菜单按新坐标重建；同一实例不会跟着 x/y 移动。
  tabMenuVisible.value = false
  nextTick(() => {
    tabMenuVisible.value = true
  })
}

function handleTabMenuSelect(key: string) {
  tabMenuVisible.value = false
  const target = tabMenuKey.value
  switch (key) {
    case 'close':
      workspace.closeTab(target)
      break
    case 'close-others':
      workspace.closeOtherTabs(target)
      break
    case 'close-right':
      workspace.closeRightTabs(target)
      break
    case 'close-all':
      workspace.closeAllTabs()
      break
  }
  syncRouteToActiveTab()
}

/** 关闭动作可能改变当前页签，路由要跟着走，否则内容区与页签不一致。 */
function syncRouteToActiveTab() {
  const activeTab = workspace.tabs.find((t) => t.key === workspace.activeKey)
  if (activeTab && route.path !== activeTab.path) {
    router.push(activeTab.path)
  }
}

function onTabsWheel(e: WheelEvent) {
  const el = e.currentTarget as HTMLElement
  if (!el) return
  if (Math.abs(e.deltaY) > Math.abs(e.deltaX)) {
    el.scrollLeft += e.deltaY * 0.8
    e.preventDefault()
  }
}

/**
 * KeepAlive 的 include。
 *
 * 除了仍在页签里的视图，还必须包含当前路由的视图名：像主机详情这种页签是在
 * 组件挂载后的异步 load 里才注册的，首屏渲染时 include 里还没有它，Vue 会给
 * 这次 vnode 打上「不缓存」标记，之后再改 include 也救不回来，切走就丢状态。
 * 页签关闭且不是当前路由时名字自然消失，KeepAlive 随即卸载并释放资源。
 */
const keepAliveInclude = computed(() => {
  const names = new Set(workspace.cachedViews)
  const current = route.meta.viewName
  if (typeof current === 'string' && current) names.add(current)
  return Array.from(names)
})

/**
 * KeepAlive 的缓存键。同一个组件在不同参数下必须各自缓存，否则从主机 A 的
 * 详情切到主机 B，B 会复用 A 的缓存实例，看到的是上一台主机的数据。
 */
function cacheKeyOf(r: RouteLocationNormalizedLoaded): string {
  const id = r.params.id
  return typeof id === 'string' && id ? `${r.name as string}:${id}` : (r.name as string) || r.path
}

const userOptions = computed(() => [
  {
    label: `${auth.user?.username ?? 'user'} (${auth.role === 'admin' ? '管理员' : auth.role === 'operator' ? '运维员' : '只读用户'})`,
    key: 'user-info',
    disabled: true,
  },
  { type: 'divider', key: 'd1' },
  { label: '修改密码', key: 'change-password' },
  { label: '退出登录', key: 'logout' },
])

function handleUser(key: string) {
  if (key === 'logout') {
    auth.logout()
    router.push({ name: 'login' })
  } else if (key === 'change-password') {
    openPwdModal()
  }
}
</script>

<template>
  <NLayout style="height: 100vh" class="app-layout">
    <!-- 顶部 Header 导航与多 Workspace 页签栏 -->
    <NLayoutHeader class="app-top-header">
      <div class="header-left">
        <div class="brand-logo" @click="router.push('/hosts')">
          <div class="logo-badge">
            <NIcon size="18" color="#ffffff"><ServerOutline /></NIcon>
          </div>
          <span class="brand-title">牧云主机管理助手</span>
        </div>

        <!-- 顶部 Workspace 多页签栏 -->
        <div class="workspace-tabs" @wheel.prevent="onTabsWheel">
          <div
            v-for="tab in workspace.tabs"
            :key="tab.key"
            class="workspace-tab-item"
            :class="{ active: workspace.activeKey === tab.key }"
            @click="handleSelectTab(tab)"
            @contextmenu="(e) => openTabMenu(e, tab.key)"
          >
            <span class="tab-dot" v-if="tab.key.startsWith('/hosts/')"></span>
            <span class="tab-title">{{ tab.title }}</span>
            <button
              v-if="tab.closable !== false"
              class="tab-close-btn"
              @click="(e) => handleCloseTab(e, tab.key)"
            >
              <NIcon size="12"><CloseOutline /></NIcon>
            </button>
          </div>
        </div>

        <!-- 页签右键菜单：关闭 / 关闭其他 / 关闭右侧 / 关闭全部。
             v-if 控制挂载：manual 触发的 NDropdown 在 show 转 false 时不会卸载
             菜单体，只切 show 会让菜单一直停在屏幕上。 -->
        <NDropdown
          v-if="tabMenuVisible"
          trigger="manual"
          placement="bottom-start"
          :show="true"
          :x="tabMenuX"
          :y="tabMenuY"
          :options="tabMenuOptions"
          @select="handleTabMenuSelect"
          @clickoutside="tabMenuVisible = false"
        />
      </div>

      <!-- 顶栏右侧辅助工具与用户菜单 -->
      <div class="header-right">
        <NSpace align="center" :size="12">
          <!-- 消息中心 -->
          <NTooltip trigger="hover">
            <template #trigger>
              <NBadge :value="notifications.unreadCount" :max="99">
                <NButton
                  quaternary
                  circle
                  size="small"
                  @click="openMessages"
                >
                  <template #icon>
                    <NIcon size="18"><NotificationsOutline /></NIcon>
                  </template>
                </NButton>
              </NBadge>
            </template>
            <span>消息中心 ({{ notifications.unreadCount }} 未读)</span>
          </NTooltip>

          <!-- 全局设置 -->
          <NTooltip trigger="hover">
            <template #trigger>
              <NButton
                quaternary
                circle
                size="small"
                @click="openSettings"
              >
                <template #icon>
                  <NIcon size="18"><SettingsOutline /></NIcon>
                </template>
              </NButton>
            </template>
            <span>系统设置</span>
          </NTooltip>

          <!-- 亮/暗主题切换 -->
          <NTooltip trigger="hover">
            <template #trigger>
              <NButton
                quaternary
                circle
                size="small"
                @click="settings.toggleTheme"
              >
                <template #icon>
                  <NIcon size="18">
                    <SunnyOutline v-if="settings.themeMode === 'dark'" />
                    <MoonOutline v-else />
                  </NIcon>
                </template>
              </NButton>
            </template>
            <span>切换至 {{ settings.themeMode === 'dark' ? '亮色' : '暗色' }}模式</span>
          </NTooltip>

          <!-- 用户个人中心 -->
          <NDropdown :options="userOptions" @select="handleUser">
            <NButton quaternary size="small" class="user-btn">
              <template #icon>
                <NIcon size="16"><PersonOutline /></NIcon>
              </template>
              <span class="user-name">{{ auth.user?.username ?? 'user' }}</span>
            </NButton>
          </NDropdown>
        </NSpace>
      </div>
    </NLayoutHeader>

    <div class="app-main">
      <!-- 左侧全局一级导航 -->
      <aside class="app-side-nav" :class="{ collapsed: navCollapsed }">
        <div class="side-nav-list">
          <NTooltip
            v-for="entry in visibleNavEntries"
            :key="entry.key"
            trigger="hover"
            placement="right"
            :disabled="!navCollapsed"
          >
            <template #trigger>
              <div
                class="side-nav-item"
                :class="{ active: activeNavKey === entry.key }"
                @click="goNav(entry)"
              >
                <NIcon size="18" class="side-nav-icon">
                  <component :is="entry.icon" />
                </NIcon>
                <span v-if="!navCollapsed" class="side-nav-label">{{ entry.title }}</span>
              </div>
            </template>
            {{ entry.title }}
          </NTooltip>
        </div>

        <div class="side-nav-footer" @click="toggleNav">
          <NIcon size="16">
            <ChevronForwardOutline v-if="navCollapsed" />
            <ChevronBackOutline v-else />
          </NIcon>
          <span v-if="!navCollapsed" class="side-nav-label">收起侧栏</span>
        </div>
      </aside>

      <NLayoutContent class="app-body">
        <!-- 页签是工作区，切走再切回不能丢状态（终端会话、已填的表单、
             翻到的页码）。用 KeepAlive 缓存仍在页签里的视图；页签关闭后
             对应 key 从 cachedViews 移除，组件随之卸载，不会一直占内存。 -->
        <RouterView v-slot="{ Component, route: r }">
          <KeepAlive :include="keepAliveInclude">
            <component :is="Component" :key="cacheKeyOf(r)" />
          </KeepAlive>
        </RouterView>
      </NLayoutContent>
    </div>
  </NLayout>

  <!-- 修改密码弹窗 -->
  <NModal v-model:show="showPwdModal" preset="card" title="修改密码" style="width: 440px">
    <NForm label-placement="top">
      <NFormItem label="当前密码">
        <NInput v-model:value="oldPwd" type="password" show-password-on="click" placeholder="当前密码" />
      </NFormItem>
      <NFormItem label="新密码">
        <NInput v-model:value="newPwd" type="password" show-password-on="click" placeholder="至少 6 位" />
      </NFormItem>
      <NFormItem label="确认新密码">
        <NInput
          v-model:value="confirmPwd"
          type="password"
          show-password-on="click"
          placeholder="再次输入新密码"
          @keyup.enter="submitPassword"
        />
      </NFormItem>
    </NForm>
    <NSpace justify="end">
      <NButton @click="showPwdModal = false">取消</NButton>
      <NButton type="primary" :loading="pwdLoading" @click="submitPassword">确认修改</NButton>
    </NSpace>
  </NModal>
</template>

<style scoped lang="scss">
.app-top-header {
  height: 50px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 16px;
  user-select: none;
  background-color: var(--bg-card);
  border-bottom: 1px solid var(--border-color);

  .header-left {
    display: flex;
    align-items: center;
    gap: 16px;
    height: 100%;
    overflow: hidden;
  }

  .brand-logo {
    display: flex;
    align-items: center;
    gap: 8px;
    cursor: pointer;

    .logo-badge {
      width: 28px;
      height: 28px;
      border-radius: 6px;
      background: linear-gradient(135deg, #6366f1 0%, #3b82f6 100%);
      display: flex;
      align-items: center;
      justify-content: center;
    }

    .brand-title {
      font-size: 14px;
      font-weight: 700;
      letter-spacing: 0.3px;
      color: var(--text-primary);
    }
  }

  .workspace-tabs {
    display: flex;
    align-items: center;
    gap: 6px;
    height: 100%;
    overflow-x: auto;
    overflow-y: hidden;
    scrollbar-width: none;
    -webkit-overflow-scrolling: touch;
    overscroll-behavior: contain;

    &::-webkit-scrollbar {
      display: none;
    }

    .workspace-tab-item {
      display: flex;
      align-items: center;
      gap: 6px;
      height: 32px;
      padding: 0 12px;
      border-radius: 6px;
      font-size: 13px;
      cursor: pointer;
      color: var(--text-secondary);
      background-color: var(--tab-bg);
      transition: all 0.15s ease;

      &:hover {
        color: var(--text-primary);
        background-color: var(--tab-hover);
      }

      &.active {
        background: #6366f1;
        color: #ffffff;
        font-weight: 600;

        .tab-dot {
          background-color: #10b981;
        }

        .tab-close-btn {
          color: rgba(255, 255, 255, 0.8);
          &:hover {
            color: #ffffff;
            background-color: rgba(255, 255, 255, 0.2);
          }
        }
      }

      .tab-dot {
        width: 8px;
        height: 8px;
        border-radius: 50%;
        background-color: #f59e0b;
      }

      .tab-title {
        white-space: nowrap;
      }

      .tab-close-btn {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 16px;
        height: 16px;
        border: none;
        background: transparent;
        border-radius: 50%;
        cursor: pointer;
        color: currentColor;
        opacity: 0.7;

        &:hover {
          opacity: 1;
          background-color: rgba(0, 0, 0, 0.15);
        }
      }
    }
  }

  .header-right {
    display: flex;
    align-items: center;

    .tool-btn {
      font-size: 12px;
    }

    .user-btn {
      display: flex;
      align-items: center;
      gap: 6px;
    }

    .user-name {
      font-size: 13px;
      font-weight: 600;
    }

    .role-tag {
      font-size: 10px;
      height: 16px;
      line-height: 16px;
      padding: 0 4px;
    }
  }
}

.app-main {
  display: flex;
  height: calc(100vh - 50px);
  overflow: hidden;
}

.app-side-nav {
  width: 176px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  background-color: var(--bg-card);
  border-right: 1px solid var(--border-color);
  padding: 10px 8px;
  box-sizing: border-box;
  user-select: none;
  transition: width 0.18s ease;

  &.collapsed {
    width: 52px;

    .side-nav-item,
    .side-nav-footer {
      justify-content: center;
      padding: 0;
    }
  }

  .side-nav-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
    overflow-y: auto;
    scrollbar-width: none;

    &::-webkit-scrollbar {
      display: none;
    }
  }

  .side-nav-item {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 36px;
    padding: 0 10px;
    border-radius: 6px;
    font-size: 13px;
    cursor: pointer;
    color: var(--text-secondary);
    transition: all 0.15s ease;

    .side-nav-icon {
      flex-shrink: 0;
    }

    &:hover {
      color: var(--text-primary);
      background-color: var(--bg-hover);
    }

    &.active {
      color: #ffffff;
      background: #6366f1;
      font-weight: 600;
    }
  }

  .side-nav-footer {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 32px;
    padding: 0 10px;
    margin-top: 8px;
    border-top: 1px solid var(--border-color);
    padding-top: 10px;
    border-radius: 6px;
    font-size: 12px;
    cursor: pointer;
    color: var(--text-secondary);

    &:hover {
      color: var(--text-primary);
    }
  }

  .side-nav-label {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
}

.app-body {
  flex: 1;
  min-width: 0;
  height: 100%;
  overflow: hidden;
  background-color: var(--bg-app);
  /* 所有路由页面的统一内边距。页面组件自身不再设置外层 padding，
     由这一处决定留白，保证各页面对齐一致。 */
  padding: var(--page-padding);
}

/* ===================== 移动端适配 ===================== */
@media (max-width: 768px) {
  .app-top-header {
    padding: 0 8px;

    .header-left {
      gap: 8px;
      flex: 1;
      min-width: 0;

      .brand-logo {
        .brand-title {
          display: none;
        }
      }

      .workspace-tabs {
        flex: 1;
        min-width: 0;
      }
    }

    .header-right {
      flex-shrink: 0;

      .user-name {
        display: none;
      }
    }
  }

  /* 窄屏强制图标模式，避免侧栏吃掉内容宽度 */
  .app-side-nav {
    width: 48px;
    padding: 8px 4px;

    .side-nav-item,
    .side-nav-footer {
      justify-content: center;
      padding: 0;
    }

    .side-nav-label {
      display: none;
    }
  }
}
</style>
