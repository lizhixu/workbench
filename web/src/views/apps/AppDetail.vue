<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import {
  NAlert, NButton, NDataTable, NDescriptions, NDescriptionsItem,
  NForm, NFormItem, NIcon, NModal, NPopconfirm, NRadio, NRadioGroup,
  NSelect, NSpace, NSwitch, NTabPane, NTabs,
  NTag, NCode, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  ArrowBackOutline, BuildOutline, CloudUploadOutline, CopyOutline,
  GitCommitOutline, PlayOutline, RefreshOutline, SparklesOutline, StopOutline,
  SwapHorizontalOutline,
} from '@vicons/ionicons5'
import {
  deployApp, diagnoseDeployment, getApp, getAppStatus, listDeployments, restartApp,
  rollbackApp, startApp, stopApp, updateApp, type AppEntity, type AppStatus, type Deployment,
  type AIDiagnosis,
} from '../../api/apps'
import { bindProxy, unbindProxy, type Certificate } from '../../api/certs'
import { listCerts } from '../../api/certs'
import { listHosts } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import { useAuthStore } from '../../stores/auth'

defineOptions({ name: 'AppDetail' })

const route = useRoute()
const message = useMessage()
const auth = useAuthStore()

const appId = computed(() => String(route.params.id ?? ''))
const app = ref<AppEntity | null>(null)
const status = ref<AppStatus | null>(null)
const deployments = ref<Deployment[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const acting = ref(false)
const activeTab = ref('overview')

// Deployment log viewer.
const showLog = ref(false)
const logDeployment = ref<Deployment | null>(null)

// AI Deployment diagnosis.
const showAIDiagnosis = ref(false)
const diagnosing = ref(false)
const aiResult = ref<AIDiagnosis | null>(null)
const diagnosingDepId = ref('')

async function doDiagnose(dep: Deployment) {
  diagnosingDepId.value = dep.id
  diagnosing.value = true
  aiResult.value = null
  showAIDiagnosis.value = true
  try {
    aiResult.value = await diagnoseDeployment(appId.value, dep.id)
  } catch (e: any) {
    message.error(e.message || 'AI 诊断失败')
  } finally {
    diagnosing.value = false
  }
}

// ---- Reverse proxy binding (domain + cert + nginx distribution) ----
const certs = ref<Certificate[]>([])
const hostOptions = ref<{ label: string; value: string }[]>([])
const proxyForm = ref({
  domain: '',
  mode: 'local' as 'local' | 'gateway',
  gateway_host_id: '',
  upstream: '',
  cert_id: '',
  websocket: true,
})
const savingProxy = ref(false)

async function loadProxyData() {
  try {
    certs.value = (await listCerts()) as Certificate[]
  } catch {
    certs.value = []
  }
  try {
    const res = await listHosts()
    const hosts = ((res as { data: { id: string; hostname: string; status: string }[] }).data ?? []) as { id: string; hostname: string; status: string }[]
    hostOptions.value = hosts.map((h) => ({ label: h.hostname, value: h.id }))
  } catch {
    hostOptions.value = []
  }
  if (app.value) {
    proxyForm.value.domain = app.value.domain ?? ''
    proxyForm.value.mode = (app.value.proxy_mode as 'local' | 'gateway') ?? 'local'
    proxyForm.value.upstream = app.value.proxy_upstream ?? ''
  }
}

const certOptions = computed(() => [
  { label: '按域名自动匹配', value: '' },
  ...certs.value.map((c) => ({ label: c.domains.join(', '), value: c.id })),
])

async function doBindProxy() {
  if (!proxyForm.value.domain.trim()) {
    message.warning('请填写域名')
    return
  }
  if (proxyForm.value.mode === 'gateway' && !proxyForm.value.gateway_host_id) {
    message.warning('网关模式需选择网关主机')
    return
  }
  savingProxy.value = true
  try {
    await bindProxy(appId.value, {
      domain: proxyForm.value.domain.trim(),
      mode: proxyForm.value.mode,
      gateway_host_id: proxyForm.value.gateway_host_id || undefined,
      upstream: proxyForm.value.upstream.trim() || undefined,
      cert_id: proxyForm.value.cert_id || undefined,
      websocket: proxyForm.value.websocket,
    })
    message.success('反代配置已下发')
    await loadApp()
  } catch (e: any) {
    message.error(e.message || '下发失败')
  } finally {
    savingProxy.value = false
  }
}

async function doUnbindProxy() {
  savingProxy.value = true
  try {
    await unbindProxy(appId.value)
    message.success('已解除域名绑定')
    proxyForm.value.domain = ''
    await loadApp()
  } catch (e: any) {
    message.error(e.message || '解绑失败')
  } finally {
    savingProxy.value = false
  }
}

let pollTimer: ReturnType<typeof setInterval> | null = null

const canWrite = computed(() => auth.role === 'admin' || auth.role === 'operator')
const deploying = computed(() => status.value?.deploying ?? false)

const webhookURL = computed(() => {
  if (!app.value?.webhook_token) return ''
  const base = window.location.origin
  return `${base}/api/v1/apps/webhook/${app.value.webhook_token}`
})

const composeEditContent = ref('')
const savingCompose = ref(false)

async function loadApp() {
  try {
    app.value = await getApp(appId.value)
    if (app.value.compose_content) {
      composeEditContent.value = app.value.compose_content
    }
  } catch (e: any) {
    message.error(e.message || '获取应用信息失败')
  }
}

async function saveAndRedeployCompose() {
  if (!composeEditContent.value.trim()) {
    message.warning('Compose 内容不能为空')
    return
  }
  savingCompose.value = true
  try {
    await updateApp(appId.value, {
      compose_content: composeEditContent.value,
    })
    message.success('配置已保存，正在重新部署...')
    await doDeploy()
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingCompose.value = false
  }
}

async function loadStatus() {
  try {
    status.value = await getAppStatus(appId.value)
  } catch {
    status.value = null
  }
}

async function loadDeployments() {
  loading.value = true
  try {
    const res = await listDeployments(appId.value, (page.value - 1) * pageSize.value, pageSize.value)
    deployments.value = res.data
    total.value = res.total
  } catch (e: any) {
    message.error(e.message || '获取部署历史失败')
  } finally {
    loading.value = false
  }
}

async function refreshAll() {
  await Promise.all([loadApp(), loadStatus(), loadDeployments()])
}

async function doDeploy() {
  acting.value = true
  try {
    await deployApp(appId.value)
    message.success('已开始部署')
    await Promise.all([loadStatus(), loadDeployments()])
  } catch (e: any) {
    message.error(e.message || '触发部署失败')
  } finally {
    acting.value = false
  }
}

async function doRollback(dep?: Deployment) {
  acting.value = true
  try {
    await rollbackApp(appId.value, dep?.id)
    message.success(dep ? `正在回滚到 ${dep.commit_hash}` : '正在回滚到上一版本')
    await Promise.all([loadStatus(), loadDeployments()])
  } catch (e: any) {
    message.error(e.message || '回滚失败')
  } finally {
    acting.value = false
  }
}

async function doAction(op: 'stop' | 'start' | 'restart') {
  acting.value = true
  try {
    const fn = op === 'stop' ? stopApp : op === 'start' ? startApp : restartApp
    await fn(appId.value)
    message.success(op === 'stop' ? '已停止' : op === 'start' ? '已启动' : '已重启')
    await loadStatus()
  } catch (e: any) {
    message.error(e.message || '操作失败')
  } finally {
    acting.value = false
  }
}

async function copyWebhook() {
  if (!webhookURL.value) return
  const ok = await copyToClipboard(webhookURL.value)
  ok ? message.success('已复制 Webhook 地址') : message.warning('复制失败，请手动复制')
}

function openLog(dep: Deployment) {
  logDeployment.value = dep
  showLog.value = true
}

const depStatusTag = (s: string) => {
  const type = s === 'success' ? 'success' : s === 'failed' ? 'error' : 'info'
  const label = s === 'success' ? '成功' : s === 'failed' ? '失败' : s === 'queued' ? '排队' : s === 'building' ? '构建中' : s === 'deploying' ? '发布中' : s
  return h(NTag, { size: 'small', type, bordered: false }, { default: () => label })
}

const triggerLabel = (t: string) =>
  t === 'webhook' ? 'Git 推送' : t === 'rollback' ? '回滚' : '手动'

const columns = computed<DataTableColumns<Deployment>>(() => [
  { title: '状态', key: 'status', width: 90, render: (r) => depStatusTag(r.status) },
  { title: '触发', key: 'trigger', width: 90, render: (r) => triggerLabel(r.trigger) },
  {
    title: 'Commit',
    key: 'commit_hash',
    width: 120,
    render: (r) => r.commit_hash
      ? h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => r.commit_hash })
      : '-',
  },
  {
    title: '提交信息',
    key: 'commit_message',
    ellipsis: { tooltip: true },
    render: (r) => r.commit_message || (r.error ?? '-'),
  },
  {
    title: '耗时',
    key: 'duration_ms',
    width: 100,
    render: (r) => (r.duration_ms ? `${(r.duration_ms / 1000).toFixed(1)}s` : '-'),
  },
  {
    title: '开始时间',
    key: 'started_at',
    width: 150,
    render: (r) => formatTime(r.started_at),
  },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => {
          const btns = [
            h(NButton, { size: 'tiny', secondary: true, onClick: () => openLog(r) }, { default: () => '日志' }),
          ]
          if (r.status === 'failed') {
            btns.push(
              h(
                NButton,
                {
                  size: 'tiny',
                  secondary: true,
                  type: 'info',
                  loading: diagnosing.value && diagnosingDepId.value === r.id,
                  onClick: () => doDiagnose(r),
                },
                {
                  icon: () => h(NIcon, null, { default: () => h(SparklesOutline) }),
                  default: () => 'AI 排障',
                },
              ),
            )
          }
          if (canWrite.value && r.status === 'success' && r.commit_hash) {
            btns.push(
              h(
                NPopconfirm,
                { onPositiveClick: () => doRollback(r) },
                {
                  trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'warning' }, { default: () => '回滚' }),
                  default: () => `将应用回滚到 Commit ${r.commit_hash}？`,
                },
              ),
            )
          }
          return btns
        },
      }),
  },
])

function formatTime(v?: string) {
  if (!v) return '-'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// 仅当仍是当前路由时才响应 tab 变化（组件被 KeepAlive 缓存，route 可能
// 已指向别的页面，见 AGENTS.md 8.4-6）。
watch(activeTab, () => {
  if (route.name !== 'app-detail') return
})
watch(page, () => {
  if (route.name !== 'app-detail') return
  loadDeployments()
})

onMounted(() => {
  refreshAll()
  loadProxyData()
  pollTimer = setInterval(() => {
    if (route.name === 'app-detail') {
      loadStatus()
      if (deploying.value) loadDeployments()
    }
  }, 5000)
})

onActivated(() => {
  refreshAll()
  loadProxyData()
})

onDeactivated(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
})
</script>

<template>
  <div v-if="app" class="app-detail-view page-flex-column">
    <div class="detail-header">
      <NButton quaternary @click="$router.push('/apps')">
        <template #icon>
          <NIcon><ArrowBackOutline /></NIcon>
        </template>
        应用列表
      </NButton>
      <NSpace align="center" style="flex: 1; justify-content: center">
        <NIcon size="20"><BuildOutline /></NIcon>
        <span class="app-title">{{ app.name }}</span>
        <NTag v-if="deploying" type="info" size="small">部署中</NTag>
        <NTag v-else-if="status?.state === 'running'" type="success" size="small">运行中</NTag>
        <NTag v-else-if="status?.state === 'undeployed'" size="small">未部署</NTag>
        <NTag v-else-if="status" :type="status.agent_online ? 'warning' : 'error'" size="small">
          {{ status.agent_online ? status.state : '主机离线' }}
        </NTag>
      </NSpace>
      <NSpace v-if="canWrite">
        <NButton size="small" type="primary" :loading="acting || deploying" @click="doDeploy">
          <template #icon>
            <NIcon><CloudUploadOutline /></NIcon>
          </template>
          部署
        </NButton>
        <NPopconfirm @positive-click="doRollback()">
          <template #trigger>
            <NButton size="small" :loading="acting || deploying" :disabled="deploying">
              <template #icon>
                <NIcon><SwapHorizontalOutline /></NIcon>
              </template>
              回滚上一版
            </NButton>
          </template>
          回滚到上一个成功版本？
        </NPopconfirm>
        <NButton size="small" :loading="acting" @click="doAction('restart')">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          重启
        </NButton>
        <NButton v-if="status?.state === 'running'" size="small" :loading="acting" @click="doAction('stop')">
          <template #icon>
            <NIcon><StopOutline /></NIcon>
          </template>
          停止
        </NButton>
        <NButton v-else size="small" :loading="acting" :disabled="status?.state === 'undeployed'" @click="doAction('start')">
          <template #icon>
            <NIcon><PlayOutline /></NIcon>
          </template>
          启动
        </NButton>
      </NSpace>
    </div>

    <div class="detail-body">
      <NTabs v-model:value="activeTab" type="line" style="height: 100%; display: flex; flex-direction: column">
        <NTabPane name="overview" tab="概览">
          <div class="overview-scroll">
            <NDescriptions :column="2" bordered size="small" label-placement="left">
              <NDescriptionsItem label="Git 仓库">
                <span class="mono">{{ app.repo_url }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="监听分支">
                <NTag size="small" :bordered="false">{{ app.branch }}</NTag>
              </NDescriptionsItem>
              <NDescriptionsItem label="当前 Commit">
                <NTag v-if="app.current_commit" size="small" type="info">
                  <template #icon>
                    <NIcon><GitCommitOutline /></NIcon>
                  </template>
                  {{ app.current_commit }}
                </NTag>
                <span v-else>-</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="容器名">
                <span class="mono">{{ app.container_name || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="镜像">
                <span class="mono">{{ app.image || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="健康检查">
                <span class="mono">{{ app.healthcheck_url || '未配置' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="端口映射">
                <span class="mono">{{ (app.ports ?? []).map((p) => `${p.host}:${p.container}`).join(', ') || '-' }}</span>
              </NDescriptionsItem>
              <NDescriptionsItem label="挂载卷">
                <span class="mono">{{ (app.volumes ?? []).join(', ') || '-' }}</span>
              </NDescriptionsItem>
            </NDescriptions>

            <NAlert v-if="app.auto_deploy && webhookURL" type="success" style="margin-top: 14px" :show-icon="true">
              <template #header>自动部署 Webhook</template>
              将此地址配置到代码仓库的 Webhooks（push 事件）即可实现推送自动构建：
              <div class="webhook-row">
                <NCode :code="webhookURL" />
                <NButton size="tiny" secondary @click="copyWebhook">
                  <template #icon>
                    <NIcon><CopyOutline /></NIcon>
                  </template>
                  复制
                </NButton>
              </div>
            </NAlert>
          </div>
        </NTabPane>

        <NTabPane name="deployments" tab="部署历史">
          <div class="deployments-pane">
            <NDataTable
              flex-height
              :columns="columns"
              :data="deployments"
              :loading="loading"
              :row-key="(r: Deployment) => r.id"
              :pagination="{
                page: page,
                pageSize: pageSize,
                itemCount: total,
                showSizePicker: true,
                pageSizes: [10, 20, 50],
                'onUpdate:page': (p: number) => (page = p),
                'onUpdate:pageSize': (s: number) => { pageSize = s; page = 1 },
              }"
              remote
            />
          </div>
        </NTabPane>

        <NTabPane name="proxy" tab="域名与反代">
          <div class="overview-scroll">
            <NAlert
              v-if="app.domain"
              type="success"
              :show-icon="true"
              style="margin-bottom: 14px"
            >
              <template #header>已绑定 {{ app.domain }}</template>
              反代模式：{{ app.proxy_mode === 'gateway' ? '网关统一入口' : '节点本地' }}，上游：{{ app.proxy_upstream }}
            </NAlert>
            <NAlert
              v-else
              type="default"
              :show-icon="false"
              style="margin-bottom: 14px"
            >
              尚未绑定域名。绑定后 Watchman 会自动把证书与 Nginx 反代配置下发到网关主机并热加载。
            </NAlert>

            <NForm label-placement="top" style="max-width: 640px">
              <NFormItem label="域名" required>
                <NInput v-model:value="proxyForm.domain" placeholder="api.example.com" />
              </NFormItem>
              <NFormItem label="反代模式">
                <NSpace vertical style="width: 100%">
                  <NRadioGroup v-model:value="proxyForm.mode">
                    <NRadio value="local">节点本地（应用所在主机自建 Nginx）</NRadio>
                    <NRadio value="gateway">统一网关（公网网关主机反代穿透到应用）</NRadio>
                  </NRadioGroup>
                  <NSelect
                    v-if="proxyForm.mode === 'gateway'"
                    v-model:value="proxyForm.gateway_host_id"
                    :options="hostOptions"
                    placeholder="选择运行 watchman-app-nginx 的网关主机"
                    filterable
                  />
                </NSpace>
              </NFormItem>
              <NFormItem label="证书">
                <NSelect v-model:value="proxyForm.cert_id" :options="certOptions" />
              </NFormItem>
              <NFormItem label="上游地址（可选，默认自动计算）">
                <NInput v-model:value="proxyForm.upstream" placeholder="127.0.0.1:8080" />
              </NFormItem>
              <NFormItem label="WebSocket 支持">
                <NSwitch v-model:value="proxyForm.websocket" />
              </NFormItem>
              <NSpace>
                <NButton type="primary" :loading="savingProxy" @click="doBindProxy">
                  下发并绑定
                </NButton>
                <NPopconfirm v-if="app.domain" @positive-click="doUnbindProxy">
                  <template #trigger>
                    <NButton secondary type="error" :loading="savingProxy">解除绑定</NButton>
                  </template>
                  确定解除 {{ app.domain }} 的反代绑定？
                </NPopconfirm>
              </NSpace>
            </NForm>
          </div>
        </NTabPane>

        <NTabPane v-if="app.source_type === 'raw_compose' || app.compose_content" name="compose" tab="Compose 编排">
          <div class="overview-scroll" style="max-width: 800px">
            <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
              当前应用为 Docker Compose 复合微服务栈。您可直接在此修改 YAML 编排内容，保存后系统会自动在目标主机更新配置并重新发布。
            </NAlert>
            <NInput
              v-model:value="composeEditContent"
              type="textarea"
              :rows="16"
              style="font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px"
            />
            <NSpace style="margin-top: 12px" justify="end">
              <NButton type="primary" :loading="savingCompose || deploying" @click="saveAndRedeployCompose">
                保存并重新部署
              </NButton>
            </NSpace>
          </div>
        </NTabPane>

        <NTabPane name="env" tab="环境变量">
          <div class="overview-scroll">
            <template v-if="app.env_vars && Object.keys(app.env_vars).length > 0">
              <div v-for="(v, k) in app.env_vars" :key="k" class="env-row">
                <NTag size="small" bordered>{{ k }}</NTag>
                <span class="mono env-value">{{ v.includes('vault:') ? '••••（Vault 引用）' : v }}</span>
              </div>
            </template>
            <NAlert v-else type="default" :show-icon="false">未配置环境变量</NAlert>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <NModal
      v-model:show="showLog"
      preset="card"
      :title="`构建日志 · ${logDeployment?.commit_hash ?? ''}`"
      style="width: 860px; max-width: 94vw"
    >
      <div class="log-body">
        <pre class="log-pre">{{ logDeployment?.build_log || '（此部署记录没有日志）' }}</pre>
        <NAlert v-if="logDeployment?.error" type="error" style="margin-top: 10px">
          {{ logDeployment.error }}
        </NAlert>
      </div>
    </NModal>

    <!-- AI 部署排障诊断弹窗 -->
    <NModal
      v-model:show="showAIDiagnosis"
      preset="card"
      title="AI 智能部署排障诊断"
      style="width: 720px; max-width: 94vw"
    >
      <div v-if="diagnosing" style="text-align: center; padding: 30px">
        <NSpace vertical align="center">
          <NIcon size="36" color="#18a058"><SparklesOutline /></NIcon>
          <span>AI 正在深入分析构建报错、退出码与系统上下文...</span>
        </NSpace>
      </div>
      <div v-else-if="aiResult" class="ai-diag-body">
        <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
          <template #header>根因总结</template>
          {{ aiResult.summary }}
        </NAlert>

        <div v-if="aiResult.suggestions && aiResult.suggestions.length > 0" style="margin-bottom: 14px">
          <div style="font-weight: 600; margin-bottom: 6px">修复与排查建议：</div>
          <ul style="padding-left: 20px; margin: 0">
            <li v-for="(s, idx) in aiResult.suggestions" :key="idx" style="margin-bottom: 4px">{{ s }}</li>
          </ul>
        </div>

        <div v-if="aiResult.commands && aiResult.commands.length > 0">
          <div style="font-weight: 600; margin-bottom: 6px">推荐执行命令：</div>
          <div v-for="(cmd, idx) in aiResult.commands" :key="idx" class="cmd-box">
            <NCode :code="cmd" />
            <NButton size="tiny" secondary @click="copyToClipboard(cmd).then(() => message.success('已复制命令'))">复制</NButton>
          </div>
        </div>
      </div>
      <div v-else style="padding: 20px; text-align: center; color: #999">
        暂无诊断分析结果
      </div>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.app-detail-view {
  gap: 10px;
}

.detail-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 12px;
}

.app-title {
  font-size: 16px;
  font-weight: 600;
}

.detail-body {
  flex: 1;
  min-height: 0;
  overflow: hidden;
}

.overview-scroll {
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 8px 4px;
}

.deployments-pane {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.mono {
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
}

.cmd-box {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  background: rgba(128, 128, 128, 0.08);
  padding: 6px 10px;
  border-radius: 6px;
  margin-bottom: 6px;
}

.webhook-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 6px;
}

.env-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 0;

  .env-value {
    color: var(--n-text-color);
  }
}

.log-body {
  max-height: 60vh;
  display: flex;
  flex-direction: column;
}

.log-pre {
  flex: 1;
  overflow: auto;
  overscroll-behavior: contain;
  background: rgba(128, 128, 128, 0.08);
  border-radius: 6px;
  padding: 12px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}

:deep(.n-tabs .n-tab-pane) {
  height: 100%;
  min-height: 0;
}

:deep(.n-tabs-pane-wrapper) {
  flex: 1;
  min-height: 0;
}
</style>