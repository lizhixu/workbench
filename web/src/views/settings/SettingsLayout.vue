<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NInput, NIcon, NEmpty } from 'naive-ui'
import { SearchOutline } from '@vicons/ionicons5'
import { useWorkspaceStore } from '../../stores/workspace'
import { useAuthStore } from '../../stores/auth'
import {
  settingsSections,
  resolveSectionKey,
  SETTINGS_GROUPS,
  type SettingsSection,
} from './sections'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致。
// 路由仍是单条 /settings（子分区走 ?s= 查询参数，与主机详情 ?tab= 同模式），
// 所以布局组件名沿用 'Settings'，router 的 meta.viewName 不用动。
defineOptions({ name: 'Settings' })

const route = useRoute()
const router = useRouter()
const workspace = useWorkspaceStore()
const auth = useAuthStore()

const keyword = ref('')
// 安全入口：adminOnly 分区（证书签发/域名绑定）仅管理员可见。
// 后端对应接口本就 adminOnly + 审计写操作，前端隐藏是纵深防御。
const visibleSections = computed(() =>
  settingsSections.filter((s) => !s.adminOnly || auth.role === 'admin'),
)
const activeKey = ref(resolveSectionKey(route.query.s, visibleSections.value))

function selectSection(key: string) {
  if (activeKey.value === key) return
  activeKey.value = key
  router.replace({ query: { ...route.query, s: key } })
  // 把带 s 的完整路径回写到工作区页签：点页签回来时保持在同一设置分区，
  // 与 HostDetail 的 rememberSubTab 同理。
  workspace.openTab({
    key: '/settings',
    title: '系统设置',
    path: `/settings?s=${key}`,
    closable: true,
  })
}

watch(
  () => route.query.s,
  (raw) => {
    if (route.name !== 'settings') return
    activeKey.value = resolveSectionKey(raw, visibleSections.value)
  },
)

const filteredSections = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  if (!kw) return visibleSections.value
  return visibleSections.value.filter((s) =>
    `${s.label} ${s.keywords}`.toLowerCase().includes(kw),
  )
})

const groupedSections = computed(() => {
  return SETTINGS_GROUPS.map((group) => ({
    group,
    items: filteredSections.value.filter((s) => s.group === group),
  })).filter((g) => g.items.length > 0)
})

const activeSection = computed<SettingsSection>(
  () => visibleSections.value.find((s) => s.key === activeKey.value) ?? visibleSections.value[0],
)

function onSearchEnter() {
  if (filteredSections.value.length > 0) {
    selectSection(filteredSections.value[0].key)
  }
}

onMounted(() => {
  // 直接输入 /settings?s=ai 这类深链进来时，页签路径带上分区参数，
  // 点页签回来才能还原到同一分区。
  workspace.openTab({
    key: '/settings',
    title: '系统设置',
    path: `/settings?s=${activeKey.value}`,
    closable: true,
  })
})
</script>

<template>
  <div class="settings-view">
    <div class="page-header">
      <h2 class="page-title">系统设置</h2>
    </div>
    <div class="settings-layout">
    <!-- 左侧导航 -->
    <aside class="settings-nav">
      <NInput
        v-model:value="keyword"
        placeholder="搜索设置项"
        clearable
        size="small"
        class="settings-search"
        @keydown.enter="onSearchEnter"
      >
        <template #prefix>
          <NIcon :component="SearchOutline" />
        </template>
      </NInput>

      <div class="settings-nav-list">
        <template v-for="g in groupedSections" :key="g.group">
          <div class="settings-nav-group">{{ g.group }}</div>
          <div
            v-for="s in g.items"
            :key="s.key"
            class="settings-nav-item"
            :class="{ active: activeKey === s.key }"
            @click="selectSection(s.key)"
          >
            <NIcon :component="s.icon" size="16" class="nav-item-icon" />
            <span>{{ s.label }}</span>
          </div>
        </template>
        <NEmpty
          v-if="groupedSections.length === 0"
          description="没有匹配的设置项"
          size="small"
          class="settings-nav-empty"
        />
      </div>
    </aside>

    <!-- 右侧内容：分区组件 KeepAlive 缓存，切分区不丢已填表单 -->
    <main class="settings-content">
      <KeepAlive>
        <component :is="activeSection.component" :key="activeSection.key" />
      </KeepAlive>
    </main>
    </div>
  </div>
</template>

<style scoped lang="scss">
.settings-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}

.page-header {
  flex-shrink: 0;
  margin-bottom: 16px;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 700;
  color: var(--text-primary);
}

.settings-layout {
  display: flex;
  gap: 16px;
  flex: 1;
  min-height: 0;
}

.settings-nav {
  width: 216px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-height: 0;
}

.settings-search {
  flex-shrink: 0;
}

.settings-nav-list {
  flex: 1;
  overflow-y: auto;
  min-height: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.settings-nav-group {
  font-size: 12px;
  color: var(--text-tertiary, #9ca3af);
  font-weight: 600;
  padding: 10px 10px 4px;
  letter-spacing: 0.04em;

  &:first-child {
    padding-top: 2px;
  }
}

.settings-nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 8px;
  font-size: 13.5px;
  color: var(--text-secondary);
  cursor: pointer;
  user-select: none;
  transition: background-color 0.15s ease, color 0.15s ease;

  &:hover {
    background-color: rgba(255, 255, 255, 0.05);
    color: var(--text-primary);
  }

  &.active {
    background-color: rgba(99, 102, 241, 0.14);
    color: #818cf8;
    font-weight: 600;
  }

  .nav-item-icon {
    flex-shrink: 0;
  }
}

.settings-nav-empty {
  padding: 24px 0;
}

.settings-content {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  min-height: 0;
}

@media (max-width: 768px) {
  .settings-layout {
    flex-direction: column;
  }

  .settings-nav {
    width: 100%;
  }

  .settings-nav-list {
    flex-direction: row;
    overflow-x: auto;
    overflow-y: hidden;
    gap: 6px;
    padding-bottom: 4px;
  }

  .settings-nav-group {
    display: none;
  }

  .settings-nav-item {
    white-space: nowrap;
    flex-shrink: 0;
  }
}
</style>
