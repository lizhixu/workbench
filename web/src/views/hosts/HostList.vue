<script setup lang="ts">
import { onMounted, ref, computed, h } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NSpace,
  NInput,
  NModal,
  NCode,
  NSpin,
  NDropdown,
  useMessage,
  useDialog,
  NIcon,
} from 'naive-ui'
import {
  AddCircleOutline,
  RefreshOutline,
  SearchOutline,
  DesktopOutline,
  PaperPlaneOutline,
  EllipsisHorizontalCircleOutline,
  TerminalOutline,
  FolderOutline,
  CubeOutline,
  TrashOutline,
  PulseOutline,
  SparklesOutline,
  DocumentTextOutline,
} from '@vicons/ionicons5'
import OsLogo from '../../components/common/OsLogo.vue'
import { useHostsStore } from '../../stores/hosts'
import { useWorkspaceStore } from '../../stores/workspace'
import { enroll, deleteHost } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import type { Host } from '../../api/types'

const router = useRouter()
const store = useHostsStore()
const workspace = useWorkspaceStore()
const message = useMessage()
const dialog = useDialog()

const search = ref('')
const showEnroll = ref(false)
const enrollToken = ref('')
const enrollCmd = ref('')
const enrolling = ref(false)

const filteredHosts = computed(() => {
  const q = search.value.trim().toLowerCase()
  return store.hosts.filter((h) => {
    if (!q) return true
    return (
      h.hostname.toLowerCase().includes(q) ||
      h.id.toLowerCase().includes(q) ||
      (h.os || '').toLowerCase().includes(q) ||
      (h.distro || '').toLowerCase().includes(q) ||
      (h.internal_ip || '').toLowerCase().includes(q) ||
      (h.public_ip || '').toLowerCase().includes(q) ||
      (h.group || '').toLowerCase().includes(q)
    )
  })
})

function formatArch(arch?: string): string {
  const a = (arch || '').toLowerCase()
  if (a === 'amd64' || a === 'x86_64' || a === 'x64') return 'x64'
  if (a === 'arm64' || a === 'aarch64') return 'arm64'
  if (a === '386' || a === 'x86') return 'x86'
  return arch || 'x64'
}

function formatMem(bytes?: number): string {
  if (!bytes || bytes <= 0) return '-'
  const gb = bytes / (1024 * 1024 * 1024)
  if (gb >= 1) {
    return gb.toFixed(1) + ' GB'
  }
  const mb = bytes / (1024 * 1024)
  return Math.round(mb) + ' MB'
}

function fmtUptime(row: Host): string {
  let secs = 0
  if (row.uptime && row.uptime > 0) {
    secs = row.uptime
  } else {
    const end = row.status === 'online' ? Date.now() : new Date(row.last_seen || row.registered).getTime()
    const start = new Date(row.registered || row.last_seen || end).getTime()
    secs = Math.max(0, Math.floor((end - start) / 1000))
  }
  if (secs < 60) return `${Math.max(1, secs)} 秒`
  const m = Math.floor(secs / 60)
  if (m < 60) return `${m} 分钟`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时 ${m % 60} 分`
  const d = Math.floor(h / 24)
  if (d >= 30) {
    const months = Math.floor(d / 30)
    const remDays = d % 30
    return `${months} 月 ${remDays} 天`
  }
  return `${d} 天 ${h % 24} 小时`
}

function goDetail(host: Host, tab = 'metrics') {
  workspace.openTab({
    key: `/hosts/${host.id}`,
    title: host.hostname,
    path: `/hosts/${host.id}`,
    closable: true,
  })
  router.push({ path: `/hosts/${host.id}`, query: { tab } })
}

function goExec() {
  router.push('/exec')
}

function goAiChat() {
  workspace.openTab({
    key: '/ai/chat',
    title: 'AI 助手',
    path: '/ai/chat',
    closable: true,
  })
  router.push('/ai/chat')
}

function goAiReport() {
  workspace.openTab({
    key: '/ai/report',
    title: '运维报告',
    path: '/ai/report',
    closable: true,
  })
  router.push('/ai/report')
}

function handleMenuSelect(key: string, host: Host) {
  if (key === 'detail') {
    goDetail(host, 'metrics')
  } else if (key === 'terminal') {
    goDetail(host, 'terminal')
  } else if (key === 'files') {
    goDetail(host, 'files')
  } else if (key === 'docker') {
    goDetail(host, 'docker')
  } else if (key === 'exec') {
    router.push('/exec')
  } else if (key === 'unbind') {
    dialog.warning({
      title: '解绑主机确认',
      content: `确定要解绑主机 "${host.hostname}" (${host.id}) 吗？解绑后该主机将从控制台移除。`,
      positiveText: '确认解绑',
      negativeText: '取消',
      onPositiveClick: async () => {
        try {
          await deleteHost(host.id)
          message.success('已成功解绑主机')
          await store.fetchList()
        } catch (e: any) {
          message.error(e.message || '解绑失败')
        }
      },
    })
  }
}

const menuOptions = [
  {
    label: '运维监控',
    key: 'detail',
    icon: () => h(NIcon, null, { default: () => h(PulseOutline) }),
  },
  {
    label: '在线终端',
    key: 'terminal',
    icon: () => h(NIcon, null, { default: () => h(TerminalOutline) }),
  },
  {
    label: '文件管理',
    key: 'files',
    icon: () => h(NIcon, null, { default: () => h(FolderOutline) }),
  },
  {
    label: 'Docker 管理',
    key: 'docker',
    icon: () => h(NIcon, null, { default: () => h(CubeOutline) }),
  },
  {
    label: '推送命令',
    key: 'exec',
    icon: () => h(NIcon, null, { default: () => h(PaperPlaneOutline) }),
  },
  {
    type: 'divider',
    key: 'd1',
  },
  {
    label: '解绑主机',
    key: 'unbind',
    icon: () => h(NIcon, { color: '#ef4444' }, { default: () => h(TrashOutline) }),
  },
]

async function openEnroll() {
  showEnroll.value = true
  enrolling.value = true
  try {
    const res = await enroll()
    enrollToken.value = res.enroll_token
    const proto = window.location.protocol || 'http:'
    const host = window.location.hostname || 'localhost'
    const port = window.location.port ? `:${window.location.port}` : ''
    enrollCmd.value = res.install || `curl -kfsSL '${proto}//${host}${port}/install?token=${res.enroll_token}' | sudo bash`
  } catch (e: any) {
    message.error(e.message || '获取安装脚本失败')
  } finally {
    enrolling.value = false
  }
}

async function copyCmd() {
  const ok = await copyToClipboard(enrollCmd.value)
  if (ok) {
    message.success('已复制安装脚本命令到剪贴板')
  } else {
    message.error('复制失败，请手动选中文本复制')
  }
}

async function refresh() {
  await store.fetchList()
}

onMounted(() => {
  workspace.setActiveKey('/hosts')
  store.fetchList().catch((e) => message.error(e.message))
})
</script>

<template>
  <div class="baichuan-host-list-view">
    <div class="main-table-area">
      <!-- 顶部工具栏 Toolbar -->
      <div class="toolbar-bar">
        <div class="toolbar-left">
          <NInput
            v-model:value="search"
            placeholder="快速搜索"
            clearable
            size="small"
            style="width: 240px"
          >
            <template #prefix>
              <NIcon :component="SearchOutline" />
            </template>
          </NInput>
          <span class="total-count-text">共 {{ store.hosts.length }} 台主机</span>
        </div>

        <div class="toolbar-right">
          <NButton type="primary" size="small" @click="openEnroll">
            <template #icon>
              <NIcon :component="AddCircleOutline" />
            </template>
            绑定主机
          </NButton>

          <NButton secondary type="primary" size="small" @click="goExec">
            <template #icon>
              <NIcon :component="PaperPlaneOutline" />
            </template>
            推送命令
          </NButton>

          <NButton secondary type="info" size="small" @click="goAiChat">
            <template #icon>
              <NIcon :component="SparklesOutline" />
            </template>
            AI 助手
          </NButton>

          <NButton secondary type="info" size="small" @click="goAiReport">
            <template #icon>
              <NIcon :component="DocumentTextOutline" />
            </template>
            运维报告
          </NButton>

          <NButton quaternary size="small" @click="refresh" :loading="store.loading">
            <template #icon>
              <NIcon :component="RefreshOutline" />
            </template>
          </NButton>
        </div>
      </div>

      <!-- 主机列表卡片容器 -->
      <div class="host-list-container">
        <NSpin v-if="store.loading && store.hosts.length === 0" style="padding: 40px" />

        <div v-else-if="filteredHosts.length === 0" class="empty-placeholder">
          <NIcon :component="DesktopOutline" size="48" style="color: var(--text-tertiary); margin-bottom: 12px;" />
          <div style="color: var(--text-secondary); font-size: 14px;">暂无匹配的主机设备</div>
        </div>

        <div v-else class="host-row-list">
          <div
            v-for="host in filteredHosts"
            :key="host.id"
            class="host-row-item"
            @click="goDetail(host)"
          >
	            <!-- 1. OS Logo 图标 -->
	            <div class="col-os-logo">
	              <OsLogo :os="host.os" :distro="host.distro" :badge-size="38" :size="22" />
	            </div>

            <!-- 2. 主机名称与系统版本 -->
            <div class="col-host-info">
              <div class="host-name-row">
                <span class="host-name">{{ host.hostname }}</span>
              </div>
              <div class="host-distro">
                {{ host.distro || host.os || 'Linux' }}
              </div>
            </div>

            <!-- 3. 开机状态 Pill 标签 -->
            <div class="col-status">
              <div
                class="uptime-pill"
                :class="{ 'is-online': host.status === 'online', 'is-offline': host.status !== 'online' }"
              >
                {{ host.status === 'online' ? `已开机 ${fmtUptime(host)}` : '当前已离线' }}
              </div>
            </div>

            <!-- 4. CPU 与内存规格 -->
            <div class="col-specs">
              <div class="spec-line">
                <span class="spec-label">CPU</span>
                <span class="spec-value">{{ (host.cpu_cores || 1) }} 核 ({{ formatArch(host.arch) }})</span>
              </div>
              <div class="spec-line">
                <span class="spec-label">内存</span>
                <span class="spec-value">{{ formatMem(host.mem_total) }}</span>
              </div>
            </div>

            <!-- 5. 内网与外网 IP + 地理位置 -->
            <div class="col-ips">
              <div class="ip-line">
                <span class="ip-label">内</span>
                <span class="ip-value">{{ host.internal_ip || '-' }}</span>
              </div>
              <div class="ip-line">
                <span class="ip-label">外</span>
                <span class="ip-value">
                  {{ host.public_ip || '-' }}
                  <span v-if="host.location" class="ip-location">({{ host.location }})</span>
                </span>
              </div>
            </div>

            <!-- 6. 右侧更多操作按钮 -->
            <div class="col-actions" @click.stop>
              <NDropdown
                trigger="click"
                placement="bottom-end"
                :options="menuOptions"
                @select="(key: string) => handleMenuSelect(key, host)"
              >
                <button class="more-action-btn" title="更多操作">
                  <NIcon :component="EllipsisHorizontalCircleOutline" size="22" />
                </button>
              </NDropdown>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 绑定主机 Modal -->
    <NModal
      v-model:show="showEnroll"
      preset="card"
      title="添加/绑定主机"
      style="width: 620px"
    >
      <NSpin v-if="enrolling" />
      <template v-else>
        <div class="enroll-modal-content">
          <p class="guide-text">
            在目标 Linux 服务器上执行以下安装指令（含 Token）：
          </p>
          <div class="code-container">
            <NCode :code="enrollCmd" language="bash" word-wrap />
          </div>
          <NSpace justify="end" style="margin-top: 14px">
            <NButton type="primary" @click="copyCmd">复制命令</NButton>
          </NSpace>
        </div>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.baichuan-host-list-view {
  display: flex;
  height: 100%;
  padding: 16px;
  box-sizing: border-box;

  .main-table-area {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 12px;
    background-color: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 16px 20px;
    box-shadow: var(--shadow-sm);

    .toolbar-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding-bottom: 4px;

      .toolbar-left,
      .toolbar-right {
        display: flex;
        align-items: center;
        gap: 12px;
      }

      .total-count-text {
        font-size: 13px;
        color: var(--text-secondary);
      }
    }

    .host-list-container {
      flex: 1;
      overflow-y: auto;

      .empty-placeholder {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        padding: 60px 20px;
      }

      .host-row-list {
        display: flex;
        flex-direction: column;

        .host-row-item {
          display: flex;
          align-items: center;
          padding: 18px 12px;
          border-bottom: 1px solid var(--border-color);
          cursor: pointer;
          transition: background-color 0.15s ease;

          &:hover {
            background-color: var(--hover-bg, rgba(99, 102, 241, 0.04));
          }

          &:last-child {
            border-bottom: none;
          }

          // 1. OS Logo
          .col-os-logo {
            flex: 0 0 54px;
            display: flex;
            align-items: center;
          }

          // 2. 主机名与系统
          .col-host-info {
            flex: 0 0 190px;
            display: flex;
            flex-direction: column;
            gap: 4px;
            padding-right: 12px;

            .host-name-row {
              display: flex;
              align-items: center;

              .host-name {
                font-weight: 600;
                font-size: 15px;
                color: var(--text-primary);
                letter-spacing: 0.2px;
              }
            }

            .host-distro {
              font-size: 12px;
              color: var(--text-secondary);
            }
          }

          // 3. 运行时间 Pill 标签
          .col-status {
            flex: 0 0 170px;
            display: flex;
            align-items: center;
            padding-right: 12px;

            .uptime-pill {
              display: inline-flex;
              align-items: center;
              justify-content: center;
              padding: 4px 14px;
              border-radius: 14px;
              font-size: 12px;
              font-weight: 500;

              &.is-online {
                background-color: #e6f8f0;
                color: #059669;
              }

              &.is-offline {
                background-color: rgba(148, 163, 184, 0.15);
                color: #94a3b8;
              }
            }
          }

          // 4. CPU 与内存规格
          .col-specs {
            flex: 0 0 180px;
            display: flex;
            flex-direction: column;
            gap: 4px;
            font-size: 13px;
            padding-right: 12px;

            .spec-line {
              display: flex;
              align-items: center;

              .spec-label {
                color: var(--text-tertiary, #8c8c8c);
                width: 38px;
                flex-shrink: 0;
              }

              .spec-value {
                color: var(--text-primary);
                font-weight: 500;
              }
            }
          }

          // 5. 内网与外网 IP
          .col-ips {
            flex: 1;
            min-width: 0;
            display: flex;
            flex-direction: column;
            gap: 4px;
            font-size: 13px;
            padding-right: 12px;

            .ip-line {
              display: flex;
              align-items: center;

              .ip-label {
                color: var(--text-tertiary, #8c8c8c);
                width: 24px;
                flex-shrink: 0;
              }

              .ip-value {
                color: var(--text-primary);
                font-weight: 400;
                white-space: nowrap;
                overflow: hidden;
                text-overflow: ellipsis;

                .ip-location {
                  color: var(--text-secondary);
                  margin-left: 4px;
                  font-size: 12px;
                }
              }
            }
          }

          // 6. 更多操作按钮
          .col-actions {
            flex: 0 0 44px;
            display: flex;
            align-items: center;
            justify-content: flex-end;

            .more-action-btn {
              background: transparent;
              border: none;
              cursor: pointer;
              color: var(--text-tertiary, #94a3b8);
              padding: 4px;
              border-radius: 50%;
              display: flex;
              align-items: center;
              justify-content: center;
              transition: color 0.15s, background-color 0.15s;

              &:hover {
                color: var(--primary-color, #6366f1);
                background-color: var(--hover-bg, rgba(99, 102, 241, 0.08));
              }
            }
          }
        }
      }
    }
  }

  .enroll-modal-content {
    display: flex;
    flex-direction: column;
    gap: 10px;

    .code-container {
      background: var(--code-box-bg);
      border: 1px solid var(--border-color);
      padding: 12px;
      border-radius: 6px;
    }
  }
}
</style>
