<script setup lang="ts">
import { computed } from 'vue'
import { NTag, NSpace } from 'naive-ui'
import { bothMeshed } from '../../utils/mesh'

// 创建页「流量拓扑预览」：把表单里的主机/端口范围/域名/反代模式/组网转发实时渲染成
// 公网用户 -> 网关 -> Tailscale 组网 -> 应用主机 -> 应用容器 的链路，以及
// TCP 客户端 -> 入口主机 (socat) -> 组网/直连 -> 应用容器 的中继链路，节点
// 离线或未组网时降亮提示，避免把配错的链路提交上去。
// 视觉语言对齐 AppDetail「域名与反代」页的 topology-box。

export interface TopologyPort {
  host: number
  container: number
  bind_scope: 'public' | 'mesh'
}

const props = defineProps<{
  domain?: string
  domains?: string[]
  proxyMode: 'local' | 'gateway'
  appName: string
  hostName: string
  hostOnline: boolean
  hostPublicIp: string
  hostMeshIp: string
  hostMeshSource: '' | 'live' | 'cache'
  gatewayName: string
  gatewayOnline: boolean
  gatewayPublicIp: string
  gatewayMeshIp: string
  ports: TopologyPort[]
  // 证书状态（HTTPS）：'covered' 已有证书覆盖 | 'issue' 提交时在线签发 |
  // 'selected' 显式指定证书 | '' 域名未填/探测中/探测失败（不展示）
  certStatus?: '' | 'covered' | 'issue' | 'selected'
  // composeMode 下端口来自 compose YAML 声明，绑定范围不可逐条选择，
  // 端口条带如实标注「compose 声明」而非公网/组网绑定范围。
  composeMode?: boolean
  meshForwardEnabled?: boolean
  meshForwardHostName?: string
  meshForwardHostOnline?: boolean
  meshForwardHostPublicIp?: string
  meshForwardHostMeshIp?: string
  meshForwardEntryPort?: number | null
  meshForwardTargetPort?: number | null
}>()

// 网关到应用主机的链路：双方都在组网内则走 100.x，否则直连（公网/内网 IP）。
// 异地组网只是其中一种路径，不是网关模式的必需条件。
const meshLink = computed(() => bothMeshed(props.hostMeshIp, props.gatewayMeshIp))

// 组网转发入口到应用主机的链路：双方都在组网内走 100.x，否则直连。
const meshForwardLink = computed(() => bothMeshed(props.hostMeshIp, props.meshForwardHostMeshIp))

const displayDomain = computed(() => {
  if (props.domains && props.domains.length > 0) {
    return props.domains.join(', ')
  }
  return props.domain || ''
})

const hasDomain = computed(() => Boolean(displayDomain.value))

// Nginx/SSL 节点的证书状态标签：创建页覆盖探测结果的实时映射。
const certTag = computed(() => {
  if (!hasDomain.value || !props.certStatus) return null
  switch (props.certStatus) {
    case 'covered': return { label: '已有证书', type: 'success' as const }
    case 'issue': return { label: '提交时签发', type: 'warning' as const }
    case 'selected': return { label: '指定证书', type: 'info' as const }
  }
  return null
})

function portBindLabel(p: TopologyPort): string {
  if (props.composeMode) {
    return `${p.host} → ${p.container}`
  }
  if (p.bind_scope === 'mesh') {
    return `${props.hostMeshIp || '组网IP'}:${p.host} → ${p.container}`
  }
  return `0.0.0.0:${p.host} → ${p.container}`
}
</script>

<template>
  <div class="topo-preview">
    <div class="topo-head">
      <span class="topo-title">流量拓扑预览</span>
      <NSpace :size="6" align="center" :wrap="true">
        <NTag v-if="hostMeshIp" size="tiny" :type="hostMeshSource === 'live' ? 'success' : 'warning'" :bordered="false">
          组网 IP {{ hostMeshIp }}（{{ hostMeshSource === 'live' ? '实时探测' : '缓存' }}）
        </NTag>
        <NTag v-else-if="hostName" size="tiny" type="info" :bordered="false">应用主机未加入组网</NTag>
      </NSpace>
    </div>

    <div class="topology-box">
      <div class="topology-line">
        <div class="topology-node" :class="{ dim: !hasDomain }">
          <div class="topology-label">公网用户</div>
          <div class="topology-value">{{ displayDomain || '未绑定域名' }}</div>
        </div>
        <div class="topology-arrow">-&gt;</div>

        <template v-if="proxyMode === 'gateway'">
          <div class="topology-node" :class="{ dim: !gatewayOnline }">
            <div class="topology-label">网关主机（Nginx / SSL 终止）</div>
            <div class="topology-value">{{ gatewayName || '未选择网关' }}</div>
            <div class="topo-meta">
              <NTag size="tiny" :type="gatewayOnline ? 'success' : 'error'" :bordered="false">
                {{ gatewayOnline ? '在线' : '离线' }}
              </NTag>
              <NTag v-if="gatewayMeshIp" size="tiny" type="success" :bordered="false">组网 {{ gatewayMeshIp }}</NTag>
              <NTag v-else size="tiny" type="info" :bordered="false">未组网</NTag>
              <NTag v-if="gatewayPublicIp" size="tiny" type="info" :bordered="false">公网 {{ gatewayPublicIp }}</NTag>
              <NTag v-if="certTag" size="tiny" :type="certTag.type" :bordered="false">{{ certTag.label }}</NTag>
            </div>
          </div>
          <div class="topology-arrow">{{ meshLink ? 'Tailscale 组网' : '直连' }}</div>
          <div class="topology-node" :class="{ dim: !hostOnline }">
            <div class="topology-label">应用主机</div>
            <div class="topology-value">{{ hostName || '未选择主机' }}</div>
            <div class="topo-meta">
              <NTag size="tiny" :type="hostOnline ? 'success' : 'error'" :bordered="false">
                {{ hostOnline ? '在线' : '离线' }}
              </NTag>
              <NTag v-if="hostMeshIp" size="tiny" type="success" :bordered="false">组网 {{ hostMeshIp }}</NTag>
              <NTag v-else size="tiny" type="info" :bordered="false">未组网</NTag>
              <NTag v-if="hostPublicIp" size="tiny" :type="meshLink ? 'default' : 'info'" :bordered="false">
                公网 {{ hostPublicIp }}
              </NTag>
            </div>
          </div>
        </template>

        <template v-else>
          <div class="topology-node" :class="{ dim: !hostOnline || (hasDomain && !hostPublicIp) }">
            <div class="topology-label">应用主机（Nginx / SSL 终止）</div>
            <div class="topology-value">{{ hostName || '未选择主机' }}</div>
            <div class="topo-meta">
              <NTag size="tiny" :type="hostOnline ? 'success' : 'error'" :bordered="false">
                {{ hostOnline ? '在线' : '离线' }}
              </NTag>
              <NTag v-if="hostPublicIp" size="tiny" type="info" :bordered="false">公网 {{ hostPublicIp }}</NTag>
              <NTag v-else size="tiny" type="warning" :bordered="false">无公网 IP</NTag>
              <NTag v-if="certTag" size="tiny" :type="certTag.type" :bordered="false">{{ certTag.label }}</NTag>
            </div>
          </div>
        </template>

        <div class="topology-arrow">-&gt;</div>
        <div class="topology-node">
          <div class="topology-label">应用容器</div>
          <div class="topology-value">{{ appName || '未命名应用' }}</div>
        </div>
      </div>

      <!-- TCP 组网端口转发（4层流量中继） -->
      <div v-if="meshForwardEnabled" class="topology-fwd-block">
        <div class="topology-fwd-head">
          <span class="fwd-badge">TCP 中继</span>
          <span>4 层端口转发链路</span>
        </div>
        <div class="topology-line">
          <div class="topology-node" :class="{ dim: !meshForwardEntryPort }">
            <div class="topology-label">外部访问 / 客户端</div>
            <div class="topology-value">
              {{ meshForwardHostPublicIp || meshForwardHostName || '入口主机' }}:{{ meshForwardEntryPort || '未配端口' }}
            </div>
          </div>
          <div class="topology-arrow">-&gt;</div>
          <div class="topology-node" :class="{ dim: !meshForwardHostOnline }">
            <div class="topology-label">入口主机（socat 中继容器）</div>
            <div class="topology-value">{{ meshForwardHostName || '未选择入口' }}</div>
            <div class="topo-meta">
              <NTag size="tiny" :type="meshForwardHostOnline ? 'success' : 'error'" :bordered="false">
                {{ meshForwardHostOnline ? '在线' : '离线' }}
              </NTag>
              <NTag v-if="meshForwardHostMeshIp" size="tiny" type="success" :bordered="false">组网 {{ meshForwardHostMeshIp }}</NTag>
              <NTag v-else size="tiny" type="info" :bordered="false">未组网</NTag>
              <NTag v-if="meshForwardHostPublicIp" size="tiny" type="info" :bordered="false">公网 {{ meshForwardHostPublicIp }}</NTag>
            </div>
          </div>
          <div class="topology-arrow">{{ meshForwardLink ? 'Tailscale 组网' : '直连' }}</div>
          <div class="topology-node" :class="{ dim: !hostOnline }">
            <div class="topology-label">目标：应用主机</div>
            <div class="topology-value">
              {{ hostMeshIp || hostPublicIp || hostName || '应用主机' }}:{{ meshForwardTargetPort || '端口' }}
            </div>
            <div class="topo-meta">
              <NTag size="tiny" :type="hostOnline ? 'success' : 'error'" :bordered="false">
                {{ hostOnline ? '在线' : '离线' }}
              </NTag>
              <NTag v-if="hostMeshIp" size="tiny" type="success" :bordered="false">组网 {{ hostMeshIp }}</NTag>
              <NTag v-else size="tiny" type="info" :bordered="false">未组网</NTag>
            </div>
          </div>
          <div class="topology-arrow">-&gt;</div>
          <div class="topology-node">
            <div class="topology-label">应用容器</div>
            <div class="topology-value">{{ appName || '未命名应用' }}</div>
          </div>
        </div>
      </div>

      <div class="topo-ports">
        <span v-for="p in ports" :key="`${p.host}:${p.container}:${p.bind_scope}`" class="topo-port">
          <span class="topo-port-bind">{{ portBindLabel(p) }}</span>
          <NTag size="tiny" :type="composeMode ? 'info' : p.bind_scope === 'mesh' ? 'success' : 'default'" :bordered="false">
            {{ composeMode ? 'compose 声明' : p.bind_scope === 'mesh' ? '仅异地组网' : '公网开放' }}
          </NTag>
        </span>
        <span v-if="!ports.length" class="topology-hint">尚未添加端口映射</span>
      </div>

      <div class="topology-hint">
        <template v-if="proxyMode === 'gateway' && meshLink">
          应用主机无需公网 IP：公网流量在网关主机卸载 TLS 后，经 Tailscale 组网转发到应用容器。
        </template>
        <template v-else-if="proxyMode === 'gateway'">
          网关经直连转发到应用主机（{{ hostPublicIp || '内网 IP' }}），无需异地组网；请确认该地址从网关可达。
        </template>
        <template v-else-if="domain && !hostPublicIp">
          该主机没有公网 IP，local 模式下域名无法从公网访问应用；如主机位于 NAT 内网，建议改用「统一网关」。
        </template>
        <template v-else>
          Nginx 运行在应用所在主机，域名需解析到该主机的公网 IP。
        </template>

        <div v-if="meshForwardEnabled" style="margin-top: 6px; padding-top: 6px; border-top: 1px dashed rgba(99, 102, 241, 0.15)">
          <template v-if="meshForwardLink">
            TCP 中继：外部连接入口主机的 <code>:{{ meshForwardEntryPort || '端口' }}</code> 将由 <code>watchman-fwd-</code> 容器经 Tailscale 组网（{{ hostMeshIp }}）中继至应用容器 <code>:{{ meshForwardTargetPort || '目标端口' }}</code>。
          </template>
          <template v-else>
            TCP 中继：由于两侧未完全加入组网，入口主机将经直连网络转发至应用主机（{{ hostPublicIp || '内网 IP' }}）；请确认入口主机网络可直达。
          </template>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped lang="scss">
.topo-preview {
  width: 100%;
  margin: 4px 0 16px;
}

.topo-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;

  .topo-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
  }
}

.topology-box {
  background: rgba(99, 102, 241, 0.06);
  border: 1px solid rgba(99, 102, 241, 0.2);
  border-radius: 8px;
  padding: 14px;
}

.topology-fwd-block {
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px dashed rgba(99, 102, 241, 0.25);

  .topology-fwd-head {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;

    .fwd-badge {
      display: inline-block;
      font-size: 10px;
      font-weight: 700;
      color: #6366f1;
      background: rgba(99, 102, 241, 0.15);
      padding: 1px 6px;
      border-radius: 4px;
      text-transform: uppercase;
      letter-spacing: 0.5px;
    }
  }
}

.topology-line {
  display: flex;
  align-items: stretch;
  gap: 12px;
  flex-wrap: wrap;
}

.topology-node {
  flex: 1;
  min-width: 140px;
  background: var(--bg-card);
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 8px 12px;

  &.dim {
    opacity: 0.55;
  }

  .topology-label {
    font-size: 11px;
    color: var(--text-secondary);
    margin-bottom: 2px;
  }

  .topology-value {
    font-size: 13px;
    font-weight: 600;
    word-break: break-all;
  }

  .topo-meta {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-wrap: wrap;
    margin-top: 6px;
  }
}

.topology-arrow {
  color: #6366f1;
  font-weight: 600;
  font-size: 13px;
  flex-shrink: 0;
  align-self: center;
}

.topo-ports {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 12px;

  .topo-port {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    font-family: 'JetBrains Mono', Consolas, monospace;
    background: var(--bg-card);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    padding: 3px 8px;
  }
}

.topology-hint {
  margin-top: 10px;
  font-size: 12px;
  color: var(--text-secondary);
  line-height: 1.6;
}
</style>
