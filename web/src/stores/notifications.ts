import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import {
  listEvents,
  ackEvent,
  ackAllEvents,
  deleteEvent,
  clearEvents,
  type AlertEvent,
} from '../api/alerts'

export const useNotificationStore = defineStore('notifications', () => {
  const events = ref<AlertEvent[]>([])
  const loading = ref(false)
  let pollTimer: any = null

  const unreadCount = computed(() => {
    return events.value.filter((e) => !e.resolved).length
  })

  async function fetchEvents() {
    try {
      loading.value = true
      const data = await listEvents()
      events.value = data || []
    } catch {
      // ignore in background polling
    } finally {
      loading.value = false
    }
  }

  async function markAsRead(id: string) {
    await ackEvent(id)
    const target = events.value.find((e) => e.id === id)
    if (target) {
      target.resolved = true
      target.resolved_at = new Date().toISOString()
    }
  }

  async function markAllAsRead() {
    await ackAllEvents()
    const now = new Date().toISOString()
    for (const e of events.value) {
      e.resolved = true
      e.resolved_at = now
    }
  }

  async function removeEvent(id: string) {
    await deleteEvent(id)
    events.value = events.value.filter((e) => e.id !== id)
  }

  async function clearAll(resolvedOnly = false) {
    await clearEvents(resolvedOnly)
    if (resolvedOnly) {
      events.value = events.value.filter((e) => !e.resolved)
    } else {
      events.value = []
    }
  }

  function startPolling(intervalMs = 15000) {
    if (pollTimer) return
    fetchEvents()
    pollTimer = setInterval(() => {
      fetchEvents()
    }, intervalMs)
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  }

  return {
    events,
    loading,
    unreadCount,
    fetchEvents,
    markAsRead,
    markAllAsRead,
    removeEvent,
    clearAll,
    startPolling,
    stopPolling,
  }
})
