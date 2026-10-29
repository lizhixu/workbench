<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import {
  NAlert, NButton, NCard, NCollapse, NCollapseItem, NDataTable,
  NDescriptions, NDescriptionsItem, NForm, NFormItem, NIcon, NInput,
  NModal, NPopconfirm, NPopover, NRadio, NRadioGroup, NSelect, NSpace, NSpin,
  NSwitch, NTabPane, NTabs, NTag, useDialog, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, CloudUploadOutline, CopyOutline, CreateOutline,
  DownloadOutline, LockClosedOutline, RefreshOutline, ShieldCheckmarkOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import {
  certDownloadUrl, createACMEAccount, deleteACMEAccount, deleteCert, getCertConfig,
  getCertDetail, getCertKey, importCert, issueCert, listACMEAccounts, listCerts, listPresets,
  renewCert, updateACMEAccount, updateCertConfig, updateCertRemark,
  startManualDNSIssue, getManualDNSOrder, confirmManualDNSOrder, cancelManualDNSOrder,
  type ACMEAccount, type ACMEPreset, type Certificate, type CertDetail,
  type CertHubConfig, type ChallengeMode, type ManualDNSOrder,
} from '../../api/certs'
import { useRouter } from 'vue-router'
import { useTablePagination } from '../../composables/useTablePagination'
import { fmtDate } from '../../utils/time'

defineOptions({ name: 'CertManager' })

const message = useMessage()
const dialog = useDialog()

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

// 证书列表列（宝塔式 11 列）：域名 / 其他域名 / 申请方式 / ACME 账号 / 状态 /
// 日志 / 颁发组织 / 备注 / 自动续签 / 过期时间 / 操作。
const certColumns = computed<DataTableColumns<Certificate>>(() => [
  {
    title: '域名',
    key: 'domains',
    width: 160,
    fixed: 'left',
    ellipsis: { tooltip: true },
    render: (row) => h('span', { style: 'font-weight: 500' }, row.domains[0] || '-'),
  },
  {
    title: '其他域名',
    key: 'other_domains',
    width: 130,
    ellipsis: { tooltip: true },
    render: (row) => {
      const rest = row.domains.slice(1)
      return rest.length
        ? h('span', null, rest.join('、'))
        : h('span', { style: 'color: #999' }, '-')
    },
  },
  {
    title: '申请方式',
    key: 'source',
    width: 100,
    render: (r) => {
      if (!r.source) return h('span', { style: 'color: #999' }, '-')
      const m = certSourceMeta(r.source)
      return h(NTag, { size: 'small', type: m.type, bordered: false }, { default: () => m.text })
    },
  },
  {
    title: 'ACME 账号',
    key: 'account_name',
    width: 110,
    ellipsis: { tooltip: true },
    render: (r) => r.account_name || h('span', { style: 'color: #999' }, '-'),
  },
  {
    title: '状态',
    key: 'status',
    width: 80,
    render: (r) => {
      const s = certStatus(r)
      return h(NTag, { size: 'small', type: s.type, bordered: false }, { default: () => s.text })
    },
  },
  {
    title: '日志',
    key: 'last_error',
    width: 130,
    render: (r) => {
      if (!r.last_error) return h('span', { style: 'color: #999' }, '-')
      // 失败原因全文以限宽弹层展示（替代内置 ellipsis tooltip：后者对 300+ 字符
      // 的错误实测会铺出 1088px 宽的巨框）；单元格内手动省略截断。
      return h(
        NPopover,
        { trigger: 'hover', placement: 'top', showArrow: false },
        {
          trigger: () =>
            h(
              'span',
              { style: 'color: #d03050; display: block; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;' },
              r.last_error,
            ),
          default: () => [
            h('div', { style: 'font-size: 11px; color: #999; margin-bottom: 4px' }, '最近一次失败原因'),
            h('div', { style: 'font-size: 12px; line-height: 1.6; word-break: break-all; max-width: 340px; max-height: 200px; overflow: auto;' }, r.last_error),
          ],
        },
      )
    },
  },
  {
    title: '颁发组织',
    key: 'issuer',
    width: 120,
    ellipsis: { tooltip: true },
    render: (r) => r.issuer || h('span', { style: 'color: #999' }, '-'),
  },
  {
    title: '备注',
    key: 'remark',
    width: 140,
    render: (r) => renderRemarkCell(r),
  },
  {
    title: '自动续签',
    key: 'auto_renew',
    width: 90,
    render: (r) =>
      r.auto_renew
        ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '自动续签' })
        : h(NTag, { size: 'small', type: 'warning', bordered: false, title: '该证书不会自动续期，到期前将经告警中心提醒，请手动重新签发或上传' }, { default: () => '手动维护' }),
  },
  {
    title: '过期时间',
    key: 'not_after',
    width: 120,
    render: (row) => {
      const days = daysLeft(row)
      const type = days > 30 ? 'success' : days > 7 ? 'warning' : 'error'
      return h('span', null, [
        h(NTag, { size: 'small', type, bordered: false }, { default: () => (days > 0 ? `${days} 天后到期` : '已过期') }),
        h('div', { style: 'font-size: 11px; color: #999; margin-top: 2px' }, formatDate(row.not_after)),
      ])
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 185,
    fixed: 'right',
    render: (r) =>
      h(NSpace, { size: 6, wrap: false }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openDetail(r) }, { default: () => '详情' }),
          h(
            NButton,
            { size: 'tiny', secondary: true, title: '下载证书（.crt 公钥链，不含私钥）', onClick: () => window.open(certDownloadUrl(r.id), '_blank') },
            { icon: () => h(NIcon, null, { default: () => h(DownloadOutline) }) },
          ),
          // 手动上传的证书没有 ACME 账户：续期会被服务端回落到默认账户重新签发并
          // 覆盖上传的材料，语义上应为「重新上传」而非续期，故不展示续期按钮。
          ...(r.source === 'manual_upload'
            ? []
            : [h(NButton, { size: 'tiny', secondary: true, onClick: () => doRenew(r) }, { default: () => '续期' })]),
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

// ---- 证书备注行内编辑（PUT /certs/:id/remark，仅元信息，adminOnly + 审计） ----
const remarkEditId = ref<string | null>(null)
const remarkDraft = ref('')
const remarkSaving = ref(false)

function startEditRemark(r: Certificate) {
  if (remarkEditId.value === r.id) {
    remarkEditId.value = null
    return
  }
  remarkDraft.value = r.remark || ''
  remarkEditId.value = r.id
}

async function saveRemark(r: Certificate) {
  if (remarkSaving.value) return
  remarkSaving.value = true
  try {
    const updated = await updateCertRemark(r.id, remarkDraft.value.trim())
    r.remark = updated.remark
    message.success('备注已保存')
    remarkEditId.value = null
  } catch (e: any) {
    message.error(e?.message || '备注保存失败')
  } finally {
    remarkSaving.value = false
  }
}

// 备注单元格：内容省略展示 + 铅笔按钮弹出编辑框（点击空白处自动关闭）。
function renderRemarkCell(r: Certificate) {
  return h(NSpace, { size: 2, align: 'center', wrap: false }, {
    default: () => [
      h(
        'span',
        {
          style:
            'display: inline-block; max-width: 96px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; vertical-align: middle;' +
            (r.remark ? '' : ' color: #999'),
          title: r.remark || '',
        },
        r.remark || '-',
      ),
      h(
        NPopover,
        {
          show: remarkEditId.value === r.id,
          'onUpdate:show': (v: boolean) => {
            if (!v && !remarkSaving.value) remarkEditId.value = null
          },
          trigger: 'manual',
          placement: 'left',
        },
        {
          trigger: () =>
            h(
              NButton,
              { size: 'tiny', quaternary: true, title: '编辑备注', onClick: () => startEditRemark(r) },
              { icon: () => h(NIcon, null, { default: () => h(CreateOutline) }) },
            ),
          default: () =>
            h('div', { style: 'width: 280px' }, [
              h('div', { style: 'font-weight: 600; margin-bottom: 6px' }, `备注 · ${r.domains[0] || ''}`),
              h(NInput, {
                value: remarkDraft.value,
                'onUpdate:value': (v: string) => {
                  remarkDraft.value = v
                },
                type: 'textarea',
                rows: 3,
                maxlength: 200,
                showCount: true,
                placeholder: '记录证书用途、申请人等（最多 200 字）',
              }),
              h(NSpace, { justify: 'end', style: 'margin-top: 8px' }, {
                default: () => [
                  h(NButton, { size: 'tiny', onClick: () => { remarkEditId.value = null } }, { default: () => '取消' }),
                  h(NButton, { size: 'tiny', type: 'primary', loading: remarkSaving.value, onClick: () => saveRemark(r) }, { default: () => '保存' }),
                ],
              }),
            ]),
        },
      ),
    ],
  })
}

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

type TagType = 'default' | 'success' | 'warning' | 'error' | 'info'

// 列表与详情弹窗共用的状态标签判定（续期中/异常优先，随后按有效期判定）
function certStatus(c: Certificate): { type: TagType; text: string } {
  if (c.renewing) return { type: 'info', text: '续期中' }
  if (c.last_error) return { type: 'error', text: '异常' }
  const now = Date.now()
  if (c.not_after && new Date(c.not_after).getTime() <= now) return { type: 'error', text: '已过期' }
  if (c.not_before && new Date(c.not_before).getTime() > now) return { type: 'warning', text: '未生效' }
  return { type: 'success', text: '正常' }
}

function certSourceMeta(s: NonNullable<Certificate['source']>): { type: TagType; text: string } {
  switch (s) {
    case 'acme':
      return { type: 'info', text: 'ACME 自动签发' }
    case 'manual_dns01':
      return { type: 'warning', text: '手动 DNS-01 签发' }
    default:
      return { type: 'default', text: '手动上传' }
  }
}

// 两表分页走统一 composable（AGENTS.md §8.2）：页码随行数收敛
const { pagination: certPagination } = useTablePagination({ rowCount: computed(() => certs.value.length) })
const { pagination: accountPagination } = useTablePagination({ rowCount: computed(() => accounts.value.length) })

// ---- 证书详情弹窗 ----
const router = useRouter()
const showDetail = ref(false)
const detail = ref<CertDetail | null>(null)
const detailLoading = ref(false)
const detailError = ref('')
// 私钥明文：与证书链并列展示，打开详情时经显式查看端点拉取（adminOnly + 高危审计）
const keyPEM = ref('')
const keyError = ref('')

// 有效期进度：证书生命周期内已消耗的百分比，供有效期进度条展示
const validityPct = computed(() => {
  const d = detail.value
  if (!d?.not_before || !d?.not_after) return null
  const start = new Date(d.not_before).getTime()
  const end = new Date(d.not_after).getTime()
  if (!isFinite(start) || !isFinite(end) || end <= start) return null
  const pct = ((Date.now() - start) / (end - start)) * 100
  return Math.min(100, Math.max(0, Math.round(pct)))
})

// 进度条颜色随剩余天数分档：>30 天绿、>7 天琥珀、其余红
const validityColor = computed(() => {
  const days = detail.value ? daysLeft(detail.value) : 0
  if (days > 30) return '#10b981'
  if (days > 7) return '#f59e0b'
  return '#ef4444'
})

async function openDetail(cert: Certificate) {
  showDetail.value = true
  detail.value = null
  detailError.value = ''
  keyPEM.value = ''
  keyError.value = ''
  detailLoading.value = true
  try {
    detail.value = await getCertDetail(cert.id)
  } catch (e: any) {
    detailError.value = e?.message || '加载证书详情失败'
  } finally {
    detailLoading.value = false
  }
  // 私钥经显式查看端点按需拉取；失败仅提示，不影响其余详情展示。
  if (detail.value) {
    try {
      keyPEM.value = await getCertKey(cert.id)
    } catch (e: any) {
      keyError.value = e?.message || '加载私钥失败'
    }
  }
}

// 通用复制：DNS TXT 记录 / 序列号 / 指纹 / 证书链共用，复制结果经全局 message 反馈
async function copyText(text: string | undefined, label: string) {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    message.success(`${label}已复制到剪贴板`)
  } catch {
    message.error('复制失败，请手动选择文本复制')
  }
}

async function copyChain() {
  await copyText(detail.value?.chain_pem, '证书链')
}

async function copyKey() {
  await copyText(keyPEM.value, '私钥')
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
    const msg = e.message || ''
    if (msg.includes('force=true') || msg.includes('已被以下应用绑定') || msg.includes('已被应用绑定') || msg.includes('仍被应用')) {
      dialog.warning({
        title: '证书正在使用中',
        content: `${msg}。是否强制删除？（强制删除将解除相关应用的显式证书绑定）`,
        positiveText: '强制删除',
        negativeText: '取消',
        onPositiveClick: async () => {
          try {
            await deleteCert(cert.id, true)
            message.success('证书已强制删除并解除相关应用绑定')
            await loadCerts()
          } catch (err: any) {
            message.error(err.message || '强制删除失败')
          }
        },
      })
    } else {
      message.error(msg || '删除失败')
    }
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
    <!-- 标题与全局操作同一行：此前按钮单独占一行、左侧整片空白，标题下方显得很空 -->
    <div class="page-header">
      <h2 class="page-title">证书中心</h2>
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
                :pagination="certPagination"
                :bordered="false"
                :scroll-x="1365"
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
                :pagination="accountPagination"
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

    <!-- 证书详情弹窗：服务端现场解析 leaf 与链 + 用途反查，链 PEM 仅为公钥材料 -->
    <NModal
      v-model:show="showDetail"
      preset="card"
      class="cert-detail-modal"
      :content-style="{ maxHeight: 'calc(100vh - 120px)', overflow: 'auto' }"
      style="width: 680px; max-width: 94vw"
    >
      <template #header>
        <div class="cert-detail-header">
          <span class="cert-detail-domain">{{ detail?.domains?.[0] || '证书详情' }}</span>
          <NTag v-if="detail" size="small" :type="certStatus(detail).type" :bordered="false">
            {{ certStatus(detail).text }}
          </NTag>
        </div>
      </template>
      <NSpin :show="detailLoading">
        <div class="cert-detail-body">
          <NAlert v-if="detailError" type="error" :show-icon="true">{{ detailError }}</NAlert>
        <template v-if="detail">
          <NAlert v-if="detail.parse_error" type="warning" :show-icon="true" style="margin-bottom: 12px">
            证书链解析失败：{{ detail.parse_error }}（指纹与算法区暂不可展示）
          </NAlert>
          <NDescriptions v-else title="指纹与算法" :column="2" bordered size="small" label-placement="left">
            <NDescriptionsItem label="主题 (Subject)">{{ detail.subject_cn || '-' }}</NDescriptionsItem>
            <NDescriptionsItem label="证书链">{{ detail.chain_length }} 张</NDescriptionsItem>
            <NDescriptionsItem label="序列号" :span="2">
              <div class="mono-row">
                <span class="mono-val">{{ detail.serial_number || '-' }}</span>
                <NButton
                  v-if="detail.serial_number"
                  size="tiny"
                  quaternary
                  title="复制序列号"
                  @click="copyText(detail.serial_number, '序列号')"
                >
                  <template #icon><NIcon><CopyOutline /></NIcon></template>
                </NButton>
              </div>
            </NDescriptionsItem>
            <NDescriptionsItem label="SHA-256 指纹" :span="2">
              <div class="mono-row">
                <span class="mono-val">{{ detail.sha256_fingerprint || '-' }}</span>
                <NButton
                  v-if="detail.sha256_fingerprint"
                  size="tiny"
                  quaternary
                  title="复制 SHA-256 指纹"
                  @click="copyText(detail.sha256_fingerprint, 'SHA-256 指纹')"
                >
                  <template #icon><NIcon><CopyOutline /></NIcon></template>
                </NButton>
              </div>
            </NDescriptionsItem>
            <NDescriptionsItem label="密钥算法">{{ detail.public_key_algorithm || '-' }}</NDescriptionsItem>
            <NDescriptionsItem label="签名算法">{{ detail.signature_algorithm || '-' }}</NDescriptionsItem>
          </NDescriptions>

          <NDescriptions title="有效期与续期" :column="2" bordered size="small" label-placement="left">
            <NDescriptionsItem label="生效时间">
              {{ formatDate(detail.not_before) }}
              <NTag v-if="detail.not_yet_valid" size="small" type="warning" :bordered="false" style="margin-left: 6px">尚未生效</NTag>
            </NDescriptionsItem>
            <NDescriptionsItem label="到期时间">{{ formatDate(detail.not_after) }}</NDescriptionsItem>
            <NDescriptionsItem label="剩余天数">
              <NTag size="small" :type="daysLeft(detail) > 30 ? 'success' : daysLeft(detail) > 7 ? 'warning' : 'error'" :bordered="false">
                {{ daysLeft(detail) > 0 ? `${daysLeft(detail)} 天后到期` : '已过期' }}
              </NTag>
            </NDescriptionsItem>
            <NDescriptionsItem label="自动续期">
              <template v-if="detail.auto_renew">
                到期前自动续期（提前 {{ detail.auto_renew_days }} 天内触发，短期证书按有效期 1/3 收紧）
              </template>
              <template v-else>不自动续期，到期前经告警中心提醒一次，请手动重新签发或上传</template>
            </NDescriptionsItem>
            <NDescriptionsItem v-if="validityPct !== null" label="有效期进度" :span="2">
              <div class="validity-track">
                <div class="validity-bar">
                  <div class="validity-fill" :style="{ width: `${validityPct}%`, background: validityColor }"></div>
                </div>
                <span class="validity-hint">证书生命周期已消耗 {{ validityPct }}%</span>
              </div>
            </NDescriptionsItem>
          </NDescriptions>

          <NDescriptions title="使用情况" :column="2" bordered size="small" label-placement="left">
            <NDescriptionsItem label="面板 HTTPS">
              <NTag v-if="detail.panel_bound" size="small" type="success" :bordered="false">绑定为面板证书</NTag>
              <NTag v-else-if="detail.panel_fallback" size="small" type="info" :bordered="false">面板兜底匹配（未显式绑定）</NTag>
              <NTag v-else size="small" :bordered="false">未使用</NTag>
            </NDescriptionsItem>
            <NDescriptionsItem label="使用中的应用">
              <NSpace v-if="detail.used_by_apps?.length" :size="4">
                <NButton
                  v-for="app in detail.used_by_apps"
                  :key="app.id"
                  size="tiny"
                  secondary
                  @click="showDetail = false; router.push(`/apps/${app.id}`)"
                >
                  {{ app.name }}（{{ app.domain }}）
                </NButton>
              </NSpace>
              <span v-else class="detail-muted">暂无应用使用</span>
            </NDescriptionsItem>
          </NDescriptions>

          <template v-if="detail.chain_pem || keyPEM">
            <div class="detail-section-title">证书材料</div>
            <NCollapse :default-expanded-names="['chain', 'key']">
              <NCollapseItem v-if="detail.chain_pem" title="证书链（公钥 PEM）" name="chain">
                <pre class="cert-chain-pre">{{ detail.chain_pem }}</pre>
                <NButton size="tiny" secondary @click="copyChain">复制证书链</NButton>
              </NCollapseItem>
              <NCollapseItem title="私钥（KEY PEM，敏感）" name="key">
                <pre v-if="keyPEM" class="cert-chain-pre">{{ keyPEM }}</pre>
                <NButton v-if="keyPEM" size="tiny" secondary @click="copyKey">复制私钥</NButton>
                <span v-else-if="keyError" class="key-load-error">{{ keyError }}</span>
                <NSpin v-else size="small" />
                <p v-if="keyPEM" class="key-warning">私钥即站点控制权：仅复制到安全位置，切勿提交到代码仓库或发送给他人。</p>
              </NCollapseItem>
            </NCollapse>
          </template>
          </template>
        </div>
      </NSpin>
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

  .page-header {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .page-title {
    margin: 0;
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .cert-toolbar {
    flex-shrink: 0;
    display: flex;
    align-items: center;
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

.mono-val {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  word-break: break-all;
}

// 证书详情弹窗内容区：NModal teleport 到 body 后卡片根节点不带本组件 data-v 作用域，
// 深选择器必须锚定在模板内自己渲染的 .cert-detail-body 上才能命中 Naive 内部节点。
.cert-detail-body {
  // 分区标题：紧凑视觉层级（Naive 默认 18px 偏大），左侧品牌色竖条作视觉锚点
  :deep(.n-descriptions-header) {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 10px;

    &::before {
      content: '';
      flex-shrink: 0;
      width: 3px;
      height: 13px;
      border-radius: 2px;
      background: #6366f1;
    }
  }

  // 分区间距统一收口（取代逐个内联 margin-bottom）
  :deep(.n-descriptions) {
    margin-bottom: 12px;
  }

  // 「证书材料」区为自渲染标题（非 NDescriptions header），复刻同一视觉语言
  .detail-section-title {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 10px;

    &::before {
      content: '';
      flex-shrink: 0;
      width: 3px;
      height: 13px;
      border-radius: 2px;
      background: #6366f1;
    }
  }
}

// 弹窗头部：域名 + 状态标签平铺，一眼可读
.cert-detail-header {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  flex-wrap: wrap;

  .cert-detail-domain {
    font-size: 15px;
    font-weight: 600;
    word-break: break-all;
  }
}

// 长等宽值（序列号 / 指纹）+ 行内复制按钮
.mono-row {
  display: flex;
  align-items: center;
  gap: 4px;
  min-width: 0;

  .mono-val {
    flex: 1;
    min-width: 0;
  }
}

// 有效期进度条：填充色随剩余天数分档（绿 >30 天 / 琥珀 >7 天 / 红）
.validity-track {
  width: 100%;

  .validity-bar {
    height: 6px;
    border-radius: 999px;
    background: rgba(128, 128, 128, 0.16);
    overflow: hidden;
    margin: 3px 0 5px;
  }

  .validity-fill {
    height: 100%;
    border-radius: 999px;
    transition: width 0.3s ease;
  }

  .validity-hint {
    font-size: 11px;
    color: var(--text-muted);
  }
}

.cert-chain-pre {
  margin: 0 0 10px;
  padding: 10px 12px;
  max-height: 240px;
  overflow: auto;
  font-family: 'JetBrains Mono', Consolas, ui-monospace, Menlo, monospace;
  font-size: 11.5px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
  background: var(--code-box-bg);
  border: 1px solid var(--code-border);
  border-radius: 6px;
}

.detail-muted {
  color: #999;
  font-size: 12px;
}

// 私钥区：拉取失败提示与安全告诫
.key-load-error {
  color: #d03050;
  font-size: 12px;
}

.key-warning {
  margin: 8px 0 0;
  font-size: 12px;
  color: #d03050;
}
</style>
