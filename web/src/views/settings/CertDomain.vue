<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
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
const router = useRouter()
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

async function refreshStatus(showError = false) {
  try {
    certStatus.value = await getPanelCertStatus()
    domainInput.value = certStatus.value.panel_domain || ''
  } catch (e: any) {
    if (showError) message.error(e?.message || '刷新面板状态失败，请刷新页面重试')
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

const failedPorts = computed(() =>
  (certStatus.value?.standard_ports || []).filter((p) => !p.listening),
)

// 当前可用的免端口入口：优先 HTTPS，其次 HTTP；都不可用时为空（主端口回退）。
const activeEntryURL = computed(() => {
  const domain = (certStatus.value?.panel_domain || '').trim()
  if (!domain) return ''
  const ports = certStatus.value?.standard_ports || []
  if (certStatus.value?.ssl_enabled && ports.some((p) => p.port === 443 && p.listening)) {
    return `https://${domain}`
  }
  if (ports.some((p) => p.port === 80 && p.listening)) return `http://${domain}`
  return ''
})

// 标准端口监听失败时的可用入口：:443 不在监听时服务端会自动跳过强制 HTTPS
// 跳转，避免把浏览器 301 到一个打不开的地址。
const availableEntryHint = computed(() => {
  const domain = (certStatus.value?.panel_domain || '').trim()
  if (!domain || !failedPorts.value.length) return ''
  if (activeEntryURL.value) {
    const hint = `面板仍可通过 ${activeEntryURL.value} 访问`
    return certStatus.value?.ssl_enabled && activeEntryURL.value.startsWith('http://')
      ? `${hint}（:443 不可用，已跳过强制 HTTPS 跳转）`
      : hint
  }
  return `免端口入口暂不可用，面板回退到 ${fallbackURL(domain)}`
})

// 绑定域名后标准端口（:80，面板 SSL 开启时含 :443）的免端口访问地址；
// 主监听端口仅在标准端口不可用时作为回退（标准端口一旦监听，主端口对远程关闭）。
function portlessURL(domain: string, scheme: 'http' | 'https' = 'http'): string {
  return `${scheme}://${domain}`
}

function fallbackURL(domain: string, scheme: 'http' | 'https' = 'http'): string {
  const port = (certStatus.value?.panel_http_port || '').trim()
  if (!port || (scheme === 'http' && port === '80') || (scheme === 'https' && port === '443')) {
    return `${scheme}://${domain}`
  }
  return `${scheme}://${domain}:${port}`
}

// 关闭 HTTPS 后要引导到的地址：域名仍绑定且 :80 在监听时优先免端口地址，
// 否则回落到「域名 + 主端口」（标准端口不可用时主端口才是合法回退）。
function preferredPanelURL(): string {
  const domain = (certStatus.value?.panel_domain || '').trim()
  const port80 = (certStatus.value?.standard_ports || []).some((p) => p.port === 80 && p.listening)
  if (domain && port80) return portlessURL(domain)
  return fallbackURL(window.location.hostname)
}

// 绑定/解绑后当前地址立即失效（严格域名限制，旧地址的接口请求会被 403，
// 无法再回读状态）或 TLS 已关闭，必须直接引导管理员到新地址，而不是留在
// 旧页面里对着 403 / 连接失败发呆。
function showMoveDialog(domain: string, opts: { title: string; sslReset?: boolean; ipCertPaused?: boolean }) {
  const primary = portlessURL(domain)
  const fallback = fallbackURL(domain)
  const children = [
    h('p', `请使用以下地址重新打开面板：`),
    h('p', { style: 'margin-top:8px;font-family:monospace;font-size:13px' }, primary),
  ]
  if (fallback !== primary) {
    children.push(
      h(
        'p',
        { style: 'margin-top:6px;font-size:12px;color:var(--text-secondary)' },
        `打不开时可在服务器本机用 ${fallback} 访问，或用解绑命令恢复 IP 访问。`,
      ),
    )
  }
  if (opts.sslReset) {
    children.push(
      h(
        'p',
        { style: 'margin-top:8px;color:#d03050;font-size:12px' },
        '原证书绑定、面板 SSL 与强制 HTTPS 已自动清除，请在新地址下重新签发或绑定证书。',
      ),
    )
  }
  if (opts.ipCertPaused) {
    children.push(
      h(
        'p',
        { style: 'margin-top:8px;font-size:12px;color:var(--text-secondary)' },
        '此前为 IP 签发的证书已停止自动续期、无用途，建议到证书中心删除。',
      ),
    )
  }
  dialog.warning({
    title: opts.title,
    content: () => h('div', { style: 'line-height:1.8' }, children),
    positiveText: '立即前往新地址',
    negativeText: '稍后手动切换',
    onPositiveClick: () => {
      window.location.href = primary
    },
  })
}

async function doBind(v: string, sslWasReset: boolean) {
  try {
    await saveSystemSettings({ [SETTING_KEYS.panelDomain]: v })
  } catch (e: any) {
    message.error(e?.message || '绑定域名失败')
    await refreshStatus(true)
    return
  }
  const current = window.location.hostname.toLowerCase()
  if (current !== v.toLowerCase()) {
    // 旧地址的后续请求会被严格域名限制拦截，这里不再回读状态。
    showMoveDialog(v, { title: '面板域名已绑定', sslReset: sslWasReset, ipCertPaused: true })
    return
  }
  await refreshStatus()
  message.success(`已绑定域名 ${v}`)
  if (sslWasReset) {
    message.info('面板 SSL 与证书绑定已清除，请重新签发或绑定证书')
  }
}

function bindDomain() {
  const v = domainInput.value.trim()
  if (!v) {
    message.warning('请先填写要绑定的域名')
    return
  }
  const current = (certStatus.value?.panel_domain || '').trim()
  const changing = current !== '' && current.toLowerCase() !== v.toLowerCase()
  const sslActive = !!(certStatus.value?.ssl_enabled || certStatus.value?.force_https)
  const children = [
    h(
      'p',
      `绑定后只能通过 ${v} 访问（IP / 其他域名返回 403）。请确认 DNS 已解析到本服务器。`,
    ),
    // IP 证书在绑定后失去唯一入口，恒提示建议删除（不挂在 SSL 分支下）。
    h('p', { style: 'margin-top:8px' }, [
      '此前为 IP 签发的证书将停止自动续期、无用途，建议到',
      h(
        'a',
        {
          style: 'color:var(--primary-color);cursor:pointer',
          onClick: () => {
            // 先关闭确认弹窗再跳转，避免弹窗滞留在证书中心页面上。
            dialog.destroyAll()
            router.push('/certs')
          },
        },
        '证书中心',
      ),
      '删除（解绑后可重新申请）。',
    ]),
  ]
  if (changing) {
    children.push(h('p', { style: 'margin-top:8px' }, `当前绑定域名为 ${current}，将切换为 ${v}。`))
  }
  if (sslActive) {
    children.push(
      h(
        'p',
        { style: 'margin-top:8px;color:#d03050' },
        '当前已开启面板 SSL / 强制 HTTPS：更换域名会清除原证书绑定、面板 SSL 与强制 HTTPS，请在新域名下重新签发或绑定证书。',
      ),
    )
  }
  dialog.warning({
    title: changing ? '更换面板域名' : '绑定面板域名',
    content: () => h('div', { style: 'line-height:1.8' }, children),
    positiveText: changing ? '确认更换' : '确认绑定',
    negativeText: '取消',
    onPositiveClick: () => doBind(v, sslActive),
  })
}

function unbindDomain() {
  const sslActive = !!(certStatus.value?.ssl_enabled || certStatus.value?.force_https)
  const children = [
    h('p', '解绑后恢复 IP 访问，不再拦截直接 IP 访问。'),
  ]
  if (sslActive) {
    children.push(
      h(
        'p',
        { style: 'margin-top:8px;color:#d03050' },
        '解绑会同步关闭面板 SSL、清除证书绑定与强制 HTTPS，:443 HTTPS 监听也会停止。',
      ),
    )
  }
  dialog.warning({
    title: '解绑面板域名',
    content: () => h('div', { style: 'line-height:1.8' }, children),
    positiveText: '确定解绑',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await saveSystemSettings({ [SETTING_KEYS.panelDomain]: '' })
        domainInput.value = ''
        await refreshStatus()
        message.success('已解绑，面板恢复 IP 访问')
        void detectIP()
        if (window.location.protocol === 'https:') {
          const target = fallbackURL(window.location.hostname)
          dialog.info({
            title: 'HTTPS 已关闭，请切换访问地址',
            content: () =>
              h('div', { style: 'line-height:1.8' }, [
                h('p', '面板 SSL 已随解绑关闭，请使用以下地址访问：'),
                h('p', { style: 'margin-top:8px;font-family:monospace;font-size:13px' }, target),
              ]),
            positiveText: '立即前往',
            negativeText: '稍后手动切换',
            onPositiveClick: () => {
              window.location.href = target
            },
          })
        }
      } catch (e: any) {
        message.error(e?.message || '解绑失败')
      }
    },
  })
}

async function setSSLEnabled(v: boolean) {
  if (!v && window.location.protocol === 'https:') {
    dialog.warning({
      title: '关闭面板 HTTPS',
      content:
        '当前页面通过 HTTPS 访问，关闭后本页面将无法继续使用。确定关闭？',
      positiveText: '确定关闭',
      negativeText: '取消',
      onPositiveClick: async () => {
        try {
          await saveSystemSettings({ [SETTING_KEYS.panelSSLEnabled]: v })
          message.success('已关闭面板 HTTPS')
          const target = preferredPanelURL()
          dialog.info({
            title: 'HTTPS 已关闭，请切换访问地址',
            content: () =>
              h('div', { style: 'line-height:1.8' }, [
                h('p', '请使用以下地址继续访问面板：'),
                h('p', { style: 'margin-top:8px;font-family:monospace;font-size:13px' }, target),
              ]),
            positiveText: '立即前往',
            negativeText: '稍后手动切换',
            onPositiveClick: () => {
              window.location.href = target
            },
          })
        } catch (e: any) {
          message.error(e?.message || '保存失败')
          await refreshStatus(true)
        }
      },
    })
    return
  }
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
  if (v && !certStatus.value?.active) {
    message.warning('当前没有生效的 HTTPS 证书，开启后 HTTP 将被重定向到不可用的 HTTPS；请先签发或绑定证书')
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
        message.info('签发任务已创建，请添加 TXT 解析记录后再确认验证')
        return
      }
      if (j.status !== 'running') {
        stopJobPoll()
        if (j.status === 'done') {
          message.success('证书签发成功，已自动绑定并启用 HTTPS')
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
            <NTag
              v-for="p in certStatus.standard_ports || []"
              :key="p.port"
              size="small"
              :type="p.listening ? 'success' : 'warning'"
              :title="p.error || ''"
            >
              :{{ p.port }} {{ p.listening ? '已监听' : '监听失败' }}
            </NTag>
            <NButton size="small" type="error" ghost @click="unbindDomain">解绑</NButton>

            <NAlert v-if="failedPorts.length" type="warning" :show-icon="true" class="port-alert">
              <template #header>标准端口监听失败</template>
              <div v-for="p in failedPorts" :key="p.port">
                <code>:{{ p.port }}</code> {{ p.error }}
              </div>
              <div v-if="availableEntryHint" class="port-error-hint">{{ availableEntryHint }}。</div>
            </NAlert>
          </div>
          <p v-if="certStatus?.panel_domain" class="muted tip-hint bound-tip">
            仅可通过该域名访问（IP / 其他域名返回 403）。
            <template v-if="activeEntryURL">免端口入口：<code>{{ activeEntryURL }}</code>。</template>
            Agent 安装包地址使用该域名，内网请确保能解析。
          </p>
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
            </NFormItem>
          </NForm>
          <p class="muted tip-hint" style="margin: 8px 0 0">
            自动提供 :80（HTTPS 开启时含 :443）免端口访问；更换域名会清除原证书绑定并关闭 HTTPS。
          </p>

          <NAlert type="warning" :show-icon="true" class="emergency-alert">
            <template #header>无法访问面板？</template>
            在服务器终端执行
            <code>watchman-server -reset-panel-domain -data /opt/watchman/data</code>
            解绑域名恢复 IP 访问，重启后生效。
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
              默认 ACME 账户签发；invalidContact 报错请检查账户邮箱是否为有效公网域名邮箱。
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
                签发成功后自动绑定并启用 HTTPS。自动模式优先 HTTP-01（需 :80 可被 CA 访问），失败回退 DNS-01
                （需启用 dns-mng）；「手动解析」需自行添加 TXT 记录。
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
                HTTP-01 验证，签发期间需临时开放 :80；成功后自动绑定并启用 HTTPS，证书有效期较短、自动续期。
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
                到 DNS 服务商添加以上记录，解析生效后点击下方按钮通知 CA 验证；任务保留 2 小时，超时需重新发起。
              </p>
              <NButton type="primary" size="small" :loading="confirming" @click="confirmJob">
                <template #icon><NIcon><ShieldCheckmarkOutline /></NIcon></template>
                我已完成解析，开始验证
              </NButton>
              <NPopconfirm @positive-click="cancelJob">
                <template #trigger>
                  <NButton size="small" :disabled="cancelling" style="margin-left: 8px">取消任务</NButton>
                </template>
                取消后 CA 侧订单将过期，已添加的 TXT 记录需自行删除。
              </NPopconfirm>
            </NAlert>

            <NAlert
              v-if="job?.status === 'running'"
              type="info"
              :show-icon="true"
              size="small"
              style="margin-top: 12px"
            >
              正在为 {{ job.target }} 签发证书（{{ challengeLabel(job) }}），请稍候……
            </NAlert>

            <NAlert
              v-if="job?.status === 'error'"
              type="error"
              :show-icon="true"
              size="small"
              style="margin-top: 12px"
            >
              签发失败：{{ job.error }}
              <template v-if="job.terminal">该订单已失败，不可重试，请重新发起。</template>
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
                  {{ certStatus?.ssl_enabled ? '已启用' : '未启用（仅 HTTP）' }}
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
                  {{ certStatus?.force_https ? '已开启（HTTP 自动跳转到 HTTPS）' : '未开启' }}
                </span>
              </NSpace>
            </NFormItem>
          </NForm>
          <p class="muted tip-hint" style="margin: 8px 0 0; font-size: 12px">
            启用后同时在 :443 提供免端口 HTTPS（需已绑定域名）；端口被占用时上方会显示占用进程。
          </p>

          <!-- 上传已有证书 -->
          <NCollapse class="upload-cert-collapse" style="margin-top: 8px">
            <NCollapseItem title="上传已有证书（自动进入证书中心）" name="upload">
              <NForm class="upload-cert-form" label-placement="top" :show-feedback="false">
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
            <template #header>HTTPS 无法访问？</template>
            在服务器终端执行
            <code>watchman-server -reset-panel-ssl -data /opt/watchman/data</code>
            关闭面板 SSL 恢复 HTTP 访问，重启后生效。
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

  .port-alert {
    width: 100%;
    margin-top: 4px;

    code {
      margin-right: 4px;
    }
  }

  .port-error-hint {
    margin-top: 6px;
    font-size: 12px;
  }
}

/* 绑定说明段落：普通块级段落，不再作为 .bound-row 的 flex 子项，
   避免多行说明在 flex 行内被挤压、截断，只按常规折行完整展示。 */
.bound-tip {
  margin: 8px 0 0;
}

.switch-item {
  margin-bottom: 12px;
}

/* 上传已有证书表单：NForm 关闭 feedback 后 naive 不再给 item 间距，
   这里显式补齐 label 与控件、控件与下一项、按钮与控件之间的留白。 */
.upload-cert-collapse {
  :deep(.n-collapse-item__content-inner) {
    padding: 16px 0 8px;
  }
}

.upload-cert-form {
  :deep(.n-form-item) {
    margin-bottom: 14px;
  }

  /* 最后一项是提交按钮，与上方私钥框留出与其他项一致的间距 */
  :deep(.n-form-item:last-child) {
    margin-top: 2px;
    margin-bottom: 0;
  }

  :deep(.n-form-item-label) {
    padding-bottom: 8px;
  }
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
