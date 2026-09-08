import { http, unwrap } from './http'

export interface PortMapping {
  host: number
  container: number
}

export interface AppEntity {
  id: string
  name: string
  host_id: string
  source_type: 'git' | 'image' | 'compose' | 'raw_compose'
  repo_url?: string
  branch?: string
  auth_vault_id?: string
  auto_deploy: boolean
  webhook_token?: string
  build_type?: string
  dockerfile?: string
  build_context?: string
  build_timeout_sec?: number
  compose_content?: string
  image?: string
  env_vars?: Record<string, string>
  ports?: PortMapping[]
  volumes?: string[]
  healthcheck_url?: string
  container_name?: string
  domain?: string
  proxy_mode?: 'local' | 'gateway'
  proxy_upstream?: string
  current_commit?: string
  last_deploy_at?: string
  created_at: string
}

export interface Deployment {
  id: string
  app_id: string
  commit_hash?: string
  commit_message?: string
  trigger: 'webhook' | 'manual' | 'rollback'
  status: 'queued' | 'building' | 'deploying' | 'success' | 'failed'
  started_by?: string
  started_at: string
  finished_at?: string
  duration_ms?: number
  exit_code?: number
  error?: string
  build_log?: string
}

export interface AppCreateRequest {
  name: string
  host_id: string
  source_type?: 'git' | 'raw_compose'
  repo_url?: string
  branch?: string
  auth_vault_id?: string
  auto_deploy?: boolean
  build_type?: 'dockerfile' | 'compose'
  dockerfile?: string
  build_context?: string
  build_timeout_sec?: number
  compose_content?: string
  env_vars?: Record<string, string>
  ports?: PortMapping[]
  volumes?: string[]
  healthcheck_url?: string
  container_name?: string
}

export interface AppStatus {
  app_id: string
  deploying: boolean
  agent_online: boolean
  state: string
  current_commit?: string
  started_at?: string
}

export function listApps() {
  return unwrap<{ data: AppEntity[] }>(http.get('/apps')).then((r) => r.data)
}

export function getApp(id: string) {
  return unwrap<{ data: AppEntity }>(http.get(`/apps/${id}`)).then((r) => r.data)
}

export function createApp(req: AppCreateRequest) {
  return unwrap<{ data: AppEntity }>(http.post('/apps', req)).then((r) => r.data)
}

export function updateApp(id: string, req: Partial<AppCreateRequest>) {
  return unwrap<{ data: AppEntity }>(http.put(`/apps/${id}`, req)).then((r) => r.data)
}

export function deleteApp(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/apps/${id}`))
}

export function deployApp(id: string) {
  return unwrap<{ data: Deployment }>(http.post(`/apps/${id}/deploy`)).then((r) => r.data)
}

export function rollbackApp(id: string, deploymentId?: string) {
  const q = deploymentId ? `?deployment_id=${encodeURIComponent(deploymentId)}` : ''
  return unwrap<{ data: Deployment }>(http.post(`/apps/${id}/rollback${q}`)).then((r) => r.data)
}

export function stopApp(id: string) {
  return unwrap<{ ok: boolean }>(http.post(`/apps/${id}/stop`))
}

export function startApp(id: string) {
  return unwrap<{ ok: boolean }>(http.post(`/apps/${id}/start`))
}

export function restartApp(id: string) {
  return unwrap<{ ok: boolean }>(http.post(`/apps/${id}/restart`))
}

export function listDeployments(appId: string, offset = 0, pageSize = 20) {
  return unwrap<{ data: Deployment[]; total: number; offset: number; page_size: number }>(
    http.get(`/apps/${appId}/deployments`, { params: { offset, page_size: pageSize } }),
  )
}

export function getDeployment(appId: string, depId: string) {
  return unwrap<{ data: Deployment }>(http.get(`/apps/${appId}/deployments/${depId}`)).then((r) => r.data)
}

export interface AIDiagnosis {
  summary: string
  suggestions: string[]
  commands?: string[]
}

export function diagnoseDeployment(appId: string, depId: string) {
  return unwrap<{ data: AIDiagnosis }>(
    http.post(`/apps/${appId}/deployments/${depId}/diagnose`),
  ).then((r) => r.data)
}

export function getAppStatus(id: string) {
  return unwrap<{ data: AppStatus }>(http.get(`/apps/${id}/status`)).then((r) => r.data)
}