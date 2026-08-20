import { http, unwrap } from './http'
import type { EnrollResponse, HealthResponse, Host, ListResponse, SessionRecord } from './types'

export function listHosts() {
  return unwrap<ListResponse<Host>>(http.get('/hosts'))
}

export function getHost(id: string) {
  return unwrap<{ data: Host }>(http.get(`/hosts/${id}`)).then((r) => r.data)
}

export function enroll() {
  return unwrap<EnrollResponse>(http.post('/hosts/enroll'))
}

export function health() {
  return unwrap<HealthResponse>(http.get('/system/health'))
}

export function deleteHost(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/hosts/${id}`))
}

export function setHostGroup(id: string, group: string) {
  return unwrap<{ ok: boolean }>(http.put(`/hosts/${id}/group`, { group }))
}

export function setHostTags(id: string, tags: string[]) {
  return unwrap<{ ok: boolean }>(http.put(`/hosts/${id}/tags`, { tags }))
}

// ---- Terminal ----
export interface TerminalSession {
  session_id: string
  ws_url: string
}

export function openTerminal(hostId: string, shell = '') {
  // Pass the current JWT as ws_token so the server echoes it back in ws_url;
  // the WS gateway requires ?token=<jwt> for authentication.
  const token = localStorage.getItem('watchman_token') || ''
  return unwrap<TerminalSession>(
    http.post(`/hosts/${hostId}/terminals`, shell ? { shell } : null, {
      params: token ? { ws_token: token } : {},
    }),
  )
}

export interface ShareTerminalResponse {
  share_token: string
  share_code: string
  session_id: string
  mode: 'view' | 'control'
  expires_at: string
}

export function shareTerminal(hostId: string, sid: string, data: { expire_minutes?: number; mode?: 'view' | 'control' } = {}) {
  return unwrap<ShareTerminalResponse>(http.post(`/hosts/${hostId}/terminals/${sid}/share`, data))
}

export interface ShareInfoResponse {
  session_id: string
  host_id: string
  hostname: string
  mode: 'view' | 'control'
  expires_at: string
}

export function getShareInfo(token: string) {
  return unwrap<ShareInfoResponse>(http.get(`/terminals/share/${token}`))
}

// ---- Files ----
export function fileList(hostId: string, path: string) {
  return unwrap<{ data: any[] }>(http.get(`/hosts/${hostId}/files`, { params: { path } })).then((r) => r.data)
}

export function fileStat(hostId: string, path: string) {
  return unwrap<{ data: any }>(http.get(`/hosts/${hostId}/files/stat`, { params: { path } })).then((r) => r.data)
}

export function fileMkdir(hostId: string, path: string) {
  return unwrap<{ ok: boolean }>(http.post(`/hosts/${hostId}/files/mkdir`, { path }))
}

export function fileMove(hostId: string, path: string, destPath: string) {
  return unwrap<{ ok: boolean }>(http.post(`/hosts/${hostId}/files/move`, { path, dest_path: destPath }))
}

export function fileCopy(hostId: string, path: string, destPath: string) {
  return unwrap<{ ok: boolean }>(http.post(`/hosts/${hostId}/files/copy`, { path, dest_path: destPath }))
}

export function fileRemove(hostId: string, path: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/hosts/${hostId}/files`, { params: { path } }))
}

export function fileDownloadUrl(hostId: string, path: string) {
  const token = localStorage.getItem('watchman_token') || ''
  const tokParam = token ? `&token=${encodeURIComponent(token)}` : ''
  return `/api/v1/hosts/${hostId}/files/download?path=${encodeURIComponent(path)}${tokParam}`
}

export async function fileUpload(hostId: string, path: string, file: File) {
  const resp = await http.post(`/hosts/${hostId}/files/upload?path=${encodeURIComponent(path)}`, file, {
    headers: { 'Content-Type': 'application/octet-stream' },
  })
  return resp.data
}

// ---- Exec ----
export function execCommand(hostId: string, command: string, shell = '', timeoutSec = 60, isScript = false, confirmRisk = false) {
  return unwrap<any>(http.post(`/hosts/${hostId}/exec`, {
    command,
    shell,
    timeout_sec: timeoutSec,
    is_script: isScript,
  }, confirmRisk ? { headers: { 'X-Confirm-Risk': 'true' } } : {}))
}

export function batchExec(hostIds: string[], command: string, shell = '', timeoutSec = 60, confirmRisk = false) {
  return unwrap<{ results: any[] }>(http.post('/hosts/batch-exec', {
    host_ids: hostIds,
    command,
    shell,
    timeout_sec: timeoutSec,
  }, confirmRisk ? { headers: { 'X-Confirm-Risk': 'true' } } : {})).then((r) => r.results)
}

// ---- Metrics ----
export function getMetrics(hostId: string) {
  return unwrap<any>(http.get(`/hosts/${hostId}/metrics`))
}

export interface MetricPoint {
  ts: number
  cpu_usage: number
  mem_usage: number
  mem_total: number
  mem_used: number
  net_rx: number
  net_tx: number
  disk_read: number
  disk_write: number
}

export interface MetricsHistoryResponse {
  host_id: string
  from: number
  to: number
  step: number
  points: MetricPoint[]
}

export function getMetricsHistory(hostId: string, params: { from?: number; to?: number; step?: number } = {}) {
  return unwrap<MetricsHistoryResponse>(http.get(`/hosts/${hostId}/metrics/history`, { params }))
}

// ---- App Store ----
export interface EnvField {
  key: string
  label: string
  default: string
  type?: 'text' | 'password' | 'number'
  required?: boolean
  description?: string
}

export interface AppTemplate {
  id: string
  name: string
  category: string
  icon: string
  version: string
  description: string
  image: string
  default_port: number
  container_port: number
  default_volume: string
  env_fields: EnvField[]
}

export interface AppInstance {
  name: string
  app_id: string
  image: string
  status: string
  state: string
  ports: string
  created: number
  running: boolean
}

export function getAppCatalog() {
  return unwrap<{ data: AppTemplate[] }>(http.get('/apps/catalog')).then((r) => r.data || [])
}

export function getInstalledApps(hostId: string) {
  return unwrap<{ data: AppInstance[] }>(http.get(`/hosts/${hostId}/apps`)).then((r) => r.data || [])
}

export function installApp(
  hostId: string,
  payload: {
    app_id: string
    name?: string
    port?: number
    volume?: string
    env?: Record<string, string>
    restart?: string
  },
) {
  return unwrap<any>(http.post(`/hosts/${hostId}/apps/install`, payload))
}

export function uninstallApp(hostId: string, appName: string) {
  return unwrap<any>(http.delete(`/hosts/${hostId}/apps/${appName}`))
}

export function getDockerInstallScript(hostId: string) {
  return unwrap<{ script: string; command: string }>(http.get(`/hosts/${hostId}/apps/install-docker-script`))
}

// ---- SysInfo ----
export function getSysInfo(hostId: string, kind: string) {
  return unwrap<any>(http.get(`/hosts/${hostId}/sysinfo/${kind}`))
}

// ---- Docker ----
export function dockerPs(hostId: string) {
  return unwrap<{ data: any[] }>(http.get(`/hosts/${hostId}/docker/ps`)).then((r) => r.data || [])
}

export function dockerImages(hostId: string) {
  return unwrap<{ data: any[] }>(http.get(`/hosts/${hostId}/docker/images`)).then((r) => r.data || [])
}

// dockerAll fetches containers + images in a single round trip.
export function dockerAll(hostId: string): Promise<{ containers: any[]; images: any[] }> {
  return unwrap<{ data: { containers: any[]; images: any[] } }>(
    http.get(`/hosts/${hostId}/docker/all`),
  ).then((r) => r.data || { containers: [], images: [] })
}

export function dockerOp(hostId: string, op: string, container = '', image = '') {
  return unwrap<any>(http.post(`/hosts/${hostId}/docker/${op}`, { container, image }))
}

// ---- Sessions ----
export function listSessions() {
  return unwrap<ListResponse<SessionRecord>>(http.get('/sessions'))
}

export function getSession(id: string) {
  return unwrap<{ data: SessionRecord }>(http.get(`/sessions/${id}`)).then((r) => r.data)
}

export function deleteSession(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/sessions/${id}`))
}

export function sessionRecordingUrl(id: string): string {
  const baseURL = (http.defaults.baseURL || '/api/v1').replace(/\/$/, '')
  const token = localStorage.getItem('watchman_token') || ''
  return `${baseURL}/sessions/${id}/recording?token=${encodeURIComponent(token)}`
}