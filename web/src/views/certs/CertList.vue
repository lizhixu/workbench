<script setup lang="ts">
import { computed, h, onActivated, onDeactivated, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NDataTable, NForm, NFormItem, NIcon,
  NInput, NModal, NPopconfirm, NSpace, NSwitch, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  AddCircleOutline, LockClosedOutline, RefreshOutline,
  ShieldCheckmarkOutline, TrashOutline,
} from '@vicons/ionicons5'
import {
  deleteCert, getCertConfig, issueCert, listCerts,
  renewCert, updateCertConfig, type Certificate, type CertHubConfig,
} from '../../api/certs'

defineOptions({ name: 'CertList' })

const message = useMessage()

const certs = ref<Certificate[]>([])
const loading = ref(false)
const showConfig = ref(false)
const savingConfig = ref(false)
const showIssue = ref(false)
const issuing = ref(false)
const issueDomains = ref('')

const config = reactive<CertHubConfig>({
  enabled: false,
  base_url: '',
  username: '',
  password: '',
  directory_url: '',
  email: '',
})

let pollTimer: ReturnType<typeof setInterval> | null = null

const configReady = computed(() => config.enabled && config.base_url !== '')

const daysLeft = (cert: Certificate) =>
  Math.max(0, Math.floor((new Date(cert.not_after).getTime() - Date.now()) / 86400000))

const columns = computed<DataTableColumns<Certificate>>(() => [
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
  { title: '颁发者', key: 'issuer', width: 160, ellipsis: { tooltip: true }, render: (r) => r.issuer || '-' },
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
    width: 170,
    render: (r) =>
      h(NSpace, { size: 6 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => doRenew(r) }, { default: () => '续期' }),
          h(
            NPopconfirm,
            { onPositiveClick: () => doDelete(r) },
            {
              trigger: () => h(NButton, { size: 'tiny', secondary: true, type: 'error' }, { default: () => h(NIcon, null, { default: () => h(TrashOutline) }) }),
              default: () => `确定删除证书（${r.domains[0]} 等）？已下发的配置不受影响。`,
            },
          ),
        ],
      }),
  },
])

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

async function loadCerts() {
  loading.value = true
  try {
    certs.value = (await listCerts()) as Certificate[]
  } catch (e: any) {
    message.error(e.message || '获取证书列表失败')
  } finally {
    loading.value = false
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
    message.success('证书中心配置已保存')
    showConfig.value = false
  } catch (e: any) {
    message.error(e.message || '保存失败')
  } finally {
    savingConfig.value = false
  }
}

function openIssue() {
  if (!configReady.value) {
    message.warning('请先在设置中启用并配置 dns-mng 集成')
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
    const res = await issueCert(domains)
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
  loadCerts()
  pollTimer = setInterval(loadCerts, 30000)
})

onActivated(() => {
  loadConfig()
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
    <div class="table-toolbar">
      <NSpace align="center">
        <NButton secondary @click="showConfig = true">
          <template #icon>
            <NIcon><LockClosedOutline /></NIcon>
          </template>
          dns-mng 集成{{ configReady ? '' : '（未配置）' }}
        </NButton>
        <NButton :loading="loading" @click="loadCerts">
          <template #icon>
            <NIcon><RefreshOutline /></NIcon>
          </template>
          刷新
        </NButton>
      </NSpace>
      <NButton type="primary" @click="openIssue">
        <template #icon>
          <NIcon><AddCircleOutline /></NIcon>
        </template>
        申请证书
      </NButton>
    </div>

    <NAlert v-if="!configReady" type="warning" :show-icon="true" style="flex-shrink: 0">
      证书中心尚未启用：申请证书需要先配置 dns-mng 服务地址与 Basic Auth 凭据，
      DNS-01 验证将由 dns-mng 自动在对应云厂商完成（支持 Cloudflare/阿里云/腾讯云 DNSPod 等）。
    </NAlert>

    <div class="table-card table-flex-fill">
      <NDataTable
        flex-height
        :columns="columns"
        :data="certs"
        :row-key="(r: Certificate) => r.id"
        :pagination="{ pageSize: 20 }"
      />
    </div>

    <NModal
      v-model:show="showConfig"
      preset="card"
      title="dns-mng 集成配置"
      style="width: 560px; max-width: 94vw"
    >
      <NForm label-placement="top">
        <NFormItem label="启用证书中心">
          <NSwitch v-model:value="config.enabled" />
        </NFormItem>
        <NFormItem label="dns-mng 服务地址">
          <NInput v-model:value="config.base_url" placeholder="http://10.0.0.2:8080" :disabled="!config.enabled" />
        </NFormItem>
        <NFormItem label="Basic Auth 用户名">
          <NInput v-model:value="config.username" :disabled="!config.enabled" />
        </NFormItem>
        <NFormItem label="Basic Auth 密码">
          <NInput v-model:value="config.password" type="password" show-password-on="click" :disabled="!config.enabled" />
        </NFormItem>
        <NFormItem label="ACME 目录（可选，默认 Let's Encrypt）">
          <NInput
            v-model:value="config.directory_url"
            placeholder="https://acme-v02.api.letsencrypt.org/directory"
            :disabled="!config.enabled"
          />
        </NFormItem>
        <NFormItem label="联系邮箱（ACME 账户）">
          <NInput v-model:value="config.email" :disabled="!config.enabled" />
        </NFormItem>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showConfig = false">取消</NButton>
          <NButton type="primary" :loading="savingConfig" @click="saveConfig">保存</NButton>
        </NSpace>
      </template>
    </NModal>

    <NModal
      v-model:show="showIssue"
      preset="card"
      title="申请 SSL 证书（ACME DNS-01）"
      style="width: 520px; max-width: 94vw"
    >
      <NAlert type="info" :show-icon="true" style="margin-bottom: 12px">
        域名的 DNS 解析须已托管在 dns-mng 接入的云厂商账号中。通配符示例：*.example.com 与 example.com。
      </NAlert>
      <NInput
        v-model:value="issueDomains"
        type="textarea"
        :rows="3"
        placeholder="*.example.com&#10;example.com"
      />
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showIssue = false">取消</NButton>
          <NButton type="primary" :loading="issuing" @click="doIssue">
            <template #icon>
              <NIcon><ShieldCheckmarkOutline /></NIcon>
            </template>
            申请
          </NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.certs-view {
  gap: 12px;
}

.table-toolbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.table-card {
  background: var(--n-color, rgba(128, 128, 128, 0.06));
  border-radius: 8px;
  padding: 4px;
}
</style>