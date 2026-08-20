import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface WorkspaceTab {
  key: string
  title: string
  path: string
  closable?: boolean
  icon?: string
}

export const useWorkspaceStore = defineStore('workspace', () => {
  const tabs = ref<WorkspaceTab[]>([
    {
      key: '/hosts',
      title: '主机列表',
      path: '/hosts',
      closable: false,
    },
  ])

  const activeKey = ref<string>('/hosts')

  function openTab(tab: WorkspaceTab) {
    const existing = tabs.value.find((t) => t.key === tab.key)
    if (!existing) {
      tabs.value.push(tab)
    } else {
      existing.title = tab.title
      existing.path = tab.path
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

  function setActiveKey(key: string) {
    activeKey.value = key
  }

  return {
    tabs,
    activeKey,
    openTab,
    closeTab,
    setActiveKey,
  }
})
