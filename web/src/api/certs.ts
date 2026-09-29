import { http, unwrap } from './http'

export interface CertHubConfig {
  enabled: boolean
  base_url: string
  username: string
  password: string
  updated_at?: string
}

export interface ACMEPreset {
  id: string
  name: string
  directory_url: string
  requires_eab: boolean
  description: string
}

export interface ACMEAccount {
  id: string
  name: string
  provider_id: string
  directory_url: string
  email: string
  eab_key_id?: string
  eab_hmac_key?: string
  is_default: boolean
  created_at: string
}

export interface Certificate {
  id: string
  account_id?: string
  domains: string[]
  not_before: string
  not_after: string
  issuer?: string
  auto_renew: boolean
  created_at: string
  renewing?: boolean
  last_error?: string
}

export function getCertConfig() {
  return unwrap<{ data: CertHubConfig }>(http.get('/certs/config')).then((r) => r.data)
}

export function updateCertConfig(cfg: CertHubConfig) {
  return unwrap<{ data: CertHubConfig }>(http.put('/certs/config', cfg)).then((r) => r.data)
}

export function listPresets() {
  return unwrap<{ data: ACMEPreset[] }>(http.get('/certs/presets')).then((r) => r.data)
}

export function listACMEAccounts() {
  return unwrap<{ data: ACMEAccount[] }>(http.get('/certs/accounts')).then((r) => r.data)
}

export function createACMEAccount(req: Partial<ACMEAccount>) {
  return unwrap<{ data: ACMEAccount }>(http.post('/certs/accounts', req)).then((r) => r.data)
}

export function updateACMEAccount(id: string, req: Partial<ACMEAccount>) {
  return unwrap<{ data: ACMEAccount }>(http.put(`/certs/accounts/${id}`, req)).then((r) => r.data)
}

export function deleteACMEAccount(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/certs/accounts/${id}`))
}

export function listCerts() {
  return unwrap<{ data: Certificate[] }>(http.get('/certs')).then((r) => r.data)
}

// challenge: ''（自动）| 'http-01' | 'dns-01' | 'dns-01-manual'（手动添加 DNS TXT 解析）。
export type ChallengeMode = '' | 'http-01' | 'dns-01' | 'dns-01-manual'

export function issueCert(domains: string[], accountId?: string, challenge?: ChallengeMode) {
  return unwrap<{ ok: boolean; message: string }>(
    http.post('/certs/issue', { domains, account_id: accountId, challenge: challenge || undefined }),
  )
}

// ---- 手动 DNS-01（管理员手动添加 TXT 解析） ----

export interface DNSRecord {
  domain: string // 标识符，如 example.com 或 *.example.com
  host: string // _acme-challenge.example.com
  type: string // 'TXT'
  value: string // TXT 记录值
}

export interface ManualDNSOrder {
  id: string
  account_id: string
  identifiers: string[]
  records: DNSRecord[]
  status: 'awaiting_dns' | 'verifying' | 'done' | 'error'
  cert_id?: string
  error?: string
  // terminal=true 表示 CA 已终态判定失败，该订单不可重试，需重新发起
  terminal?: boolean
  created_at: string
  expires_at: string
}

// 发起手动 DNS-01 签发：服务端创建 ACME 订单并返回待添加的 TXT 记录，
// 管理员在 DNS 服务商添加解析后调用 confirmManualDNSOrder 触发 CA 验证。
export function startManualDNSIssue(domains: string[], accountId?: string) {
  return unwrap<{ data: ManualDNSOrder }>(
    http.post('/certs/issue', { domains, account_id: accountId, challenge: 'dns-01-manual' }),
  ).then((r) => r.data)
}

export function getManualDNSOrder(id: string) {
  return unwrap<{ data: ManualDNSOrder }>(http.get(`/certs/manual/${id}`)).then((r) => r.data)
}

// 确认已完成 DNS 解析，通知 CA 开始验证（异步，轮询 getManualDNSOrder 看结果）。
export function confirmManualDNSOrder(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/certs/manual/${id}/confirm`))
}

// 取消待处理的手动 DNS-01 订单（CA 侧订单自然过期；已添加的 TXT 记录需自行到 DNS 服务商删除）。
export function cancelManualDNSOrder(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.delete(`/certs/manual/${id}`))
}

export function renewCert(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/certs/${id}/renew`))
}

// 上传已有证书（其他渠道申请的 PEM 证书+私钥）：导入证书中心，
// 进入已签发证书列表，可用于应用代理或面板绑定。私钥不经接口回显。
export function importCert(certPEM: string, keyPEM: string) {
  return unwrap<{ data: Certificate }>(
    http.post('/certs/import', { cert_pem: certPEM, key_pem: keyPEM }),
  ).then((r) => r.data)
}

export function deleteCert(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/certs/${id}`))
}

export interface ProxyBindRequest {
  domain: string
  mode: 'local' | 'gateway'
  gateway_host_id?: string
  upstream?: string
  cert_id?: string
  websocket?: boolean
}

export function bindProxy(appId: string, req: ProxyBindRequest) {
  return unwrap<{ ok: boolean; domain: string; mode: string; upstream: string }>(
    http.put(`/apps/${appId}/proxy`, req),
  )
}

export function unbindProxy(appId: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/apps/${appId}/proxy`))
}

// ---- 面板一键证书签发 ----

export interface PanelCertJob {
  id: string
  mode: 'domain' | 'ip'
  target: string
  challenge?: ChallengeMode
  status: 'running' | 'awaiting_dns' | 'done' | 'error'
  cert_id?: string
  error?: string
  pending_id?: string
  dns_records?: DNSRecord[]
  // terminal=true 表示 CA 已终态判定失败，不可重试，需重新发起
  terminal?: boolean
  created_at: string
}

// 启动面板证书签发任务（异步）：mode=domain 为绑定域名签发（HTTP-01 优先，
// 未接入 80 端口时回退到 DNS-01，需配置 dns-mng；通配符仅支持 DNS-01），
// mode=ip 为公网 IP 签发免费证书（HTTP-01，需临时开放 80 端口）。
// challenge 可显式指定验证方式：'' 自动 | 'http-01' | 'dns-01' | 'dns-01-manual'。
// 手动 DNS-01 时任务状态为 awaiting_dns 并返回 dns_records，管理员添加解析后
// 调用 confirmPanelCertIssue 触发验证。
export function issuePanelCert(mode: 'domain' | 'ip', ip?: string, challenge?: ChallengeMode) {
  return unwrap<{ data: PanelCertJob; message?: string }>(
    http.post('/system/panel-cert/issue', { mode, ip, challenge: challenge || undefined }),
  )
}

// 确认面板手动 DNS-01 任务：已添加 TXT 解析后通知 CA 开始验证（异步）。
export function confirmPanelCertIssue(jobId: string) {
  return unwrap<{ data: PanelCertJob; message?: string }>(
    http.post(`/system/panel-cert/issue/${jobId}/confirm`),
  )
}

// 取消面板手动 DNS-01 任务（同时取消证书中心的待处理订单）。
export function cancelPanelCertIssue(jobId: string) {
  return unwrap<{ ok: boolean; message: string }>(
    http.delete(`/system/panel-cert/issue/${jobId}`),
  )
}

export function getPanelCertJob(jobId: string) {
  return unwrap<{ data: PanelCertJob }>(http.get(`/system/panel-cert/issue/${jobId}`)).then(
    (r) => r.data,
  )
}

// 检测服务器公网出口 IP（用于 IP 证书签发）。
export function detectPublicIP() {
  return unwrap<{ data: { ip: string } }>(http.get('/system/panel-cert/public-ip')).then(
    (r) => r.data.ip,
  )
}
