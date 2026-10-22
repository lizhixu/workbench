import { http, unwrap } from './http'

export interface PortMapping {
  host: number
  container: number
  /** "public" binds 0.0.0.0; "mesh" binds the host's Tailscale IP. Empty means public. */
  bind_scope?: 'public' | 'mesh' | ''
}

export type AppSourceType = 'git' | 'image' | 'template' | 'raw_compose'

export interface AppEntity {
  id: string
  name: string
  host_id: string
  source_type: AppSourceType
  template_id?: string
  template_params?: Record<string, string>
  repo_url?: string
  branch?: string
  auth_vault_id?: string
  auto_deploy: boolean
  webhook_token?: string
  webhook_auto_managed?: boolean
  github_hook_id?: number
  webhook_error?: string
  build_type?: string
  dockerfile?: string
  build_context?: string
  build_timeout_sec?: number
  compose_content?: string
  image?: string
  /** 组网端口转发（TCP 中继）配置 */
  mesh_forward_enabled?: boolean
  mesh_forward_host_id?: string
  mesh_forward_entry_port?: number
  mesh_forward_target_port?: number
  mesh_forward_target?: string
  env_vars?: Record<string, string>
  ports?: PortMapping[]
  volumes?: string[]
  healthcheck_url?: string
  container_name?: string
  domain?: string
  proxy_mode?: 'local' | 'gateway'
  proxy_gateway_host_id?: string
  proxy_upstream?: string
  current_commit?: string
  last_deploy_at?: string
  created_at: string
  /** Advances on every persisted mutation (config edit, deploy, proxy bind). */
  updated_at?: string
}

export interface Deployment {
  id: string
  app_id: string
  commit_hash?: string
  commit_message?: string
  trigger: 'create' | 'webhook' | 'manual' | 'rollback'
  status: 'queued' | 'building' | 'deploying' | 'success' | 'failed' | 'cancelled'
  started_by?: string
  cancelled_by?: string
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
  source_type?: AppSourceType
  template_id?: string
  template_params?: Record<string, string>
  repo_url?: string
  branch?: string
  auth_vault_id?: string
  auto_deploy?: boolean
  build_type?: 'dockerfile' | 'compose'
  dockerfile?: string
  build_context?: string
  build_timeout_sec?: number
  compose_content?: string
  image?: string
  mesh_forward_enabled?: boolean
  mesh_forward_host_id?: string
  mesh_forward_entry_port?: number
  mesh_forward_target_port?: number
  env_vars?: Record<string, string>
  ports?: PortMapping[]
  volumes?: string[]
  healthcheck_url?: string
  container_name?: string
}

export interface EnvField {
  key: string
  label: string
  description: string
  default: string
  required: boolean
  is_secret: boolean
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

export function listAppCatalog() {
  return unwrap<{ data: AppTemplate[] }>(http.get('/apps/catalog')).then((r) => r.data)
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

export function deleteApp(id: string, purge = false) {
  return unwrap<{ ok: boolean; purged?: boolean; warnings?: string[] }>(
    http.delete(`/apps/${id}`, { params: { purge } }),
  )
}

export function deployApp(id: string, force = false) {
  return unwrap<{ data: Deployment }>(
    http.post(`/apps/${id}/deploy`, { force }),
  ).then((r) => r.data)
}

export function cancelDeployment(id: string) {
  return unwrap<{ ok: boolean; cancelled: boolean; message: string }>(
    http.post(`/apps/${id}/deployments/cancel`),
  )
}

/** 应用挂载卷备份归档（volbackup.go 的 HTTP 面） */
export interface VolumeArchive {
  id: string
  app_id: string
  host_id: string
  /** 原始 docker -v 规格，如 "/opt/apps/redis/data:/data" */
  volume_spec: string
  /** 解析后的宿主机数据目录（bind 路径或 Docker 具名卷挂载点） */
  host_path: string
  /** 容器内路径 */
  container_path: string
  /** 归档在被管主机上的路径 /var/backups/watchman/apps/<appID>/<id>.tar.gz */
  file_path: string
  size: number
  sha256?: string
  /** running | success | failed */
  status: string
  error?: string
  note?: string
  created_at: string
}

export interface VolumeBackupListResp {
  data: VolumeArchive[]
  total: number
  offset: number
  page_size: number
}

export function listVolumeBackups(id: string, params?: { volume?: string; offset?: number; page_size?: number }) {
  return unwrap<VolumeBackupListResp>(http.get(`/apps/${id}/volume-backups`, { params }))
}

export function backupAppVolumes(id: string, req: { volumes?: string[]; note?: string }) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/apps/${id}/volume-backups`, req))
}

export function restoreVolumeArchive(id: string, archiveId: string) {
  return unwrap<{ ok: boolean; message: string }>(
    http.post(`/apps/${id}/volume-backups/${archiveId}/restore`),
  )
}

export function deleteVolumeArchive(id: string, archiveId: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/apps/${id}/volume-backups/${archiveId}`))
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

export function syncAppWebhook(appId: string) {
  return unwrap<{ ok: boolean; message: string; data: AppEntity }>(
    http.post(`/apps/${encodeURIComponent(appId)}/webhook/sync`),
  )
}

// ---- Ground-truth metadata probing (read-only, used by the create form's
// auto-fill hints and the AI assistant fact injection) ----

export interface ImageMetadata {
  registry: string
  repository: string
  tag: string
  reference: string
  os: string
  architecture: string
  exposed_ports: number[]
  env_vars: Record<string, string>
  volumes: string[]
  cmd: string[]
  entrypoint: string[]
  working_dir: string
}

export interface ComposePort {
  service: string
  host: number
  container: number
}

export interface RepoMetadata {
  repo_url: string
  owner: string
  name: string
  branch: string
  has_compose: boolean
  compose_file: string
  /** Raw compose file content (server-capped at 64KB); may be empty. */
  compose_content: string
  compose_ports: ComposePort[]
  compose_env: Record<string, string>
  has_dockerfile: boolean
  dockerfile_path: string
  exposed_ports: number[]
  dockerfile_env: Record<string, string>
  volumes: string[]
  env_example: Record<string, string>
  language: string
}

// 探测接口放宽单请求超时：服务端 ctx 预算为镜像 15s / 仓库 20s，
// 默认 http 实例 15s 超时会在后端即将返回前误报失败。
export function inspectImage(image: string) {
  return unwrap<{ data: ImageMetadata }>(
    http.post('/apps/inspect-image', { image }, { timeout: 20000 }),
  ).then((r) => r.data)
}

export function inspectRepo(repoUrl: string) {
  return unwrap<{ data: RepoMetadata }>(
    http.post('/apps/inspect-repo', { repo_url: repoUrl }, { timeout: 30000 }),
  ).then((r) => r.data)
}