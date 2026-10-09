<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem, NIcon, NInput,
  NModal, NPopconfirm, NRadio, NRadioGroup, NSelect, NSpace, NSwitch,
  NTabPane, NTabs, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, CloudUploadOutline, LockClosedOutline,
  RefreshOutline, ShieldCheckmarkOutline, TrashOutline,
} from '@vicons/ionicons5'
import {
  createACMEAccount, deleteACMEAccount, deleteCert, getCertConfig,
  importCert, issueCert, listACMEAccounts, listCerts, listPresets, renewCert,
  updateACMEAccount, updateCertConfig, startManualDNSIssue, getManualDNSOrder,
  confirmManualDNSOrder, cancelManualDNSOrder, type ACMEAccount, type ACMEPreset,
  type Certificate, type CertHubConfig, type ChallengeMode,
  type ManualDNSOrder,
} from '../../api/certs'
import { fmtDate } from '../../utils/time'

defineOptions({ name: 'CertManager' })

const message = useMessage()

const activeMainTab = ref<'certs' | 'accounts'>('certs')

// ---- 证书列表相关 ----
const certs = ref<Certificate[]>([])
const loadingCerts = ref(false)
const showIssue = ref(false)
const issuing = ref(false)
const issueDomains = ref('')
const selectedIssueAccountID = ref<string>('')
const issueChallenge = ref<ChallengeMode>('')

// ---- 手动 DNS-01（管理员手动添加 TXT 解析） ----
const showManual = ref(false)
const manualOrder = ref<ManualDNSOrder | null>(null)
const confirming = ref(false)
const cancelling = ref(false)
let manualPollTimer: ReturnType<typeof setInterval> | null = null

// ---- 上传已有证书 ----
const showImport = ref(false)
const importing = ref(false)
const importCertPEM = ref('')
const importKeyPEM = ref('')

// ---- ACME 账户相关 ----
const accounts = ref<ACMEAccount[]>([])
const presets = ref<ACMEPreset[]>([])
const loadingAccounts = ref(false)
const showAccountModal = ref(false)
const editingAccountID = ref<string | null>(null)
const savingAccount = ref(false)

const accountForm = reactive({
  name: '',
  provider_id: 'letsencrypt',
  directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
  email: '',
  eab_key_id: '',
  eab_hmac_key: '',
  is_default: false,
})

// ---- dns-mng 集成配置 ----
const showConfig = ref(false)
const savingConfig = ref(false)
const config = reactive<CertHubConfig>({
  enabled: false,
  base_url: '',
  username: '',
  password: '',
})

let pollTimer: ReturnType<typeof setInterval> | null = null

const configReady = computed(() => config.enabled && config.base_url !== '')

const daysLeft = (cert: Certificate) =>
  Math.max(0, Math.floor((new Date(cert.not_after).getTime() - Date.now()) / 86400000))

// 证书列表列
const certColumns = computed<DataTableColumns<Certificate>>(() => [
  {
    title: '域名',
    key: 'domains',
    render: (row) =>
      h(NSpace, { size: 4, wrap: false }, {
        default: () => row.domains.map((d) =>
          h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => d })),
      }),
  },
  {
    title: '有效期',
    key: 'not_after',
    width: 220,
    render: (row) => {
      const days = daysLeft(row)
      const type = days > 30 ? 'success' : days > 7 ? 'warning' : 'error'
      return h('span', null, [
        h(NTag, { size: 'small', type, bordered: false }, { default: () => `${days} 天后到期` }),
        !row.auto_renew
          ? h(NTag, { size: 'small', type: 'warning', bordered: false, style: 'margin-left: 6px', title: '该证书不会自动续期，到期前将经告警中心提醒，请手动重新签发或上传' }, { default: () => '手动维护' })
          : null,
        h('span', { style: 'margin-left: 8px; font-size: 12px; color: #999' },
          formatDate(row.not_after)),
      ])
    },
  },
  {
    title: '颁发 CA 机构',
    key: 'issuer',
    width: 170,
    ellipsis: { tooltip: true },
    render: (r) => {
      const acc = accounts.value.find((a) => a.id === r.account_id)
      return acc ? `${acc.name} (${r.issuer || 'ACME'})` : r.issuer || 'Let\'s Encrypt'
    },
  },
  {
    title: '状态',
    key: 'status',
    width: 110,
    render: (r) =>
      r.renewing
        ? h(NTag, { size: 'small', type: 'info', bordered: false }, { default: () => '续期中' })
        : r.last_error
          ? h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => '异常' })
          : h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '正常' }),
  },
  {
    title: '操作',
    key: 'actions',
    width: 160,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => doRenew(r) }, { default: () => '续期' }),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDelete(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) }),
              default: () => `确定删除证书（${r.domains[0]} 等）？`,
            },
          ),
        ],
      }),
  },
])

// ACME 账户列表列
const accountColumns = computed<DataTableColumns<ACMEAccount>>(() => [
  {
    title: '机构名称',
    key: 'name',
    width: 220,
    render: (row) =>
      h(NSpace, { size: 6, align: 'center' }, {
        default: () => [
          h('span', { style: 'font-weight: 600' }, row.name),
          row.is_default ? h(NTag, { size: 'tiny', type: 'success', bordered: false }, { default: () => '默认' }) : null,
        ],
      }),
  },
  {
    title: 'ACME Directory URL',
    key: 'directory_url',
    ellipsis: { tooltip: true },
    render: (r) => h('code', { style: 'font-size: 11px' }, r.directory_url),
  },
  { title: '账户邮箱', key: 'email', width: 180, ellipsis: { tooltip: true } },
  {
    title: 'EAB 外部绑定',
    key: 'eab_key_id',
    width: 140,
    render: (r) => r.eab_key_id ? h(NTag, { size: 'tiny', type: 'info', bordered: false }, { default: () => '已配置 EAB' }) : h('span', { style: 'color: #999' }, '无需 EAB'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 160,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openEditAccount(r) }, { default: () => '编辑' }),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDeleteAccount(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) }),
              default: () => `确定删除机构账户「${rowName(r)}」？`,
            },
          ),
        ],
      }),
  },
])

function rowName(r: ACMEAccount) { return r.name }

const accountSelectOptions = computed(() =>
  accounts.value.map((a) => ({
    label: `${a.name}${a.is_default ? ' (默认)' : ''} [${a.directory_url}]`,
    value: a.id,
  })),
)

function formatDate(v: string) {
  return fmtDate(v)
}

async function loadConfig() {
  try {
    const cfg = await getCertConfig()
    Object.assign(config, cfg)
  } catch {
    /* keep defaults */
  }
}

async function loadPresets() {
  try {
    presets.value = await listPresets()
  } catch {
    presets.value = []
  }
}

async function loadAccounts() {
  loadingAccounts.value = true
  try {
    accounts.value = await listACMEAccounts()
    const def = accounts.value.find((a) => a.is_default) || accounts.value[0]
    if (def && !selectedIssueAccountID.value) {
      selectedIssueAccountID.value = def.id
    }
  } catch (e: any) {
    message.error(e.message || '获取 ACME 机构列表失败')
  } finally {
    loadingAccounts.value = false
  }
}

async function loadCerts() {
  loadingCerts.value = true
  try {
    certs.value = (await listCerts()) as Certificate[]
  } catch (e: any) {
    message.error(e.message || '获取证书列表失败')
  } finally {
    loadingCerts.value = false
  }
}

async function saveConfig() {
  if (config.enabled) {
    if (!config.base_url.trim()) {
      message.warning('请填写 dns-mng 服务地址')
      return
    }
    if (!config.username || !config.password) {
      message.warning('请填写 dns-mng Basic Auth 账号密码')
      return
    }
  }
  savingConfig.value = true
  try {
    const saved = await updateCertConfig({ ...config })
    Object.assign(config, saved)
    message.success('证书中心 dns-mng 配置已保存')
    showConfig.value = false
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingConfig.value = false
  }
}

function openCreateAccount() {
  editingAccountID.value = null
  accountForm.name = "Google Trust Services"
  accountForm.provider_id = "google"
  accountForm.directory_url = "https://dv.acme.pki.goog/directory"
  accountForm.email = ""
  accountForm.eab_key_id = ""
  accountForm.eab_hmac_key = ""
  accountForm.is_default = false
  showAccountModal.value = true
}

function applyPreset(preset: ACMEPreset) {
  accountForm.name = preset.name
  accountForm.provider_id = preset.id
  accountForm.directory_url = preset.directory_url
}

function openEditAccount(acc: ACMEAccount) {
  editingAccountID.value = acc.id
  accountForm.name = acc.name
  accountForm.provider_id = acc.provider_id
  accountForm.directory_url = acc.directory_url
  accountForm.email = acc.email
  accountForm.eab_key_id = acc.eab_key_id || ''
  accountForm.eab_hmac_key = acc.eab_hmac_key || ''
  accountForm.is_default = acc.is_default
  showAccountModal.value = true
}

async function submitAccount() {
  if (!accountForm.name.trim()) {
    message.warning('请填写机构名称')
    return
  }
  if (!accountForm.directory_url.trim()) {
    message.warning('请填写 ACME Directory URL')
    return
  }
  savingAccount.value = true
  try {
    if (editingAccountID.value) {
      await updateACMEAccount(editingAccountID.value, { ...accountForm })
      message.success('机构账户已更新')
    } else {
      await createACMEAccount({ ...accountForm })
      message.success('机构账户已添加')
    }
    showAccountModal.value = false
    await loadAccounts()
  } catch (e: any) {
    message.error(e.message || '保存机构账户失败')
  } finally {
    savingAccount.value = false
  }
}

async function doDeleteAccount(acc: ACMEAccount) {
  try {
    await deleteACMEAccount(acc.id)
    message.success('机构账户已删除')
    await loadAccounts()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

function openIssue() {
  // 不再强制要求 dns-mng：普通域名优先走 HTTP-01（临时监听 80 端口）即可签发；
  // 未接入 80 端口或通配符域名才需要 dns-mng（DNS-01）。
  issueDomains.value = ''
  issueChallenge.value = ''
  showIssue.value = true
}

function openImport() {
  importCertPEM.value = ''
  importKeyPEM.value = ''
  showImport.value = true
}

async function doImport() {
  if (!importCertPEM.value.trim() || !importKeyPEM.value.trim()) {
    message.warning('请填写完整的证书内容与私钥内容')
    return
  }
  importing.value = true
  try {
    const crt = await importCert(importCertPEM.value.trim(), importKeyPEM.value.trim())
    message.success(`证书已导入（${crt.domains.join('、') || crt.id}）`)
    showImport.value = false
    await loadCerts()
  } catch (e: any) {
    message.error(e.message || '导入失败')
  } finally {
    importing.value = false
  }
}

async function doIssue() {
  const domains = issueDomains.value
    .split(/[\n,]/)
    .map((d) => d.trim())
    .filter((d) => d.length > 0)
  if (domains.length === 0) {
    message.warning('请填写至少一个域名')
    return
  }
  if (issueChallenge.value === 'dns-01' && !configReady.value) {
    message.warning('已选择 DNS-01 验证，请先配置并启用 dns-mng 集成')
    showConfig.value = true
    return
  }
  issuing.value = true
  try {
    // 手动 DNS-01：同步返回待添加的 TXT 记录，打开解析指引弹窗。
    if (issueChallenge.value === 'dns-01-manual') {
      const order = await startManualDNSIssue(domains, selectedIssueAccountID.value || undefined)
      showIssue.value = false
      openManualModal(order)
      return
    }
    const res = await issueCert(domains, selectedIssueAccountID.value || undefined, issueChallenge.value || undefined)
    message.success(res.message || '签发请求已受理')
    showIssue.value = false
    // 签发是异步的（DNS-01 需等待 TXT 生效），签发后每 5 秒刷新一次、
    // 最长 2 分钟，直到新证书出现在列表中。
    const before = new Set(certs.value.map((c) => c.id))
    let tries = 0
    const timer = setInterval(async () => {
      tries++
      try {
        await loadCerts()
      } catch {
        /* 忽略单次刷新失败 */
      }
      const arrived = certs.value.some((c) => !before.has(c.id))
      if (arrived || tries >= 24) {
        clearInterval(timer)
        if (arrived) message.success('新证书已签发完成')
      }
    }, 5000)
  } catch (e: any) {
    message.error(e.message || '签发请求失败')
  } finally {
    issuing.value = false
  }
}

// ---- 手动 DNS-01：管理员按指引添加 TXT 解析后确认验证 ----
function openManualModal(order: ManualDNSOrder) {
  manualOrder.value = order
  showManual.value = true
}

function stopManualPolling() {
  if (manualPollTimer) {
    clearInterval(manualPollTimer)
    manualPollTimer = null
  }
}

function closeManualModal() {
  stopManualPolling()
  showManual.value = false
}

// 弹窗被遮罩/ESC 关闭时也要停止轮询
watch(showManual, (v) => {
  if (!v) stopManualPolling()
})

async function copyText(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`${label}已复制`)
  } catch {
    message.warning('自动复制失败，请手动选中复制')
  }
}

// 管理员确认已完成 DNS 解析：通知 CA 开始验证，随后轮询任务状态。
async function confirmDNS() {
  if (!manualOrder.value) return
  confirming.value = true
  try {
    await confirmManualDNSOrder(manualOrder.value.id)
    message.success('已通知 CA 开始验证，请稍候')
    pollManualOrder()
  } catch (e: any) {
    message.error(e.message || '确认验证失败')
  } finally {
    confirming.value = false
  }
}

// 取消待处理的手动签发任务：服务端删除订单，CA 侧订单自然过期。
async function cancelManual() {
  if (!manualOrder.value) return
  cancelling.value = true
  try {
    const res = await cancelManualDNSOrder(manualOrder.value.id)
    message.success(res.message || '已取消签发任务')
    closeManualModal()
    manualOrder.value = null
  } catch (e: any) {
    message.error(e.message || '取消任务失败')
  } finally {
    cancelling.value = false
  }
}

function pollManualOrder() {
  stopManualPolling()
  manualPollTimer = setInterval(async () => {
    if (!manualOrder.value) {
      stopManualPolling()
      return
    }
    try {
      manualOrder.value = await getManualDNSOrder(manualOrder.value.id)
    } catch {
      return // 忽略单次刷新失败
    }
    if (manualOrder.value.status === 'done') {
      stopManualPolling()
      message.success('证书签发成功，已入库')
      await loadCerts()
    } else if (manualOrder.value.status === 'error') {
      stopManualPolling()
      message.error(manualOrder.value.error || 'CA 验证失败，请检查 DNS 解析后重试')
    }
  }, 5000)
}

async function doRenew(cert: Certificate) {
  try {
    const res = await renewCert(cert.id)
    message.success(res.message || '续期请求已受理')
    cert.renewing = true
  } catch (e: any) {
    message.error(e.message || '续期失败')
  }
}

async function doDelete(cert: Certificate) {
  try {
    await deleteCert(cert.id)
    message.success('证书已删除')
    await loadCerts()
  } catch (e: any) {
    message.error(e.message || '删除失败')
  }
}

function startPolling() {
  if (pollTimer) return
  pollTimer = setInterval(loadCerts, 30000)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

onMounted(() => {
  loadConfig()
  loadPresets()
  loadAccounts()
  loadCerts()
  startPolling()
})

onActivated(() => {
  loadConfig()
  loadAccounts()
  loadCerts()
  startPolling()
})

onDeactivated(() => {
  stopPolling()
  stopManualPolling()
})

onUnmounted(() => {
  stopPolling()
  stopManualPolling()
})
</script>

<template>
  <div class="cert-manager">
    <!-- 工具栏（嵌入设置页等场景不再渲染页面级标题） -->
    <div class="cert-toolbar">
      <NSpace align="center" :size="10">
        <NButton secondary @click="showConfig = true">
          <template #icon>
            <NIcon><LockClosedOutline /></NIcon>
          </template>
          dns-mng 集成{{ configReady ? ' (已配置)' : ' (未配置)' }}
        </NButton>
        <NButton :loading="loadingCerts || loadingAccounts" @click="() => { loadAccounts(); loadCerts() }">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          刷新
        </NButton>
        <NButton v-if="activeMainTab === 'certs'" @click="openImport">
          <template #icon>
            <NIcon><CloudUploadOutline /></NIcon>
          </template>
          上传证书
        </NButton>
        <NButton v-if="activeMainTab === 'accounts'" type="primary" @click="openCreateAccount">
          <template #icon>
            <NIcon><AddCircleOutline /></NIcon>
          </template>
          添加 ACME 机构
        </NButton>
        <NButton v-else type="primary" @click="openIssue">
          <template #icon>
            <NIcon><ShieldCheckmarkOutline /></NIcon>
          </template>
          申请 SSL 证书
        </NButton>
      </NSpace>
    </div>

    <NAlert v-if="!configReady" type="warning" :show-icon="true" style="flex-shrink: 0">
      未配置 dns-mng 时，普通域名仍可通过 HTTP-01（签发时临时监听 80 端口）签发，无需 DNS 解析托管；通配符域名或 80 端口不可用时才需要配置 dns-mng（DNS-01 自动添加与清理 TXT 解析，支持 11 家主流云厂商）。
    </NAlert>

    <!-- Tabs Container：Tab 与表格分层，完全对齐 AlertList 规范 -->
    <div class="tabs-container">
      <NTabs v-model:value="activeMainTab" type="line">
        <NTabPane name="certs" tab="已签发证书">
          <div class="tab-pane-content">
            <NCard :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="certColumns"
                :data="certs"
                :row-key="(r: Certificate) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                size="small"
              />
            </NCard>
          </div>
        </NTabPane>

        <NTabPane name="accounts" tab="ACME 机构账户">
          <div class="tab-pane-content">
            <NAlert type="info" :show-icon="true" class="tip-hint" style="margin-bottom: 10px; flex-shrink: 0">
              系统支持多 CA 机构并存。配置 Google Trust Services、ZeroSSL、SSL.com 等机构时需填入官方颁发的 EAB (External Account Binding) 凭据。
            </NAlert>
            <NCard :bordered="false" class="table-flex-fill">
              <NDataTable
                flex-height
                :columns="accountColumns"
                :data="accounts"
                :row-key="(r: ACMEAccount) => r.id"
                :pagination="{ pageSize: 20 }"
                :bordered="false"
                size="small"
              />
            </NCard>
          </div>
        </NTabPane>
      </NTabs>
    </div>

    <!-- dns-mng 集成配置弹窗 -->
    <NModal
      v-model:show="showConfig"
      preset="card"
      title="dns-mng 集成配置 (DNS-01 验证后端)"
      style="width: 560px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="启用证书中心">
          <NSwitch v-model:value="config.enabled" />
        </NFormItem>
        <NFormItem label="dns-mng 服务地址" required>
          <NInput v-model:value="config.base_url" placeholder="http://127.0.0.1:8080" :disabled="!config.enabled" />
        </NFormItem>
        <NFormItem label="Basic Auth 用户名" required>
          <NInput v-model:value="config.username" placeholder="admin" :disabled="!config.enabled" />
        </NFormItem>
        <NFormItem label="Basic Auth 密码" required>
          <NInput v-model:value="config.password" type="password" show-password-on="click" :disabled="!config.enabled" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showConfig = false">取消</NButton>
          <NButton type="primary" :loading="savingConfig" @click="saveConfig">保存</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 添加/编辑 ACME 机构账户弹窗 -->
    <NModal
      v-model:show="showAccountModal"
      preset="card"
      :title="editingAccountID ? '编辑 ACME 机构账户' : '添加 ACME 机构账户'"
      style="width: 640px; max-width: 94vw"
    >
      <div v-if="!editingAccountID" style="margin-bottom: 14px">
        <div style="font-size: 12px; color: #999; margin-bottom: 6px">快速载入已知机构预设：</div>
        <NSpace :size="6" wrap>
          <NButton
            v-for="p in presets"
            :key="p.id"
            size="tiny"
            secondary
            @click="applyPreset(p)"
          >
            {{ p.name }}
          </NButton>
        </NSpace>
      </div>

      <NForm label-placement="top">
        <NFormItem label="账户/机构别名" required>
          <NInput v-model:value="accountForm.name" placeholder="如 Google Trust Services" />
        </NFormItem>

        <NFormItem label="ACME Directory URL" required>
          <NInput v-model:value="accountForm.directory_url" placeholder="https://..." />
        </NFormItem>

        <NFormItem label="联系邮箱">
          <NInput v-model:value="accountForm.email" placeholder="可选；留空则不向 CA 提交联系邮箱" />
        </NFormItem>

        <NFormItem label="EAB Key ID (KID)（ZeroSSL/Google/SSL.com 需填写）">
          <NInput v-model:value="accountForm.eab_key_id" placeholder="由 CA 机构控制台生成的 Key ID" />
        </NFormItem>

        <NFormItem label="EAB HMAC Key（密文保存在本地服务器，不向外泄露）">
          <NInput
            v-model:value="accountForm.eab_hmac_key"
            type="password"
            show-password-on="click"
            placeholder="由 CA 机构控制台生成的 HMAC Key"
          />
        </NFormItem>

        <NFormItem label="设为默认签发机构">
          <NSwitch v-model:value="accountForm.is_default" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showAccountModal = false">取消</NButton>
          <NButton type="primary" :loading="savingAccount" @click="submitAccount">保存账户</NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 申请证书弹窗（支持选择 CA 机构） -->
    <NModal
      v-model:show="showIssue"
      preset="card"
      title="申请 SSL 证书（ACME）"
      style="width: 560px; max-width: 94vw"
    >
      <NAlert type="info" :show-icon="true" class="tip-hint" style="margin-bottom: 12px">
        普通域名优先通过 HTTP-01 验证签发（需服务器 80 端口可被 CA 访问，签发时临时监听）；80 端口不可用时自动回退到 DNS-01（需配置 dns-mng）。通配符域名仅支持 DNS-01，需将 DNS 解析托管在 dns-mng 接入的云解析厂商，或选择「DNS-01（手动解析）」由管理员手动添加 TXT 记录。
      </NAlert>

      <NForm label-placement="top">
        <NFormItem label="选择签发 CA 机构">
          <NSelect v-model:value="selectedIssueAccountID" :options="accountSelectOptions" placeholder="选择 ACME 机构账户" />
        </NFormItem>

        <NFormItem label="域名列表（每行一个，支持通配符）" required>
          <NInput
            v-model:value="issueDomains"
            type="textarea"
            :rows="3"
            placeholder="*.example.com&#10;example.com"
          />
        </NFormItem>

        <NFormItem label="验证方式">
          <NRadioGroup v-model:value="issueChallenge">
            <NSpace>
              <NRadio value="">自动（推荐）</NRadio>
              <NRadio value="http-01">HTTP-01</NRadio>
              <NRadio value="dns-01">DNS-01（dns-mng）</NRadio>
              <NRadio value="dns-01-manual">DNS-01（手动解析）</NRadio>
            </NSpace>
          </NRadioGroup>
          <template #feedback>
            <span class="muted tip-hint">
              自动：普通域名优先 HTTP-01（需 80 端口可被 CA 访问），失败或 80 不可用时回退 DNS-01；
              通配符固定 DNS-01，IP 固定 HTTP-01。显式指定后不再自动回退。
              手动解析：服务端先生成 TXT 记录，您到 DNS 服务商手动添加解析后再确认验证，不依赖 dns-mng。
            </span>
          </template>
        </NFormItem>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="showIssue = false">取消</NButton>
          <NButton type="primary" :loading="issuing" @click="doIssue">
            <template #icon>
              <NIcon><ShieldCheckmarkOutline /></NIcon>
            </template>
            申请签发
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 手动 DNS-01 解析指引弹窗 -->
    <NModal
      v-model:show="showManual"
      preset="card"
      title="手动添加 DNS 解析（DNS-01）"
      style="width: 640px; max-width: 94vw"
    >
      <NAlert type="info" :show-icon="true" class="tip-hint" style="margin-bottom: 12px">
        请到您的 DNS 服务商为以下每个域名添加一条 TXT 解析记录；添加后等待解析生效
        （一般几分钟，视 DNS 厂商而定），再点击「我已完成解析，开始验证」。
        CA 验证通过后证书自动签发并入库。任务保留 2 小时，超时需重新发起。
      </NAlert>

      <div v-if="manualOrder" style="display: flex; flex-direction: column; gap: 12px">
        <NCard
          v-for="rec in manualOrder.records"
          :key="rec.host"
          size="small"
          :title="rec.domain"
          embedded
        >
          <div class="dns-rec-row">
            <span class="dns-rec-label">记录类型</span>
            <NTag size="small" type="info" bordered>TXT</NTag>
          </div>
          <div class="dns-rec-row">
            <span class="dns-rec-label">主机记录</span>
            <code class="dns-rec-value">{{ rec.host }}</code>
            <NButton size="small" @click="copyText(rec.host, '主机记录')">复制</NButton>
          </div>
          <div class="dns-rec-row">
            <span class="dns-rec-label">记录值</span>
            <code class="dns-rec-value">{{ rec.value }}</code>
            <NButton size="small" @click="copyText(rec.value, '记录值')">复制</NButton>
          </div>
        </NCard>

        <NAlert v-if="manualOrder.status === 'verifying'" type="warning" :show-icon="true">
          CA 正在验证 DNS 解析，请稍候，页面会自动刷新状态……
        </NAlert>
        <NAlert v-if="manualOrder.status === 'done'" type="success" :show-icon="true">
          证书签发成功，已入库，可在证书列表中查看。
        </NAlert>
        <NAlert v-if="manualOrder.status === 'error'" type="error" :show-icon="true">
          {{ manualOrder.error || 'CA 验证失败' }}。
          <template v-if="manualOrder.terminal">该订单已终态失败，不可重试，请关闭后重新发起签发。</template>
          <template v-else>请检查 TXT 记录是否已正确添加并生效，然后重新验证。</template>
        </NAlert>
      </div>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="closeManualModal">关闭</NButton>
          <NPopconfirm
            v-if="manualOrder && (manualOrder.status === 'awaiting_dns' || manualOrder.status === 'error')"
            @positive-click="cancelManual"
          >
            <template #trigger>
              <NButton :disabled="cancelling">取消任务</NButton>
            </template>
            确定取消该手动签发任务？CA 侧订单将自然过期，已添加的 TXT 记录需自行到 DNS 服务商删除。
          </NPopconfirm>
          <NButton
            v-if="manualOrder && (manualOrder.status === 'awaiting_dns' || (manualOrder.status === 'error' && !manualOrder.terminal))"
            type="primary"
            :loading="confirming"
            @click="confirmDNS"
          >
            <template #icon>
              <NIcon><ShieldCheckmarkOutline /></NIcon>
            </template>
            我已完成解析，开始验证
          </NButton>
        </NSpace>
      </template>
    </NModal>

    <!-- 上传已有证书弹窗 -->
    <NModal
      v-model:show="showImport"
      preset="card"
      title="上传已有证书"
      style="width: 560px; max-width: 94vw"
    >
      <NAlert type="info" :show-icon="true" class="tip-hint" style="margin-bottom: 12px">
        上传其他渠道申请的证书（PEM 格式），导入后进入已签发证书列表，可用于应用代理绑定或面板 HTTPS。私钥仅保存在服务器本地，不向外回显。
      </NAlert>

      <NForm label-placement="top">
        <NFormItem label="证书内容（PEM，可含证书链）" required>
          <NInput
            v-model:value="importCertPEM"
            type="textarea"
            :rows="5"
            placeholder="-----BEGIN CERTIFICATE-----"
            spellcheck="false"
          />
        </NFormItem>

        <NFormItem label="私钥内容（PEM）" required>
          <NInput
            v-model:value="importKeyPEM"
            type="textarea"
            :rows="5"
            placeholder="-----BEGIN PRIVATE KEY-----"
            spellcheck="false"
          />
        </NFormItem>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="showImport = false">取消</NButton>
          <NButton type="primary" :loading="importing" @click="doImport">
            导入证书
          </NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.cert-manager {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
  gap: 14px;
  overflow: hidden;

  // 手动 DNS-01 解析记录展示行（弹窗内）
  :deep(.dns-rec-row) {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 8px;

    &:last-child {
      margin-bottom: 0;
    }
  }

  :deep(.dns-rec-label) {
    flex-shrink: 0;
    width: 64px;
    font-size: 12px;
    color: #999;
  }

  :deep(.dns-rec-value) {
    flex: 1;
    min-width: 0;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 12px;
    word-break: break-all;
    background: rgba(0, 0, 0, 0.04);
    border-radius: 4px;
    padding: 4px 8px;
  }

  .cert-toolbar {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: flex-end;
  }

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
}
</style>
