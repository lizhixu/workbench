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

export function issueCert(domains: string[], accountId?: string) {
  return unwrap<{ ok: boolean; message: string }>(
    http.post('/certs/issue', { domains, account_id: accountId }),
  )
}

export function renewCert(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/certs/${id}/renew`))
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
  status: 'running' | 'done' | 'error'
  cert_id?: string
  error?: string
  created_at: string
}

// 启动面板证书签发任务（异步）：mode=domain 为绑定域名签发（DNS-01），
// mode=ip 为公网 IP 签发免费证书（HTTP-01，需临时开放 80 端口）。
export function issuePanelCert(mode: 'domain' | 'ip', ip?: string) {
  return unwrap<{ data: PanelCertJob; message?: string }>(
    http.post('/system/panel-cert/issue', { mode, ip }),
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
