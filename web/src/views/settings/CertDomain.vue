<script setup lang="ts">
import { computed, onActivated, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCard,
  NDivider,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NRadio,
  NRadioGroup,
  NSelect,
  NSpace,
  NSpin,
  NSwitch,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  CheckmarkCircleOutline,
  GlobeOutline,
  LockClosedOutline,
  OpenOutline,
  RefreshOutline,
  ShieldCheckmarkOutline,
} from '@vicons/ionicons5'
import { listCerts, type Certificate } from '../../api/certs'
import {
  SETTING_KEYS,
  getPanelCertStatus,
  getSystemSettings,
  saveSystemSettings,
  type PanelCertStatus,
} from '../../api/settings'

// 系统设置 → 面板域名与证书（仅管理员可见，见 sections.ts 的 adminOnly）。
// 专用于控制台/面板自身的公网访问地址、绑定域名、防直接IP扫描及 HTTPS/SSL 加密证书。
// 应用中心的应用反代域名由「应用中心」独立维护，不在此处混杂。
defineOptions({ name: 'CertDomain' })

const router = useRouter()
const message = useMessage()
const loading = ref(false)
const saving = ref(false)
const certs = ref<Certificate[]>([])
const activeCert = ref<PanelCertStatus | null>(null)

const form = reactive({
  public_url: '',
  panel_domain: '',
  panel_domain_strict: false,
  panel_ssl_enabled: false,
  panel_ssl_mode: 'cert_center' as 'cert_center' | 'custom',
  panel_ssl_cert_id: '',
  panel_ssl_cert_pem: '',
  panel_ssl_key_pem: '',
  panel_force_https: false,
})

const certOptions = computed(() => [
  { label: '自动匹配（根据绑定域名自动匹配证书中心证书）', value: '' },
  ...certs.value.map((c) => ({
    label: `${c.domains.join(', ')}（${c.issuer || 'ACME'}，到期: ${c.not_after ? c.not_after.slice(0, 10) : '未知'}）`,
    value: c.id,
  })),
])

async function loadData() {
  loading.value = true
  try {
    const [settingsRes, certsRes, certStatusRes] = await Promise.allSettled([
      getSystemSettings(),
      listCerts(),
      getPanelCertStatus(),
    ])

    if (settingsRes.status === 'fulfilled') {
      const data = settingsRes.value.data
      form.public_url = (data[SETTING_KEYS.publicURL] as string) || ''
      form.panel_domain = (data[SETTING_KEYS.panelDomain] as string) || ''
      form.panel_domain_strict = data[SETTING_KEYS.panelDomainStrict] === true
      form.panel_ssl_enabled = data[SETTING_KEYS.panelSSLEnabled] === true
      form.panel_ssl_mode = ((data[SETTING_KEYS.panelSSLMode] as string) || 'cert_center') as 'cert_center' | 'custom'
      form.panel_ssl_cert_id = (data[SETTING_KEYS.panelSSLCertID] as string) || ''
      form.panel_ssl_cert_pem = (data[SETTING_KEYS.panelSSLCertPEM] as string) || ''
      form.panel_ssl_key_pem = (data[SETTING_KEYS.panelSSLKeyPEM] as string) || ''
      form.panel_force_https = data[SETTING_KEYS.panelForceHTTPS] === true
    }

    if (certsRes.status === 'fulfilled') {
      certs.value = (certsRes.value as Certificate[]) || []
    }

    if (certStatusRes.status === 'fulfilled') {
      activeCert.value = certStatusRes.value
    } else {
      activeCert.value = null
    }
  } catch (e: any) {
    message.error(e?.message || '加载配置数据失败')
  } finally {
    loading.value = false
  }
}

function validate(): string | null {
  if (form.panel_domain_strict && !form.panel_domain.trim()) {
    return '开启「禁止未绑定域名/直接IP访问」前，必须先填写「面板绑定域名」'
  }
  if (form.panel_force_https && !form.panel_ssl_enabled) {
    return '开启「强制 HTTPS 访问」前，必须先开启「面板 SSL / HTTPS」'
  }
  if (form.public_url.trim()) {
    const u = form.public_url.trim()
    if (!u.startsWith('http://') && !u.startsWith('https://')) {
      return '面板公网访问地址格式不正确，必须以 http:// 或 https:// 开头'
    }
  }
  if (form.panel_ssl_enabled && form.panel_ssl_mode === 'custom') {
    if (!form.panel_ssl_cert_pem.trim() || !form.panel_ssl_key_pem.trim()) {
      return '自定义证书模式下，必须提供证书内容 (PEM) 与私钥内容 (PEM)'
    }
  }
  return null
}

async function doSave() {
  const err = validate()
  if (err) {
    message.warning(err)
    return
  }
  saving.value = true
  try {
    await saveSystemSettings({
      [SETTING_KEYS.publicURL]: form.public_url.trim(),
      [SETTING_KEYS.panelDomain]: form.panel_domain.trim(),
      [SETTING_KEYS.panelDomainStrict]: form.panel_domain_strict,
      [SETTING_KEYS.panelSSLEnabled]: form.panel_ssl_enabled,
      [SETTING_KEYS.panelSSLMode]: form.panel_ssl_mode,
      [SETTING_KEYS.panelSSLCertID]: form.panel_ssl_cert_id,
      [SETTING_KEYS.panelSSLCertPEM]: form.panel_ssl_cert_pem.trim(),
      [SETTING_KEYS.panelSSLKeyPEM]: form.panel_ssl_key_pem.trim(),
      [SETTING_KEYS.panelForceHTTPS]: form.panel_force_https,
    })
    message.success('面板域名与证书配置已保存')
    try {
      activeCert.value = await getPanelCertStatus()
    } catch {
      // ignore
    }
  } catch (e: any) {
    message.error(e?.message || '保存配置失败')
  } finally {
    saving.value = false
  }
}

function goToCertCenter() {
  router.push('/certs')
}

onMounted(loadData)
onActivated(loadData)
</script>

<template>
  <div class="cert-domain-settings">
    <NSpin :show="loading">
      <NSpace vertical size="large">
        <NAlert type="info" :show-icon="true" class="page-intro">
          此页面用于配置 <strong>Watchman 控制台面板自身</strong> 的访问域名、公网入口、直接 IP
          访问防护策略及 HTTPS/SSL 加密证书。
          <br>
          <span class="muted tip-hint">
            注：应用容器反向代理与业务域名请前往「应用中心」在具体应用详情中绑定，与面板管理控制台隔离。
          </span>
        </NAlert>

        <!-- 面板访问与域名绑定 -->
        <NCard title="面板访问与域名控制" :bordered="false" size="small" class="settings-card">
          <template #header-extra>
            <NIcon size="20" class="header-icon"><GlobeOutline /></NIcon>
          </template>

          <NForm label-placement="top" :show-feedback="false">
            <NFormItem label="面板绑定域名 (Panel Domain)">
              <NInput
                v-model:value="form.panel_domain"
                placeholder="例如：panel.example.com"
                clearable
              />
              <template #feedback>
                <span class="muted tip-hint">
                  访问面板控制台的专属域名。配置后建议开启下方「禁止直接 IP 访问」以隐藏面板。
                </span>
              </template>
            </NFormItem>

            <NFormItem label="禁止未绑定域名 / 直接 IP 访问" class="switch-item">
              <NSpace align="center" size="small">
                <NSwitch v-model:value="form.panel_domain_strict" />
                <span class="switch-label">
                  {{ form.panel_domain_strict ? '已开启严格域名限制（仅允许绑定域名与回环地址访问）' : '未开启（允许任意域名或直接通过服务器 IP 访问）' }}
                </span>
              </NSpace>
              <template #feedback>
                <span class="muted tip-hint">
                  开启后，任何通过服务器 IP 或未授权域名发起的控制台请求将被 403 拦截，防止全网自动化扫描发现面板；Agent 注册与数据接口不受影响。
                </span>
              </template>
            </NFormItem>

            <NFormItem label="面板公网访问地址 (Public URL)">
              <NInput
                v-model:value="form.public_url"
                placeholder="例如：https://panel.example.com:18789"
                clearable
              />
              <template #feedback>
                <span class="muted tip-hint">
                  用于生成被管主机 Agent 一键安装命令、客户端下载地址与终端临时分享链接。留空时默认取当前请求地址。
                </span>
              </template>
            </NFormItem>
          </NForm>

          <NAlert type="warning" :show-icon="true" class="emergency-alert">
            <template #header>域名防失联与紧急恢复指南</template>
            若误开启严格域名限制导致无法通过 IP 或新域名登录面板，可在控制台部署服务器终端执行：
            <code>watchman-server -reset-panel-domain -data /opt/watchman/data</code>
            立即解除域名拦截与 IP 访问限制。
          </NAlert>
        </NCard>

        <!-- 面板 SSL / HTTPS 安全证书 -->
        <NCard title="面板 SSL / HTTPS 安全证书" :bordered="false" size="small" class="settings-card">
          <template #header-extra>
            <NIcon size="20" class="header-icon"><LockClosedOutline /></NIcon>
          </template>

          <NForm label-placement="top" :show-feedback="false">
            <NFormItem label="启用面板 SSL / HTTPS" class="switch-item">
              <NSpace align="center" size="small">
                <NSwitch v-model:value="form.panel_ssl_enabled" />
                <span class="switch-label">
                  {{ form.panel_ssl_enabled ? '已启用（支持通过 HTTPS 安全连接访问面板）' : '未启用（仅以 HTTP 明文方式运行）' }}
                </span>
              </NSpace>
              <template #feedback>
                <span class="muted tip-hint">
                  Watchman 控制台端口原生支持协议动态自适应，开启后无需重启服务即可即时处理 HTTPS 请求。
                </span>
              </template>
            </NFormItem>

            <template v-if="form.panel_ssl_enabled">
              <NFormItem label="强制 HTTPS 访问 (HTTP → HTTPS 自动重定向)" class="switch-item">
                <NSpace align="center" size="small">
                  <NSwitch v-model:value="form.panel_force_https" />
                  <span class="switch-label">
                    {{ form.panel_force_https ? '已开启强制重定向（明文 HTTP 自动 301 重定向至 HTTPS）' : '未开启（同时允许 HTTP 与 HTTPS 请求）' }}
                  </span>
                </NSpace>
              </NFormItem>

              <NFormItem label="证书来源模式">
                <NRadioGroup v-model:value="form.panel_ssl_mode" name="ssl_mode">
                  <NSpace>
                    <NRadio value="cert_center">从证书中心选取（推荐，支持 ACME 自动签发/续期）</NRadio>
                    <NRadio value="custom">手动粘贴自定义证书与私钥 (PEM)</NRadio>
                  </NSpace>
                </NRadioGroup>
              </NFormItem>

              <div v-if="form.panel_ssl_mode === 'cert_center'" class="cert-picker-block">
                <NFormItem label="选取证书中心已签发证书">
                  <NSpace style="width: 100%" vertical>
                    <NSelect
                      v-model:value="form.panel_ssl_cert_id"
                      :options="certOptions"
                      placeholder="请选择证书中心管理的有效证书"
                      clearable
                    />
                    <NSpace justify="space-between" align="center">
                      <span class="muted tip-hint">
                        选择「自动匹配」时，系统将在有请求到达时按访问域名自动从证书中心检索匹配的有效证书。
                      </span>
                      <NButton text type="primary" size="small" @click="goToCertCenter">
                        <template #icon><NIcon><OpenOutline /></NIcon></template>
                        前往证书中心申请或管理证书
                      </NButton>
                    </NSpace>
                  </NSpace>
                </NFormItem>
              </div>

              <div v-else class="custom-cert-block">
                <NFormItem label="证书内容 (PEM 格式，含完整证书链)">
                  <NInput
                    v-model:value="form.panel_ssl_cert_pem"
                    type="textarea"
                    placeholder="-----BEGIN CERTIFICATE-----&#10;...&#10;-----END CERTIFICATE-----"
                    :rows="6"
                  />
                </NFormItem>
                <NFormItem label="私钥内容 (PEM 格式)">
                  <NInput
                    v-model:value="form.panel_ssl_key_pem"
                    type="textarea"
                    placeholder="-----BEGIN PRIVATE KEY-----&#10;...&#10;-----END PRIVATE KEY-----"
                    :rows="5"
                  />
                </NFormItem>
              </div>

              <!-- 当前证书生效状态卡片 -->
              <div class="active-cert-status">
                <NDivider dashed style="margin: 12px 0 16px 0;">当前生效证书详情</NDivider>
                <div v-if="activeCert && activeCert.active" class="cert-info-grid">
                  <div class="info-row">
                    <span class="label">生效状态：</span>
                    <NTag type="success" size="small">
                      <template #icon><NIcon><CheckmarkCircleOutline /></NIcon></template>
                      HTTPS 正常运行中
                    </NTag>
                  </div>
                  <div class="info-row">
                    <span class="label">证书来源：</span>
                    <span>{{ activeCert.source }}</span>
                  </div>
                  <div class="info-row">
                    <span class="label">证书主题 (Subject)：</span>
                    <span class="monospace-text">{{ activeCert.subject || '-' }}</span>
                  </div>
                  <div class="info-row">
                    <span class="label">颁发者 (Issuer)：</span>
                    <span class="monospace-text">{{ activeCert.issuer || '-' }}</span>
                  </div>
                  <div class="info-row">
                    <span class="label">关联域名 (DNS Names)：</span>
                    <span>{{ activeCert.dns_names && activeCert.dns_names.length ? activeCert.dns_names.join(', ') : '-' }}</span>
                  </div>
                  <div class="info-row">
                    <span class="label">有效截止日期：</span>
                    <span>
                      {{ activeCert.not_after ? activeCert.not_after.slice(0, 10) : '-' }}
                      <NTag
                        :type="activeCert.days_left > 15 ? 'info' : (activeCert.days_left > 0 ? 'warning' : 'error')"
                        size="small"
                        style="margin-left: 8px;"
                      >
                        剩余 {{ activeCert.days_left }} 天
                      </NTag>
                    </span>
                  </div>
                </div>
                <NAlert v-else type="warning" :show-icon="true" size="small">
                  {{ activeCert?.error || '当前尚未匹配到有效的面板 SSL 证书，请确认已选择有效证书或上传了完整 PEM 证书对。' }}
                </NAlert>
              </div>
            </template>
          </NForm>

          <NAlert type="warning" :show-icon="true" class="emergency-alert">
            <template #header>SSL 异常与紧急恢复指南</template>
            若上传了无效证书或私钥导致控制台 HTTPS 握手失败且无法访问，可在控制台部署服务器终端执行：
            <code>watchman-server -reset-panel-ssl -data /opt/watchman/data</code>
            立即关闭面板 SSL 及强制重定向，恢复明文 HTTP 登录。
          </NAlert>
        </NCard>

        <!-- 底部保存按钮 -->
        <NSpace justify="end" class="action-footer">
          <NButton :loading="loading" @click="loadData">
            <template #icon><NIcon><RefreshOutline /></NIcon></template>
            刷新配置
          </NButton>
          <NButton type="primary" :loading="saving" @click="doSave">
            <template #icon><NIcon><ShieldCheckmarkOutline /></NIcon></template>
            保存配置
          </NButton>
        </NSpace>
      </NSpace>
    </NSpin>
  </div>
</template>

<style scoped lang="scss">
.cert-domain-settings {
  max-width: 920px;
}

.settings-card {
  background: var(--bg-card);
  border-radius: 8px;
}

.header-icon {
  color: var(--primary-color, #18a058);
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

.cert-info-grid {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 13px;
  background: var(--n-color-embedded, rgba(0, 0, 0, 0.02));
  padding: 12px 16px;
  border-radius: 6px;

  .info-row {
    display: flex;
    align-items: baseline;
    gap: 8px;

    .label {
      color: var(--text-color-3);
      min-width: 140px;
      flex-shrink: 0;
    }
  }
}

.monospace-text {
  font-family: monospace;
  font-size: 12px;
}

.action-footer {
  padding-top: 8px;
}
</style>
