<!-- AppMigrateModal: cross-host migration of a deployed application
     (服务迁移). Picks a target host, runs a read-only precheck against it
     (agent / Docker / port occupancy / S3 readiness), then submits the
     async migration. Progress is followed in the deployment-history tab,
     which the caller switches to on @migrated.
     Used from AppDetail (header button) and AppList (row action). -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  NAlert, NButton, NIcon, NModal, NSelect, NSpace, NSpin, NSwitch, NTag, useMessage,
} from 'naive-ui'
import { ArrowForwardOutline, SwapHorizontalOutline } from '@vicons/ionicons5'

import { migrateApp, precheckMigration, type AppEntity, type MigratePrecheck } from '../../api/apps'
import { listHosts } from '../../api/hosts'
import { listNetworkNodes } from '../../api/network'
import type { Host } from '../../api/types'
import AppTopologyPreview, { type TopologyPort } from './AppTopologyPreview.vue'

const props = defineProps<{
  show: boolean
  app: AppEntity | null
}>()
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  /** 迁移已受理（202）：调用方切到部署历史看进度并刷新列表。 */
  (e: 'migrated'): void
}>()

const message = useMessage()

const hosts = ref<Host[]>([])
const meshIPById = ref<Map<string, string>>(new Map())
const loadingHosts = ref(false)

const targetHostId = ref<string | null>(null)
const precheck = ref<MigratePrecheck | null>(null)
const prechecking = ref(false)
const precheckError = ref('')

const migrateVolumes = ref(false)
const cleanSource = ref(false)
const submitting = ref(false)

const targetHost = computed(() => hosts.value.find((h) => h.id === targetHostId.value) || null)
const appHost = computed(() => hosts.value.find((h) => h.id === props.app?.host_id) || null)
const gatewayHost = computed(() =>
  hosts.value.find((h) => h.id === props.app?.proxy_gateway_host_id) || null)
const forwardHost = computed(() =>
  hosts.value.find((h) => h.id === props.app?.mesh_forward_host_id) || null)

const volumeCount = computed(() => props.app?.volumes?.length ?? 0)
// Compose 应用的卷在编排 YAML 内声明，记录里既没有卷规格也不在白名单内，
// 后端对这类应用的卷迁移明确拒绝，前端不再提供开关。
const composeVolumes = computed(() =>
  props.app?.source_type === 'raw_compose' && volumeCount.value > 0)
const canMigrateVolumes = computed(() =>
  volumeCount.value > 0 && !composeVolumes.value && !!precheck.value?.s3_ready)

// 目标主机下拉：排除当前主机与（不可迁移的）Windows 主机，离线主机置灰垫底。
const hostOptions = computed(() => {
  const list = hosts.value.map((h) => {
    const isSelf = h.id === props.app?.host_id
    const isWindows = h.os === 'windows'
    const mesh = meshIPById.value.get(h.id)
    const tags = [h.os === 'windows' ? 'Windows' : 'Linux']
    if (h.public_ip) tags.push(`公网 ${h.public_ip}`)
    tags.push(mesh ? `组网 ${mesh}` : '未组网')
    if (isSelf) tags.push('当前主机')
    else if (isWindows) tags.push('暂不支持迁移')
    else if (h.status !== 'online') tags.push('离线')
    return {
      label: `${h.hostname}（${tags.join(' · ')}）`,
      value: h.id,
      disabled: isSelf || isWindows || h.status !== 'online',
      rank: isSelf ? 1 : isWindows || h.status !== 'online' ? 2 : 0,
    }
  })
  list.sort((a, b) => a.rank - b.rank)
  return list
})

const ports = computed<TopologyPort[]>(() =>
  (props.app?.ports ?? []).map((p) => ({
    host: p.host,
    container: p.container,
    bind_scope: (p.bind_scope === 'mesh' ? 'mesh' : 'public') as 'public' | 'mesh',
  })))

const targetMeshIP = computed(() =>
  targetHostId.value ? meshIPById.value.get(targetHostId.value) || '' : '')

const proxyMode = computed(() => (props.app?.proxy_mode as 'local' | 'gateway') ?? 'local')

async function loadHosts() {
  loadingHosts.value = true
  try {
    const res = await listHosts()
    hosts.value = res.data ?? []
  } catch (e: any) {
    hosts.value = []
    message.error(e?.message || '获取主机列表失败')
  } finally {
    loadingHosts.value = false
  }
  try {
    const nodes = (await listNetworkNodes()) ?? []
    meshIPById.value = new Map(nodes.filter((n) => n.ip).map((n) => [n.host_id, n.ip]))
  } catch {
    meshIPById.value = new Map()
  }
}

watch(
  () => props.show,
  (v) => {
    if (!v) return
    targetHostId.value = null
    precheck.value = null
    precheckError.value = ''
    migrateVolumes.value = false
    cleanSource.value = false
    submitting.value = false
    loadHosts()
  },
)

watch(targetHostId, async (id) => {
  precheck.value = null
  precheckError.value = ''
  if (!id || !props.app) return
  prechecking.value = true
  try {
    precheck.value = await precheckMigration(props.app.id, id)
    // 漏迁数据是最难补救的错误：条件允许时默认勾选卷同步，由操作员显式关闭。
    if (canMigrateVolumes.value) migrateVolumes.value = true
  } catch (e: any) {
    precheckError.value = e?.message || '预检失败'
  } finally {
    prechecking.value = false
  }
})

async function submit() {
  if (!props.app || !targetHostId.value) return
  submitting.value = true
  try {
    await migrateApp(props.app.id, {
      target_host_id: targetHostId.value,
      migrate_volumes: migrateVolumes.value && canMigrateVolumes.value,
      clean_source: cleanSource.value,
    })
    message.success('迁移任务已启动，正在切换主机与网络路由')
    emit('migrated')
    emit('update:show', false)
  } catch (e: any) {
    message.error(e?.message || '发起迁移失败')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    title="迁移服务"
    style="width: 760px; max-width: 94vw"
    @update:show="(v) => emit('update:show', v)"
  >
    <div v-if="app" class="migrate-body">
      <!-- 迁移链：源主机 -> 目标主机 -->
      <div class="mig-flow">
        <div class="mig-flow-node">
          <div class="mig-flow-label">当前主机</div>
          <div class="mig-flow-value">{{ appHost?.hostname || app.host_id }}</div>
        </div>
        <NIcon size="18" class="mig-flow-arrow"><ArrowForwardOutline /></NIcon>
        <div class="mig-flow-node" :class="{ dim: !targetHost }">
          <div class="mig-flow-label">目标主机</div>
          <div class="mig-flow-value">{{ targetHost?.hostname || '待选择' }}</div>
        </div>
      </div>

      <div class="mig-section">
        <div class="mig-section-title">目标主机</div>
        <NSelect
          v-model:value="targetHostId"
          :options="hostOptions"
          :loading="loadingHosts"
          filterable
          placeholder="选择接收该应用的主机（离线与 Windows 主机不可选）"
        />
      </div>

      <NSpin v-if="prechecking" size="small" style="display: block; margin: 12px 0" />

      <template v-else-if="precheck">
        <div class="mig-status-grid">
          <div class="mig-status-item">
            <span class="mig-status-label">Agent</span>
            <NTag size="tiny" :type="precheck.agent_online ? 'success' : 'error'" :bordered="false">
              {{ precheck.agent_online ? '在线' : '离线' }}
            </NTag>
          </div>
          <div class="mig-status-item">
            <span class="mig-status-label">Docker</span>
            <NTag size="tiny" :type="precheck.docker_running ? 'success' : 'error'" :bordered="false">
              {{ precheck.docker_running ? '运行中' : '未就绪' }}
            </NTag>
          </div>
          <div class="mig-status-item">
            <span class="mig-status-label">存储目标</span>
            <NTag size="tiny" :type="precheck.s3_ready ? 'success' : 'warning'" :bordered="false">
              {{ precheck.s3_ready ? 'S3 就绪' : '未配置 S3' }}
            </NTag>
            <span v-if="precheck.s3_bucket" class="mig-status-hint mono-font">{{ precheck.s3_bucket }}</span>
          </div>
          <div class="mig-status-item">
            <span class="mig-status-label">端口占用</span>
            <NTag v-if="precheck.port_conflicts?.length" size="tiny" type="error" :bordered="false">
              {{ precheck.port_conflicts.join(', ') }}
            </NTag>
            <NTag v-else size="tiny" type="success" :bordered="false">无冲突</NTag>
          </div>
        </div>

        <NAlert
          v-for="(w, idx) in precheck.warnings"
          :key="idx"
          type="warning"
          :show-icon="false"
          style="margin-top: 8px; font-size: 12px"
        >
          {{ w }}
        </NAlert>
      </template>

      <NAlert v-else-if="precheckError" type="error" :show-icon="false" style="margin-top: 12px">
        预检失败：{{ precheckError }}（不影响直接发起，失败将自动回滚）
      </NAlert>

      <!-- 数据卷：仅在应用声明了可迁移的卷时出现 -->
      <div v-if="volumeCount > 0" class="mig-section">
        <div class="mig-section-title">
          挂载卷数据
          <span class="mig-count">{{ volumeCount }} 个</span>
        </div>
        <NAlert v-if="composeVolumes" type="info" :show-icon="false" style="font-size: 12px">
          Compose 应用的卷在编排 YAML 内声明，不支持自动同步；目标主机将由编排重新创建卷。
        </NAlert>
        <template v-else>
          <NSpace align="center" :size="10">
            <NSwitch v-model:value="migrateVolumes" :disabled="!canMigrateVolumes" />
            <span>经 S3 中转同步卷数据</span>
            <NTag v-if="!precheck?.s3_ready" size="tiny" type="warning" :bordered="false">
              需先配置默认 S3 存储目标
            </NTag>
          </NSpace>
          <div class="mig-volumes mono-font">
            <span v-for="v in app.volumes" :key="v" class="mig-volume-item">{{ v }}</span>
          </div>
          <div class="mig-hint">
            源容器先停止再打包，保证快照一致；目标主机落盘后逐卷校验 SHA256。
            关闭此项时目标主机将以空目录启动，历史数据不会跟随。
          </div>
        </template>
      </div>

      <div class="mig-section">
        <div class="mig-section-title">源主机处理</div>
        <NSpace align="center" :size="10">
          <NSwitch v-model:value="cleanSource" />
          <span>迁移成功后清理源主机容器</span>
        </NSpace>
        <div class="mig-hint">
          勾选后删除源主机上的应用容器（含 -prev / -next 临时容器）；
          <strong>bind mount 的宿主数据目录始终保留</strong>，不会被清空。
          不勾选时源容器保持停止状态，可作为冷备随时回切。
        </div>
      </div>

      <!-- 迁移后的流量与网络路径（反代与组网转发会一并重定向） -->
      <div class="mig-section">
        <div class="mig-section-title">迁移后的流量与网络</div>
        <AppTopologyPreview
          :domain="app.domain || ''"
          :proxy-mode="proxyMode"
          :app-name="app.name"
          :host-name="targetHost?.hostname || ''"
          :host-online="targetHost?.status === 'online'"
          :host-public-ip="targetHost?.public_ip || ''"
          :host-mesh-ip="targetMeshIP"
          :host-mesh-source="targetMeshIP ? 'cache' : ''"
          :gateway-name="gatewayHost?.hostname || ''"
          :gateway-online="gatewayHost?.status === 'online'"
          :gateway-public-ip="gatewayHost?.public_ip || ''"
          :gateway-mesh-ip="app.proxy_gateway_host_id ? meshIPById.get(app.proxy_gateway_host_id) || '' : ''"
          :ports="ports"
          :compose-mode="app.source_type === 'raw_compose'"
          :mesh-forward-enabled="!!app.mesh_forward_enabled"
          :mesh-forward-host-name="forwardHost?.hostname || ''"
          :mesh-forward-host-online="forwardHost?.status === 'online'"
          :mesh-forward-host-public-ip="forwardHost?.public_ip || ''"
          :mesh-forward-host-mesh-ip="app.mesh_forward_host_id ? meshIPById.get(app.mesh_forward_host_id) || '' : ''"
          :mesh-forward-entry-port="app.mesh_forward_entry_port"
          :mesh-forward-target-port="app.mesh_forward_target_port"
        />
        <div class="mig-hint">
          <template v-if="app.domain">
            已绑定域名 <strong>{{ app.domain }}</strong>：
            {{ proxyMode === 'gateway' ? '网关主机会重新解析到目标主机的上游并热加载。' : '目标主机会初始化系统托管反代容器并接管该域名，源主机上的旧配置同时清理。' }}
          </template>
          <template v-else>该应用未绑定域名，无需重定向反代。</template>
          <br />
          <template v-if="app.mesh_forward_enabled">
            组网端口转发的中继容器会更新为目标主机的组网 / 直连地址。
          </template>
          <template v-else>未启用组网端口转发。</template>
        </div>
      </div>

      <NAlert type="info" :show-icon="false" style="margin-top: 4px; font-size: 12px">
        迁移期间源容器会被停止。任一步（卷同步 / 路由切换 / 目标主机构建与健康检查）失败，
        应用主机绑定、反代与组网转发都会自动回滚到源主机并重新拉起旧容器，服务不会停留在半迁移状态。
      </NAlert>
    </div>

    <template #footer>
      <NSpace justify="end">
        <NButton @click="emit('update:show', false)">取消</NButton>
        <NButton
          type="primary"
          :loading="submitting"
          :disabled="!targetHostId || prechecking"
          @click="submit"
        >
          <template #icon>
            <NIcon><SwapHorizontalOutline /></NIcon>
          </template>
          开始迁移
        </NButton>
      </NSpace>
    </template>
  </NModal>
</template>

<style scoped>
.migrate-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.mig-flow {
  display: flex;
  align-items: center;
  gap: 12px;
}
.mig-flow-node {
  flex: 1;
  min-width: 0;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 8px 12px;
}
.mig-flow-node.dim {
  opacity: 0.55;
}
.mig-flow-label {
  font-size: 11px;
  color: var(--text-secondary);
  margin-bottom: 2px;
}
.mig-flow-value {
  font-size: 13px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mig-flow-arrow {
  color: var(--text-secondary);
  flex-shrink: 0;
}
.mig-section {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.mig-section-title {
  font-size: 13px;
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 8px;
}
.mig-count {
  font-weight: 400;
  font-size: 12px;
  color: var(--text-secondary);
}
.mig-status-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 8px;
}
.mig-status-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 6px;
  background: rgba(99, 102, 241, 0.06);
  border: 1px solid rgba(99, 102, 241, 0.2);
}
.mig-status-label {
  font-size: 12px;
  color: var(--text-secondary);
}
.mig-status-hint {
  font-size: 11px;
  color: var(--text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mig-volumes {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.mig-volume-item {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 4px;
  background: var(--n-action-color, rgba(128, 128, 128, 0.1));
}
.mig-hint {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
}
</style>
