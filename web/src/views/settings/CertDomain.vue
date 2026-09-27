<script setup lang="ts">
import { computed, h, onActivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem, NIcon, NInput,
  NModal, NPopconfirm, NSelect, NSpace, NSwitch, NTabPane, NTabs, NTag,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { AddCircleOutline, TrashOutline } from '@vicons/ionicons5'
import CertManager from '../../components/cert/CertManager.vue'
import { listApps, type AppEntity } from '../../api/apps'
import { listHosts } from '../../api/hosts'
import type { Host } from '../../api/types'
import {
  bindProxy, listCerts, unbindProxy, type Certificate,
} from '../../api/certs'

// 系统设置 → 证书与域名（仅管理员可见，见 sections.ts 的 adminOnly）。
// 证书签发/续期/ACME 账户复用 CertManager；域名绑定管理应用的反代域名
//（后端 PUT /apps/:id/proxy，写操作 admin/operator + 审计）。
defineOptions({ name: 'CertDomain' })

const message = useMessage()
const activeTab = ref<'certs' | 'domains'>('certs')

// ---- 域名绑定 ----
const apps = ref<AppEntity[]>([])
const hosts = ref<Host[]>([])
const certs = ref<Certificate[]>([])
const loading = ref(false)
const showBind = ref(false)
const binding = ref(false)

const bindForm = reactive({
  app_id: '',
  domain: '',
  mode: 'local' as 'local' | 'gateway',
  gateway_host_id: '',
  upstream: '',
  cert_id: '',
  websocket: false,
})

const hostOptions = computed(() =>
  hosts.value.map((h) => ({
    label: `${h.hostname || h.id} (${h.id.slice(0, 8)})`,
    value: h.id,
  })),
)

const appOptions = computed(() =>
  apps.value.map((a) => ({
    label: `${a.name}${a.domain ? `（已绑 ${a.domain}）` : ''}`,
    value: a.id,
  })),
)

const certOptions = computed(() => [
  { label: '自动匹配（按域名查找证书，无则自动签发）', value: '' },
  ...certs.value.map((c) => ({
    label: `${c.domains.join(', ')}（${c.issuer || 'ACME'}）`,
    value: c.id,
  })),
])

const domainColumns = computed<DataTableColumns<AppEntity>>(() => [
  {
    title: '应用',
    key: 'name',
    width: 200,
    render: (r) => h('span', { style: 'font-weight: 600' }, r.name),
  },
  {
    title: '绑定域名',
    key: 'domain',
    render: (r) =>
      r.domain
        ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => r.domain })
        : h('span', { style: 'color: #999' }, '未绑定'),
  },
  {
    title: '反代模式',
    key: 'proxy_mode',
    width: 130,
    render: (r) => {
      if (!r.domain) return h('span', { style: 'color: #999' }, '-')
      return h(NTag, { size: 'small', type: 'info', bordered: false }, {
        default: () => (r.proxy_mode === 'gateway' ? '网关模式' : '本机模式'),
      })
    },
  },
  {
    title: '上游',
    key: 'proxy_upstream',
    ellipsis: { tooltip: true },
    render: (r) => r.domain ? h('code', { style: 'font-size: 11px' }, r.proxy_upstream || '-') : h('span', { style: 'color: #999' }, '-'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 190,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openBind(r.id) }, { default: () => (r.domain ? '重新绑定' : '绑定域名') }),
          r.domain
            ? h(
              NPopconfirm,
              { onPositiveClick: () => doUnbind(r) },
              {
                trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) }),
                default: () => `确定解绑应用「${r.name}」的域名 ${r.domain}？`,
              },
            )
            : null,
        ],
      }),
  },
])

async function loadAll() {
  loading.value = true
  try {
    const [appList, hostList, certList] = await Promise.all([
      listApps(),
      listHosts(),
      listCerts().catch(() => [] as Certificate[]),
    ])
    apps.value = appList
    hosts.value = hostList.data
    certs.value = certList
  } catch (e: any) {
    message.error(e.message || '加载应用/主机列表失败')
  } finally {
    loading.value = false
  }
}

function openBind(appId = '') {
  bindForm.app_id = appId
  const app = apps.value.find((a) => a.id === appId)
  bindForm.domain = app?.domain || ''
  bindForm.mode = app?.proxy_mode || 'local'
  bindForm.gateway_host_id = app?.proxy_gateway_host_id || ''
  bindForm.upstream = app?.proxy_upstream || ''
  bindForm.cert_id = ''
  bindForm.websocket = false
  showBind.value = true
}

async function doBind() {
  if (!bindForm.app_id) {
    message.warning('请选择应用')
    return
  }
  const domain = bindForm.domain.trim().toLowerCase()
  if (!domain) {
    message.warning('请填写域名')
    return
  }
  if (bindForm.mode === 'gateway' && !bindForm.gateway_host_id) {
    message.warning('网关模式必须选择网关主机')
    return
  }
  binding.value = true
  try {
    await bindProxy(bindForm.app_id, {
      domain,
      mode: bindForm.mode,
      gateway_host_id: bindForm.gateway_host_id || undefined,
      upstream: bindForm.upstream.trim() || undefined,
      cert_id: bindForm.cert_id || undefined,
      websocket: bindForm.websocket,
    })
    message.success(`域名 ${domain} 绑定成功`)
    showBind.value = false
    await loadAll()
  } catch (e: any) {
    message.error(e.message || '绑定失败')
  } finally {
    binding.value = false
  }
}

async function doUnbind(app: AppEntity) {
  try {
    await unbindProxy(app.id)
    message.success('域名解绑成功')
    await loadAll()
  } catch (e: any) {
    message.error(e.message || '解绑失败')
  }
}

onMounted(loadAll)
// 设置分区被 KeepAlive 缓存：切回页签时刷新，避免绑定状态过期。
onActivated(loadAll)
</script>

<template>
  <div class="cert-domain-view">
    <div class="tabs-container">
      <NTabs v-model:value="activeTab" type="line">
        <NTabPane name="certs" tab="证书管理">
          <div class="tab-pane-content">
            <CertManager />
          </div>
        </NTabPane>
        <NTabPane name="domains" tab="域名绑定">
          <div class="tab-pane-content domains-pane">
            <NAlert type="info" :show-icon="true" class="tip-hint" style="flex-shrink: 0; margin-bottom: 10px">
              给应用绑定公网域名：控制端自动匹配证书中心的证书（无覆盖证书时用默认 ACME 账户签发），
              并在目标主机下发 Nginx 反代配置。本机模式跑在应用所在主机，网关模式跑在指定的网关主机。
            </NAlert>
            <div class="domains-toolbar">
              <NSpace>
                <NButton type="primary" @click="openBind()">
                  <template #icon>
                    <NIcon><AddCircleOutline /></NIcon>
                  </template>
                  绑定域名
                </NButton>
                <NButton :loading="loading" @click="loadAll">刷新</NButton>
              </NSpace>
            </div>
            <NCard :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="domainColumns"
                :data="apps"
                :row-key="(r: AppEntity) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                :loading="loading"
                size="small"
              />
            </NCard>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <!-- 绑定域名弹窗 -->
    <NModal
      v-model:show="showBind"
      preset="card"
      title="绑定域名"
      style="width: 560px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="应用" required>
          <NSelect v-model:value="bindForm.app_id" :options="appOptions" placeholder="选择应用" filterable />
        </NFormItem>
        <NFormItem label="域名" required>
          <NInput v-model:value="bindForm.domain" placeholder="如 api.example.com" />
        </NFormItem>
        <NFormItem label="反代模式">
          <NSelect
            v-model:value="bindForm.mode"
            :options="[
              { label: '本机模式（Nginx 跑在应用所在主机）', value: 'local' },
              { label: '网关模式（Nginx 跑在网关主机，经组网回源）', value: 'gateway' },
            ]"
          />
        </NFormItem>
        <NFormItem v-if="bindForm.mode === 'gateway'" label="网关主机" required>
          <NSelect v-model:value="bindForm.gateway_host_id" :options="hostOptions" placeholder="选择网关主机" filterable />
        </NFormItem>
        <NFormItem label="上游地址（可选，默认自动推导）">
          <NInput v-model:value="bindForm.upstream" placeholder="如 127.0.0.1:8080，留空自动推导" />
        </NFormItem>
        <NFormItem label="证书">
          <NSelect v-model:value="bindForm.cert_id" :options="certOptions" placeholder="自动匹配" />
        </NFormItem>
        <NFormItem label="WebSocket 支持">
          <NSwitch v-model:value="bindForm.websocket" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showBind = false">取消</NButton>
          <NButton type="primary" :loading="binding" @click="doBind">确认绑定</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.cert-domain-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;

  .tabs-container {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;

    :deep(.n-tabs) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
    }

    :deep(.n-tabs-pane-wrapper) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    :deep(.n-tab-pane) {
      flex: 1;
      min-height: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }
  }

  .tab-pane-content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .domains-pane {
    .domains-toolbar {
      flex-shrink: 0;
      margin-bottom: 10px;
    }
  }
}
</style>
