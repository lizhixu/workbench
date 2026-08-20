<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { NBadge, NIcon, NEmpty } from 'naive-ui'
import {
  NotificationsOutline,
  ChevronDownOutline,
  ChevronUpOutline,
  CheckmarkCircleOutline,
  CloseCircleOutline,
} from '@vicons/ionicons5'
import { useHostsStore } from '../../stores/hosts'

export interface MessageEvent {
  id: string
  title: string
  subtitle: string
  time: string
  type: 'online' | 'offline' | 'alert'
}

const collapsed = ref(false)
const store = useHostsStore()

// Derive online/offline events from the real hosts list. There is no
// dedicated event-stream API yet, so we present the current host statuses
// as the message feed and surface a clear empty state when there are no hosts.
const events = computed<MessageEvent[]>(() => {
  return store.hosts.map((h) => ({
    id: h.id,
    title: `${h.status === 'online' ? '主机上线' : '主机离线'} - ${h.hostname}`,
    subtitle: h.group || '默认分组',
    time: (h.last_seen || h.registered || '').slice(0, 19).replace('T', ' ') || '-',
    type: (h.status === 'online' ? 'online' : 'offline') as 'online' | 'offline',
  }))
})

const unreadCount = computed(() => events.value.length)

onMounted(() => {
  if (store.hosts.length === 0) {
    store.fetchList().catch(() => {})
  }
})
</script>

<template>
  <div class="message-list-panel" :class="{ 'is-collapsed': collapsed }">
    <div class="panel-header" @click="collapsed = !collapsed">
      <div class="header-left">
        <NIcon size="16" color="#6366f1"><NotificationsOutline /></NIcon>
        <span class="panel-title">消息列表</span>
        <NBadge :value="unreadCount" :max="99" type="error" />
      </div>
      <div class="header-right">
        <NIcon size="14" class="toggle-icon">
          <ChevronUpOutline v-if="!collapsed" />
          <ChevronDownOutline v-else />
        </NIcon>
      </div>
    </div>

    <div v-if="!collapsed" class="panel-body">
      <NEmpty v-if="events.length === 0" size="small" description="暂无主机状态消息" />
      <div v-for="item in events" :key="item.id" class="event-item">
        <div class="item-dot" :class="item.type">
          <NIcon size="10">
            <CheckmarkCircleOutline v-if="item.type === 'online'" />
            <CloseCircleOutline v-else />
          </NIcon>
        </div>
        <div class="item-content">
          <div class="item-title">{{ item.title }}</div>
          <div class="item-sub">
            <span>{{ item.subtitle }}</span>
            <span class="item-time">{{ item.time }}</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.message-list-panel {
  border-radius: 8px;
  border: 1px solid var(--border-color);
  background-color: var(--bg-card);
  overflow: hidden;
  transition: all 0.2s ease;

  .panel-header {
    height: 42px;
    padding: 0 12px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    cursor: pointer;
    border-bottom: 1px solid var(--border-color);

    .header-left {
      display: flex;
      align-items: center;
      gap: 8px;

      .panel-title {
        font-size: 13px;
        font-weight: 600;
        color: var(--text-primary);
      }
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 6px;
    }
  }

  .panel-body {
    padding: 8px 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    max-height: 260px;
    overflow-y: auto;

    &::-webkit-scrollbar {
      width: 4px;
    }
    &::-webkit-scrollbar-thumb {
      background: var(--border-color);
      border-radius: 2px;
    }
  }

  .event-item {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    padding: 6px 4px;
    font-size: 12px;
    border-bottom: 1px dashed var(--border-dashed);

    &:last-child {
      border-bottom: none;
    }

    .item-dot {
      width: 16px;
      height: 16px;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
      margin-top: 2px;

      &.online {
        background-color: rgba(16, 185, 129, 0.15);
        color: #10b981;
      }

      &.offline {
        background-color: rgba(239, 68, 68, 0.15);
        color: #ef4444;
      }
    }

    .item-content {
      flex: 1;
      min-width: 0;

      .item-title {
        font-weight: 500;
        line-height: 1.3;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }

      .item-sub {
        display: flex;
        justify-content: space-between;
        font-size: 11px;
        color: #9ca3af;
        margin-top: 2px;
      }
    }
  }
}
</style>