<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem, NIcon, NInput,
  NModal, NPopconfirm, NSelect, NSpace, NSwitch,
  NTabPane, NTabs, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, LockClosedOutline,
  RefreshOutline, ShieldCheckmarkOutline, TrashOutline,
} from '@vicons/ionicons5'
import {
  createACMEAccount, deleteACMEAccount, deleteCert, getCertConfig,
  issueCert, listACMEAccounts, listCerts, listPresets, renewCert,
  updateACMEAccount, updateCertConfig, type ACMEAccount, type ACMEPreset,
  type Certificate, type CertHubConfig,
} from '../../api/certs'

defineOptions({ name: 'CertList' })

const message = useMessage()

const activeMainTab = ref<'certs' | 'accounts'>('certs')

// ---- 证书列表相关 ----
const certs = ref<Certificate[]>([])
const loadingCerts = ref(false)
const showIssue = ref(false)
const issuing = ref(false)
const issueDomains = ref('')
const selectedIssueAccountID = ref<string>('')

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
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
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
  accountForm.email = "admin@example.com"
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
  if (!accountForm.email.trim()) {
    message.warning('请填写联系邮箱')
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
  if (!configReady.value) {
    message.warning('请先配置并启用 dns-mng 集成')
    showConfig.value = true
    return
  }
  issueDomains.value = ''
  showIssue.value = true
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
  issuing.value = true
  try {
    const res = await issueCert(domains, selectedIssueAccountID.value || undefined)
    message.success(res.message || '签发请求已受理')
    showIssue.value = false
    setTimeout(loadCerts, 3000)
  } catch (e: any) {
    message.error(e.message || '签发请求失败')
  } finally {
    issuing.value = false
  }
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

onMounted(() => {
  loadConfig()
  loadPresets()
  loadAccounts()
  loadCerts()
  pollTimer = setInterval(loadCerts, 30000)
})

onActivated(() => {
  loadConfig()
  loadAccounts()
  loadCerts()
})

onDeactivated(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
})
</script>

<template>
  <div class="certs-view page-flex-column">
    <!-- Header -->
    <div class="page-header">
      <div class="header-left">
        <h2 class="page-title">证书中心</h2>
      </div>
      <div class="header-right">
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
    </div>

    <NAlert v-if="!configReady" type="warning" :show-icon="true" style="flex-shrink: 0">
      证书中心需依赖 dns-mng（同级项目）：ACME DNS-01 验证将通过 dns-mng 的接口在 11 家主流云厂商（Cloudflare、阿里云、腾讯云等）自动添加与清理 TXT 解析。请先配置 dns-mng。
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
            <NAlert type="info" :show-icon="true" style="margin-bottom: 10px; flex-shrink: 0">
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

        <NFormItem label="联系邮箱" required>
          <NInput v-model:value="accountForm.email" placeholder="admin@yourdomain.com" />
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
      title="申请 SSL 证书（ACME DNS-01）"
      style="width: 560px; max-width: 94vw"
    >
      <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
        域名的 DNS 解析须已托管在 dns-mng 接入的云解析厂商中。通配符示例：*.example.com 与 example.com。
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
  </div>
</template>

<style scoped lang="scss">
.certs-view {
  gap: 14px;
  overflow: hidden;

  .page-header {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;

    .header-left {
      display: flex;
      align-items: center;

      .page-title {
        margin: 0;
        font-size: 18px;
        font-weight: 600;
        color: var(--text-primary);
      }
    }
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
