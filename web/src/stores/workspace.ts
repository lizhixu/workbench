import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

export interface WorkspaceTab {
  key: string
  title: string
  path: string
  closable?: boolean
  icon?: string
  /** 该页签视图的组件名，用于 KeepAlive 的 include 匹配。 */
  viewName?: string
}

export const useWorkspaceStore = defineStore('workspace', () => {
  const tabs = ref<WorkspaceTab[]>([
    {
      key: '/hosts',
      title: '主机列表',
      path: '/hosts',
      closable: false,
      viewName: 'HostList',
    },
  ])

  const activeKey = ref<string>('/hosts')

  /**
   * 仍处于打开状态的页签对应的组件名，交给 KeepAlive 的 include。
   * 只缓存还在页签栏里的视图：页签一关，名字从这里消失，KeepAlive 随即
   * 卸载该组件并触发它的 onUnmounted（终端断开、定时器清理都靠这个）。
   */
  const cachedViews = computed(() =>
    Array.from(new Set(tabs.value.map((t) => t.viewName).filter((n): n is string => !!n))),
  )

  function openTab(tab: WorkspaceTab) {
    const existing = tabs.value.find((t) => t.key === tab.key)
    if (!existing) {
      tabs.value.push(tab)
    } else {
      existing.title = tab.title
      existing.path = tab.path
      if (tab.viewName) existing.viewName = tab.viewName
    }
    activeKey.value = tab.key
  }

  function closeTab(key: string) {
    const idx = tabs.value.findIndex((t) => t.key === key)
    if (idx === -1) return
    const tabToClose = tabs.value[idx]
    if (tabToClose.closable === false) return // immutable tab

    tabs.value.splice(idx, 1)
    if (activeKey.value === key) {
      const prevTab = tabs.value[Math.max(0, idx - 1)]
      if (prevTab) {
        activeKey.value = prevTab.key
      }
    }
  }

  /** 关闭除 key 之外的所有可关闭页签，并把 key 设为当前页签。 */
  function closeOtherTabs(key: string) {
    tabs.value = tabs.value.filter((t) => t.key === key || t.closable === false)
    if (tabs.value.some((t) => t.key === key)) {
      activeKey.value = key
    }
  }

  /** 关闭 key 右侧的所有可关闭页签。 */
  function closeRightTabs(key: string) {
    const idx = tabs.value.findIndex((t) => t.key === key)
    if (idx === -1) return
    tabs.value = tabs.value.filter((t, i) => i <= idx || t.closable === false)
    // 当前页签若被关掉，回落到右键操作的那个页签
    if (!tabs.value.some((t) => t.key === activeKey.value)) {
      activeKey.value = key
    }
  }

  /** 关闭全部可关闭页签，只留固定页签并激活第一个。 */
  function closeAllTabs() {
    tabs.value = tabs.value.filter((t) => t.closable === false)
    activeKey.value = tabs.value[0]?.key ?? ''
  }

  /** 除自身与固定页签外是否还有可关闭页签，供右键菜单置灰用。 */
  function hasClosableOthers(key: string) {
    return tabs.value.some((t) => t.key !== key && t.closable !== false)
  }

  /** key 右侧是否存在可关闭页签。 */
  function hasClosableRight(key: string) {
    const idx = tabs.value.findIndex((t) => t.key === key)
    if (idx === -1) return false
    return tabs.value.slice(idx + 1).some((t) => t.closable !== false)
  }

  /**
   * 按路径回填页签的 viewName。页签可能从主机列表、告警跳转等处打开，那些
   * 调用点不一定知道组件名；缺了它该页签就不会进 KeepAlive 的 include。
   */
  /**
   * 按路径回填页签的 viewName。页签可能从主机列表、告警跳转等处打开，那些
   * 调用点不一定知道组件名；缺了它该页签就不会进 KeepAlive 的 include。
   * path 可能带 query（主机详情记录了子页签），比对时只看路径部分。
   */
  function ensureViewName(path: string, viewName: string) {
    const bare = (s: string) => s.split('?')[0]
    const tab = tabs.value.find((t) => bare(t.path) === bare(path) || bare(t.key) === bare(path))
    if (tab && tab.viewName !== viewName) {
      tab.viewName = viewName
    }
  }

  function setActiveKey(key: string) {
    activeKey.value = key
  }

  /**
   * 拖拽调整页签顺序：将 fromIndex 位置的页签移动到 targetIndex 的前面(left)或后面(right)
   */
  function moveTab(fromIndex: number, targetIndex: number, position: 'left' | 'right' = 'left') {
    if (fromIndex < 0 || fromIndex >= tabs.value.length) return
    if (targetIndex < 0 || targetIndex >= tabs.value.length) return
    if (fromIndex === targetIndex) return

    const item = tabs.value[fromIndex]
    tabs.value.splice(fromIndex, 1)

    let newIdx = targetIndex
    if (fromIndex < targetIndex) {
      newIdx = position === 'right' ? targetIndex : targetIndex - 1
    } else {
      newIdx = position === 'right' ? targetIndex + 1 : targetIndex
    }
    if (newIdx < 0) newIdx = 0
    if (newIdx > tabs.value.length) newIdx = tabs.value.length
    tabs.value.splice(newIdx, 0, item)
  }

  return {
    tabs,
    activeKey,
    cachedViews,
    openTab,
    closeTab,
    closeOtherTabs,
    closeRightTabs,
    closeAllTabs,
    hasClosableOthers,
    hasClosableRight,
    ensureViewName,
    setActiveKey,
    moveTab,
  }
})
