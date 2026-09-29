<script setup lang="ts">
import { onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCollapse,
  NCollapseItem,
  NDivider,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NPopconfirm,
  NRadio,
  NRadioGroup,
  NSpace,
  NSpin,
  NSwitch,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import {
  CheckmarkCircleOutline,
  GlobeOutline,
  LockClosedOutline,
  RefreshOutline,
  ShieldCheckmarkOutline,
} from '@vicons/ionicons5'
import {
  detectPublicIP,
  getPanelCertJob,
  importCert,
  issuePanelCert,
  confirmPanelCertIssue,
  cancelPanelCertIssue,
  type ChallengeMode,
  type PanelCertJob,
} from '../../api/certs'
import {
  SETTING_KEYS,
  getPanelCertStatus,
  getSystemSettings,
  saveSystemSettings,
  type PanelCertStatus,
} from '../../api/settings'

// 系统设置 → 面板域名与证书（仅管理员可见）。
// 宝塔式简化模型：
//  1. 绑定域名 → 面板只能通过该域名访问（自动严格，无独立开关）；不绑定 → 通过 IP 访问。
//  2. 证书：已绑定域名 → 一键签发域名证书；未绑定 → 为公网 IP 签发免费证书；
//     也支持手动上传已有证书。签发成功后自动绑定并启用 HTTPS。
defineOptions({ name: 'CertDomain' })

const dialog = useDialog()
const message = useMessage()
const loading = ref(false)
const certStatus = ref<PanelCertStatus | null>(null)

const domainInput = ref('')
const ipInput = ref('')
const ipDetecting = ref(false)
const customCert = reactive({ cert_pem: '', key_pem: '' })
const uploading = ref(false)

// 签发任务轮询
const job = ref<PanelCertJob | null>(null)
const jobTimer = ref<number | null>(null)

function stopJobPoll() {
  if (jobTimer.value !== null) {
    window.clearInterval(jobTimer.value)
    jobTimer.value = null
  }
}

async function refreshStatus() {
  try {
    certStatus.value = await getPanelCertStatus()
    domainInput.value = certStatus.value.panel_domain || ''
  } catch {
    // ignore
  }
}

async function loadData() {
  loading.value = true
  stopJobPoll()
  job.value = null
  try {
    const [settingsRes, statusRes] = await Promise.allSettled([
      getSystemSettings(),
      getPanelCertStatus(),
    ])
    if (settingsRes.status === 'fulfilled') {
      const data = settingsRes.value.data
      domainInput.value = (data[SETTING_KEYS.panelDomain] as string) || ''
    }
    if (statusRes.status === 'fulfilled') {
      certStatus.value = statusRes.value
      domainInput.value = statusRes.value.panel_domain || domainInput.value
    }
    // 未绑定域名时，自动检测公网 IP 供 IP 证书使用
    if (!certStatus.value?.panel_domain) {
      void detectIP()
    }
  } catch (e: any) {
    message.error(e?.message || '加载配置数据失败')
  } finally {
    loading.value = false
  }
}

async function detectIP() {
  if (ipInput.value || ipDetecting.value) return
  ipDetecting.value = true
  try {
    ipInput.value = await detectPublicIP()
  } catch {
    // 检测失败则留空，由用户手动填写
  } finally {
    ipDetecting.value = false
  }
}

async function bindDomain() {
  const v = domainInput.value.trim()
  if (!v) {
    message.warning('请先填写要绑定的域名')
    return
  }
  try {
    await saveSystemSettings({ [SETTING_KEYS.panelDomain]: v })
    message.success(`已绑定域名 ${v}，面板现在只能通过该域名访问`)
    await refreshStatus()
  } catch (e: any) {
    message.error(e?.message || '绑定域名失败')
  }
}

function unbindDomain() {
  dialog.warning({
    title: '解绑面板域名',
    content: '解绑后面板将恢复为通过 IP 访问，且不再拦截直接 IP 访问。确定继续？',
    positiveText: '确定解绑',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await saveSystemSettings({ [SETTING_KEYS.panelDomain]: '' })
        domainInput.value = ''
        message.success('已解绑，面板恢复 IP 访问')
        await refreshStatus()
        void detectIP()
      } catch (e: any) {
        message.error(e?.message || '解绑失败')
      }
    },
  })
}

async function setSSLEnabled(v: boolean) {
  try {
    await saveSystemSettings({ [SETTING_KEYS.panelSSLEnabled]: v })
    message.success(v ? '已启用面板 HTTPS' : '已关闭面板 HTTPS')
    await refreshStatus()
  } catch (e: any) {
    message.error(e?.message || '保存失败')
    await refreshStatus()
  }
}

async function setForceHTTPS(v: boolean) {
  if (v && !certStatus.value?.ssl_enabled) {
    message.warning('请先启用面板 HTTPS，再开启强制跳转')
    await refreshStatus()
    return
  }
  try {
    await saveSystemSettings({ [SETTING_KEYS.panelForceHTTPS]: v })
    message.success(v ? '已开启强制 HTTPS 跳转' : '已关闭强制 HTTPS 跳转')
    await refreshStatus()
  } catch (e: any) {
    message.error(e?.message || '保存失败')
    await refreshStatus()
  }
}

function pollJob(jobId: string) {
  stopJobPoll()
  let rounds = 0
  jobTimer.value = window.setInterval(async () => {
    rounds += 1
    try {
      const j = await getPanelCertJob(jobId)
      job.value = j
      if (j.status === 'awaiting_dns') {
        // 手动 DNS-01：停止轮询，等待管理员添加 TXT 解析后手动确认
        stopJobPoll()
        message.info('签发任务已创建，请按指引手动添加 TXT 解析记录后再确认验证')
        return
      }
      if (j.status !== 'running') {
        stopJobPoll()
        if (j.status === 'done') {
          message.success('证书签发成功，已自动绑定并启用 HTTPS（证书已进入证书中心「已签发证书」列表）')
        } else {
          message.error(j.error || '证书签发失败')
        }
        await refreshStatus()
      } else if (rounds >= 60) {
        // 约 3 分钟仍未完成则停止轮询，任务在后台继续，可稍后刷新查看
        stopJobPoll()
        message.warning('签发仍在进行中，请稍后点击刷新查看结果')
        await refreshStatus()
      }
    } catch (e: any) {
      stopJobPoll()
      message.error(e?.message || '查询签发进度失败')
    }
  }, 3000)
}

const panelChallenge = ref<ChallengeMode>('')

const confirming = ref(false)
const cancelling = ref(false)

// 手动 DNS-01：管理员已添加 TXT 解析，确认后通知 CA 开始验证
async function confirmJob() {
  if (!job.value) return
  confirming.value = true
  try {
    const r = await confirmPanelCertIssue(job.value.id)
    job.value = r.data
    message.info(r.message || '已通知 CA 开始验证')
    pollJob(r.data.id)
  } catch (e: any) {
    message.error(e?.message || '确认验证失败')
  } finally {
    confirming.value = false
  }
}

// 取消待处理的手动 DNS-01 面板签发任务（同时取消证书中心订单）
async function cancelJob() {
  if (!job.value) return
  cancelling.value = true
  try {
    const r = await cancelPanelCertIssue(job.value.id)
    message.success(r.message || '已取消签发任务')
    stopJobPoll()
    job.value = null
  } catch (e: any) {
    message.error(e?.message || '取消任务失败')
  } finally {
    cancelling.value = false
  }
}

async function copyText(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`${label}已复制`)
  } catch {
    message.warning('自动复制失败，请手动选中复制')
  }
}

async function startIssue(mode: 'domain' | 'ip') {
  if (job.value?.status === 'running') {
    message.warning('已有签发任务在进行中，请稍候')
    return
  }
  const ip = mode === 'ip' ? ipInput.value.trim() : undefined
  if (mode === 'ip' && !ip) {
    message.warning('请先填写或等待检测公网 IP')
    return
  }
  try {
    const r = await issuePanelCert(mode, ip, mode === 'domain' ? panelChallenge.value || undefined : undefined)
    job.value = r.data
    message.info(r.message || '签发任务已启动')
    pollJob(r.data.id)
  } catch (e: any) {
    message.error(e?.message || '启动签发任务失败')
  }
}

function challengeLabel(j: PanelCertJob | null): string {
  if (!j) return ''
  if (j.challenge === 'dns-01') return 'DNS-01 验证'
  if (j.challenge === 'dns-01-manual') return 'DNS-01 手动解析验证'
  if (j.challenge === 'http-01') return 'HTTP-01 验证'
  return j.mode === 'ip' ? 'HTTP-01 验证' : '自动选择（HTTP-01 优先）'
}

async function uploadCustomCert() {
  if (!customCert.cert_pem.trim() || !customCert.key_pem.trim()) {
    message.warning('请填写完整的证书内容与私钥内容')
    return
  }
  uploading.value = true
  try {
    // 先导入证书中心（服务端校验 PEM、入库），证书进入已签发证书列表；
    // 再把面板绑定到该证书（cert_center 模式），私钥不再重复存入 settings。
    const crt = await importCert(customCert.cert_pem.trim(), customCert.key_pem.trim())
    await saveSystemSettings({
      [SETTING_KEYS.panelSSLMode]: 'cert_center',
      [SETTING_KEYS.panelSSLCertID]: crt.id,
      [SETTING_KEYS.panelSSLEnabled]: true,
    })
    message.success(`自定义证书已导入证书中心并启用 HTTPS（${crt.domains.join('、') || crt.id}）`)
    customCert.cert_pem = ''
    customCert.key_pem = ''
    await refreshStatus()
  } catch (e: any) {
    message.error(e?.message || '上传证书失败')
  } finally {
    uploading.value = false
  }
}

onMounted(loadData)
onActivated(loadData)
onDeactivated(stopJobPoll)
onUnmounted(stopJobPoll)
</script>

<template>
  <NCard :bordered="false" size="small">
    <template #header>
      <span style="font-size: 16px; font-weight: 700">
        <NIcon style="vertical-align: middle; margin-right: 6px"><LockClosedOutline /></NIcon>
        面板域名与证书
      </span>
    </template>
    <template #header-extra>
      <NButton size="small" quaternary :loading="loading" @click="loadData">
        <template #icon><NIcon><RefreshOutline /></NIcon></template>
        刷新
      </NButton>
    </template>

    <NSpin :show="loading">
      <NSpace vertical size="large">
        <!-- 1. 域名绑定：绑定即严格，只能通过域名访问 -->
        <div>
          <div class="section-title">
            <NIcon style="vertical-align: middle; margin-right: 6px"><GlobeOutline /></NIcon>
            1. 面板域名绑定
          </div>

          <div v-if="certStatus?.panel_domain" class="bound-row">
            <NTag type="success" size="medium">
              <template #icon><NIcon><CheckmarkCircleOutline /></NIcon></template>
              已绑定：{{ certStatus.panel_domain }}
            </NTag>
            <span class="muted">面板只能通过该域名访问，直接使用 IP 访问将被拦截（回环地址除外）。<br />注意：绑定后「系统升级」中批量升级 Agent 时，安装包下载地址将使用该域名，请确保 Agent 所在主机能解析该域名；纯内网环境可在服务端启动参数加 <code>-public-url http://&lt;内网IP&gt;:18789</code> 覆盖为 IP 地址。</span>
            <NButton size="small" type="error" ghost @click="unbindDomain">解绑</NButton>
          </div>
          <NForm v-else label-placement="top" :show-feedback="false">
            <NFormItem label="绑定域名后，面板只能通过该域名访问">
              <NSpace style="width: 100%">
                <NInput
                  v-model:value="domainInput"
                  placeholder="例如：panel.example.com"
                  clearable
                  style="flex: 1"
                />
                <NButton type="primary" @click="bindDomain">绑定域名</NButton>
              </NSpace>
              <template #feedback>
                <span class="muted tip-hint">
                  绑定后自动启用严格域名限制：任何通过服务器 IP 或其他域名发起的面板请求将被 403 拦截；
                  Agent 注册与数据接口不受影响。不绑定则保持通过 IP 访问。<br />
                  注意：绑定后「系统升级」中批量升级 Agent 时，安装包下载地址将使用该域名，请确保 Agent 所在主机能解析该域名；
                  纯内网环境可在服务端启动参数加 <code>-public-url http://&lt;内网IP&gt;:18789</code> 覆盖为 IP 地址。
                </span>
              </template>
            </NFormItem>
          </NForm>

          <NAlert type="warning" :show-icon="true" class="emergency-alert">
            <template #header>域名防失联与紧急恢复</template>
            若绑定域名后无法访问面板（如 DNS 未生效），可在服务器终端执行：
            <code>watchman-server -reset-panel-domain -data /opt/watchman/data</code>
            立即解绑域名、恢复 IP 访问。
          </NAlert>
        </div>

        <NDivider style="margin: 4px 0" />

        <!-- 2. 证书 -->
        <div>
          <div class="section-title">
            <NIcon style="vertical-align: middle; margin-right: 6px"><LockClosedOutline /></NIcon>
            2. 面板证书
          </div>

          <!-- 当前证书状态 -->
          <div v-if="certStatus?.active" class="cert-info-grid">
            <div class="info-row">
              <span class="label">生效状态：</span>
              <NTag type="success" size="small">
                <template #icon><NIcon><CheckmarkCircleOutline /></NIcon></template>
                HTTPS 正常运行中
              </NTag>
            </div>
            <div class="info-row">
              <span class="label">证书来源：</span>
              <span>{{ certStatus.source || '-' }}</span>
            </div>
            <div class="info-row">
              <span class="label">证书主题：</span>
              <span class="monospace-text">{{ certStatus.subject || '-' }}</span>
            </div>
            <div class="info-row">
              <span class="label">颁发者：</span>
              <span class="monospace-text">{{ certStatus.issuer || '-' }}</span>
            </div>
            <div class="info-row">
              <span class="label">关联域名 / IP：</span>
              <span>{{ certStatus.dns_names?.length ? certStatus.dns_names.join(', ') : '-' }}</span>
            </div>
            <div class="info-row">
              <span class="label">有效截止：</span>
              <span>
                {{ certStatus.not_after ? certStatus.not_after.slice(0, 10) : '-' }}
                <NTag
                  :type="certStatus.days_left > 15 ? 'info' : (certStatus.days_left > 0 ? 'warning' : 'error')"
                  size="small"
                  style="margin-left: 8px;"
                >
                  剩余 {{ certStatus.days_left }} 天
                </NTag>
              </span>
            </div>
          </div>
          <NAlert v-else type="warning" :show-icon="true" size="small" style="margin-bottom: 12px">
            {{ certStatus?.error || '当前未启用 HTTPS，面板以 HTTP 明文运行。' }}
          </NAlert>

          <!-- 一键签发 -->
          <div class="issue-block">
            <NAlert type="info" :show-icon="true" size="small" style="margin-bottom: 12px">
              签发使用「证书中心」的默认 ACME 账户；账户联系邮箱为选填，留空则不向 CA 提交。
              如签发报错 invalidContact，请到「证书中心」检查账户邮箱是否为有效公网域名邮箱。
            </NAlert>
            <template v-if="certStatus?.panel_domain">
              <NSpace align="center" style="width: 100%">
                <NButton
                  type="primary"
                  :loading="job?.status === 'running'"
                  @click="startIssue('domain')"
                >
                  <template #icon><NIcon><ShieldCheckmarkOutline /></NIcon></template>
                  为绑定域名申请免费证书
                </NButton>
                <NRadioGroup v-model:value="panelChallenge" size="small">
                  <NSpace :size="8">
                    <NRadio value="">自动</NRadio>
                    <NRadio value="http-01">HTTP-01</NRadio>
                    <NRadio value="dns-01">DNS-01</NRadio>
                    <NRadio value="dns-01-manual">手动解析</NRadio>
                  </NSpace>
                </NRadioGroup>
              </NSpace>
              <p class="muted tip-hint" style="margin: 8px 0 0">
                通过证书中心 ACME 为 {{ certStatus.panel_domain }} 签发证书，成功后自动绑定并启用 HTTPS。
                自动：优先 HTTP-01（需 80 端口可被 CA 访问），失败时回退 DNS-01；选择 DNS-01 需先在「证书中心」配置并启用 dns-mng；
                选择「手动解析」则由您到 DNS 服务商手动添加 TXT 记录，无需 dns-mng。
              </p>
            </template>
            <template v-else>
              <NSpace align="center" style="width: 100%">
                <NInput
                  v-model:value="ipInput"
                  placeholder="公网 IP，留空自动检测"
                  clearable
                  style="max-width: 280px"
                  :loading="ipDetecting"
                />
                <NButton
                  type="primary"
                  :loading="job?.status === 'running'"
                  @click="startIssue('ip')"
                >
                  <template #icon><NIcon><ShieldCheckmarkOutline /></NIcon></template>
                  为 IP 申请免费证书
                </NButton>
                <NButton text size="small" @click="detectIP" :loading="ipDetecting">重新检测 IP</NButton>
              </NSpace>
              <p class="muted tip-hint" style="margin: 8px 0 0">
                为公网 IP 签发 Let's Encrypt 免费证书（HTTP-01 验证，签发期间需临时开放 80 端口），
                成功后自动绑定并启用 HTTPS。证书有效期较短，系统会自动续期。
              </p>
            </template>

            <NAlert
              v-if="job?.status === 'awaiting_dns'"
              type="warning"
              :show-icon="true"
              size="small"
              style="margin-top: 12px"
            >
              <template #header>请手动添加以下 TXT 解析记录</template>
              <div
                v-for="rec in job.dns_records || []"
                :key="rec.host"
                class="dns-rec-block"
              >
                <div class="dns-rec-line">
                  <span class="label">标识符</span>
                  <NTag size="small" bordered>{{ rec.domain }}</NTag>
                </div>
                <div class="dns-rec-line">
                  <span class="label">记录类型</span>
                  <NTag size="small" type="info" bordered>TXT</NTag>
                </div>
                <div class="dns-rec-line">
                  <span class="label">主机记录</span>
                  <code class="mono">{{ rec.host }}</code>
                  <NButton size="tiny" @click="copyText(rec.host, '主机记录')">复制</NButton>
                </div>
                <div class="dns-rec-line">
                  <span class="label">记录值</span>
                  <code class="mono">{{ rec.value }}</code>
                  <NButton size="tiny" @click="copyText(rec.value, '记录值')">复制</NButton>
                </div>
              </div>
              <p class="muted tip-hint" style="margin: 8px 0">
                请到您的 DNS 服务商添加以上记录，等待解析生效后再点击下方按钮通知 CA 验证。
                任务保留 2 小时，超时需重新发起。
              </p>
              <NButton type="primary" size="small" :loading="confirming" @click="confirmJob">
                <template #icon><NIcon><ShieldCheckmarkOutline /></NIcon></template>
                我已完成解析，开始验证
              </NButton>
              <NPopconfirm @positive-click="cancelJob">
                <template #trigger>
                  <NButton size="small" :disabled="cancelling" style="margin-left: 8px">取消任务</NButton>
                </template>
                确定取消该面板证书签发任务？CA 侧订单将自然过期，已添加的 TXT 记录需自行到 DNS 服务商删除。
              </NPopconfirm>
            </NAlert>

            <NAlert
              v-if="job?.status === 'running'"
              type="info"
              :show-icon="true"
              size="small"
              style="margin-top: 12px"
            >
              正在为 {{ job.target }} 签发证书（{{ challengeLabel(job) }}），
              通常需要几十秒，请稍候……
            </NAlert>

            <NAlert
              v-if="job?.status === 'error'"
              type="error"
              :show-icon="true"
              size="small"
              style="margin-top: 12px"
            >
              签发失败：{{ job.error }}
              <template v-if="job.terminal">该订单已终态失败，不可重试，请重新发起签发。</template>
              <NButton
                v-if="job.challenge === 'dns-01-manual' && !job.terminal"
                size="small"
                type="primary"
                ghost
                :loading="confirming"
                style="margin-left: 8px"
                @click="confirmJob"
              >
                检查解析后重新验证
              </NButton>
            </NAlert>
          </div>

          <NDivider dashed style="margin: 16px 0" />

          <!-- 开关 -->
          <NForm label-placement="top" :show-feedback="false">
            <NFormItem label="启用面板 HTTPS" class="switch-item">
              <NSpace align="center" size="small">
                <NSwitch
                  :value="!!certStatus?.ssl_enabled"
                  @update:value="setSSLEnabled"
                />
                <span class="switch-label">
                  {{ certStatus?.ssl_enabled ? '已启用（支持 HTTPS 安全连接）' : '未启用（仅 HTTP 明文运行）' }}
                </span>
              </NSpace>
            </NFormItem>
            <NFormItem label="强制 HTTPS 访问" class="switch-item">
              <NSpace align="center" size="small">
                <NSwitch
                  :value="!!certStatus?.force_https"
                  :disabled="!certStatus?.ssl_enabled"
                  @update:value="setForceHTTPS"
                />
                <span class="switch-label">
                  {{ certStatus?.force_https ? '已开启（HTTP 自动 301 跳转到 HTTPS）' : '未开启' }}
                </span>
              </NSpace>
            </NFormItem>
          </NForm>

          <!-- 上传已有证书 -->
          <NCollapse style="margin-top: 8px">
            <NCollapseItem title="上传已有证书（其他渠道申请的证书，自动进入证书中心）" name="upload">
              <NForm label-placement="top" :show-feedback="false">
                <NFormItem label="证书内容 (PEM)">
                  <NInput
                    v-model:value="customCert.cert_pem"
                    type="textarea"
                    :rows="6"
                    placeholder="-----BEGIN CERTIFICATE-----&#10;...&#10;-----END CERTIFICATE-----"
                  />
                </NFormItem>
                <NFormItem label="私钥内容 (PEM)">
                  <NInput
                    v-model:value="customCert.key_pem"
                    type="textarea"
                    :rows="5"
                    placeholder="-----BEGIN PRIVATE KEY-----&#10;...&#10;-----END PRIVATE KEY-----"
                  />
                </NFormItem>
                <NFormItem :show-label="false">
                  <NButton type="primary" :loading="uploading" @click="uploadCustomCert">
                    保存并启用 HTTPS
                  </NButton>
                </NFormItem>
              </NForm>
            </NCollapseItem>
          </NCollapse>

          <NAlert type="warning" :show-icon="true" class="emergency-alert">
            <template #header>SSL 异常与紧急恢复</template>
            若证书配置错误导致 HTTPS 无法访问，可在服务器终端执行：
            <code>watchman-server -reset-panel-ssl -data /opt/watchman/data</code>
            立即关闭面板 SSL，恢复 HTTP 访问。
          </NAlert>
        </div>
      </NSpace>
    </NSpin>
  </NCard>
</template>

<style scoped lang="scss">
.muted {
  color: var(--text-secondary);
  font-size: 13px;
}

.section-title {
  display: flex;
  align-items: center;
  font-size: 14px;
  font-weight: 700;
  color: var(--text-primary);
  margin-bottom: 12px;
}

.bound-row {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 4px;
}

.switch-item {
  margin-bottom: 12px;
}

.switch-label {
  font-size: 13px;
  color: var(--text-color-2);
}

.emergency-alert {
  margin-top: 16px;
  font-size: 13px;

  code {
    display: inline-block;
    padding: 2px 6px;
    margin: 2px 4px;
    background: rgba(0, 0, 0, 0.06);
    border-radius: 4px;
    font-family: monospace;
    font-size: 12px;
    color: var(--primary-color, #18a058);
  }
}

.issue-block {
  margin-top: 12px;
}

.dns-rec-block {
  margin-top: 10px;
  padding: 10px 12px;
  background: rgba(0, 0, 0, 0.03);
  border-radius: 6px;

  .dns-rec-line {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;

    &:last-child {
      margin-bottom: 0;
    }

    .label {
      flex-shrink: 0;
      width: 56px;
      font-size: 12px;
      color: var(--text-color-3);
    }

    .mono {
      flex: 1;
      min-width: 0;
      font-family: monospace;
      font-size: 12px;
      word-break: break-all;
    }
  }
}

.cert-info-grid {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 13px;
  background: var(--n-color-embedded, rgba(0, 0, 0, 0.02));
  padding: 12px 16px;
  border-radius: 6px;
  margin-bottom: 12px;

  .info-row {
    display: flex;
    align-items: baseline;
    gap: 8px;

    .label {
      color: var(--text-color-3);
      min-width: 110px;
      flex-shrink: 0;
    }
  }
}

.monospace-text {
  font-family: monospace;
  font-size: 12px;
}
</style>
