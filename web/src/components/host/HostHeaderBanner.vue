<script setup lang="ts">
import { ref, computed } from 'vue'
import { NButton, NIcon, NPopconfirm, useMessage } from 'naive-ui'
import {
  PowerOutline,
  ChevronUpOutline,
  ChevronDownOutline,
} from '@vicons/ionicons5'
import OsLogo from '../common/OsLogo.vue'
import { execCommand } from '../../api/hosts'
import type { Host } from '../../api/types'

const props = defineProps<{
  host: Host
}>()

const message = useMessage()
const expanded = ref(true)
const powering = ref(false)

const distro = computed(() => props.host.distro || props.host.os || '')

// Uptime: prefer the real OS uptime (seconds since boot) reported by the
// agent via metrics. Fall back to the registered→now span only when the
// agent hasn't reported a sample yet (e.g. just enrolled, still offline).
const uptimeText = computed(() => {
  const realUptime = props.host.uptime || 0
  let secs = 0
  if (realUptime > 0) {
    secs = realUptime
  } else {
    const end =
      props.host.status === 'online'
        ? Date.now()
        : new Date(props.host.last_seen || props.host.registered).getTime()
    const start = new Date(props.host.registered || props.host.last_seen || end).getTime()
    secs = Math.max(0, Math.floor((end - start) / 1000))
  }
  if (secs < 60) return `${secs}秒`
  const m = Math.floor(secs / 60)
  if (m < 60) return `${m}分钟`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}小时${m % 60}分`
  const d = Math.floor(h / 24)
  return `${d}天${h % 24}小时`
})

async function handlePowerOff() {
  if (props.host.status !== 'online') {
    message.warning('主机离线，无法下发指令')
    return
  }
  powering.value = true
  try {
    // Execute a shutdown command via the real exec API.
    const cmd =
      (props.host.os || '').toLowerCase().includes('windows')
        ? 'shutdown /s /t 0'
        : 'shutdown -h now'
    await execCommand(props.host.id, cmd)
    message.success('已向受控 Agent 下发关机指令')
  } catch (e: any) {
    message.error(e.message || '关机指令下发失败')
  } finally {
    powering.value = false
  }
}
</script>

<template>
  <div class="host-header-banner">
    <div class="banner-main-row">
      <!-- Host Icon & Name & Distro -->
      <div class="host-identity-box">
        <OsLogo :os="host.os" :distro="host.distro" :badge-size="40" :size="24" />
        <div class="host-title-box">
          <div class="host-name">{{ host.hostname }}</div>
          <div class="host-os">{{ distro || '-' }}</div>
        </div>
      </div>

      <!-- Center Specifications (OS / Arch / Agent) -->
      <div v-if="expanded" class="host-specs-grid">
        <div class="spec-column">
          <div class="spec-row">
            <span class="spec-label">OS</span>
            <span class="spec-value">{{ host.os || '-' }}</span>
          </div>
          <div class="spec-row">
            <span class="spec-label">Arch</span>
            <span class="spec-value">{{ host.arch || '-' }}</span>
          </div>
        </div>

        <div class="spec-column">
          <div class="spec-row">
            <span class="spec-label">Agent</span>
            <span class="spec-value mono-font">{{ host.agent_version || '-' }}</span>
          </div>
          <div class="spec-row">
            <span class="spec-label">分组</span>
            <span class="spec-value">{{ host.group || '默认分组' }}</span>
          </div>
        </div>
      </div>

      <!-- Right Status & Power Control -->
      <div class="host-status-box">
        <div class="uptime-badge" :class="{ offline: host.status !== 'online' }">
          {{ host.status === 'online' ? `已开机 ${uptimeText}` : '当前已离线' }}
        </div>

        <NPopconfirm @positive-click="handlePowerOff">
          <template #trigger>
            <NButton circle quaternary size="medium" class="power-btn" :loading="powering">
              <template #icon>
                <NIcon size="20" color="#9ca3af"><PowerOutline /></NIcon>
              </template>
            </NButton>
          </template>
          确认向主机发送电源关机/重启控制指令？
        </NPopconfirm>

        <button class="expand-toggle-btn" @click="expanded = !expanded">
          <NIcon size="14">
            <ChevronUpOutline v-if="expanded" />
            <ChevronDownOutline v-else />
          </NIcon>
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.host-header-banner {
  background-color: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  box-shadow: var(--shadow-sm);
  padding: 12px 20px;
  margin-bottom: 12px;

  .banner-main-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 20px;
  }

  .host-identity-box {
    display: flex;
    align-items: center;
    gap: 14px;

    .host-icon-ring {
      width: 44px;
      height: 44px;
      border-radius: 50%;
      border: 2px solid #f97316;
      display: flex;
      align-items: center;
      justify-content: center;
      background: rgba(249, 115, 22, 0.1);
    }

    .host-title-box {
      .host-name {
        font-size: 16px;
        font-weight: 700;
        line-height: 1.2;
        color: var(--text-primary);
      }
      .host-os {
        font-size: 12px;
        color: var(--text-secondary);
        margin-top: 2px;
      }
    }
  }

  .host-specs-grid {
    display: flex;
    align-items: center;
    gap: 40px;
    font-size: 13px;

    .spec-column {
      display: flex;
      flex-direction: column;
      gap: 4px;
    }

    .spec-row {
      display: flex;
      align-items: center;
      gap: 12px;

      .spec-label {
        color: var(--text-secondary);
        width: 44px;
      }

      .spec-value {
        font-weight: 500;
        color: var(--text-primary);
      }
    }
  }

  .host-status-box {
    display: flex;
    align-items: center;
    gap: 14px;

    .uptime-badge {
      font-size: 13px;
      color: #10b981;
      font-weight: 500;

      &.offline {
        color: var(--text-muted);
      }
    }

    .power-btn {
      &:hover {
        background-color: rgba(239, 68, 68, 0.15);
      }
    }

    .expand-toggle-btn {
      background: transparent;
      border: none;
      color: var(--text-secondary);
      cursor: pointer;
      display: flex;
      align-items: center;
      padding: 4px;
      border-radius: 4px;

      &:hover {
        background-color: var(--bg-hover);
        color: var(--text-primary);
      }
    }
  }
}
</style>