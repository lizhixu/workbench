import { http, unwrap } from './http'

export interface PortMapping {
  host: number
  container: number
  /** "public" binds 0.0.0.0; "mesh" binds the host's Tailscale IP. Empty means public. */
  bind_scope?: 'public' | 'mesh' | ''
}

export type AppSourceType = 'git' | 'image' | 'template' | 'raw_compose'

export interface DomainBinding {
  domain: string
  cert_id: string
  // true = 证书由服务端按域名自动匹配（保存时物化具体 ID）；前端展示为
  // 「自动匹配」并在下次保存时重新解析，不把旧证书钉死成显式选择。
  auto?: boolean
  // true = 该域名 80 端口 301 跳转 https；缺省/false = 80 直接提供明文访问。
  force_https?: boolean
}

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
  proxy_domains?: DomainBinding[]
  proxy_mode?: 'local' | 'gateway'
  proxy_gateway_host_id?: string
  proxy_upstream?: string
  proxy_websocket?: boolean
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
  trigger: 'create' | 'webhook' | 'github-app' | 'manual' | 'rollback' | 'migrate' | 'upgrade'
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

/** 更新应用的返回：data 之外，后端可能在顶层附带 mesh_forward_warning
 *（组网中继同步失败时配置已保存但中继未生效）。此前 updateApp 只 unwrap
 * .data，警告被静默丢弃，前端无条件报“已保存并同步”——假成功。 */
export interface AppUpdateResult {
  data: AppEntity
  meshForwardWarning?: string
}

export function updateApp(id: string, req: Partial<AppCreateRequest>): Promise<AppUpdateResult> {
  return unwrap<{ data: AppEntity; mesh_forward_warning?: string }>(
    http.put(`/apps/${id}`, req),
  ).then((r) => ({ data: r.data, meshForwardWarning: r.mesh_forward_warning }))
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

export function upgradeApp(id: string) {
  return unwrap<{ data: Deployment }>(
    http.post(`/apps/${id}/upgrade`, {}),
  ).then((r) => r.data)
}

export function cancelDeployment(id: string) {
  return unwrap<{ ok: boolean; cancelled: boolean; message: string }>(
    http.post(`/apps/${id}/deployments/cancel`),
  )
}

/** 服务迁移：把应用（可选连同挂载卷数据）搬到另一台主机。 */
export interface MigrateAppRequest {
  target_host_id: string
  /** 经 S3 中转同步挂载卷数据；需要默认 S3 存储目标 */
  migrate_volumes?: boolean
  /** 迁移成功后删除源主机容器（bind mount 宿主目录始终保留） */
  clean_source?: boolean
}

/** 目标主机就绪探测结果。所有问题都以 warnings 下发，不阻断请求。 */
export interface MigratePrecheck {
  target_host_id: string
  agent_online: boolean
  docker_running: boolean
  volume_count: number
  s3_ready: boolean
  s3_bucket?: string
  port_conflicts?: number[]
  warnings?: string[]
}

/** 只读预检：探测目标主机连通性、Docker、端口占用与卷迁移前置条件。 */
export function precheckMigration(id: string, targetHostId: string) {
  return unwrap<{ data: MigratePrecheck }>(
    http.post(`/apps/${id}/migrate/precheck`, { target_host_id: targetHostId }),
  ).then((r) => r.data)
}

/** 发起迁移（异步 202）：返回部署记录，进度经部署历史轮询。 */
export function migrateApp(id: string, req: MigrateAppRequest) {
  return unwrap<{ data: Deployment }>(
    http.post(`/apps/${id}/migrate`, req),
  ).then((r) => r.data)
}

/** 应用运行时挂载卷（单容器声明卷或 Compose 动态发现卷） */
export interface DiscoveredVolume {
  volume_spec: string
  volume_type: string // "bind" | "volume"
  host_path: string
  container_path: string
  service?: string
  read_only?: boolean
}

/** 归档内单个卷元数据 */
export interface VolumeItemArchive {
  volume_spec: string
  volume_type?: string
  host_path?: string
  container_path?: string
  service?: string
  size?: number
  sha256?: string
}

/** 应用挂载卷备份归档（volbackup.go 的 HTTP 面，支持 S3 与本地存储） */
export interface VolumeArchive {
  id: string
  app_id: string
  host_id: string
  /** 原始 docker -v 规格，多卷快照时为 "ALL" */
  volume_spec: string
  /** 解析后的宿主机数据目录（bind 路径或 Docker 具名卷挂载点） */
  host_path: string
  /** 容器内路径 */
  container_path: string
  /** 快照包含的全部挂载卷清单 */
  volumes?: VolumeItemArchive[]
  /** 备份一致性模式：online (在线) | pause (暂停写) | stop (停机冷备) */
  consistency_mode?: 'online' | 'pause' | 'stop'
  /** 归档写入的存储目标（"local" | S3 目标 ID） */
  storage_target: string
  storage_target_name?: string
  /** 存储路径（本地相对路径或 S3 对象键） */
  storage_path: string
  size: number
  sha256?: string
  /** running | success | failed | uploaded（uploaded = 上传恢复记录） */
  status: string
  /** 归档来源：空/manual = 面板备份，job = 快照定时任务，upload = 上传恢复记录 */
  source?: string
  error?: string
  note?: string
  created_at: string
  finished_at?: string
  /** 最近一次从该归档恢复的时间/结果（success | failed）与失败原因 */
  last_restore_at?: string
  last_restore_status?: string
  last_restore_error?: string
}

/** 卷备份的 S3 去向状态：ready=false 时不能发起备份 */
export interface VolumeS3Status {
  ready: boolean
  target_id?: string
  target_name?: string
  bucket?: string
}

export interface VolumeBackupListResp {
  data: VolumeArchive[]
  total: number
  offset: number
  page_size: number
  s3: VolumeS3Status
  volumes?: DiscoveredVolume[]
  volumes_error?: string
}

export function listVolumeBackups(id: string, params?: { volume?: string; offset?: number; page_size?: number }) {
  return unwrap<VolumeBackupListResp>(http.get(`/apps/${id}/volume-backups`, { params }))
}

export function getAppVolumes(id: string) {
  return unwrap<{ data: DiscoveredVolume[] }>(http.get(`/apps/${id}/volumes`)).then((r) => r.data)
}

export function backupAppVolumes(id: string, req: {
  volumes?: string[]
  note?: string
  target_id?: string
  consistency_mode?: 'online' | 'pause' | 'stop'
}) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/apps/${id}/volume-backups`, req))
}

export function restoreVolumeArchive(id: string, archiveId: string, req?: {
  clean_restore?: boolean
  volumes?: string[]
}) {
  return unwrap<{ ok: boolean; message: string }>(
    http.post(`/apps/${id}/volume-backups/${archiveId}/restore`, req || {}),
  )
}

export function deleteVolumeArchive(id: string, archiveId: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/apps/${id}/volume-backups/${archiveId}`))
}

/** 上传识别：一份已暂存文件自动识别出的一个候选目标卷 */
export interface VolumeScanMatch {
  volume_spec: string
  container_path?: string
  host_path?: string
  service?: string
  volume_type?: string
  /** manifest（快照清单命中）| container_path（内容路径命中）| basename（目录名命中）| filename（文件名特征命中）| single_candidate（唯一可用卷） */
  reason: 'manifest' | 'container_path' | 'basename' | 'filename' | 'single_candidate'
}

/** 上传识别结果：一份已暂存的备份文件及其目标卷识别状态 */
export interface VolumeScanItem {
  archive_id: string
  filename: string
  size: number
  sha256: string
  /** 本系统导出的多卷快照 tar（内嵌 manifest.json） */
  snapshot: boolean
  /** 归档顶层目录/文件名（展示识别依据用） */
  top_dirs?: string[]
  /** 快照内声明的全部卷清单 */
  manifest_volumes?: VolumeItemArchive[]
  /** manifest（快照清单）| unique（唯一命中）| ambiguous（多个候选）| none（未识别） */
  match: 'manifest' | 'unique' | 'ambiguous' | 'none'
  matched: VolumeScanMatch[]
}

/**
 * 离线上传第一步：把备份文件直传面板暂存，服务端解开 tar 读取内容并
 * 自动识别每份文件该恢复到哪个挂载卷（快照读内嵌 manifest.json，裸 tar
 * 按顶层目录与容器路径匹配）。识别结果回显给操作员过目，确认走
 * commitVolumeUploadRestore。文件可能达 GB 级，不设 axios 超时。
 */
export function scanVolumeUploads(id: string, form: FormData) {
  return unwrap<{ ok: boolean; items: VolumeScanItem[] }>(
    http.post(`/apps/${id}/volume-backups/upload-scan`, form, { timeout: 0 }),
  )
}

/**
 * 离线上传第二步：提交识别/改选后的（文件 → 目标卷）配对。服务端校验后
 * 202 受理异步执行一次批量恢复（全部文件校验通过后一次停机、逐卷解包、
 * 一次重启），返回归档记录 id 列表（archive_id 为首个，兼容单目标调用方）
 * 供详情页轮询各自最近恢复结果。SHA256 由服务端在暂存时计算，提交即
 * 作为 Agent 下载后的硬校验，不再依赖客户端手工填写。
 */
export function commitVolumeUploadRestore(
  id: string,
  pairs: { archive_id: string; specs: string[] }[],
) {
  return unwrap<{ ok: boolean; archive_id: string; archive_ids: string[]; message: string }>(
    http.post(`/apps/${id}/volume-backups/upload-commit`, { pairs }, { timeout: 0 }),
  )
}

/** 归档下载地址：服务端校验后 302 到短期 S3 预签名 URL；导航下载走 ?token= */
export function volumeArchiveDownloadUrl(id: string, archiveId: string) {
  const token = localStorage.getItem('watchman_token') || ''
  const tokParam = token ? `?token=${encodeURIComponent(token)}` : ''
  return `/api/v1/apps/${id}/volume-backups/${archiveId}/download${tokParam}`
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