<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { NButton, NIcon, NPopconfirm, useMessage } from 'naive-ui'
import {
  PowerOutline,
  ChevronUpOutline,
  ChevronDownOutline,
  ArrowUpCircleOutline,
} from '@vicons/ionicons5'
import OsLogo from '../common/OsLogo.vue'
import { execCommand, upgradeAgent } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import type { Host } from '../../api/types'
import type { NetworkNode } from '../../api/network'
import { copyToClipboard } from '../../utils/clipboard'
import { useAuthStore } from '../../stores/auth'

const props = defineProps<{
  host: Host
}>()
const emit = defineEmits<{
  (e: 'refresh'): void
}>()

const message = useMessage()
const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')
const expanded = ref(true)
const powering = ref(false)
const upgrading = ref(false)
const tailscaleIp = ref<string>('')

async function fetchTailscaleIp() {
  try {
    const nodes = await listNetworkNodes()
    const found = nodes.find((n: NetworkNode) => n.host_id === props.host.id)
    if (found && found.online && found.ip) {
      tailscaleIp.value = found.ip
    } else {
      tailscaleIp.value = ''
    }
  } catch {
    tailscaleIp.value = ''
  }
}

onMounted(fetchTailscaleIp)
watch(() => props.host.id, fetchTailscaleIp)

async function copyTailscaleIp() {
  if (!tailscaleIp.value) return
  const ok = await copyToClipboard(tailscaleIp.value)
  if (ok) {
    message.success(`已复制异地组网虚拟 IP (${tailscaleIp.value})`)
  } else {
    message.warning('复制失败')
  }
}

const distro = computed(() => props.host.distro || props.host.os || '')

function formatMem(bytes?: number): string {
  if (!bytes || bytes <= 0) return '-'
  const gb = bytes / (1024 * 1024 * 1024)
  if (gb >= 1) {
    return gb.toFixed(1) + ' GB'
  }
  const mb = bytes / (1024 * 1024)
  return Math.round(mb) + ' MB'
}

const cpuCoresDisplay = computed(() => {
  if (props.host.cpu_cores) {
    return `${props.host.cpu_cores} 核`
  }
  return '-'
})

const ipDisplay = computed(() => {
  const pub = props.host.public_ip
  const priv = props.host.internal_ip
  if (pub && priv && pub !== priv) {
    return `${pub} / ${priv}`
  }
  return pub || priv || '-'
})

async function copyIp() {
  const text = props.host.public_ip || props.host.internal_ip
  if (!text) {
    message.warning('无可用 IP 地址')
    return
  }
  const ok = await copyToClipboard(text)
  if (ok) {
    message.success(`已复制 IP 地址 (${text}) 到剪贴板`)
  } else {
    message.error('复制失败，请手动选中复制')
  }
}

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

	async function handleUpgradeAgent() {
	  if (props.host.status !== 'online') {
	    message.warning('主机已离线，无法下发在线升级指令')
	    return
	  }
	  upgrading.value = true
	  message.info('正在向 Agent 下发热升级任务，下载并替换二进制中…')
	  try {
	    const res = await upgradeAgent(props.host.id)
	    message.success(res.message || 'Agent 升级成功，正在重启自愈连线！')
	    emit('refresh')
	  } catch (e: any) {
	    message.error(e.message || 'Agent 升级失败')
	  } finally {
	    upgrading.value = false
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
          <div class="host-name-row">
            <span class="host-name">{{ host.hostname }}</span>
            <span v-if="host.location" class="location-tag">{{ host.location }}</span>
          </div>
          <div class="host-os">{{ distro || '-' }}</div>
        </div>
      </div>

      <!-- Center Specifications (IP / CPU / Arch / Memory / Agent / Group) -->
      <div v-if="expanded" class="host-specs-grid">
        <div class="spec-column">
          <div class="spec-row">
            <span class="spec-label">IP 地址</span>
            <span
              class="spec-value mono-font copyable-ip"
              :class="{ 'is-clickable': ipDisplay !== '-' }"
              :title="ipDisplay !== '-' ? '点击复制 IP 地址' : ''"
              @click="copyIp"
            >
              {{ ipDisplay }}
            </span>
          </div>
          <div class="spec-row">
            <span class="spec-label">异地组网</span>
            <span v-if="tailscaleIp" class="spec-value mono-font copyable-ip is-clickable text-emerald" title="点击复制 Tailscale 虚拟 IP" @click="copyTailscaleIp">
              {{ tailscaleIp }}
            </span>
            <span v-else class="spec-value text-muted" style="font-size: 12px">未连接</span>
          </div>
        </div>

        <div class="spec-column">
          <div class="spec-row">
            <span class="spec-label">CPU</span>
            <span class="spec-value" :title="host.cpu_model ? `处理器型号: ${host.cpu_model}` : ''">{{ cpuCoresDisplay }}</span>
          </div>
          <div class="spec-row">
            <span class="spec-label">内存</span>
            <span class="spec-value">{{ formatMem(host.mem_total) }}</span>
          </div>
        </div>

        <div class="spec-column">
          <div class="spec-row">
            <span class="spec-label">Agent</span>
            <div class="agent-version-wrap">
              <span class="spec-value mono-font">{{ host.agent_version || '-' }}</span>
              <NPopconfirm v-if="isAdmin && host.status === 'online'" @positive-click="handleUpgradeAgent">
                <template #trigger>
                  <button
                    class="upgrade-agent-btn"
                    :class="{ 'is-loading': upgrading }"
                    :disabled="upgrading"
                    title="一键热升级 Agent 到控制端最新版本"
                  >
                    <NIcon size="13" :component="ArrowUpCircleOutline" />
                    <span>{{ upgrading ? '升级中' : '升级' }}</span>
                  </button>
                </template>
                确认将主机 {{ host.hostname }} 的 Agent 升级至控制端最新版本？<br/>
                升级过程将下载适配该系统架构的二进制，校验并平滑重启服务。
              </NPopconfirm>
            </div>
          </div>
          <div class="spec-row">
            <span class="spec-label">所属分组</span>
            <span class="spec-value">{{ host.group || '默认分组' }}</span>
          </div>
        </div>
      </div>

      <!-- Right Status & Power Control -->
      <div class="host-status-box">
        <div class="status-indicator-box">
          <div class="uptime-badge" :class="{ offline: host.status !== 'online' }">
            <span class="status-dot" :class="host.status"></span>
            {{ host.status === 'online' ? `已开机 ${uptimeText}` : '当前已离线' }}
          </div>
          <div v-if="host.load1 !== undefined && host.status === 'online'" class="load-badge">
            负载: {{ host.load1 }}
          </div>
        </div>

        <NPopconfirm @positive-click="handlePowerOff">
          <template #trigger>
            <NButton circle quaternary size="medium" class="power-btn" :loading="powering" title="电源控制">
              <template #icon>
                <NIcon size="20" color="#9ca3af"><PowerOutline /></NIcon>
              </template>
            </NButton>
          </template>
          确认向受控主机发送电源关机指令？
        </NPopconfirm>

        <button class="expand-toggle-btn" :title="expanded ? '收起规格信息' : '展开规格信息'" @click="expanded = !expanded">
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
    flex-shrink: 0;

    .host-title-box {
      .host-name-row {
        display: flex;
        align-items: center;
        gap: 8px;

        .host-name {
          font-size: 16px;
          font-weight: 700;
          line-height: 1.2;
          color: var(--text-primary);
        }

        .location-tag {
          font-size: 11px;
          padding: 1px 6px;
          border-radius: 4px;
          background: rgba(99, 102, 241, 0.1);
          color: #6366f1;
          font-weight: 500;
        }
      }

      .host-os {
        font-size: 12px;
        color: var(--text-secondary);
        margin-top: 3px;
      }
    }
  }

  .host-specs-grid {
    display: flex;
    align-items: center;
    gap: 32px;
    font-size: 13px;
    flex: 1;
    justify-content: center;

    .spec-column {
      display: flex;
      flex-direction: column;
      gap: 4px;
    }

    .spec-row {
      display: flex;
      align-items: center;
      gap: 10px;

      .spec-label {
        color: var(--text-secondary);
        width: 52px;
        flex-shrink: 0;
      }

      .spec-value {
        font-weight: 500;
        color: var(--text-primary);
        white-space: nowrap;

        &.mono-font {
          font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
        }

        &.copyable-ip.is-clickable {
          cursor: pointer;
          transition: color 0.15s ease, background-color 0.15s ease;
          border-radius: 4px;
          padding: 1px 4px;
          margin-left: -4px;

          &:hover {
            color: #6366f1;
            background-color: rgba(99, 102, 241, 0.08);
          }
        }

        &.text-emerald {
          color: #10b981;
          font-weight: 600;

          &:hover {
            color: #059669;
            background-color: rgba(16, 185, 129, 0.08);
          }
        }

        &.cpu-value {
          max-width: 220px;
          overflow: hidden;
          text-overflow: ellipsis;
        }
      }

      .agent-version-wrap {
        display: flex;
        align-items: center;
        gap: 6px;

        .upgrade-agent-btn {
          display: inline-flex;
          align-items: center;
          gap: 2px;
          padding: 1px 6px;
          border-radius: 4px;
          border: 1px solid rgba(99, 102, 241, 0.3);
          background: rgba(99, 102, 241, 0.1);
          color: #6366f1;
          font-size: 11px;
          font-weight: 500;
          cursor: pointer;
          transition: all 0.15s ease;
          outline: none;

          &:hover:not(:disabled) {
            background: #6366f1;
            color: #ffffff;
          }

          &.is-loading,
          &:disabled {
            opacity: 0.6;
            cursor: not-allowed;
          }
        }
      }
    }
  }

  .host-status-box {
    display: flex;
    align-items: center;
    gap: 14px;
    flex-shrink: 0;

    .status-indicator-box {
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 2px;

      .uptime-badge {
        font-size: 13px;
        color: #10b981;
        font-weight: 500;
        display: flex;
        align-items: center;
        gap: 6px;

        .status-dot {
          width: 6px;
          height: 6px;
          border-radius: 50%;
          background: #10b981;

          &.offline {
            background: var(--text-muted);
          }
        }

        &.offline {
          color: var(--text-muted);
        }
      }

      .load-badge {
        font-size: 11px;
        color: var(--text-secondary);
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

/* ===================== 移动端适配 ===================== */
@media (max-width: 1024px) {
  .host-header-banner {
    .host-specs-grid {
      gap: 16px;

      .spec-row .spec-value.cpu-value {
        max-width: 140px;
      }
    }
  }
}

@media (max-width: 768px) {
  .host-header-banner {
    padding: 10px 12px;
    margin-bottom: 8px;

    .banner-main-row {
      flex-wrap: wrap;
      gap: 10px;
    }

    .host-specs-grid {
      order: 3;
      flex: 1 1 100%;
      justify-content: space-between;
      gap: 12px;
      font-size: 12px;
      flex-wrap: wrap;

      .spec-row .spec-label {
        width: 48px;
      }
    }

    .host-status-box {
      margin-left: auto;
      gap: 8px;

      .uptime-badge {
        font-size: 12px;
      }
    }
  }
}
</style>