<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  NCard, NSpace, NTag, NButton, NAlert, NPopconfirm,
  NIcon, useMessage, NDescriptions, NDescriptionsItem, NUpload,
  NModal, NProgress,
} from 'naive-ui'
import {
  RocketOutline, CloudUploadOutline, RefreshOutline, ArrowUpCircleOutline,
  CheckmarkCircleOutline, SyncOutline,
} from '@vicons/ionicons5'
import { uploadServerBinary, restartServer, batchUpgradeAgents, type ServerUpgradeResult } from '../../api/upgrade'
import { health, listHosts } from '../../api/hosts'
import { useAuthStore } from '../../stores/auth'
import type { Host } from '../../api/types'

const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'admin')
const message = useMessage()

const healthData = ref<any>(null)
const hosts = ref<Host[]>([])
const loading = ref(false)

// 离线上传升级控制端弹窗
const showUploadModal = ref(false)
const uploadRef = ref<any>(null)
const uploadFile = ref<File | null>(null)
const uploading = ref(false)
const uploadPercent = ref(0)
const uploadResult = ref<ServerUpgradeResult | null>(null)

// 重启控制端
const restarting = ref(false)

// 批量升级 Agent
const batchUpgrading = ref(false)

// 统计过时的在线 Agent 列表
const currentAgentVersion = computed(() => healthData.value?.agent_latest_version || '0.1.0-dev')

const outdatedHosts = computed(() => {
  const targetVer = currentAgentVersion.value
  return hosts.value.filter(
    (h) => h.status === 'online' && h.agent_version && h.agent_version !== targetVer,
  )
})

const onlineHosts = computed(() => hosts.value.filter((h) => h.status === 'online'))

// 控制端运行环境取自 /system/health，避免展示硬编码的假数据
const runtimeEnv = computed(() => {
  const os = healthData.value?.os
  const arch = healthData.value?.arch
  if (!os && !arch) return '未知'
  return [os, arch].filter(Boolean).join(' / ')
})

const serverHealthy = computed(() => healthData.value?.ok === true)

async function refreshData() {
  loading.value = true
  try {
    const [hRes, hList] = await Promise.all([
      health().catch(() => null),
      listHosts().catch(() => ({ data: [] })),
    ])
    healthData.value = hRes
    hosts.value = hList.data || []
  } finally {
    loading.value = false
  }
}

function onFileChange(data: { fileList: any[] }) {
  if (data.fileList.length > 0) {
    uploadFile.value = data.fileList[0].file as File
  } else {
    uploadFile.value = null
  }
}

async function doUploadServer() {
  if (!uploadFile.value) {
    message.warning('请选择需要上传的控制端可执行文件二进制')
    return
  }
  uploading.value = true
  uploadPercent.value = 0
  uploadResult.value = null
  try {
    const res = await uploadServerBinary(uploadFile.value, (pct) => {
      uploadPercent.value = pct
    })
    uploadResult.value = res
    message.success(res.message || '控制端二进制上传替换成功！')
    uploadFile.value = null
    uploadRef.value?.clear?.()
  } catch (e: any) {
    message.error(e.message || '上传更新失败')
  } finally {
    uploading.value = false
  }
}

// 轮询 /system/health 直到控制端重新可用，避免用固定 setTimeout 赌重启耗时。
async function waitForServerBack(timeoutMs = 45000) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    try {
      const h = await health()
      if (h?.ok) return true
    } catch {
      // 重启中，连接失败属预期
    }
    await new Promise((r) => setTimeout(r, 2000))
  }
  return false
}

async function doRestartServer() {
  restarting.value = true
  try {
    try {
      const res = await restartServer()
      message.success(res.message || '已触发平滑重载！')
    } catch {
      // 服务可能在返回响应前就已断开，忽略并继续等待其恢复
    }
    showUploadModal.value = false
    const back = await waitForServerBack()
    if (back) {
      message.success('控制端已重新上线')
      await refreshData()
    } else {
      message.warning('控制端在预期时间内未恢复，请检查服务状态')
    }
  } finally {
    restarting.value = false
  }
}

async function doBatchUpgradeAgents(force = false) {
  batchUpgrading.value = true
  try {
    // 不指定 host_ids 时由服务端挑选：force=false 只选版本过旧的，
    // force=true 重推全部在线主机（开发版版本号相同时用）。
    const res = await batchUpgradeAgents([], force)
    message.success(res.message || '已成功下发批量升级任务')
    setTimeout(() => {
      refreshData()
      batchUpgrading.value = false
    }, 5000)
  } catch (e: any) {
    message.error(e.message || '批量升级失败')
    batchUpgrading.value = false
  }
}

onMounted(() => {
  refreshData()
})
</script>

<template>
  <NCard :bordered="false" size="small" class="system-upgrade-card">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px" color="#6366f1"><RocketOutline /></NIcon>
        版本与系统维护 / 在线与离线升级
      </span>
    </template>
    <template #header-extra>
      <NSpace align="center" :size="8">
        <NButton size="small" quaternary :loading="loading" @click="refreshData">
          <template #icon><NIcon><RefreshOutline /></NIcon></template>
          刷新
        </NButton>
      </NSpace>
    </template>

    <div class="upgrade-container">
      <!-- 顶部基础指标 -->
      <NDescriptions :columns="3" label-placement="left" class="version-desc-grid">
        <NDescriptionsItem label="控制端当前版本">
          <span class="mono-font highlight">{{ healthData?.version || '0.1.0-dev' }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="内置 Agent 最新版">
          <span class="mono-font">{{ currentAgentVersion }}</span>
        </NDescriptionsItem>
        <NDescriptionsItem label="控制端运行环境">
          <span class="mono-font">{{ runtimeEnv }}</span>
        </NDescriptionsItem>
      </NDescriptions>

      <div class="upgrade-sections-row">
        <!-- 模块 1：控制端服务自升级 (Server) -->
        <div class="upgrade-col">
          <div class="section-box">
            <div class="section-header">
              <div class="title-wrap">
                <span class="sec-title">控制端服务 (Watchman Server)</span>
                <NTag
                  size="small"
                  :type="serverHealthy ? 'success' : 'warning'"
                  :bordered="false"
                  round
                >
                  {{ serverHealthy ? '运行正常' : '健康状态未知' }}
                </NTag>
                <NTag v-if="healthData" size="small" :bordered="false" round>
                  {{ healthData.online_agents ?? 0 }} / {{ healthData.agents ?? 0 }} 台在线
                </NTag>
              </div>
              <p class="sec-desc">
                支持上传最新编译的 <code>watchman-server</code> 单二进制进行原子安全替换，替换后系统自动下发维护预告帧并支持一键平滑重启。
              </p>
            </div>

            <div class="sec-actions">
              <NSpace :size="10">
                <NButton
                  v-if="isAdmin"
                  type="primary"
                  secondary
                  size="small"
                  @click="showUploadModal = true"
                >
                  <template #icon><NIcon :component="CloudUploadOutline" /></template>
                  离线上传新二进制升级
                </NButton>

                <NPopconfirm v-if="isAdmin" @positive-click="doRestartServer">
                  <template #trigger>
                    <NButton size="small" :loading="restarting">
                      <template #icon><NIcon :component="SyncOutline" /></template>
                      平滑重启服务 (维护握手模式)
                    </NButton>
                  </template>
                  确认平滑重启控制端服务？<br />
                  系统将在关机前向全网在线 Agent 广播维护通知，并在重启后的维护窗口内自动消除误报。
                </NPopconfirm>
              </NSpace>
            </div>
          </div>
        </div>

        <!-- 模块 2：被管端 Agent 全网批量升级 (Agents) -->
        <div class="upgrade-col">
          <div class="section-box">
            <div class="section-header">
              <div class="title-wrap">
                <span class="sec-title">被管端 Agent (全网批量维护)</span>
                <NTag v-if="outdatedHosts.length > 0" size="small" type="warning" :bordered="false" round>
                  {{ outdatedHosts.length }} 台需升级
                </NTag>
                <NTag v-else size="small" type="success" :bordered="false" round>
                  全部 Agent 最新
                </NTag>
              </div>
              <p class="sec-desc">
                自动比对全网已纳管主机的 Agent 版本。支持一键并发批量下发自升级指令，并自动开启 3 分钟维护静默期，平滑升级零假告警。
              </p>
            </div>

            <div class="sec-actions">
              <NSpace :size="10" align="center">
                <NPopconfirm
                  v-if="isAdmin && outdatedHosts.length > 0"
                  @positive-click="doBatchUpgradeAgents(false)"
                >
                  <template #trigger>
                    <NButton type="primary" size="small" :loading="batchUpgrading">
                      <template #icon><NIcon :component="ArrowUpCircleOutline" /></template>
                      一键批量升级过时 Agent ({{ outdatedHosts.length }}台)
                    </NButton>
                  </template>
                  确认向以下 {{ outdatedHosts.length }} 台版本偏旧的主机下发在线升级任务？<br />
                  <span class="text-secondary" style="font-size: 12px">
                    {{ outdatedHosts.map((h) => h.hostname).join(', ') }}
                  </span><br />
                  升级过程已自动配置专属维护静默期，不会触发离线与上线告警通知。
                </NPopconfirm>

                <NPopconfirm
                  v-if="isAdmin && outdatedHosts.length === 0 && onlineHosts.length > 0"
                  @positive-click="doBatchUpgradeAgents(true)"
                >
                  <template #trigger>
                    <NButton size="small" :loading="batchUpgrading">
                      <template #icon><NIcon :component="ArrowUpCircleOutline" /></template>
                      强制重推升级 ({{ onlineHosts.length }}台)
                    </NButton>
                  </template>
                  所有在线主机均已是版本 {{ currentAgentVersion }}，仍要强制重新推送一次升级？<br />
                  适用于开发版（版本号相同但需要重刷二进制）的场景。
                </NPopconfirm>

                <span v-if="onlineHosts.length === 0" class="muted-hint">当前没有在线主机</span>
                <span v-else-if="outdatedHosts.length === 0" class="muted-hint">
                  <NIcon :component="CheckmarkCircleOutline" color="#10b981" style="vertical-align: middle; margin-right: 4px;" />
                  当前所有在线主机已运行最新版本 Agent
                </span>
              </NSpace>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 上传控制端新版本二进制 Modal -->
    <NModal
      v-model:show="showUploadModal"
      preset="card"
      title="离线上传控制端新二进制 (Self-Upgrade)"
      style="width: 560px; max-width: 92vw"
    >
      <div class="upload-dialog-content">
        <NAlert type="info" :bordered="false" style="margin-bottom: 14px">
          请选择为您当前控制端架构编译的最新版 <code>watchman-server</code> 可执行文件。<br />
          上传后服务端将进行原子备份与覆盖替换，并广播维护预告，保护全网 0 误报。
        </NAlert>

        <NUpload
          ref="uploadRef"
          :max="1"
          :default-upload="false"
          accept="*"
          @change="onFileChange"
        >
          <NButton secondary block type="primary">
            <template #icon><NIcon :component="CloudUploadOutline" /></template>
            选择新版 watchman-server 二进制
          </NButton>
        </NUpload>

        <div v-if="uploadFile" class="file-picked-info">
          已选择文件: <code>{{ uploadFile.name }}</code> ({{ (uploadFile.size / (1024 * 1024)).toFixed(2) }} MB)
        </div>

        <NProgress
          v-if="uploading"
          type="line"
          :percentage="uploadPercent"
          indicator-placement="inside"
          style="margin-top: 12px"
        />

        <NAlert v-if="uploadResult" type="success" :bordered="false" style="margin-top: 14px">
          {{ uploadResult.message }}<br />
          <span style="font-size: 12px; color: var(--text-secondary)">备份文件路径: {{ uploadResult.bak_path }}</span>
        </NAlert>
      </div>

      <template #footer>
        <NSpace justify="space-between" align="center">
          <NButton v-if="uploadResult?.ok" type="warning" :loading="restarting" @click="doRestartServer">
            立即平滑重启生效
          </NButton>
          <span v-else></span>

          <NSpace>
            <NButton @click="showUploadModal = false">取消</NButton>
            <NButton
              type="primary"
              :loading="uploading"
              :disabled="!uploadFile"
              @click="doUploadServer"
            >
              开始上传替换
            </NButton>
          </NSpace>
        </NSpace>
      </template>
    </NModal>
  </NCard>
</template>

<style scoped lang="scss">
.system-upgrade-card {
  .upgrade-container {
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .version-desc-grid {
    background: var(--n-color-modal);
    padding: 10px 16px;
    border-radius: 6px;
    border: 1px solid var(--n-border-color);

    .highlight {
      color: var(--primary-color, #6366f1);
      font-weight: 700;
    }
  }

  .upgrade-sections-row {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 14px;

    @media (max-width: 900px) {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  .section-box {
    background: var(--n-color-modal);
    border: 1px solid var(--n-border-color);
    border-radius: 6px;
    padding: 14px;
    display: flex;
    flex-direction: column;
    justify-content: space-between;
    height: 100%;
    box-sizing: border-box;

    .section-header {
      .title-wrap {
        display: flex;
        align-items: center;
        gap: 8px;
        margin-bottom: 6px;

        .sec-title {
          font-weight: 700;
          font-size: 14px;
        }
      }

      .sec-desc {
        margin: 0 0 12px;
        font-size: 12px;
        color: var(--n-text-color-3);
        line-height: 1.5;
      }
    }

    .sec-actions {
      display: flex;
      align-items: center;

      .muted-hint {
        font-size: 12px;
        color: var(--n-text-color-3);
      }
    }
  }

  .upload-dialog-content {
    display: flex;
    flex-direction: column;

    .file-picked-info {
      margin-top: 10px;
      font-size: 12.5px;
      color: var(--text-secondary);
    }
  }
}
</style>
