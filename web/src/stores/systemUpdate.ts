import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { checkSystemUpdate, onlineUpgradeServer, type UpdateInfo } from '../api/upgrade'

export const useSystemUpdateStore = defineStore('systemUpdate', () => {
  const updateInfo = ref<UpdateInfo | null>(null)
  const checking = ref(false)
  const checkError = ref('')
  const upgrading = ref(false)

  const hasUpdate = computed(() => updateInfo.value?.has_update === true)
  const latestVersion = computed(() => updateInfo.value?.latest_version || '')
  const currentVersion = computed(() => updateInfo.value?.current_version || '')
  const isBeta = computed(() => updateInfo.value?.is_beta === true)

  async function check(force = false, beta?: boolean) {
    checking.value = true
    checkError.value = ''
    try {
      const data = await checkSystemUpdate(force, beta)
      updateInfo.value = data
      return data
    } catch (e: any) {
      checkError.value = e?.message || '检查更新失败'
      return null
    } finally {
      checking.value = false
    }
  }

  async function performUpgrade(targetVersion?: string) {
    upgrading.value = true
    try {
      const res = await onlineUpgradeServer(targetVersion || latestVersion.value)
      return res
    } finally {
      upgrading.value = false
    }
  }

  return {
    updateInfo,
    checking,
    checkError,
    upgrading,
    hasUpdate,
    latestVersion,
    currentVersion,
    isBeta,
    check,
    performUpgrade,
  }
})
