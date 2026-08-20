import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Host } from '../api/types'
import { listHosts as apiListHosts } from '../api/hosts'

export const useHostsStore = defineStore('hosts', () => {
  const hosts = ref<Host[]>([])
  const loading = ref(false)

  async function fetchList() {
    loading.value = true
    try {
      const res = await apiListHosts()
      hosts.value = res.data
    } finally {
      loading.value = false
    }
  }

  return { hosts, loading, fetchList }
})