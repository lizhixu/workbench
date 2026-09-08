import { http, unwrap } from './http'

export interface CertHubConfig {
  enabled: boolean
  base_url: string
  username: string
  password: string
  directory_url: string
  email: string
  updated_at?: string
}

export interface Certificate {
  id: string
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

export function listCerts() {
  return unwrap<{ data: Certificate[] }>(http.get('/certs')).then((r) => r.data)
}

export function issueCert(domains: string[]) {
  return unwrap<{ ok: boolean; message: string }>(http.post('/certs/issue', { domains }))
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