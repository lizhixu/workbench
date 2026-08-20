<script setup lang="ts">
import { computed, ref, watch, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute, RouterView } from 'vue-router'
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

// 监听路由同步 Workspace 页签
watch(
  () => route.path,
  (newPath) => {
    if (newPath === '/settings') {
      workspace.openTab({
        key: '/settings',
        title: '系统设置',
        path: '/settings',
        closable: true,
      })
    } else if (newPath === '/alerts') {
      workspace.openTab({
        key: '/alerts',
        title: '消息中心',
        path: '/alerts',
        closable: true,
      })
    } else if (newPath === '/hosts') {
      workspace.setActiveKey('/hosts')
    }
  },
  { immediate: true }
)

function openSettings() {
  workspace.openTab({
    key: '/settings',
    title: '系统设置',
    path: '/settings',
    closable: true,
  })
  router.push('/settings')
}

function openMessages() {
  workspace.openTab({
    key: '/alerts',
    title: '消息中心',
    path: '/alerts',
    closable: true,
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
  if (workspace.activeKey) {
    const activeTab = workspace.tabs.find((t) => t.key === workspace.activeKey)
    if (activeTab) {
      router.push(activeTab.path)
    }
  }
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
    <NLayoutHeader bordered class="app-top-header">
      <div class="header-left">
        <div class="brand-logo" @click="router.push('/hosts')">
          <div class="logo-badge">
            <NIcon size="18" color="#ffffff"><ServerOutline /></NIcon>
          </div>
          <span class="brand-title">牧云主机管理助手</span>
        </div>

        <!-- 顶部 Workspace 多页签栏 -->
        <div class="workspace-tabs">
          <div
            v-for="tab in workspace.tabs"
            :key="tab.key"
            class="workspace-tab-item"
            :class="{ active: workspace.activeKey === tab.key }"
            @click="handleSelectTab(tab)"
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
      </div>

      <!-- 顶栏右侧辅助工具与用户菜单 -->
      <div class="header-right">
        <NSpace align="center" :size="12">
          <!-- 消息中心 🔔 -->
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

          <!-- 全局设置 ⚙ -->
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

          <!-- 亮/暗主题切换 ☀/🌙 -->
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

    <NLayoutContent class="app-body">
      <RouterView />
    </NLayoutContent>
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

.app-body {
  height: calc(100vh - 50px);
  overflow: hidden;
  background-color: var(--bg-app);
}
</style>
